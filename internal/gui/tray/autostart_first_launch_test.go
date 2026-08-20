package tray

import (
	"errors"
	"os"
	"testing"
)

// fakeAutostart records the real arguments so a test can tell "Enable
// was called" from "Enable was called with the wrong command line"
// (CLAUDE.md §Test discipline: fakes take and record the real
// arguments).
type fakeAutostart struct {
	enabled      bool
	isEnabledErr error
	enableErr    error

	enableCalls  int
	disableCalls int
	gotPath      string
	gotArgs      []string
}

func (f *fakeAutostart) Enable(programPath string, args []string) error {
	f.enableCalls++
	f.gotPath = programPath
	f.gotArgs = append([]string(nil), args...)
	if f.enableErr != nil {
		return f.enableErr
	}
	f.enabled = true
	return nil
}

func (f *fakeAutostart) Disable() error {
	f.disableCalls++
	f.enabled = false
	return nil
}

func (f *fakeAutostart) IsEnabled() (bool, error) {
	if f.isEnabledErr != nil {
		return false, f.isEnabledErr
	}
	return f.enabled, nil
}

// TestFirstLaunchAutostartApplies pins which platforms register their
// own login entry on first launch. Product contract: waired-agent#833
// (macOS was excluded on a stale "still stubbed" comment while its
// LaunchAgent backend was already complete); the Linux exclusion is
// the system-wide /etc/xdg/autostart entry the .deb ships
// (packaging/nfpm/waired-tray.yaml.tmpl).
func TestFirstLaunchAutostartApplies(t *testing.T) {
	for _, tc := range []struct {
		goos string
		want bool
	}{
		{"windows", true},
		{"darwin", true},
		{"linux", false},
	} {
		if got := firstLaunchAutostartApplies(tc.goos); got != tc.want {
			t.Errorf("firstLaunchAutostartApplies(%q) = %v, want %v", tc.goos, got, tc.want)
		}
	}
}

// TestEnsureAutostartOnFirstLaunchRegistersPerOS drives the real
// method (not just the predicate) across the three OSes, so a future
// change that keeps the predicate but drops the call site still fails.
func TestEnsureAutostartOnFirstLaunchRegistersPerOS(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	for _, tc := range []struct {
		goos       string
		wantEnable int
	}{
		{"darwin", 1},
		{"windows", 1},
		{"linux", 0},
	} {
		t.Run(tc.goos, func(t *testing.T) {
			f := &fakeAutostart{}
			tr := &tray{autostartMgr: f}
			tr.opts.MgmtURL = "http://127.0.0.1:9476"

			tr.ensureAutostartOnFirstLaunchFor(tc.goos)

			if f.enableCalls != tc.wantEnable {
				t.Fatalf("Enable called %d times, want %d", f.enableCalls, tc.wantEnable)
			}
			if tc.wantEnable == 0 {
				return
			}
			if f.gotPath != self {
				t.Errorf("Enable path = %q, want this executable %q", f.gotPath, self)
			}
			want := []string{"-mgmt", "http://127.0.0.1:9476"}
			if len(f.gotArgs) != len(want) || f.gotArgs[0] != want[0] || f.gotArgs[1] != want[1] {
				t.Errorf("Enable args = %v, want %v", f.gotArgs, want)
			}
		})
	}
}

// TestEnsureAutostartOnFirstLaunchIsIdempotent covers the case the
// Windows installer now creates: the Run value / LaunchAgent plist is
// already there when the tray first starts, so the tray must leave it
// alone rather than rewrite it (packaging/install/install.ps1
// Register-TrayAutostart writes it before the tray has ever run).
func TestEnsureAutostartOnFirstLaunchIsIdempotent(t *testing.T) {
	f := &fakeAutostart{enabled: true}
	tr := &tray{autostartMgr: f}
	tr.ensureAutostartOnFirstLaunchFor("windows")
	if f.enableCalls != 0 {
		t.Fatalf("Enable called %d times on an already-registered host, want 0", f.enableCalls)
	}
}

// TestEnsureAutostartOnFirstLaunchSwallowsErrors: a failure here must
// not abort the tray boot -- the menu toggle stays as the manual
// fallback. Record of today's behaviour.
func TestEnsureAutostartOnFirstLaunchSwallowsErrors(t *testing.T) {
	probeFailed := &fakeAutostart{isEnabledErr: errors.New("probe boom")}
	tr := &tray{autostartMgr: probeFailed}
	tr.ensureAutostartOnFirstLaunchFor("darwin")
	if probeFailed.enableCalls != 0 {
		t.Errorf("Enable called after a failed probe")
	}

	enableFailed := &fakeAutostart{enableErr: errors.New("enable boom")}
	tr2 := &tray{autostartMgr: enableFailed}
	tr2.ensureAutostartOnFirstLaunchFor("darwin")
	if enableFailed.enableCalls != 1 {
		t.Errorf("Enable calls = %d, want 1", enableFailed.enableCalls)
	}
}
