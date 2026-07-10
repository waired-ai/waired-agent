//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseControlURLFromEnvFile(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"plain", "WAIRED_CONTROL_URL=https://cp.example.com\n", "https://cp.example.com"},
		{"double-quoted", "WAIRED_CONTROL_URL=\"https://cp.example.com\"\n", "https://cp.example.com"},
		{"single-quoted", "WAIRED_CONTROL_URL='https://cp.example.com'\n", "https://cp.example.com"},
		{"export prefix", "export WAIRED_CONTROL_URL=https://cp.example.com\n", "https://cp.example.com"},
		{"surrounded by comments/blanks", "# comment\n\nWAIRED_CONTROL_URL=https://cp.example.com\n# trailing\n", "https://cp.example.com"},
		{"commented out", "# WAIRED_CONTROL_URL=https://cp.example.com\n", ""},
		{"other keys only", "FOO=bar\nWAIRED_NO_TRAY=1\n", ""},
		{"empty value", "WAIRED_CONTROL_URL=\n", ""},
		{"whitespace around", "  WAIRED_CONTROL_URL = https://cp.example.com \n", "https://cp.example.com"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "agent.env")
			if err := os.WriteFile(p, []byte(c.content), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := parseControlURLFromEnvFile(p); got != c.want {
				t.Errorf("parseControlURLFromEnvFile(%q) = %q, want %q", c.content, got, c.want)
			}
		})
	}
}

func TestParseControlURLFromEnvFile_Missing(t *testing.T) {
	if got := parseControlURLFromEnvFile(filepath.Join(t.TempDir(), "nope.env")); got != "" {
		t.Errorf("missing file should yield \"\", got %q", got)
	}
}
