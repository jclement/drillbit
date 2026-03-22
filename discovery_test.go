package main

import (
	"testing"
)

func TestParseDockerContainers(t *testing.T) {
	t.Run("single container", func(t *testing.T) {
		input := `/myapp_db_1|||postgres:16|||POSTGRES_USER=admin
POSTGRES_PASSWORD=secret123
POSTGRES_DB=myapp
PATH=/usr/bin
%%%REC%%%
`
		containers := parseDockerContainers([]byte(input))
		if len(containers) != 1 {
			t.Fatalf("expected 1 container, got %d", len(containers))
		}
		c := containers[0]
		if c.name != "myapp_db_1" {
			t.Errorf("name = %q, want %q", c.name, "myapp_db_1")
		}
		if c.image != "postgres" {
			t.Errorf("image = %q, want %q", c.image, "postgres")
		}
		if c.dbUser != "admin" {
			t.Errorf("dbUser = %q, want %q", c.dbUser, "admin")
		}
		if c.password != "secret123" {
			t.Errorf("password = %q, want %q", c.password, "secret123")
		}
		if c.database != "myapp" {
			t.Errorf("database = %q, want %q", c.database, "myapp")
		}
	})

	t.Run("multiple containers", func(t *testing.T) {
		input := `/db1|||postgres:16|||POSTGRES_PASSWORD=pass1
%%%REC%%%
/db2|||postgis/postgis:latest|||POSTGRES_PASSWORD=pass2
POSTGRES_DB=geodb
%%%REC%%%
`
		containers := parseDockerContainers([]byte(input))
		if len(containers) != 2 {
			t.Fatalf("expected 2 containers, got %d", len(containers))
		}
		if containers[0].name != "db1" {
			t.Errorf("c[0].name = %q, want %q", containers[0].name, "db1")
		}
		if containers[1].image != "postgis" {
			t.Errorf("c[1].image = %q, want %q", containers[1].image, "postgis")
		}
		if containers[1].database != "geodb" {
			t.Errorf("c[1].database = %q, want %q", containers[1].database, "geodb")
		}
	})

	t.Run("defaults", func(t *testing.T) {
		input := `/mydb|||postgres:16|||POSTGRES_PASSWORD=pass
%%%REC%%%
`
		containers := parseDockerContainers([]byte(input))
		if len(containers) != 1 {
			t.Fatalf("expected 1 container, got %d", len(containers))
		}
		if containers[0].dbUser != "postgres" {
			t.Errorf("default dbUser = %q, want %q", containers[0].dbUser, "postgres")
		}
		if containers[0].database != "mydb" {
			t.Errorf("default database = %q, want %q", containers[0].database, "mydb")
		}
	})

	t.Run("skips containers without password", func(t *testing.T) {
		input := `/db_no_pass|||postgres:16|||POSTGRES_USER=admin
%%%REC%%%
/db_with_pass|||postgres:16|||POSTGRES_PASSWORD=secret
%%%REC%%%
`
		containers := parseDockerContainers([]byte(input))
		if len(containers) != 1 {
			t.Fatalf("expected 1 container (with password), got %d", len(containers))
		}
		if containers[0].name != "db_with_pass" {
			t.Errorf("name = %q, want %q", containers[0].name, "db_with_pass")
		}
	})

	t.Run("empty input", func(t *testing.T) {
		containers := parseDockerContainers([]byte(""))
		if len(containers) != 0 {
			t.Errorf("expected 0 containers, got %d", len(containers))
		}
	})

	t.Run("malformed records", func(t *testing.T) {
		input := `incomplete|||data
%%%REC%%%
|||
%%%REC%%%
`
		containers := parseDockerContainers([]byte(input))
		if len(containers) != 0 {
			t.Errorf("expected 0 containers from malformed input, got %d", len(containers))
		}
	})

	t.Run("strips leading slash from name", func(t *testing.T) {
		input := `/my-container|||postgres:16|||POSTGRES_PASSWORD=pass
%%%REC%%%
`
		containers := parseDockerContainers([]byte(input))
		if len(containers) != 1 {
			t.Fatalf("expected 1 container, got %d", len(containers))
		}
		if containers[0].name != "my-container" {
			t.Errorf("name = %q, want %q (should strip leading /)", containers[0].name, "my-container")
		}
	})
}

func TestSimplifyImageName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"postgres:16", "postgres"},
		{"postgres:latest", "postgres"},
		{"postgres", "postgres"},
		{"postgis/postgis:latest", "postgis"},
		{"postgis/postgis:3.4", "postgis"},
		{"timescale/timescaledb:2.9", "timescale"},
		{"timescale/timescaledb-ha:latest", "timescale"},
		{"registry.example.com/postgres:16", "postgres"},
		{"myorg/custom-image:latest", "custom-image"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := simplifyImageName(tt.input); got != tt.want {
				t.Errorf("simplifyImageName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
