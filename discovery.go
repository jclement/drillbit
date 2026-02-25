package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
)

// Entry represents a single discovered database.
type Entry struct {
	Env         string
	Host        string
	Root        string // remote path, e.g. /docker
	Tenant      string
	Password    string // POSTGRES_PASSWORD from .env
	ContainerIP string // resolved at connect time
	LocalPort   uint16
	Status      Status
	Error       string
}

// Status represents the connection state of a tunnel.
type Status int

const (
	StatusReady      Status = iota
	StatusConnecting
	StatusConnected
	StatusError
)

func (s Status) String() string {
	switch s {
	case StatusReady:
		return "Ready"
	case StatusConnecting:
		return "Connecting..."
	case StatusConnected:
		return "Connected"
	case StatusError:
		return "Error"
	default:
		return "Unknown"
	}
}

// discoverMsg is sent when discovery completes.
type discoverMsg struct {
	entries []Entry
	errors  []hostError
}

type hostError struct {
	host string
	err  error
}

// discoverAll runs discovery across all configured hosts concurrently.
func discoverAll(cfg *Config) tea.Cmd {
	return func() tea.Msg {
		var (
			mu      sync.Mutex
			entries []Entry
			errors  []hostError
			wg      sync.WaitGroup
		)

		for env, hosts := range cfg.Environments {
			for _, hc := range hosts {
				wg.Add(1)
				go func(env string, hc HostConfig) {
					defer wg.Done()
					tenants, err := discoverHost(hc.Host, hc.Root)
					mu.Lock()
					defer mu.Unlock()
					if err != nil {
						errors = append(errors, hostError{host: hc.Host, err: err})
						return
					}
					for _, t := range tenants {
						entries = append(entries, Entry{
							Env:      env,
							Host:     hc.Host,
							Root:     hc.Root,
							Tenant:   t.name,
							Password: t.password,
							Status:   StatusReady,
						})
					}
				}(env, hc)
			}
		}

		wg.Wait()
		AssignPorts(entries)
		return discoverMsg{entries: entries, errors: errors}
	}
}

type tenantInfo struct {
	name     string
	password string
}

// discoverHost enumerates tenant directories on a single host.
func discoverHost(host, root string) ([]tenantInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Single SSH command: find dirs with docker-compose.yml containing a db service,
	// and extract POSTGRES_PASSWORD from .env.
	script := fmt.Sprintf(`
for dir in %s/*/; do
    name=$(basename "$dir")
    compose=""
    for f in "$dir/docker-compose.yml" "$dir/docker-compose.yaml" "$dir/compose.yml" "$dir/compose.yaml"; do
        [ -f "$f" ] && compose="$f" && break
    done
    [ -z "$compose" ] && continue
    if grep -qE '^\s+db:' "$compose" 2>/dev/null; then
        pw=""
        if [ -f "$dir/.env" ]; then
            pw=$(grep -oP 'POSTGRES_PASSWORD=\K.*' "$dir/.env" 2>/dev/null || true)
        fi
        echo "$name|$pw"
    fi
done
`, root)

	cmd := exec.CommandContext(ctx, "ssh", "-o", "ConnectTimeout=10", host, "bash", "-c", script)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ssh to %s: %w", host, err)
	}

	var tenants []tenantInfo
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 2)
		t := tenantInfo{name: parts[0]}
		if len(parts) == 2 {
			t.password = parts[1]
		}
		tenants = append(tenants, t)
	}
	return tenants, nil
}
