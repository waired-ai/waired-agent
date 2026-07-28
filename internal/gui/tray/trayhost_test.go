package tray

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/platform/notification"
	"github.com/waired-ai/waired-agent/internal/platform/trayhost"
)

// TestShouldNotifyTrayHost pins the toast cadence. Product contract: a desktop
// that can never draw the icon (MATE → RepairNone) must stay silent forever, and
// a repairable-but-privileged host must not re-toast on every check.
func TestShouldNotifyTrayHost(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	var never time.Time

	tests := []struct {
		name   string
		action trayhost.RepairAction
		lastAt time.Time
		want   bool
	}{
		{"healthy host stays silent", trayhost.RepairNone, never, false},
		{"free repair is done, not announced", trayhost.RepairEnableOnly, never, false},
		{"privileged repair toasts on first sighting", trayhost.RepairInstallThenEnable, never, true},
		{"manual repair toasts on first sighting", trayhost.RepairManual, never, true},
		{
			name:   "privileged repair stays quiet inside the interval",
			action: trayhost.RepairInstallThenEnable,
			lastAt: now.Add(-trayHostRenotifyInterval + time.Minute),
			want:   false,
		},
		{
			name:   "privileged repair re-reminds once the interval elapses",
			action: trayhost.RepairInstallThenEnable,
			lastAt: now.Add(-trayHostRenotifyInterval),
			want:   true,
		},
		{
			name:   "an unfixable desktop never re-toasts however long it has been",
			action: trayhost.RepairNone,
			lastAt: now.Add(-30 * 24 * time.Hour),
			want:   false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldNotifyTrayHost(tc.action, tc.lastAt, now, trayHostRenotifyInterval)
			if got != tc.want {
				t.Errorf("shouldNotifyTrayHost(%v, lastAt=%v) = %v, want %v",
					tc.action, tc.lastAt, got, tc.want)
			}
		})
	}
}

// TestCheckTrayHost drives the real checkTrayHost over the seams, so the
// branching between "repair silently" and "ask the user to" is covered end to
// end rather than only through the pure helper.
func TestCheckTrayHost(t *testing.T) {
	tests := []struct {
		name        string
		probe       trayhost.Result
		facts       trayhost.RepairFacts // what the plan seam should answer with
		wantEnables int
		wantToast   string // substring; "" means no toast at all
		wantLevel   notification.Level
	}{
		{
			name:        "a healthy host neither repairs nor toasts",
			probe:       trayhost.Result{Status: trayhost.HostPresent, Desktop: trayhost.DesktopGNOME},
			wantEnables: 0,
		},
		{
			name:        "a headless session is left alone",
			probe:       trayhost.Result{Status: trayhost.NotApplicable, Desktop: trayhost.DesktopNone},
			wantEnables: 0,
		},
		{
			name:  "MATE cannot draw the icon, and saying so every login would be noise",
			probe: trayhost.Result{Status: trayhost.Unsupported, Desktop: trayhost.DesktopMATE, Hint: "MATE can't render SNI"},
			facts: trayhost.RepairFacts{Status: trayhost.Unsupported, Desktop: trayhost.DesktopMATE},
			// Unsupported plans to RepairNone: nothing to install, nothing to say.
			wantEnables: 0,
		},
		{
			name:  "a disabled extension is switched on silently",
			probe: trayhost.Result{Status: trayhost.NoHost, Desktop: trayhost.DesktopGNOME},
			facts: trayhost.RepairFacts{
				Status: trayhost.NoHost, Desktop: trayhost.DesktopGNOME,
				ExtensionPresent: true, GnomeShellOnPath: true, AptOnPath: true,
			},
			wantEnables: 1,
			wantToast:   "should appear in a moment",
			wantLevel:   notification.Info,
		},
		{
			name: "on Wayland the same repair tells the user to log back in",
			probe: trayhost.Result{
				Status: trayhost.NoHost, Desktop: trayhost.DesktopGNOME, Wayland: true,
			},
			facts: trayhost.RepairFacts{
				Status: trayhost.NoHost, Desktop: trayhost.DesktopGNOME,
				ExtensionPresent: true, GnomeShellOnPath: true, AptOnPath: true,
			},
			wantEnables: 1,
			wantToast:   "log out and back in",
			wantLevel:   notification.Info,
		},
		{
			name:  "a missing extension needs root, so the user is pointed at the doctor",
			probe: trayhost.Result{Status: trayhost.NoHost, Desktop: trayhost.DesktopGNOME},
			facts: trayhost.RepairFacts{
				Status: trayhost.NoHost, Desktop: trayhost.DesktopGNOME,
				GnomeShellOnPath: true, AptOnPath: true,
			},
			wantEnables: 0,
			wantToast:   "waired doctor --fix",
			wantLevel:   notification.Warning,
		},
		{
			name:  "no apt to install with also points at the doctor",
			probe: trayhost.Result{Status: trayhost.NoHost, Desktop: trayhost.DesktopGNOME},
			facts: trayhost.RepairFacts{
				Status: trayhost.NoHost, Desktop: trayhost.DesktopGNOME,
				GnomeShellOnPath: true,
			},
			wantEnables: 0,
			wantToast:   "waired doctor --fix",
			wantLevel:   notification.Warning,
		},
		{
			name:  "a non-GNOME desktop with no host is not ours to repair",
			probe: trayhost.Result{Status: trayhost.NoHost, Desktop: trayhost.DesktopKDE},
			facts: trayhost.RepairFacts{
				Status: trayhost.NoHost, Desktop: trayhost.DesktopKDE, AptOnPath: true,
			},
			wantEnables: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := resetSeams(t)
			n := &stubNotifier{}
			notifier = n
			t.Cleanup(installSeamStubs) // restore the package-wide defaults

			probe, facts := tc.probe, tc.facts
			trayHostCheck = func() trayhost.Result {
				l.mu.Lock()
				l.trayHostChecks++
				l.mu.Unlock()
				return probe
			}
			trayHostPlan = func(r trayhost.Result) trayhost.RepairAction {
				l.mu.Lock()
				l.trayHostPlans = append(l.trayHostPlans, r.Status.String())
				l.mu.Unlock()
				return trayhost.PlanRepair("linux", facts)
			}
			trayHostEnable = func(context.Context) error {
				l.mu.Lock()
				defer l.mu.Unlock()
				l.trayHostEnables++
				return nil
			}

			tr := &tray{}
			tr.checkTrayHost(context.Background())

			l.mu.Lock()
			checks, enables, plans := l.trayHostChecks, l.trayHostEnables, append([]string(nil), l.trayHostPlans...)
			l.mu.Unlock()

			if checks != 1 {
				t.Errorf("probed the session %d times, want exactly 1", checks)
			}
			// The plan must be asked about the probe's verdict, not a
			// re-derived one: that is the seam that keeps the tray honest.
			if len(plans) != 1 || plans[0] != tc.probe.Status.String() {
				t.Errorf("planned against %v, want [%v]", plans, tc.probe.Status)
			}
			if enables != tc.wantEnables {
				t.Errorf("enabled %d times, want %d", enables, tc.wantEnables)
			}

			toasts := n.snapshot()
			if tc.wantToast == "" {
				if len(toasts) != 0 {
					t.Fatalf("toasted %v, want silence", toasts)
				}
				return
			}
			if len(toasts) != 1 {
				t.Fatalf("toasted %v, want exactly one", toasts)
			}
			if !strings.Contains(strings.ToLower(toasts[0].body), strings.ToLower(tc.wantToast)) {
				t.Errorf("toast body = %q, want it to mention %q", toasts[0].body, tc.wantToast)
			}
			if toasts[0].level != tc.wantLevel {
				t.Errorf("toast level = %v, want %v", toasts[0].level, tc.wantLevel)
			}
		})
	}
}

// TestCheckTrayHost_EnableFailureFallsBackToTelling reproduces the case the
// silent repair must not swallow: gnome-extensions is present but the enable
// fails (a version-invalidated extension, a locked-down dconf). The user has to
// hear about it, because the icon still will not appear.
func TestCheckTrayHost_EnableFailureFallsBackToTelling(t *testing.T) {
	l := resetSeams(t)
	n := &stubNotifier{}
	notifier = n
	t.Cleanup(installSeamStubs)

	trayHostCheck = func() trayhost.Result {
		return trayhost.Result{Status: trayhost.NoHost, Desktop: trayhost.DesktopGNOME, Hint: "install it"}
	}
	trayHostPlan = func(trayhost.Result) trayhost.RepairAction { return trayhost.RepairEnableOnly }
	trayHostEnable = func(context.Context) error {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.trayHostEnables++
		return context.DeadlineExceeded
	}

	tr := &tray{}
	tr.checkTrayHost(context.Background())

	toasts := n.snapshot()
	if len(toasts) != 1 {
		t.Fatalf("toasted %v, want exactly one — a failed repair must not be silent", toasts)
	}
	// It must report the failure, not claim success: the icon is still absent.
	if strings.Contains(strings.ToLower(toasts[0].body), "switched on") {
		t.Errorf("toasted success after a failed enable: %q", toasts[0].body)
	}
	if !strings.Contains(toasts[0].body, "waired doctor --fix") {
		t.Errorf("toast body = %q, want it to point at `waired doctor --fix`", toasts[0].body)
	}
	if toasts[0].level != notification.Warning {
		t.Errorf("toast level = %v, want Warning", toasts[0].level)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.trayHostEnables != 1 {
		t.Errorf("attempted enable %d times, want 1", l.trayHostEnables)
	}
}
