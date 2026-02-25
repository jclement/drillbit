package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
)

// TunnelManager tracks all active SSH tunnel subprocesses.
type TunnelManager struct {
	mu      sync.Mutex
	tunnels map[string]*Tunnel
}

// Tunnel represents a running SSH tunnel process.
type Tunnel struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
	done   chan error
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

// Connect starts an SSH tunnel for the given entry.
func (tm *TunnelManager) Connect(entry *Entry) tea.Cmd {
	return func() tea.Msg {
		key := tunnelKey(entry)

		// Resolve container IP via SSH.
		ip, err := resolveContainerIP(entry.SSHHost, entry.Root, entry.Tenant)
		if err != nil {
			return tunnelErrorMsg{key: key, err: fmt.Errorf("resolve IP: %w", err)}
		}

		// Start SSH tunnel: ssh -N -L localPort:containerIP:5432 host
		ctx, cancel := context.WithCancel(context.Background())
		forwardSpec := fmt.Sprintf("%d:%s:5432", entry.LocalPort, ip)
		cmd := exec.CommandContext(ctx, "ssh", "-N",
			"-o", "ConnectTimeout=10",
			"-o", "ServerAliveInterval=30",
			"-o", "ServerAliveCountMax=3",
			"-L", forwardSpec, entry.SSHHost)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

		done := make(chan error, 1)

		if err := cmd.Start(); err != nil {
			cancel()
			return tunnelErrorMsg{key: key, err: fmt.Errorf("start ssh: %w", err)}
		}

		go func() {
			done <- cmd.Wait()
		}()

		tun := &Tunnel{cmd: cmd, cancel: cancel, done: done}
		tm.mu.Lock()
		tm.tunnels[key] = tun
		tm.mu.Unlock()

		// Wait briefly to detect immediate failures.
		select {
		case err := <-done:
			tm.mu.Lock()
			delete(tm.tunnels, key)
			tm.mu.Unlock()
			if err != nil {
				return tunnelErrorMsg{key: key, err: fmt.Errorf("ssh failed: %w", err)}
			}
			return tunnelErrorMsg{key: key, err: fmt.Errorf("ssh exited immediately")}
		case <-time.After(2 * time.Second):
			entry.ContainerIP = ip
			return tunnelConnectedMsg{key: key}
		}
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

		if !ok {
			return tunnelDisconnectedMsg{key: key}
		}

		killTunnel(tun, 5*time.Second)
		return tunnelDisconnectedMsg{key: key}
	}
}

// MonitorTunnel watches for unexpected tunnel death.
func (tm *TunnelManager) MonitorTunnel(key string) tea.Cmd {
	return func() tea.Msg {
		tm.mu.Lock()
		tun, ok := tm.tunnels[key]
		tm.mu.Unlock()
		if !ok {
			return nil
		}

		err := <-tun.done
		tm.mu.Lock()
		delete(tm.tunnels, key)
		tm.mu.Unlock()

		if err != nil {
			return tunnelErrorMsg{key: key, err: fmt.Errorf("tunnel died: %w", err)}
		}
		return tunnelDisconnectedMsg{key: key}
	}
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
			killTunnel(t, 3*time.Second)
		}(tun)
	}
	wg.Wait()
}

func killTunnel(tun *Tunnel, timeout time.Duration) {
	tun.cancel()
	if tun.cmd.Process != nil {
		_ = syscall.Kill(-tun.cmd.Process.Pid, syscall.SIGTERM)
	}
	select {
	case <-tun.done:
	case <-time.After(timeout):
		if tun.cmd.Process != nil {
			_ = tun.cmd.Process.Kill()
		}
	}
}

// resolveContainerIP gets the Docker container IP for the db service.
func resolveContainerIP(sshHost, root, tenant string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	script := fmt.Sprintf(
		`cd %s/%s && sudo docker compose ps -q db | xargs sudo docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}'`,
		root, tenant,
	)
	cmd := exec.CommandContext(ctx, "ssh", "-o", "ConnectTimeout=10", sshHost, script)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to resolve container IP for %s/%s: %w", sshHost, tenant, err)
	}

	ip := strings.TrimSpace(string(out))
	if ip == "" {
		return "", fmt.Errorf("empty container IP for %s/%s — is the db service running?", sshHost, tenant)
	}
	return ip, nil
}
