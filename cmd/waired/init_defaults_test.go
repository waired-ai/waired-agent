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

// TestInitStateDirMode covers all three OSes against the fact each one
// actually has. The windows/elevated row INVERTS what this table pinned
// before #313: `os.Geteuid()` is -1 on Windows, so the euid guard was a
// no-op there and even an elevated `waired init` resolved the per-user
// %AppData% dir — never the %ProgramData% one the daemon reads. The old
// comment deferred to "System via the SCM probe", but that probe only
// fires for paths.AutoDetect and this decision passes Interactive.
func TestInitStateDirMode(t *testing.T) {
	cases := []struct {
		name     string
		goos     string
		euid     int
		elevated bool
		want     paths.Mode
	}{
		{"linux root", "linux", 0, true, paths.System},               // sudo waired init -> /var/lib/waired (daemon's dir)
		{"linux user", "linux", 1000, false, paths.Interactive},      // non-root dev -> per-user
		{"darwin root", "darwin", 0, true, paths.System},             // sudo waired init -> /Library (system LaunchDaemon's dir, #520)
		{"darwin user", "darwin", 501, false, paths.Interactive},     // non-root dev / tray -> ~/Library
		{"windows admin", "windows", -1, true, paths.System},         // #313: the daemon's %ProgramData%\waired
		{"windows user", "windows", -1, false, paths.Interactive},    // standard user -> %AppData%
		{"linux root not elevated", "linux", 0, false, paths.System}, // euid decides on Unix; elevated is the Windows fact
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := initStateDirMode(c.goos, c.euid, c.elevated); got != c.want {
				t.Errorf("initStateDirMode(%q, %d, %v) = %v, want %v", c.goos, c.euid, c.elevated, got, c.want)
			}
		})
	}
}
