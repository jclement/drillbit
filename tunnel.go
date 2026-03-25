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

// TunnelManager tracks all active SSH tunnels, shares connections per host,
// and automatically reconnects tunnels that die — even when the Bubbletea
// event loop is blocked (e.g. while an interactive SQL client is running).
type TunnelManager struct {
	mu           sync.Mutex
	tunnels      map[string]*Tunnel
	reconnecting map[string]bool // keys with background reconnection in progress
	pool         *sshPool
	stop         chan struct{} // closed on shutdown to stop all monitor goroutines
}

// Tunnel represents a single port-forward over a shared SSH connection.
type Tunnel struct {
	sshHost   string       // pool key for Release
	container string       // container name for IP resolution on reconnect
	localPort uint16       // local listen port (preserved across reconnects)
	listener  net.Listener // local TCP listener
	done      chan struct{} // closed when the accept loop exits
}

func NewTunnelManager() *TunnelManager {
	return &TunnelManager{
		tunnels:      make(map[string]*Tunnel),
		reconnecting: make(map[string]bool),
		pool:         newSSHPool(),
		stop:         make(chan struct{}),
	}
}

func tunnelKey(e *Entry) string {
	return e.Host + ":" + e.Container
}

// --- Bubbletea messages ---

type tunnelConnectedMsg struct{ key string }
type tunnelErrorMsg struct {
	key string
	err error
}
type tunnelDisconnectedMsg struct{ key string }

// --- Tunnel status (for UI health checks) ---

// TunnelStatus represents the lifecycle state of a managed tunnel.
type TunnelStatus int

const (
	TunnelNone         TunnelStatus = iota // not tracked
	TunnelAlive                            // connected and forwarding
	TunnelReconnecting                     // background reconnection in progress
)

// Status returns the current lifecycle state of a tunnel.
func (tm *TunnelManager) Status(key string) TunnelStatus {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if _, ok := tm.tunnels[key]; ok {
		return TunnelAlive
	}
	if tm.reconnecting[key] {
		return TunnelReconnecting
	}
	return TunnelNone
}

// IsAlive checks if a tunnel is connected and forwarding.
func (tm *TunnelManager) IsAlive(key string) bool {
	return tm.Status(key) == TunnelAlive
}

// --- Tunnel setup (shared between Connect and reconnect) ---

// setupTunnel creates a port-forward tunnel. It acquires an SSH connection,
// resolves the container IP, starts a local listener, and launches the
// accept/forward loop. On success the caller is responsible for eventually
// closing the tunnel and releasing the pool reference.
func (tm *TunnelManager) setupTunnel(sshHost, container string, localPort uint16) (*Tunnel, string, error) {
	client, err := tm.pool.Acquire(sshHost)
	if err != nil {
		return nil, "", fmt.Errorf("ssh: %w", err)
	}

	ip, err := resolveContainerIP(client, container)
	if err != nil {
		tm.pool.Release(sshHost)
		return nil, "", fmt.Errorf("resolve IP: %w", err)
	}

	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
	if err != nil {
		tm.pool.Release(sshHost)
		return nil, "", fmt.Errorf("listen :%d: %w", localPort, err)
	}

	remoteAddr := net.JoinHostPort(ip, "5432")
	done := make(chan struct{})
	tun := &Tunnel{
		sshHost:   sshHost,
		container: container,
		localPort: localPort,
		listener:  listener,
		done:      done,
	}

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

	return tun, ip, nil
}

// --- Connect / Disconnect ---

// Connect establishes a port-forward for the given entry and starts a
// background monitor goroutine that will automatically reconnect the
// tunnel if it dies.
func (tm *TunnelManager) Connect(entry *Entry) tea.Cmd {
	return func() tea.Msg {
		key := tunnelKey(entry)

		// If already alive (e.g. background monitor reconnected), skip.
		if tm.Status(key) == TunnelAlive {
			return tunnelConnectedMsg{key: key}
		}

		tun, ip, err := tm.setupTunnel(entry.SSHHost, entry.Container, entry.LocalPort)
		if err != nil {
			return tunnelErrorMsg{key: key, err: err}
		}

		tm.mu.Lock()
		// If someone raced us (background reconnect finished), tear down ours.
		if _, exists := tm.tunnels[key]; exists {
			tm.mu.Unlock()
			tun.listener.Close()
			<-tun.done
			tm.pool.Release(tun.sshHost)
			return tunnelConnectedMsg{key: key}
		}
		delete(tm.reconnecting, key)
		tm.tunnels[key] = tun
		tm.mu.Unlock()

		// Start background monitor for auto-reconnection.
		go tm.monitor(key)

		entry.ContainerIP = ip
		return tunnelConnectedMsg{key: key}
	}
}

// Disconnect tears down a single tunnel, stops its monitor, and releases
// its pool reference.
func (tm *TunnelManager) Disconnect(entry *Entry) tea.Cmd {
	return func() tea.Msg {
		key := tunnelKey(entry)
		tm.mu.Lock()
		tun, ok := tm.tunnels[key]
		if ok {
			delete(tm.tunnels, key)
		}
		// Clear reconnecting flag so the monitor doesn't revive this tunnel.
		delete(tm.reconnecting, key)
		tm.mu.Unlock()

		if ok {
			tun.listener.Close()
			<-tun.done
			tm.pool.Release(tun.sshHost)
		}
		return tunnelDisconnectedMsg{key: key}
	}
}

// DisconnectAll tears down all active tunnels, stops all monitors, and
// closes all SSH connections. Used during shutdown.
func (tm *TunnelManager) DisconnectAll() {
	// Signal all monitor goroutines to stop.
	close(tm.stop)

	tm.mu.Lock()
	tunnels := make(map[string]*Tunnel, len(tm.tunnels))
	for k, v := range tm.tunnels {
		tunnels[k] = v
	}
	tm.tunnels = make(map[string]*Tunnel)
	tm.reconnecting = make(map[string]bool)
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

// --- Background monitoring and auto-reconnect ---

// monitor watches a tunnel and automatically reconnects it if it dies.
// It runs until the tunnel is deliberately disconnected or shutdown occurs.
func (tm *TunnelManager) monitor(key string) {
	for {
		// Get the current tunnel for this key.
		tm.mu.Lock()
		tun, ok := tm.tunnels[key]
		tm.mu.Unlock()
		if !ok {
			return // deliberately disconnected or not tracked
		}

		dead := tm.pool.Dead(tun.sshHost)

		// Wait for tunnel death, deliberate disconnect, or shutdown.
		select {
		case <-tm.stop:
			return
		case <-tun.done:
			// Accept loop exited (forward failure or listener closed).
		case <-dead:
			// SSH connection died — close listener to unblock accept loop.
			tun.listener.Close()
			<-tun.done
		}

		// Check if this was a deliberate disconnect (Disconnect removed it).
		tm.mu.Lock()
		current, stillTracked := tm.tunnels[key]
		if !stillTracked || current != tun {
			// Disconnect already handled cleanup, or tunnel was replaced.
			tm.mu.Unlock()
			return
		}
		// Remove the dead tunnel and mark as reconnecting.
		delete(tm.tunnels, key)
		tm.reconnecting[key] = true
		tm.mu.Unlock()

		tm.pool.Release(tun.sshHost)

		// Attempt reconnection with backoff.
		if !tm.reconnectWithBackoff(key, tun) {
			tm.mu.Lock()
			delete(tm.reconnecting, key)
			tm.mu.Unlock()
			return
		}
		// Successfully reconnected — loop back to monitor the new tunnel.
	}
}

// reconnectWithBackoff attempts to re-establish a dead tunnel with
// exponential backoff. Returns true if the tunnel was reconnected
// (either by us or by a concurrent Connect call).
func (tm *TunnelManager) reconnectWithBackoff(key string, old *Tunnel) bool {
	backoff := 2 * time.Second
	const maxBackoff = 30 * time.Second
	const maxAttempts = 10

	for attempt := range maxAttempts {
		// Back off before retrying (skip on first attempt).
		if attempt > 0 {
			select {
			case <-tm.stop:
				return false
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, maxBackoff)
		}

		// Check for shutdown.
		select {
		case <-tm.stop:
			return false
		default:
		}

		// Check if Disconnect was called or someone else reconnected.
		tm.mu.Lock()
		if !tm.reconnecting[key] {
			// Disconnect was called — stop trying.
			tm.mu.Unlock()
			return false
		}
		if _, exists := tm.tunnels[key]; exists {
			// Someone else (e.g. UI Connect) already reconnected.
			delete(tm.reconnecting, key)
			tm.mu.Unlock()
			return true
		}
		tm.mu.Unlock()

		newTun, _, err := tm.setupTunnel(old.sshHost, old.container, old.localPort)
		if err != nil {
			continue // retry
		}

		tm.mu.Lock()
		// Final check: Disconnect may have been called while we were setting up.
		if !tm.reconnecting[key] {
			tm.mu.Unlock()
			newTun.listener.Close()
			<-newTun.done
			tm.pool.Release(newTun.sshHost)
			return false
		}
		// Check if someone else won the race.
		if _, exists := tm.tunnels[key]; exists {
			tm.mu.Unlock()
			newTun.listener.Close()
			<-newTun.done
			tm.pool.Release(newTun.sshHost)
			delete(tm.reconnecting, key)
			return true
		}
		tm.tunnels[key] = newTun
		delete(tm.reconnecting, key)
		tm.mu.Unlock()
		return true
	}

	return false // gave up after max attempts
}

// --- Helpers ---

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

// resolveContainerIP gets the Docker container IP by name using docker inspect.
func resolveContainerIP(client *ssh.Client, containerName string) (string, error) {
	script := fmt.Sprintf(
		`sudo docker inspect %s -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}'`,
		shellQuote(containerName),
	)
	ip, err := runSSHCommand(client, script)
	if err != nil {
		return "", fmt.Errorf("resolve container IP for %s: %w", containerName, err)
	}
	if ip == "" {
		return "", fmt.Errorf("empty container IP for %s — is the container running?", containerName)
	}
	// If multiple IPs returned (multiple networks), take the first.
	if idx := strings.Index(ip, "\n"); idx >= 0 {
		ip = ip[:idx]
	}
	return ip, nil
}
