package main

import (
	"path/filepath"
	"testing"

	"github.com/waired-ai/waired-agent/internal/platform/paths"
)

func TestPeekStateDir(t *testing.T) {
	// Make the fallback path deterministic across CI / dev hosts so the
	// "no flag present" cases compare against a known value rather than
	// each runner's actual %AppData% / ~/.config. The env-override path
	// must be syntactically a valid path on the runner; we don't touch
	// disk so the contents are irrelevant beyond uniqueness.
	t.Setenv(paths.EnvOverride, filepath.Join(t.TempDir(), "fake-waired-state"))
	want := paths.StateDir(paths.AutoDetect)

	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no args", nil, want},
		{"empty slice", []string{}, want},
		{"unrelated flag only", []string{"-mgmt", "127.0.0.1:0"}, want},

		{"single dash, equals form", []string{"-state-dir=/tmp/x"}, "/tmp/x"},
		{"double dash, equals form", []string{"--state-dir=/tmp/x"}, "/tmp/x"},
		{"single dash, space form", []string{"-state-dir", "/tmp/x"}, "/tmp/x"},
		{"double dash, space form", []string{"--state-dir", "/tmp/x"}, "/tmp/x"},

		{
			name: "windows-style path with equals",
			args: []string{`--state-dir=C:\ProgramData\waired`},
			want: `C:\ProgramData\waired`,
		},
		{
			name: "windows-style path with space",
			args: []string{`--state-dir`, `C:\ProgramData\waired`},
			want: `C:\ProgramData\waired`,
		},

		{
			name: "preceded by other flags",
			args: []string{"-mgmt", "127.0.0.1:0", "-state-dir", "/tmp/x", "-force-relay"},
			want: "/tmp/x",
		},
		{
			name: "followed by other flags",
			args: []string{"-state-dir", "/tmp/x", "-mgmt", "127.0.0.1:0"},
			want: "/tmp/x",
		},

		{
			name: "value missing at end of args",
			args: []string{"-mgmt", "127.0.0.1:0", "-state-dir"},
			want: want,
		},
		{
			name: "double-dash terminator before flag",
			args: []string{"--", "-state-dir", "/tmp/x"},
			want: want,
		},

		{
			name: "first occurrence wins",
			args: []string{"-state-dir", "/tmp/first", "-state-dir", "/tmp/second"},
			want: "/tmp/first",
		},

		{
			name: "empty value via equals form",
			args: []string{"--state-dir="},
			want: "",
		},

		// Confirm that look-alikes do not match.
		{
			name: "look-alike flag is not state-dir",
			args: []string{"--state-directory", "/tmp/x"},
			want: want,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := peekStateDir(tt.args)
			if got != tt.want {
				t.Errorf("peekStateDir(%v) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}
