package main

import (
	"testing"
)

func TestIsNewer(t *testing.T) {
	tests := []struct {
		candidate string
		current   string
		want      bool
	}{
		{"1.0.1", "1.0.0", true},
		{"1.1.0", "1.0.0", true},
		{"2.0.0", "1.0.0", true},
		{"1.0.0", "1.0.0", false},
		{"1.0.0", "1.0.1", false},
		{"1.0.0", "2.0.0", false},
		{"v1.0.1", "v1.0.0", true},   // with v prefix
		{"1.0.1", "v1.0.0", true},    // mixed prefixes
		{"0.1.0", "0.0.9", true},
		{"1.0.0", "0.99.99", true},
		{"1.2.3", "1.2.3", false},    // equal
		{"1.2.2", "1.2.3", false},    // older
	}

	for _, tt := range tests {
		t.Run(tt.candidate+"_vs_"+tt.current, func(t *testing.T) {
			if got := isNewer(tt.candidate, tt.current); got != tt.want {
				t.Errorf("isNewer(%q, %q) = %v, want %v", tt.candidate, tt.current, got, tt.want)
			}
		})
	}
}

func TestParseSemver(t *testing.T) {
	tests := []struct {
		input string
		want  [3]int
	}{
		{"1.2.3", [3]int{1, 2, 3}},
		{"0.0.0", [3]int{0, 0, 0}},
		{"10.20.30", [3]int{10, 20, 30}},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := parseSemver(tt.input); got != tt.want {
				t.Errorf("parseSemver(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestFindChecksumForAsset(t *testing.T) {
	checksums := []byte(`abc123def456  drillbit_1.0.0_linux_amd64.tar.gz
789abc012def  drillbit_1.0.0_darwin_arm64.zip
fedcba987654  drillbit_1.0.0_windows_amd64.zip
`)

	t.Run("found", func(t *testing.T) {
		hash, err := findChecksumForAsset(checksums, "drillbit_1.0.0_darwin_arm64.zip")
		if err != nil {
			t.Fatal(err)
		}
		if hash != "789abc012def" {
			t.Errorf("hash = %q, want %q", hash, "789abc012def")
		}
	})

	t.Run("not found", func(t *testing.T) {
		_, err := findChecksumForAsset(checksums, "nonexistent.tar.gz")
		if err == nil {
			t.Fatal("expected error for missing asset")
		}
	})
}

func TestVerifyChecksum(t *testing.T) {
	// SHA256 of "hello world\n" = a948904f2f0f479b8f8564e9d7a23960c0a5b62fa6800eb215241d8582d0e21c...
	// Actually let's compute it properly.
	data := []byte("test data for checksum")
	// Pre-computed: sha256sum of "test data for checksum"

	t.Run("valid checksum", func(t *testing.T) {
		// We'll just verify that matching works with a known value.
		err := verifyChecksum([]byte{}, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
		if err != nil {
			t.Errorf("empty data checksum should match: %v", err)
		}
	})

	t.Run("mismatch", func(t *testing.T) {
		err := verifyChecksum(data, "0000000000000000000000000000000000000000000000000000000000000000")
		if err == nil {
			t.Fatal("expected error for mismatched checksum")
		}
	})

	t.Run("case insensitive", func(t *testing.T) {
		err := verifyChecksum([]byte{}, "E3B0C44298FC1C149AFBF4C8996FB92427AE41E4649B934CA495991B7852B855")
		if err != nil {
			t.Errorf("checksum comparison should be case-insensitive: %v", err)
		}
	})
}
