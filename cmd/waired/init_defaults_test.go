package main

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/platform/paths"
)

func TestResolveControlURL(t *testing.T) {
	cases := []struct {
		name            string
		explicit        string
		platformDefault string
		want            string
	}{
		{"explicit wins over everything", "https://flag.example.com", "https://envfile.example.com", "https://flag.example.com"},
		{"agent.env wins over baked default", "", "https://envfile.example.com", "https://envfile.example.com"},
		{"baked production default last resort", "", "", defaultControlURL},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveControlURL(c.explicit, c.platformDefault); got != c.want {
				t.Errorf("resolveControlURL(%q, %q) = %q, want %q", c.explicit, c.platformDefault, got, c.want)
			}
		})
	}
}

func TestDefaultControlURLConstant(t *testing.T) {
	// The baked default must itself survive normalization (so the
	// last-resort path can't produce a URL the enroll POST rejects).
	got, err := normalizeControlURL(defaultControlURL)
	if err != nil {
		t.Fatalf("defaultControlURL %q does not normalize: %v", defaultControlURL, err)
	}
	if got != defaultControlURL {
		t.Errorf("defaultControlURL %q normalizes to %q; keep it already-canonical", defaultControlURL, got)
	}
}

// TestDefaultStateDirMatchesInit guards the #3 regression: the
// daemon-interacting subcommands (status / use / runtimes / worker) must
// default to the SAME state dir as `waired init`. Otherwise `sudo waired
// status` reads an empty per-user dir and reports "Not enrolled" against a
// device that is enrolled and serving via the service-owned /var/lib/waired.
func TestDefaultStateDirMatchesInit(t *testing.T) {
	if defaultStateDir() != defaultInitStateDir() {
		t.Errorf("defaultStateDir()=%q != defaultInitStateDir()=%q; daemon-interacting commands must resolve to the daemon's state dir",
			defaultStateDir(), defaultInitStateDir())
	}
}

func TestInitStateDirMode(t *testing.T) {
	cases := []struct {
		goos string
		euid int
		want paths.Mode
	}{
		{"linux", 0, paths.System},         // sudo waired init -> /var/lib/waired (daemon's dir)
		{"linux", 1000, paths.Interactive}, // non-root dev -> per-user
		{"darwin", 0, paths.System},        // sudo waired init -> /Library (system LaunchDaemon's dir, #520)
		{"darwin", 501, paths.Interactive}, // non-root dev / tray -> ~/Library
		{"windows", -1, paths.Interactive}, // Geteuid()==-1 on Windows (System via SCM probe)
	}
	for _, c := range cases {
		if got := initStateDirMode(c.goos, c.euid); got != c.want {
			t.Errorf("initStateDirMode(%q, %d) = %v, want %v", c.goos, c.euid, got, c.want)
		}
	}
}
