package main

import (
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"golang.org/x/crypto/ssh"
)

// TunnelManager tracks all active SSH tunnels.
type TunnelManager struct {
	mu      sync.Mutex
	tunnels map[string]*Tunnel
}

// Tunnel represents a native SSH tunnel with local port forwarding.
type Tunnel struct {
	client   *ssh.Client
	listener net.Listener
	done     chan struct{} // closed when the accept loop exits
}

func NewTunnelManager() *TunnelManager {
	return &TunnelManager{
		tunnels: make(map[string]*Tunnel),
	}
}

func tunnelKey(e *Entry) string {
	return e.Host + ":" + e.Tenant
}

// --- Bubbletea messages ---

type tunnelConnectedMsg struct{ key string }
type tunnelErrorMsg struct {
	key string
	err error
}
type tunnelDisconnectedMsg struct{ key string }

// Connect establishes a native SSH tunnel for the given entry.
func (tm *TunnelManager) Connect(entry *Entry) tea.Cmd {
	return func() tea.Msg {
		key := tunnelKey(entry)

		// Establish SSH connection.
		client, err := dialSSH(entry.SSHHost)
		if err != nil {
			return tunnelErrorMsg{key: key, err: fmt.Errorf("ssh: %w", err)}
		}

		// Resolve container IP via the SSH connection.
		ip, err := resolveContainerIP(client, entry.Root, entry.Tenant)
		if err != nil {
			client.Close()
			return tunnelErrorMsg{key: key, err: fmt.Errorf("resolve IP: %w", err)}
		}

		// Start local listener for port forwarding.
		listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", entry.LocalPort))
		if err != nil {
			client.Close()
			return tunnelErrorMsg{key: key, err: fmt.Errorf("listen :%d: %w", entry.LocalPort, err)}
		}

		remoteAddr := net.JoinHostPort(ip, "5432")
		done := make(chan struct{})
		tun := &Tunnel{client: client, listener: listener, done: done}

		// Accept loop: forward local connections through SSH to the remote db.
		go func() {
			defer close(done)
			for {
				local, err := listener.Accept()
				if err != nil {
					return // listener closed
				}
				remote, err := client.Dial("tcp", remoteAddr)
				if err != nil {
					local.Close()
					// SSH connection likely dead; stop accepting.
					listener.Close()
					return
				}
				go forward(local, remote)
			}
		}()

		tm.mu.Lock()
		tm.tunnels[key] = tun
		tm.mu.Unlock()

		entry.ContainerIP = ip
		return tunnelConnectedMsg{key: key}
	}
}

// Disconnect tears down a tunnel.
func (tm *TunnelManager) Disconnect(entry *Entry) tea.Cmd {
	return func() tea.Msg {
		key := tunnelKey(entry)
		tm.mu.Lock()
		tun, ok := tm.tunnels[key]
		if ok {
			delete(tm.tunnels, key)
		}
		tm.mu.Unlock()

		if ok {
			closeTunnel(tun)
		}
		return tunnelDisconnectedMsg{key: key}
	}
}

// MonitorTunnel watches for unexpected SSH connection death.
func (tm *TunnelManager) MonitorTunnel(key string) tea.Cmd {
	return func() tea.Msg {
		tm.mu.Lock()
		tun, ok := tm.tunnels[key]
		tm.mu.Unlock()
		if !ok {
			return nil
		}

		// Block until the SSH connection closes.
		tun.client.Wait()

		tm.mu.Lock()
		_, stillTracked := tm.tunnels[key]
		delete(tm.tunnels, key)
		tm.mu.Unlock()

		// If Disconnect already removed it, this was deliberate.
		if !stillTracked {
			return nil
		}

		// Unexpected death — clean up the listener.
		tun.listener.Close()
		<-tun.done

		return tunnelErrorMsg{key: key, err: fmt.Errorf("SSH connection lost")}
	}
}

// IsAlive checks if a tunnel is still tracked.
func (tm *TunnelManager) IsAlive(key string) bool {
	tm.mu.Lock()
	_, ok := tm.tunnels[key]
	tm.mu.Unlock()
	return ok
}

// DisconnectAll tears down all active tunnels.
func (tm *TunnelManager) DisconnectAll() {
	tm.mu.Lock()
	tunnels := make(map[string]*Tunnel, len(tm.tunnels))
	for k, v := range tm.tunnels {
		tunnels[k] = v
	}
	tm.tunnels = make(map[string]*Tunnel)
	tm.mu.Unlock()

	var wg sync.WaitGroup
	for _, tun := range tunnels {
		wg.Add(1)
		go func(t *Tunnel) {
			defer wg.Done()
			closeTunnel(t)
		}(tun)
	}
	wg.Wait()
}

// closeTunnel shuts down a tunnel's listener and SSH client, then waits
// for the accept loop to exit.
func closeTunnel(tun *Tunnel) {
	tun.listener.Close()
	tun.client.Close()
	<-tun.done
}

// forward copies data bidirectionally between two connections.
func forward(local, remote net.Conn) {
	defer local.Close()
	defer remote.Close()

	errc := make(chan error, 2)
	go func() {
		_, err := io.Copy(local, remote)
		errc <- err
	}()
	go func() {
		_, err := io.Copy(remote, local)
		errc <- err
	}()
	<-errc
}

// resolveContainerIP gets the Docker container IP for the db service
// by running a command over an existing SSH connection.
func resolveContainerIP(client *ssh.Client, root, tenant string) (string, error) {
	script := fmt.Sprintf(
		`cd %s/%s && sudo docker compose ps -q db | xargs sudo docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}'`,
		shellQuote(root), shellQuote(tenant),
	)
	ip, err := runSSHCommand(client, script)
	if err != nil {
		return "", fmt.Errorf("resolve container IP for %s: %w", tenant, err)
	}
	if ip == "" {
		return "", fmt.Errorf("empty container IP for %s — is the db service running?", tenant)
	}
	// If multiple IPs returned (multiple networks), take the first.
	if idx := strings.Index(ip, "\n"); idx >= 0 {
		ip = ip[:idx]
	}
	return ip, nil
}

// resolveContainerIPFresh dials a new SSH connection to resolve the container IP.
// Used during discovery when no persistent connection exists.
func resolveContainerIPFresh(sshHost, root, tenant string) (string, error) {
	client, err := dialSSH(sshHost)
	if err != nil {
		return "", err
	}
	defer client.Close()
	return resolveContainerIP(client, root, tenant)
}

// keepAlive sends periodic keepalive requests to detect dead connections.
func keepAlive(client *ssh.Client, interval time.Duration, stop <-chan struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
			if err != nil {
				return
			}
		}
	}
}
