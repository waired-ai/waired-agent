package tray

import (
	"strings"
	"testing"
	"time"
)

// installedFacts is the ordinary case: a registered service, past any boot
// grace, nobody's email remembered.
func installedFacts() daemonDownFacts {
	return daemonDownFacts{ServiceInstalled: true}
}

// PRODUCT CONTRACT (#317 item 1). The daemon-down menu offers an action, not a
// command to retype. Before this, the whole affordance was a disabled row
// containing the literal string "Start-Service waired-agent" — the user was
// expected to read it, open an admin shell, and type it back in.
//
// Every OS gets the action, and the copy row alongside it, because every OS
// has a service manager the tray can drive through its consent mechanism.
func TestDaemonDownModelFor_OffersTheActionOnEveryOS(t *testing.T) {
	wantCmd := map[string]string{
		"windows": "Start-Service waired-agent",
		"linux":   "sudo systemctl start waired-agent",
		"darwin":  "sudo launchctl kickstart -k system/com.waired.agent",
	}
	for goos, cmd := range wantCmd {
		t.Run(goos, func(t *testing.T) {
			m := daemonDownModelFor(goos, installedFacts())

			if m.Kind != MenuDaemonDown {
				t.Errorf("Kind=%v, want MenuDaemonDown", m.Kind)
			}
			if m.StartAgentAction == "" {
				t.Error("no start action offered")
			}
			if m.StartAgentCopy == "" {
				t.Error("no copy-command row offered")
			}
			if m.StartAgentCmd != cmd {
				t.Errorf("StartAgentCmd=%q, want %q", m.StartAgentCmd, cmd)
			}
			// The status line explains the state in words. A raw command
			// belongs on the action's tooltip and the clipboard, not in the
			// sentence a non-technical user reads first.
			if strings.Contains(m.StatusMsg, cmd) {
				t.Errorf("StatusMsg=%q still contains the raw command", m.StatusMsg)
			}
			if m.StatusMsg == "" {
				t.Error("no explanation of what is wrong")
			}
		})
	}
}

// The macOS command must name the system-domain LaunchDaemon (#520). The
// tray's own copy of this string went stale for exactly this reason: it kept
// naming the per-user `gui/<uid>` job long after that job stopped existing.
func TestDaemonDownModelFor_DarwinNamesTheSystemDomainJob(t *testing.T) {
	got := daemonDownModelFor("darwin", installedFacts()).StartAgentCmd
	if strings.Contains(got, "gui/") {
		t.Errorf("StartAgentCmd=%q names the pre-#520 per-user LaunchAgent domain", got)
	}
	if !strings.Contains(got, "system/") {
		t.Errorf("StartAgentCmd=%q does not name the system domain", got)
	}
}

// A hand-built binary run from a terminal has no registered service. Offering
// a button whose only possible outcome is an error is worse than offering
// none, so the rows drop out and the text says what is actually true.
func TestDaemonDownModelFor_NoServiceMeansNoButton(t *testing.T) {
	m := daemonDownModelFor("linux", daemonDownFacts{ServiceInstalled: false})

	if m.StartAgentAction != "" || m.StartAgentCopy != "" || m.StartAgentCmd != "" {
		t.Errorf("unregistered service still offers an action: %+v", m)
	}
	if !strings.Contains(m.StatusMsg, "not registered") {
		t.Errorf("StatusMsg=%q does not say the service is not registered", m.StatusMsg)
	}
}

// An OS with no service backend at all (service_stub.go) has no command to
// offer either, even if something claimed a service was installed.
func TestDaemonDownModelFor_UnsupportedOSOffersNothing(t *testing.T) {
	m := daemonDownModelFor("plan9", installedFacts())
	if m.StartAgentAction != "" || m.StartAgentCmd != "" {
		t.Errorf("unsupported OS still offers an action: %+v", m)
	}
}

// PRODUCT CONTRACT (#315 root cause 2). The tray autostarts at login while the
// Windows service is delayed-auto-start, so for the first couple of minutes
// after every boot the agent is legitimately not up. Painting the red failure
// state there — and popping a UAC prompt for it — teaches people to ignore
// both. The action stays available: someone who does not want to wait should
// not have to.
func TestDaemonDownModelFor_StartingIsNotAFailure(t *testing.T) {
	m := daemonDownModelFor("windows", daemonDownFacts{ServiceInstalled: true, Starting: true})

	if m.Icon != IconBusy {
		t.Errorf("Icon=%v, want IconBusy while starting", m.Icon)
	}
	if strings.Contains(m.HeaderTitle, "not running") {
		t.Errorf("HeaderTitle=%q calls a normal boot a failure", m.HeaderTitle)
	}
	if m.StartAgentAction == "" {
		t.Error("start action should stay available during the grace window")
	}

	down := daemonDownModelFor("windows", installedFacts())
	if down.Icon != IconError {
		t.Errorf("Icon=%v past the grace window, want IconError", down.Icon)
	}
	if down.HeaderTitle == m.HeaderTitle {
		t.Errorf("starting and failed states share the header %q", down.HeaderTitle)
	}
}

// A stopped service does not sign anyone out. The rc7 review read the
// daemon-down menu as "logged out" and re-ran setup on a device whose identity
// was on disk and valid for months (#317 item 5).
func TestDaemonDownModelFor_KeepsTheSignedInAccount(t *testing.T) {
	m := daemonDownModelFor("linux", daemonDownFacts{
		ServiceInstalled: true,
		LastEmail:        "user@example.com",
	})
	if m.AccountEmail != "user@example.com" {
		t.Errorf("AccountEmail=%q, want the last known account", m.AccountEmail)
	}
	// ...and it must not offer a sign-in, which is what "logged out" looked
	// like. Nothing about the account changed; the service is just down.
	if m.ToggleAction != "" {
		t.Errorf("ToggleAction=%q, want no sign-in affordance", m.ToggleAction)
	}
}

// startGraceFor: Windows needs minutes (delayed-auto-start), the Unixes need
// seconds (systemd/launchd start the agent as part of boot).
func TestStartGraceFor(t *testing.T) {
	if got := startGraceFor("windows"); got < 2*time.Minute {
		t.Errorf("startGraceFor(windows)=%v, want at least the SCM's ~2 minute delayed-start wait", got)
	}
	for _, goos := range []string{"linux", "darwin"} {
		got := startGraceFor(goos)
		if got <= 0 || got > time.Minute {
			t.Errorf("startGraceFor(%s)=%v, want a short socket-open window", goos, got)
		}
	}
}

// A failed /identity call is a transport failure, not a statement about
// enrollment — Client.Identity folds 404 into {Enrolled:false} itself. The
// signed-out menu must not be rendered from a dropped poll.
func TestUpdate_IdentityErrorRendersReconnectingNotSignedOut(t *testing.T) {
	got := Update(Snapshot{Health: HealthOnline, IdentityErr: true})

	if got.Kind == MenuNotSignedIn {
		t.Error("a failed identity poll rendered the signed-out menu")
	}
	if got.ToggleAction != "" {
		t.Errorf("ToggleAction=%q, want no sign-in prompt", got.ToggleAction)
	}
	if got.Icon != IconBusy {
		t.Errorf("Icon=%v, want IconBusy", got.Icon)
	}
	if !strings.Contains(strings.ToLower(got.StatusMsg), "signed in") {
		t.Errorf("StatusMsg=%q does not reassure the user they are still signed in", got.StatusMsg)
	}
}

// The daemon answering "nobody is enrolled" is a different thing, and still
// has to produce the sign-in menu.
func TestUpdate_GenuinelyNotEnrolledStillOffersSignIn(t *testing.T) {
	got := Update(Snapshot{Health: HealthOnline})
	if got.Kind != MenuNotSignedIn {
		t.Errorf("Kind=%v, want MenuNotSignedIn", got.Kind)
	}
	if got.ToggleAction == "" {
		t.Error("want a sign-in affordance when the daemon reports no enrollment")
	}
}
