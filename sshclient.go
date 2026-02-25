package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	sshconfig "github.com/kevinburke/ssh_config"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

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

	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}

	addr := net.JoinHostPort(hostname, strconv.Itoa(port))
	client, err := ssh.Dial("tcp", addr, cfg)

	// Agent connection only needed during handshake.
	if agentConn != nil {
		agentConn.Close()
	}

	if err != nil {
		return nil, fmt.Errorf("ssh dial %s (%s): %w", alias, addr, err)
	}
	return client, nil
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

// runSSHCommand executes a command on an existing SSH connection.
func runSSHCommand(client *ssh.Client, cmd string) (string, error) {
	session, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	defer session.Close()

	out, err := session.Output(cmd)
	if err != nil {
		return "", fmt.Errorf("run command: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
