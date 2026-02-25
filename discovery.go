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
	Host        string // SSH host alias (for display)
	SSHHost     string // user@host or just host (for SSH commands)
	Root        string // remote path, e.g. /docker
	Tenant      string
	DBUser      string // postgres user (default: "postgres")
	Password    string // POSTGRES_PASSWORD from .env
	Database    string // database name (default: tenant name)
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
					sshHost := hc.SSHHost()
					tenants, err := discoverHost(sshHost, hc.Root)
					mu.Lock()
					defer mu.Unlock()
					if err != nil {
						errors = append(errors, hostError{host: hc.Host, err: err})
						return
					}
					for _, t := range tenants {
						dbUser := t.dbUser
						if dbUser == "" {
							dbUser = "postgres"
						}
						database := t.database
						if database == "" {
							database = t.name
						}
						entries = append(entries, Entry{
							Env:      env,
							Host:     hc.Host,
							SSHHost:  sshHost,
							Root:     hc.Root,
							Tenant:   t.name,
							DBUser:   dbUser,
							Password: t.password,
							Database: database,
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
	dbUser   string
	password string
	database string
}

// discoverHost enumerates tenant directories on a single host.
// Only returns tenants where the db container is currently running.
func discoverHost(host, root string) ([]tenantInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Single SSH command that:
	// 1. Finds dirs with a compose file containing a db service
	// 2. Checks the db container is actually running
	// 3. Extracts POSTGRES_USER, POSTGRES_PASSWORD, POSTGRES_DB from .env and compose
	script := fmt.Sprintf(`
for dir in %s/*/; do
    name=$(basename "$dir")
    compose=""
    for f in "$dir/docker-compose.yml" "$dir/docker-compose.yaml" "$dir/compose.yml" "$dir/compose.yaml"; do
        [ -f "$f" ] && compose="$f" && break
    done
    [ -z "$compose" ] && continue
    if ! grep -qE '^\s+db:' "$compose" 2>/dev/null; then
        continue
    fi

    # Check if db container is running
    cid=$(cd "$dir" && sudo docker compose ps -q db 2>/dev/null)
    [ -z "$cid" ] && continue
    running=$(sudo docker inspect -f '{{.State.Running}}' "$cid" 2>/dev/null)
    [ "$running" != "true" ] && continue

    # Extract credentials: check .env first, then compose environment section
    pw=""
    user=""
    dbname=""
    if [ -f "$dir/.env" ]; then
        pw=$(grep -oP 'POSTGRES_PASSWORD=\K.*' "$dir/.env" 2>/dev/null || true)
        user=$(grep -oP 'POSTGRES_USER=\K.*' "$dir/.env" 2>/dev/null || true)
        dbname=$(grep -oP 'POSTGRES_DB=\K.*' "$dir/.env" 2>/dev/null || true)
    fi
    # Fall back to compose file environment
    if [ -z "$user" ]; then
        user=$(grep -oP 'POSTGRES_USER[=:]\s*\K\S+' "$compose" 2>/dev/null | head -1 || true)
    fi
    if [ -z "$pw" ]; then
        pw=$(grep -oP 'POSTGRES_PASSWORD[=:]\s*\K\S+' "$compose" 2>/dev/null | head -1 || true)
    fi
    if [ -z "$dbname" ]; then
        dbname=$(grep -oP 'POSTGRES_DB[=:]\s*\K\S+' "$compose" 2>/dev/null | head -1 || true)
    fi

    echo "$name|$user|$pw|$dbname"
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
		parts := strings.SplitN(line, "|", 4)
		t := tenantInfo{name: parts[0]}
		if len(parts) >= 2 {
			t.dbUser = parts[1]
		}
		if len(parts) >= 3 {
			t.password = parts[2]
		}
		if len(parts) >= 4 {
			t.database = parts[3]
		}
		tenants = append(tenants, t)
	}
	return tenants, nil
}
