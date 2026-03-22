package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHostConfigSSHHost(t *testing.T) {
	tests := []struct {
		name string
		hc   HostConfig
		want string
	}{
		{"with user", HostConfig{Name: "server1", User: "deploy"}, "deploy@server1"},
		{"without user", HostConfig{Name: "server1"}, "server1"},
		{"empty user", HostConfig{Name: "server1", User: ""}, "server1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.hc.SSHHost(); got != tt.want {
				t.Errorf("SSHHost() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGetOverride(t *testing.T) {
	hc := HostConfig{
		Name: "server1",
		Databases: []DatabaseOverride{
			{Container: "db1", User: "admin", Password: "secret"},
			{Container: "db2", Auto: true},
		},
	}

	t.Run("found", func(t *testing.T) {
		ov := hc.GetOverride("db1")
		if ov == nil {
			t.Fatal("expected override, got nil")
		}
		if ov.User != "admin" {
			t.Errorf("User = %q, want %q", ov.User, "admin")
		}
	})

	t.Run("not found", func(t *testing.T) {
		if ov := hc.GetOverride("nonexistent"); ov != nil {
			t.Errorf("expected nil, got %+v", ov)
		}
	})

	t.Run("returns pointer to original", func(t *testing.T) {
		ov := hc.GetOverride("db1")
		ov.Password = "changed"
		if hc.Databases[0].Password != "changed" {
			t.Error("GetOverride should return a pointer to the original slice element")
		}
	})
}

func TestAutoconnect(t *testing.T) {
	cfg := &Config{
		Hosts: []HostConfig{
			{
				Name: "server1",
				Databases: []DatabaseOverride{
					{Container: "db1", Auto: true},
					{Container: "db2", Auto: false},
				},
			},
			{
				Name: "server2",
				Databases: []DatabaseOverride{
					{Container: "db3", Auto: true},
				},
			},
			{
				Name: "server3",
			},
		},
	}

	entries := cfg.Autoconnect()
	if len(entries) != 2 {
		t.Fatalf("expected 2 autoconnect entries, got %d", len(entries))
	}
	if entries[0].Host != "server1" || entries[0].Container != "db1" {
		t.Errorf("entries[0] = %+v, want server1/db1", entries[0])
	}
	if entries[1].Host != "server2" || entries[1].Container != "db3" {
		t.Errorf("entries[1] = %+v, want server2/db3", entries[1])
	}
}

func TestLoadConfig(t *testing.T) {
	t.Run("valid config", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		content := `hosts:
  - name: server1
    user: deploy
    env: prod
    databases:
      - container: db1
        auto: true
        password: secret
  - name: server2
`
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.Hosts) != 2 {
			t.Fatalf("expected 2 hosts, got %d", len(cfg.Hosts))
		}
		if cfg.Hosts[0].Name != "server1" {
			t.Errorf("host name = %q, want %q", cfg.Hosts[0].Name, "server1")
		}
		if cfg.Hosts[0].Databases[0].Password != "secret" {
			t.Errorf("password = %q, want %q", cfg.Hosts[0].Databases[0].Password, "secret")
		}
	})

	t.Run("empty hosts", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(path, []byte("hosts: []\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadConfig(path)
		if err == nil {
			t.Fatal("expected error for empty hosts")
		}
	})

	t.Run("missing file", func(t *testing.T) {
		_, err := LoadConfig("/nonexistent/config.yaml")
		if err == nil {
			t.Fatal("expected error for missing file")
		}
	})

	t.Run("invalid yaml", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(path, []byte("{{invalid"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadConfig(path)
		if err == nil {
			t.Fatal("expected error for invalid YAML")
		}
	})
}

func TestSaveConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "config.yaml")

	cfg := &Config{
		Hosts: []HostConfig{
			{Name: "server1", User: "deploy", Env: "prod"},
		},
	}
	if err := SaveConfig(cfg, path); err != nil {
		t.Fatal(err)
	}

	// Verify file was created.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("permissions = %o, want 600", info.Mode().Perm())
	}

	// Round-trip: load it back.
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Hosts[0].Name != "server1" {
		t.Errorf("host name = %q, want %q", loaded.Hosts[0].Name, "server1")
	}
}

func TestScaffoldConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "drillbit", "config.yaml")

	if err := ScaffoldConfig(path); err != nil {
		t.Fatal(err)
	}

	// Should produce a loadable config.
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("scaffolded config is not loadable: %v", err)
	}
	if len(cfg.Hosts) == 0 {
		t.Fatal("scaffolded config has no hosts")
	}
}
