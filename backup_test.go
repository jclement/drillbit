package main

import (
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBackupFileName(t *testing.T) {
	name := backupFileName("prod-server", "myapp_db_1", "mydb")
	if name == "" {
		t.Fatal("expected non-empty filename")
	}
	if !strings.HasSuffix(name, ".sql.gz") {
		t.Errorf("expected .sql.gz extension, got %s", name)
	}
	// Dashes should be sanitized to underscores.
	if strings.Contains(name, "prod-server") {
		t.Errorf("expected dashes sanitized, got %s", name)
	}
	if !strings.Contains(name, "prod_server") {
		t.Errorf("expected sanitized host in filename, got %s", name)
	}
	if !strings.Contains(name, "myapp_db_1") {
		t.Errorf("expected container in filename, got %s", name)
	}
	if !strings.Contains(name, "mydb") {
		t.Errorf("expected database in filename, got %s", name)
	}
	// Verify -- separator format.
	if strings.Count(name, "--") != 3 {
		t.Errorf("expected 3 -- separators, got %s", name)
	}
}

func TestSanitizeFilename(t *testing.T) {
	tests := []struct {
		input, expected string
	}{
		{"simple", "simple"},
		{"has/slash", "has_slash"},
		{"has space", "has_space"},
		{"has:colon", "has_colon"},
		{"has-dash", "has_dash"},
		{"double--dash", "double__dash"},
		{"mixed/path:name-test", "mixed_path_name_test"},
	}
	for _, tt := range tests {
		got := sanitizeFilename(tt.input)
		if got != tt.expected {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestListBackups(t *testing.T) {
	dir := t.TempDir()

	// Create test backup files in host--container--db--YYYYMMDD_HHMMSS.sql.gz format.
	files := []string{
		"prod--app_db_1--mydb--20260401_120000.sql.gz",
		"prod--app_db_1--mydb--20260402_130000.sql.gz",
		"staging--test_db--testdb--20260403_140000.sql.gz",
		"not-a-backup.txt",
		"bad_format.sql.gz",       // wrong number of -- parts
		"a--b--c--bad_time.sql.gz", // invalid timestamp
	}

	for _, f := range files {
		os.WriteFile(filepath.Join(dir, f), []byte("test"), 0o644)
	}

	backups := listBackups(dir)
	if len(backups) != 3 {
		t.Fatalf("expected 3 backups, got %d", len(backups))
	}

	// Should be newest first.
	if backups[0].Host != "staging" {
		t.Errorf("expected newest first, got host=%s", backups[0].Host)
	}
	if backups[0].Container != "test_db" {
		t.Errorf("expected container=test_db, got %s", backups[0].Container)
	}
	if backups[0].Database != "testdb" {
		t.Errorf("expected database=testdb, got %s", backups[0].Database)
	}
}

func TestListBackupsNonexistentDir(t *testing.T) {
	backups := listBackups("/nonexistent/path/that/does/not/exist")
	if backups != nil {
		t.Errorf("expected nil for nonexistent dir, got %d entries", len(backups))
	}
}

func TestListBackupsEmptyDir(t *testing.T) {
	dir := t.TempDir()
	backups := listBackups(dir)
	if len(backups) != 0 {
		t.Errorf("expected 0 backups in empty dir, got %d", len(backups))
	}
}

func TestSortBackupsForEntry(t *testing.T) {
	backups := []backupFile{
		{Host: "prod", Container: "app_db", Database: "mydb", Timestamp: time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)},
		{Host: "staging", Container: "test_db", Database: "testdb", Timestamp: time.Date(2026, 4, 2, 12, 0, 0, 0, time.UTC)},
		{Host: "prod", Container: "app_db", Database: "mydb", Timestamp: time.Date(2026, 4, 3, 12, 0, 0, 0, time.UTC)},
	}

	sorted, sepIdx := sortBackupsForEntry(backups, "prod", "app_db", "mydb")
	if sepIdx != 2 {
		t.Errorf("expected separator at 2, got %d", sepIdx)
	}
	if len(sorted) != 3 {
		t.Fatalf("expected 3, got %d", len(sorted))
	}
	if sorted[0].Host != "prod" || sorted[1].Host != "prod" {
		t.Error("expected matching entries first")
	}
	if sorted[2].Host != "staging" {
		t.Error("expected non-matching entry last")
	}
}

func TestSortBackupsForEntryNoMatch(t *testing.T) {
	backups := []backupFile{
		{Host: "staging", Container: "test_db", Database: "testdb"},
		{Host: "dev", Container: "dev_db", Database: "devdb"},
	}

	sorted, sepIdx := sortBackupsForEntry(backups, "prod", "app_db", "mydb")
	if sepIdx != 0 {
		t.Errorf("expected separator at 0 (no matches), got %d", sepIdx)
	}
	if len(sorted) != 2 {
		t.Fatalf("expected 2, got %d", len(sorted))
	}
}

func TestSortBackupsForEntryAllMatch(t *testing.T) {
	backups := []backupFile{
		{Host: "prod", Container: "app_db", Database: "mydb"},
		{Host: "prod", Container: "app_db", Database: "mydb"},
	}

	sorted, sepIdx := sortBackupsForEntry(backups, "prod", "app_db", "mydb")
	if sepIdx != 2 {
		t.Errorf("expected separator at 2 (all match), got %d", sepIdx)
	}
	if len(sorted) != 2 {
		t.Fatalf("expected 2, got %d", len(sorted))
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		input    int64
		expected string
	}{
		{0, "0 B"},
		{500, "500 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1048576, "1.0 MB"},
		{1073741824, "1.0 GB"},
	}
	for _, tt := range tests {
		got := formatBytes(tt.input)
		if got != tt.expected {
			t.Errorf("formatBytes(%d) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestVerifyGzip(t *testing.T) {
	t.Run("valid gzip", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "test.gz")
		f, _ := os.Create(path)
		gz := gzip.NewWriter(f)
		gz.Write([]byte("hello world"))
		gz.Close()
		f.Close()

		if err := verifyGzip(path); err != nil {
			t.Errorf("expected valid gzip, got error: %v", err)
		}
	})

	t.Run("invalid gzip", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bad.gz")
		os.WriteFile(path, []byte("not gzip data"), 0o644)

		if err := verifyGzip(path); err == nil {
			t.Error("expected error for invalid gzip")
		}
	})

	t.Run("nonexistent file", func(t *testing.T) {
		if err := verifyGzip("/nonexistent/file.gz"); err == nil {
			t.Error("expected error for nonexistent file")
		}
	})
}

func TestBackupFileNameRoundTrip(t *testing.T) {
	// Generate a filename and verify it can be parsed back by listBackups.
	dir := t.TempDir()
	name := backupFileName("my-host", "my_container", "my-db")
	path := filepath.Join(dir, name)

	// Create a valid gzip file.
	f, _ := os.Create(path)
	gz := gzip.NewWriter(f)
	gz.Write([]byte("-- SQL dump"))
	gz.Close()
	f.Close()

	backups := listBackups(dir)
	if len(backups) != 1 {
		t.Fatalf("expected 1 backup, got %d", len(backups))
	}

	b := backups[0]
	if b.Host != "my_host" { // dashes sanitized
		t.Errorf("host = %q, want %q", b.Host, "my_host")
	}
	if b.Container != "my_container" {
		t.Errorf("container = %q, want %q", b.Container, "my_container")
	}
	if b.Database != "my_db" { // dashes sanitized
		t.Errorf("database = %q, want %q", b.Database, "my_db")
	}
	if b.Timestamp.IsZero() {
		t.Error("expected non-zero timestamp")
	}
}
