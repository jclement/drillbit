package main

import (
	"fmt"
	"io"
	"net"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"
	"golang.org/x/crypto/ssh"
)

// TunnelManager tracks all active SSH tunnels and shares connections per host.
type TunnelManager struct {
	mu      sync.Mutex
	tunnels map[string]*Tunnel
	pool    *sshPool
}

// Tunnel represents a single port-forward over a shared SSH connection.
type Tunnel struct {
	sshHost  string       // pool key for Release
	listener net.Listener // local TCP listener
	done     chan struct{} // closed when the accept loop exits
}

func NewTunnelManager() *TunnelManager {
	return &TunnelManager{
		tunnels: make(map[string]*Tunnel),
		pool:    newSSHPool(),
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

// Connect establishes a port-forward for the given entry, reusing an
// existing SSH connection to the host if one is already open.
func (tm *TunnelManager) Connect(entry *Entry) tea.Cmd {
	return func() tea.Msg {
		key := tunnelKey(entry)

		// Acquire a shared SSH connection for this host.
		client, err := tm.pool.Acquire(entry.SSHHost)
		if err != nil {
			return tunnelErrorMsg{key: key, err: fmt.Errorf("ssh: %w", err)}
		}

		// Resolve container IP via the shared connection.
		ip, err := resolveContainerIP(client, entry.Root, entry.Tenant)
		if err != nil {
			tm.pool.Release(entry.SSHHost)
			return tunnelErrorMsg{key: key, err: fmt.Errorf("resolve IP: %w", err)}
		}

		// Start local listener for port forwarding.
		listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", entry.LocalPort))
		if err != nil {
			tm.pool.Release(entry.SSHHost)
			return tunnelErrorMsg{key: key, err: fmt.Errorf("listen :%d: %w", entry.LocalPort, err)}
		}

		remoteAddr := net.JoinHostPort(ip, "5432")
		done := make(chan struct{})
		tun := &Tunnel{sshHost: entry.SSHHost, listener: listener, done: done}

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

// Disconnect tears down a single tunnel and releases its pool reference.
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
			tun.listener.Close()
			<-tun.done
			tm.pool.Release(tun.sshHost)
		}
		return tunnelDisconnectedMsg{key: key}
	}
}

// MonitorTunnel watches for unexpected tunnel death — either the accept
// loop failing or the underlying SSH connection dropping.
func (tm *TunnelManager) MonitorTunnel(key string) tea.Cmd {
	return func() tea.Msg {
		tm.mu.Lock()
		tun, ok := tm.tunnels[key]
		tm.mu.Unlock()
		if !ok {
			return nil
		}

		dead := tm.pool.Dead(tun.sshHost)

		// Wait for either the accept loop to exit or the SSH connection to die.
		select {
		case <-tun.done:
			// Accept loop exited (forward failure or listener closed).
		case <-dead:
			// SSH connection died — close listener to unblock the accept loop.
			tun.listener.Close()
			<-tun.done
		}

		tm.mu.Lock()
		_, stillTracked := tm.tunnels[key]
		delete(tm.tunnels, key)
		tm.mu.Unlock()

		// If Disconnect already removed it, this was deliberate.
		if !stillTracked {
			return nil
		}

		tm.pool.Release(tun.sshHost)
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

// DisconnectAll tears down all active tunnels (used during shutdown).
func (tm *TunnelManager) DisconnectAll() {
	tm.mu.Lock()
	tunnels := make(map[string]*Tunnel, len(tm.tunnels))
	for k, v := range tm.tunnels {
		tunnels[k] = v
	}
	tm.tunnels = make(map[string]*Tunnel)
	tm.mu.Unlock()

	// Close all listeners to unblock accept loops.
	for _, tun := range tunnels {
		tun.listener.Close()
	}

	// Wait for all accept loops to finish.
	for _, tun := range tunnels {
		<-tun.done
	}

	// Force-close all SSH connections.
	tm.pool.CloseAll()
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
