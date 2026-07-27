package main

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/platform/paths"
)

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

func TestClaudeManagedEligibleFor(t *testing.T) {
	cases := []struct {
		name        string
		elevated    bool
		managedPath string
		want        bool
	}{
		{"elevated, unix managed path", true, "/etc/claude-code/managed-settings.json", true},
		{"non-elevated, unix managed path", false, "/etc/claude-code/managed-settings.json", false},
		// waired#749: Windows now qualifies when elevated (euid is -1 there,
		// so the old euid==0 gate excluded it even as Administrator).
		{"elevated, windows managed path", true, `C:\Program Files\ClaudeCode\managed-settings.json`, true},
		{"non-elevated, windows managed path", false, `C:\Program Files\ClaudeCode\managed-settings.json`, false},
		{"elevated but no managed path (unsupported OS)", true, "", false},
	}
	for _, c := range cases {
		if got := claudeManagedEligibleFor(c.elevated, c.managedPath); got != c.want {
			t.Errorf("%s: claudeManagedEligibleFor(%v, %q) = %v, want %v", c.name, c.elevated, c.managedPath, got, c.want)
		}
	}
}

func TestSkipClaudeRouteFlagDefaultsFromEnv(t *testing.T) {
	// The installers set WAIRED_NO_CLAUDE_PROXY=1 (from -SkipClaudeProxy /
	// --skip-claude-proxy) to carry the routing opt-out into `waired init`,
	// which is now the single decider of routing. The flag default must track
	// that env, mirroring --mask-pii / WAIRED_PII_MASK.
	for _, c := range []struct {
		env  string
		want string // cobra records the default as a string
	}{
		{"", "false"},
		{"1", "true"},
	} {
		t.Setenv("WAIRED_NO_CLAUDE_PROXY", c.env)
		f := newInitCmd().Flags().Lookup("skip-claude-route")
		if f == nil {
			t.Fatal("--skip-claude-route flag not registered on `waired init`")
		}
		if f.DefValue != c.want {
			t.Errorf("WAIRED_NO_CLAUDE_PROXY=%q: --skip-claude-route default = %q, want %q", c.env, f.DefValue, c.want)
		}
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
