package main

import (
	"testing"
)

func TestEnvColor(t *testing.T) {
	// Deterministic: same env always produces the same color.
	s1 := envColor("prod")
	s2 := envColor("prod")
	if s1.GetForeground() != s2.GetForeground() {
		t.Error("envColor should be deterministic for the same input")
	}

	// Different envs should (usually) get different colors.
	s3 := envColor("test")
	// We can't guarantee different colors, but they shouldn't be empty.
	if s3.GetForeground() == nil {
		t.Error("envColor returned empty foreground")
	}
}

func TestStyledStatus(t *testing.T) {
	tests := []struct {
		status Status
	}{
		{StatusReady},
		{StatusConnecting},
		{StatusConnected},
		{StatusError},
	}

	for _, tt := range tests {
		result := styledStatus(tt.status)
		if result == "" || result == "?" {
			t.Errorf("styledStatus(%d) returned empty or unknown", tt.status)
		}
	}
}

func TestImageTypeBadge(t *testing.T) {
	tests := []struct {
		image string
	}{
		{"postgres"},
		{"postgis"},
		{"timescale"},
		{"unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.image, func(t *testing.T) {
			badge := imageTypeBadge(tt.image, 6)
			if badge == "" {
				t.Errorf("imageTypeBadge(%q) returned empty string", tt.image)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		input  string
		maxLen int
		want   string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello world", 5, "hell\u2026"},
		{"hi", 3, "hi"},
		{"hi", 1, "h"},
		{"hello", 3, "hel"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := truncate(tt.input, tt.maxLen); got != tt.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
			}
		})
	}
}
