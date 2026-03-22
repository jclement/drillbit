package main

import (
	"testing"
)

func TestShellQuote(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"simple", "'simple'"},
		{"with spaces", "'with spaces'"},
		{"it's", "'it'\\''s'"},
		{"", "''"},
		{"hello'world'test", "'hello'\\''world'\\''test'"},
		{"$(cmd)", "'$(cmd)'"},
		{"; rm -rf /", "'; rm -rf /'"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := shellQuote(tt.input); got != tt.want {
				t.Errorf("shellQuote(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestExpandTilde(t *testing.T) {
	tests := []struct {
		input string
		tilde bool
	}{
		{"~/foo/bar", true},
		{"/absolute/path", false},
		{"relative/path", false},
		{"~other/path", false}, // only ~/... is expanded
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := expandTilde(tt.input)
			if tt.tilde {
				if got == tt.input {
					t.Errorf("expandTilde(%q) = %q, expected tilde expansion", tt.input, got)
				}
				if got[0] == '~' {
					t.Errorf("expandTilde(%q) = %q, still starts with ~", tt.input, got)
				}
			} else {
				if got != tt.input {
					t.Errorf("expandTilde(%q) = %q, expected no change", tt.input, got)
				}
			}
		})
	}
}
