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

func TestClaudeRouteEligible(t *testing.T) {
	cases := []struct {
		name            string
		integConsent    bool
		claudeManaged   bool
		renewing        bool
		skipClaudeRoute bool
		want            bool
	}{
		{"all conditions met -> route", true, true, false, false, true},
		{"no integration consent", false, true, false, false, false},
		{"not elevated / no managed path", true, false, false, false, false},
		{"renew (not a fresh enroll)", true, true, true, false, false},
		// --skip-claude-route / WAIRED_NO_CLAUDE_PROXY / installer -SkipClaudeProxy:
		// the opt-out must win even when everything else says route. This is the
		// gate that used to be bypassed by the installer's separate, unconditional
		// `waired claude enable` after init.
		{"opt-out overrides", true, true, false, true, false},
	}
	for _, c := range cases {
		if got := claudeRouteEligible(c.integConsent, c.claudeManaged, c.renewing, c.skipClaudeRoute); got != c.want {
			t.Errorf("%s: claudeRouteEligible(%v, %v, %v, %v) = %v, want %v",
				c.name, c.integConsent, c.claudeManaged, c.renewing, c.skipClaudeRoute, got, c.want)
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
