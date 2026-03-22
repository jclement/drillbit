package main

import (
	"fmt"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"golang.org/x/crypto/ssh"
)

// Entry represents a single discovered database container.
type Entry struct {
	Env         string
	Host        string // SSH host alias (for display)
	SSHHost     string // user@host or just host (for SSH commands)
	Container   string // Docker container name
	Image       string // Docker image (postgres, postgis, timescale)
	DBUser      string // postgres user (default: "postgres")
	Password    string // POSTGRES_PASSWORD from container env
	Database    string // database name (default: container name)
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

		for env, hosts := range cfg.Environments() {
			for _, hc := range hosts {
				wg.Add(1)
				go func(env string, hc HostConfig) {
					defer wg.Done()
					sshHost := hc.SSHHost()
					containers, err := discoverHost(sshHost)
					mu.Lock()
					defer mu.Unlock()
					if err != nil {
						errors = append(errors, hostError{host: hc.Name, err: err})
						return
					}
					for _, c := range containers {
						// Check for config overrides
						override := hc.GetOverride(c.name)

						dbUser := c.dbUser
						if override != nil && override.User != "" {
							dbUser = override.User
						} else if dbUser == "" {
							dbUser = "postgres"
						}

						password := c.password
						if override != nil && override.Password != "" {
							password = override.Password
						}

						database := c.database
						if override != nil && override.Database != "" {
							database = override.Database
						} else if database == "" {
							database = c.name
						}

						entries = append(entries, Entry{
							Env:       env,
							Host:      hc.Name,
							SSHHost:   sshHost,
							Container: c.name,
							Image:     c.image,
							DBUser:    dbUser,
							Password:  password,
							Database:  database,
							Status:    StatusReady,
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

type containerInfo struct {
	name     string
	image    string
	dbUser   string
	password string
	database string
}

// discoverHost discovers Postgres containers on a single host via Docker API.
func discoverHost(host string) ([]containerInfo, error) {
	client, err := dialSSH(host)
	if err != nil {
		return nil, fmt.Errorf("ssh to %s: %w", host, err)
	}
	defer client.Close()

	return discoverDockerContainers(client)
}

// discoverDockerContainers queries Docker API directly for Postgres containers.
func discoverDockerContainers(client *ssh.Client) ([]containerInfo, error) {
	script := `
# Find all running containers and filter for postgres-related images
containers=$(sudo docker ps --format '{{.ID}}|{{.Image}}' 2>/dev/null || true)
if [ -z "$containers" ]; then
    exit 0
fi

# Filter for postgres, postgis, timescale images
echo "$containers" | grep -iE 'postgres|postgis|timescale' | while IFS='|' read -r cid image; do
    # Extract name, image, and environment variables
    sudo docker inspect "$cid" --format '{{.Name}}|||'"$image"'|||{{range .Config.Env}}{{println .}}{{end}}' 2>/dev/null || continue
done
`

	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("session: %w", err)
	}
	defer session.Close()

	type result struct {
		out []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		out, err := session.Output(script)
		ch <- result{out, err}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("docker inspect: %w", r.err)
		}
		return parseDockerContainers(r.out), nil
	case <-time.After(30 * time.Second):
		session.Close()
		return nil, fmt.Errorf("timeout querying docker")
	}
}

// parseDockerContainers parses docker inspect output into containerInfo records.
// Format: name|||image|||env1\nenv2\nenv3...
func parseDockerContainers(out []byte) []containerInfo {
	var containers []containerInfo

	blocks := strings.Split(string(out), "|||")
	for i := 0; i < len(blocks); i += 3 {
		if i+2 >= len(blocks) {
			break
		}

		// First part is container name (e.g., /myapp_db_1)
		name := strings.TrimPrefix(strings.TrimSpace(blocks[i]), "/")
		if name == "" {
			continue
		}

		// Second part is image name
		image := strings.TrimSpace(blocks[i+1])

		// Simplify image name for display (postgres:16 -> postgres, postgis/postgis:latest -> postgis)
		imageType := simplifyImageName(image)

		// Third part is environment variables
		envBlock := blocks[i+2]

		// Parse environment variables
		info := containerInfo{
			name:     name,
			image:    imageType,
			dbUser:   "postgres", // default
			database: name,       // default to container name
		}

		for _, line := range strings.Split(envBlock, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "POSTGRES_USER=") {
				info.dbUser = strings.TrimPrefix(line, "POSTGRES_USER=")
			} else if strings.HasPrefix(line, "POSTGRES_PASSWORD=") {
				info.password = strings.TrimPrefix(line, "POSTGRES_PASSWORD=")
			} else if strings.HasPrefix(line, "POSTGRES_DB=") {
				info.database = strings.TrimPrefix(line, "POSTGRES_DB=")
			}
		}

		// Only include if we found a password (prevents showing containers without creds)
		if info.password != "" {
			containers = append(containers, info)
		}
	}

	return containers
}

// simplifyImageName converts full image names to simple types for display.
// Examples: postgres:16 -> postgres, postgis/postgis:latest -> postgis, timescale/timescaledb:2.9 -> timescale
func simplifyImageName(image string) string {
	// Remove tag (everything after :)
	if idx := strings.Index(image, ":"); idx >= 0 {
		image = image[:idx]
	}

	// Handle org/image format (postgis/postgis -> postgis)
	if strings.Contains(image, "/") {
		parts := strings.Split(image, "/")
		base := parts[len(parts)-1]
		// postgis/postgis -> postgis, timescale/timescaledb -> timescale
		if strings.HasPrefix(base, "postgis") {
			return "postgis"
		}
		if strings.HasPrefix(base, "timescale") {
			return "timescale"
		}
		return base
	}

	return image
}
