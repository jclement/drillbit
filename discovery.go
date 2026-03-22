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

type hostError struct {
	host string
	err  error
}

// logEntry is a single line in the discovery progress log.
type logEntry struct {
	tag  string // "CONN", "OK", "SCAN", "FIND", "ERR", "" (for sub-items)
	text string
}

// discoverUpdate streams incremental discovery progress to the UI.
type discoverUpdate struct {
	log      *logEntry  // progress log line (nil if entries-only)
	entries  []Entry    // new entries from a completed host
	hostErr  *hostError // host error if any
	hostDone bool       // true when a host finishes (success or fail)
	done     bool       // true when all discovery is complete
	next     tea.Cmd    // command to read next event from channel
}

// nextDiscoverEvent reads one event from the channel and wraps it with
// a next command to continue reading.
func nextDiscoverEvent(ch <-chan discoverUpdate) tea.Cmd {
	return func() tea.Msg {
		u, ok := <-ch
		if !ok {
			return discoverUpdate{done: true}
		}
		u.next = nextDiscoverEvent(ch)
		return u
	}
}

// discoverAll runs discovery across all configured hosts concurrently,
// streaming progress events through a channel.
func discoverAll(cfg *Config) tea.Cmd {
	ch := make(chan discoverUpdate, 50)

	go func() {
		var wg sync.WaitGroup
		for _, hc := range cfg.AllHosts() {
			wg.Add(1)
			go func(hc HostConfig) {
				defer wg.Done()
				discoverHostStreaming(hc, ch)
			}(hc)
		}
		wg.Wait()
		close(ch)
	}()

	return nextDiscoverEvent(ch)
}

// discoverHostStreaming discovers containers on a host, sending progress to ch.
func discoverHostStreaming(hc HostConfig, ch chan<- discoverUpdate) {
	sshHost := hc.SSHHost()

	ch <- discoverUpdate{log: &logEntry{tag: "CONN", text: fmt.Sprintf("Establishing link to %s...", hc.Name)}}

	client, err := dialSSH(sshHost)
	if err != nil {
		ch <- discoverUpdate{
			log:      &logEntry{tag: "ERR", text: fmt.Sprintf("%s — %v", hc.Name, err)},
			hostErr:  &hostError{host: hc.Name, err: err},
			hostDone: true,
		}
		return
	}
	defer client.Close()

	ch <- discoverUpdate{log: &logEntry{tag: "OK", text: fmt.Sprintf("%s — secure channel open", hc.Name)}}
	ch <- discoverUpdate{log: &logEntry{tag: "SCAN", text: fmt.Sprintf("%s — interrogating docker daemon...", hc.Name)}}

	containers, err := discoverDockerContainers(client)
	if err != nil {
		ch <- discoverUpdate{
			log:      &logEntry{tag: "ERR", text: fmt.Sprintf("%s — %v", hc.Name, err)},
			hostErr:  &hostError{host: hc.Name, err: err},
			hostDone: true,
		}
		return
	}

	// Report findings.
	n := len(containers)
	word := "targets"
	if n == 1 {
		word = "target"
	}
	if n == 0 {
		ch <- discoverUpdate{
			log:      &logEntry{tag: "SCAN", text: fmt.Sprintf("%s — no postgres targets", hc.Name)},
			hostDone: true,
		}
		return
	}
	ch <- discoverUpdate{log: &logEntry{tag: "FIND", text: fmt.Sprintf("%s — %d %s acquired", hc.Name, n, word)}}

	// Build entries with overrides.
	var entries []Entry
	for _, c := range containers {
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
			Env:       hc.Env,
			Host:      hc.Name,
			SSHHost:   sshHost,
			Container: c.name,
			Image:     c.image,
			DBUser:    dbUser,
			Password:  password,
			Database:  database,
			Status:    StatusReady,
		})

		ch <- discoverUpdate{log: &logEntry{tag: "", text: fmt.Sprintf("  %s/%s \u2190 %s", hc.Name, c.name, c.image)}}
	}

	ch <- discoverUpdate{entries: entries, hostDone: true}
}

type containerInfo struct {
	name     string
	image    string
	dbUser   string
	password string
	database string
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
    echo "%%%REC%%%"
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
// Each record is delimited by %%%REC%%%, and fields within a record by |||.
// Format per record: name|||image|||env1\nenv2\nenv3...
func parseDockerContainers(out []byte) []containerInfo {
	var containers []containerInfo

	records := strings.Split(string(out), "%%%REC%%%")
	for _, rec := range records {
		rec = strings.TrimSpace(rec)
		if rec == "" {
			continue
		}

		parts := strings.SplitN(rec, "|||", 3)
		if len(parts) < 3 {
			continue
		}

		// First part is container name (e.g., /myapp_db_1)
		name := strings.TrimPrefix(strings.TrimSpace(parts[0]), "/")
		if name == "" {
			continue
		}

		// Second part is image name
		imageType := simplifyImageName(strings.TrimSpace(parts[1]))

		// Third part is environment variables
		info := containerInfo{
			name:     name,
			image:    imageType,
			dbUser:   "postgres", // default
			database: name,       // default to container name
		}

		for _, line := range strings.Split(parts[2], "\n") {
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
