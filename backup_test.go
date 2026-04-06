package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBackupFileName(t *testing.T) {
	name := backupFileName("prod-server", "myapp_db_1", "mydb")
	if name == "" {
		t.Fatal("expected non-empty filename")
	}
	if !hasExtension(name, ".sql.gz") {
		t.Errorf("expected .sql.gz extension, got %s", name)
	}
	if !containsSubstring(name, "prod-server") {
		t.Errorf("expected host in filename, got %s", name)
	}
	if !containsSubstring(name, "myapp_db_1") {
		t.Errorf("expected container in filename, got %s", name)
	}
	if !containsSubstring(name, "mydb") {
		t.Errorf("expected database in filename, got %s", name)
	}
	// Verify -- separator format.
	if !containsSubstring(name, "--") {
		t.Errorf("expected -- separator in filename, got %s", name)
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

	// Create some test backup files in host--container--db--YYYYMMDD_HHMMSS.sql.gz format.
	files := []string{
		"prod--app_db_1--mydb--20260401_120000.sql.gz",
		"prod--app_db_1--mydb--20260402_130000.sql.gz",
		"staging--test_db--testdb--20260403_140000.sql.gz",
		"not-a-backup.txt",
		"bad_format.sql.gz", // wrong number of -- parts
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
	// First two should be matching.
	if sorted[0].Host != "prod" || sorted[1].Host != "prod" {
		t.Error("expected matching entries first")
	}
	// Third should be non-matching.
	if sorted[2].Host != "staging" {
		t.Error("expected non-matching entry last")
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

func hasExtension(name, ext string) bool {
	return len(name) > len(ext) && name[len(name)-len(ext):] == ext
}

func containsSubstring(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && findSubstring(s, sub))
}

func findSubstring(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
