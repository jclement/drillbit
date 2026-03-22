package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	sshconfig "github.com/kevinburke/ssh_config"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// sshPool manages shared SSH connections, one per host.
type sshPool struct {
	mu    sync.Mutex
	conns map[string]*pooledConn
}

type pooledConn struct {
	client *ssh.Client
	refs   int
	dead   chan struct{} // closed when the connection dies
}

func newSSHPool() *sshPool {
	return &sshPool{conns: make(map[string]*pooledConn)}
}

// Acquire returns a shared SSH client for the host, dialing a new connection
// if one doesn't exist or the existing one is dead. Caller must Release when done.
func (p *sshPool) Acquire(sshHost string) (*ssh.Client, error) {
	p.mu.Lock()
	if pc, ok := p.conns[sshHost]; ok {
		select {
		case <-pc.dead:
			// Connection is dead, will dial a new one below.
			delete(p.conns, sshHost)
		default:
			pc.refs++
			p.mu.Unlock()
			return pc.client, nil
		}
	}
	p.mu.Unlock()

	// Dial without holding the lock (may take seconds).
	client, err := dialSSH(sshHost)
	if err != nil {
		return nil, err
	}

	dead := make(chan struct{})
	go func() {
		client.Wait()
		close(dead)
	}()
	go keepAlive(client, 30*time.Second, dead)

	p.mu.Lock()
	defer p.mu.Unlock()

	// Another goroutine may have raced us and already connected.
	if pc, ok := p.conns[sshHost]; ok {
		select {
		case <-pc.dead:
			// Still dead, replace with ours.
		default:
			// They won the race — use theirs, discard ours.
			pc.refs++
			client.Close()
			return pc.client, nil
		}
	}

	p.conns[sshHost] = &pooledConn{client: client, refs: 1, dead: dead}
	return client, nil
}

// Release decrements the reference count for a host's connection.
// When refs reach zero the SSH connection is closed.
func (p *sshPool) Release(sshHost string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pc, ok := p.conns[sshHost]
	if !ok {
		return
	}
	pc.refs--
	if pc.refs <= 0 {
		pc.client.Close()
		delete(p.conns, sshHost)
	}
}

// Dead returns a channel that is closed when the host's SSH connection dies.
// If no connection exists, returns an already-closed channel.
func (p *sshPool) Dead(sshHost string) <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pc, ok := p.conns[sshHost]; ok {
		return pc.dead
	}
	ch := make(chan struct{})
	close(ch)
	return ch
}

// CloseAll forcibly closes every pooled connection (used during shutdown).
func (p *sshPool) CloseAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for host, pc := range p.conns {
		pc.client.Close()
		delete(p.conns, host)
	}
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

// --- SSH dial and helpers ---

// dialSSH establishes an SSH connection to the given host.
// sshHost can be "user@host" or just "host". SSH config (~/.ssh/config)
// is consulted for HostName, User, Port, IdentityFile, and IdentityAgent.
func dialSSH(sshHost string) (*ssh.Client, error) {
	var alias, explicitUser string
	if at := strings.LastIndex(sshHost, "@"); at >= 0 {
		explicitUser = sshHost[:at]
		alias = sshHost[at+1:]
	} else {
		alias = sshHost
	}

	hostname := sshconfig.Get(alias, "HostName")
	if hostname == "" || hostname == alias {
		hostname = alias
	}

	user := explicitUser
	if user == "" {
		if u := sshconfig.Get(alias, "User"); u != "" {
			user = u
		}
	}
	if user == "" {
		user = os.Getenv("USER")
	}

	port := 22
	if p := sshconfig.Get(alias, "Port"); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			port = n
		}
	}

	// Connect to SSH agent (kept alive through the Dial handshake).
	var agentConn net.Conn
	agentSocket := expandTilde(sshconfig.Get(alias, "IdentityAgent"))
	if agentSocket == "" {
		agentSocket = os.Getenv("SSH_AUTH_SOCK")
	}
	if agentSocket != "" {
		agentConn, _ = net.Dial("unix", agentSocket)
	}

	// Build auth methods.
	var authMethods []ssh.AuthMethod

	if agentConn != nil {
		agentClient := agent.NewClient(agentConn)
		authMethods = append(authMethods, ssh.PublicKeysCallback(agentClient.Signers))
	}

	// Key file auth.
	identitiesOnly := strings.EqualFold(sshconfig.Get(alias, "IdentitiesOnly"), "yes")
	identityFiles := sshconfig.GetAll(alias, "IdentityFile")

	// Filter empty defaults and expand paths.
	var keyPaths []string
	for _, f := range identityFiles {
		f = expandTilde(strings.TrimSpace(f))
		if f != "" {
			keyPaths = append(keyPaths, f)
		}
	}

	if len(keyPaths) == 0 && !identitiesOnly {
		home, _ := os.UserHomeDir()
		keyPaths = []string{
			filepath.Join(home, ".ssh", "id_ed25519"),
			filepath.Join(home, ".ssh", "id_ecdsa"),
			filepath.Join(home, ".ssh", "id_rsa"),
		}
	}

	for _, kf := range keyPaths {
		if signer := loadKeyFile(kf); signer != nil {
			authMethods = append(authMethods, ssh.PublicKeys(signer))
		}
	}

	if len(authMethods) == 0 {
		if agentConn != nil {
			agentConn.Close()
		}
		return nil, fmt.Errorf("no SSH auth methods available for %s", sshHost)
	}

	hostKeyCallback, err := hostKeyTOFU()
	if err != nil {
		if agentConn != nil {
			agentConn.Close()
		}
		return nil, fmt.Errorf("host key setup: %w", err)
	}

	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            authMethods,
		HostKeyCallback: hostKeyCallback,
		Timeout:         10 * time.Second,
	}

	addr := net.JoinHostPort(hostname, strconv.Itoa(port))
	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		if agentConn != nil {
			agentConn.Close()
		}
		return nil, fmt.Errorf("ssh dial %s (%s): %w", alias, addr, err)
	}

	// Keep agent connection alive for the lifetime of the SSH client —
	// the agent may be needed for re-keying on long-lived connections.
	if agentConn != nil {
		go func() {
			client.Wait()
			agentConn.Close()
		}()
	}

	return client, nil
}

// hostKeyTOFU returns a host key callback that trusts on first use:
// - Known hosts from ~/.ssh/known_hosts are verified normally
// - Unknown hosts are auto-accepted and appended to ~/.ssh/known_hosts
// - Changed keys (potential MITM) are rejected with an error
func hostKeyTOFU() (ssh.HostKeyCallback, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("home dir: %w", err)
	}

	knownHostsFile := filepath.Join(home, ".ssh", "known_hosts")

	// Ensure the file exists.
	if _, err := os.Stat(knownHostsFile); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(knownHostsFile), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(knownHostsFile, nil, 0o644); err != nil {
			return nil, err
		}
	}

	cb, err := knownhosts.New(knownHostsFile)
	if err != nil {
		return nil, fmt.Errorf("parsing known_hosts: %w", err)
	}

	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := cb(hostname, remote, key)
		if err == nil {
			return nil // known and matches
		}

		// If the key is wrong (changed), reject — potential MITM.
		var keyErr *knownhosts.KeyError
		if errors.As(err, &keyErr) && len(keyErr.Want) > 0 {
			return fmt.Errorf("HOST KEY CHANGED for %s — possible MITM attack. Remove old key from %s", hostname, knownHostsFile)
		}

		// Unknown host — trust on first use, append to known_hosts.
		line := knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key)
		f, err := os.OpenFile(knownHostsFile, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Errorf("saving host key: %w", err)
		}
		defer f.Close()
		if _, err := fmt.Fprintln(f, line); err != nil {
			return fmt.Errorf("writing host key: %w", err)
		}

		return nil // accepted
	}, nil
}

// loadKeyFile attempts to load and parse an SSH private key file.
func loadKeyFile(path string) ssh.Signer {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil
	}
	return signer
}

// expandTilde expands a leading ~ to the user's home directory.
func expandTilde(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}

// shellQuote quotes a string for safe interpolation into a remote shell command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// runSSHCommand executes a command on an existing SSH connection with a timeout.
func runSSHCommand(client *ssh.Client, cmd string) (string, error) {
	session, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	defer session.Close()

	type result struct {
		out []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		out, err := session.Output(cmd)
		ch <- result{out, err}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			return "", fmt.Errorf("run command: %w", r.err)
		}
		return strings.TrimSpace(string(r.out)), nil
	case <-time.After(30 * time.Second):
		session.Close()
		return "", fmt.Errorf("command timed out after 30s")
	}
}
