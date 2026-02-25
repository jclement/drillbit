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
	Environments map[string][]HostConfig `json:"-"`
	Autoconnect  []AutoconnectEntry      `json:"autoconnect"`
}

// HostConfig represents a single SSH host with its settings.
type HostConfig struct {
	Host string `json:"host"`
	Root string `json:"root"`
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
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "drillbit", "config.json")
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
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	if len(raw.Environments) == 0 {
		return nil, fmt.Errorf("config has no environments defined")
	}

	cfg := &Config{
		Environments: make(map[string][]HostConfig),
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

// ScaffoldConfig creates a documented example config file.
func ScaffoldConfig(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	content := `{
  "environments": {
    "prod": [
      {"host": "prod-server-1", "root": "/docker"},
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
