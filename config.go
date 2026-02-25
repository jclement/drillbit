package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config is the top-level configuration for DrillBit.
type Config struct {
	Environments map[string][]HostConfig  `json:"-"`
	Autoconnect  []AutoconnectEntry       `json:"autoconnect"`
	Overrides    map[string]EntryOverride `json:"overrides"`
}

// HostConfig represents a single SSH host with its settings.
type HostConfig struct {
	Host string `json:"host"`
	User string `json:"user"` // SSH user (optional, uses ssh config default if empty)
	Root string `json:"root"`
}

// SSHHost returns "user@host" if user is set, otherwise just "host".
func (hc HostConfig) SSHHost() string {
	if hc.User != "" {
		return hc.User + "@" + hc.Host
	}
	return hc.Host
}

// EntryOverride stores user-edited values for a specific host:tenant.
type EntryOverride struct {
	DBUser   string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
	Database string `json:"database,omitempty"`
}

// AutoconnectEntry is a [host, tenant] pair to auto-connect on startup.
type AutoconnectEntry struct {
	Host   string
	Tenant string
}

// EnvType classifies an environment for color coding.
type EnvType int

const (
	EnvProd EnvType = iota
	EnvTest
	EnvOther
)

const defaultRoot = "/docker"

// ClassifyEnv determines the environment type for styling.
func ClassifyEnv(name string) EnvType {
	lower := strings.ToLower(name)
	switch {
	case strings.HasPrefix(lower, "prod"):
		return EnvProd
	case strings.HasPrefix(lower, "test"),
		strings.HasPrefix(lower, "stag"),
		strings.HasPrefix(lower, "dev"):
		return EnvTest
	default:
		return EnvOther
	}
}

func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	return filepath.Join(home, ".config", "drillbit", "config.json")
}

// LoadConfig reads and parses the config file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	// Parse the raw JSON structure.
	var raw struct {
		Environments map[string][]json.RawMessage `json:"environments"`
		Autoconnect  [][]string                   `json:"autoconnect"`
		Overrides    map[string]EntryOverride      `json:"overrides"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	if len(raw.Environments) == 0 {
		return nil, fmt.Errorf("config has no environments defined")
	}

	cfg := &Config{
		Environments: make(map[string][]HostConfig),
		Overrides:    raw.Overrides,
	}
	if cfg.Overrides == nil {
		cfg.Overrides = make(map[string]EntryOverride)
	}

	// Parse each host entry — can be a string or an object.
	for env, hosts := range raw.Environments {
		for _, raw := range hosts {
			hc, err := parseHostConfig(raw)
			if err != nil {
				return nil, fmt.Errorf("environment %q: %w", env, err)
			}
			cfg.Environments[env] = append(cfg.Environments[env], hc)
		}
	}

	// Parse autoconnect pairs.
	for _, pair := range raw.Autoconnect {
		if len(pair) != 2 {
			return nil, fmt.Errorf("autoconnect entries must be [host, tenant] pairs")
		}
		cfg.Autoconnect = append(cfg.Autoconnect, AutoconnectEntry{
			Host:   pair[0],
			Tenant: pair[1],
		})
	}

	return cfg, nil
}

// parseHostConfig handles both "hostname" and {"host":"...", "root":"..."}.
func parseHostConfig(data json.RawMessage) (HostConfig, error) {
	// Try string first.
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		return HostConfig{Host: s, Root: defaultRoot}, nil
	}

	// Try object.
	var hc HostConfig
	if err := json.Unmarshal(data, &hc); err != nil {
		return HostConfig{}, fmt.Errorf("invalid host entry: %s", string(data))
	}
	if hc.Host == "" {
		return HostConfig{}, fmt.Errorf("host entry missing 'host' field: %s", string(data))
	}
	if hc.Root == "" {
		hc.Root = defaultRoot
	}
	return hc, nil
}

// SaveConfig writes the config back to disk as normalized JSON.
func SaveConfig(cfg *Config, path string) error {
	type hostJSON struct {
		Host string `json:"host"`
		User string `json:"user,omitempty"`
		Root string `json:"root,omitempty"`
	}
	type configJSON struct {
		Environments map[string][]hostJSON    `json:"environments"`
		Autoconnect  [][]string               `json:"autoconnect"`
		Overrides    map[string]EntryOverride `json:"overrides,omitempty"`
	}

	cj := configJSON{
		Environments: make(map[string][]hostJSON),
		Autoconnect:  make([][]string, 0),
	}
	for env, hosts := range cfg.Environments {
		for _, h := range hosts {
			hj := hostJSON{Host: h.Host, User: h.User}
			if h.Root != defaultRoot {
				hj.Root = h.Root
			}
			cj.Environments[env] = append(cj.Environments[env], hj)
		}
	}
	for _, ac := range cfg.Autoconnect {
		cj.Autoconnect = append(cj.Autoconnect, []string{ac.Host, ac.Tenant})
	}
	// Only include overrides that have at least one non-empty field.
	if len(cfg.Overrides) > 0 {
		cj.Overrides = make(map[string]EntryOverride)
		for k, v := range cfg.Overrides {
			if v.DBUser != "" || v.Password != "" || v.Database != "" {
				cj.Overrides[k] = v
			}
		}
		if len(cj.Overrides) == 0 {
			cj.Overrides = nil
		}
	}

	data, err := json.MarshalIndent(cj, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// ScaffoldConfig creates a documented example config file.
func ScaffoldConfig(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	content := `{
  "environments": {
    "prod": [
      {"host": "prod-server-1", "user": "deploy", "root": "/docker"},
      "prod-server-2"
    ],
    "test": [
      "test-server-1"
    ]
  },
  "autoconnect": [
    ["test-server-1", "my-app"]
  ]
}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	return nil
}
