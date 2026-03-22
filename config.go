package main

import (
	"fmt"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"
)

// Config is the top-level configuration for DrillBit.
type Config struct {
	Hosts []HostConfig `yaml:"hosts"`
}

// HostConfig represents a single SSH host with optional database overrides.
type HostConfig struct {
	Name      string              `yaml:"name"`
	User      string              `yaml:"user,omitempty"`
	Env       string              `yaml:"env,omitempty"`
	Databases []DatabaseOverride  `yaml:"databases,omitempty"`
}

// DatabaseOverride allows per-database configuration.
type DatabaseOverride struct {
	Container string `yaml:"container"`
	Auto      bool   `yaml:"auto,omitempty"`
	User      string `yaml:"user,omitempty"`
	Password  string `yaml:"password,omitempty"`
	Database  string `yaml:"database,omitempty"`
}

// SSHHost returns "user@name" if user is set, otherwise just "name".
func (hc HostConfig) SSHHost() string {
	if hc.User != "" {
		return hc.User + "@" + hc.Name
	}
	return hc.Name
}

// GetOverride returns the override for a specific container, or nil if none exists.
func (hc HostConfig) GetOverride(container string) *DatabaseOverride {
	for i := range hc.Databases {
		if hc.Databases[i].Container == container {
			return &hc.Databases[i]
		}
	}
	return nil
}

// Autoconnect returns a list of containers marked with auto:true.
func (cfg *Config) Autoconnect() []AutoconnectEntry {
	var entries []AutoconnectEntry
	for _, host := range cfg.Hosts {
		for _, db := range host.Databases {
			if db.Auto {
				entries = append(entries, AutoconnectEntry{
					Host:      host.Name,
					Container: db.Container,
				})
			}
		}
	}
	return entries
}

// AutoconnectEntry is a [host, container] pair to auto-connect on startup.
type AutoconnectEntry struct {
	Host      string
	Container string
}

func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	return filepath.Join(home, ".config", "drillbit", "config.yaml")
}

// LoadConfig reads and parses the YAML config file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing YAML config: %w", err)
	}

	if len(cfg.Hosts) == 0 {
		return nil, fmt.Errorf("config has no hosts defined")
	}

	return &cfg, nil
}

// SaveConfig writes the config back to disk as YAML.
func SaveConfig(cfg *Config, path string) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}
	return os.WriteFile(path, data, 0o600)
}

// ScaffoldConfig creates a documented example config file.
func ScaffoldConfig(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	content := `# DrillBit Configuration
hosts:
  - name: prod-server-1
    user: deploy
    env: prod                              # environment label (optional)
    databases:
      - container: myapp_db_1
        auto: true
      - container: otherapp_db_1
        auto: false
        password: custom-override-password  # optional override

  - name: prod-server-2
    env: prod
    # No databases configured - will discover all on this host

  - name: test-server-1
    env: test
    databases:
      - container: testapp_db_1
        auto: true
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	return nil
}
