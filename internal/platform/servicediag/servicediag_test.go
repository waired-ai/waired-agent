package servicediag

import (
	"runtime"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/platform/service"
)

// PRODUCT CONTRACT (#315). This is the case the whole package exists for.
//
// On the rc7 Windows host the service was not running after a reboot. The
// System log carried SCM 7000 ("An Application Control policy has blocked this
// file") and the CodeIntegrity log carried 3033 in the same second — Smart App
// Control refusing the unsigned waired-agent.exe at boot, when its cloud
// reputation check is unavailable. Reaching that conclusion took correlating
// two logs in Event Viewer. Nobody does that; `waired doctor` said only
// "unreachable".
func TestExplain_WindowsSmartAppControlBlock(t *testing.T) {
	events := []Event{
		{Source: "Service Control Manager", ID: 7000,
			Message: "The Waired Agent service failed to start due to the following error: " +
				"An Application Control policy has blocked this file."},
		{Source: "Microsoft-Windows-CodeIntegrity", ID: 3033,
			Message: `Code Integrity determined that a process (services.exe) attempted to load ` +
				`waired-agent.exe that did not meet the Enterprise signing level requirements.`},
	}

	got := Explain("windows", false, events)

	if got.Status != Failed {
		t.Errorf("Status=%v, want Failed", got.Status)
	}
	// The user is told what happened in words they can act on, without
	// "CodeIntegrity", "3033", or "signing level" in the sentence itself.
	for _, want := range []string{"blocked", "signed"} {
		if !strings.Contains(strings.ToLower(got.Cause), want) {
			t.Errorf("Cause=%q does not mention %q", got.Cause, want)
		}
	}
	if got.Hint == "" {
		t.Error("no hint: the user is told what happened and not what to do")
	}
	// ...but the raw record is still quoted, so the diagnosis can be checked
	// rather than taken on faith.
	if !strings.Contains(got.Evidence, "7000") {
		t.Errorf("Evidence=%q does not quote the SCM record", got.Evidence)
	}
}

// SCM 7000 carries its reason in the message, so the block is decodable even
// when the CodeIntegrity channel is unreadable — which it is for an
// unprivileged doctor run on some builds.
func TestExplain_WindowsPolicyBlockFromTheSCMRecordAlone(t *testing.T) {
	got := Explain("windows", false, []Event{
		{Source: "Service Control Manager", ID: 7000,
			Message: "The Waired Agent service failed to start due to the following error: " +
				"An Application Control policy has blocked this file."},
	})
	if got.Status != Failed || !strings.Contains(strings.ToLower(got.Cause), "blocked") {
		t.Errorf("got %+v, want the policy block decoded from 7000 alone", got)
	}
}

func TestExplain_WindowsOtherFailures(t *testing.T) {
	cases := map[string]struct {
		event    Event
		wantWord string
	}{
		"generic start failure": {
			Event{Source: "Service Control Manager", ID: 7000, Message: "The service failed to start: access denied."},
			"failed to start",
		},
		"start timeout": {
			Event{Source: "Service Control Manager", ID: 7009, Message: "Timed out (30000 ms) waiting for the service to connect."},
			"too long",
		},
		"unexpected termination": {
			Event{Source: "Service Control Manager", ID: 7031, Message: "The service terminated unexpectedly."},
			"stopped unexpectedly",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := Explain("windows", false, []Event{c.event})
			if got.Status != Failed {
				t.Errorf("Status=%v, want Failed", got.Status)
			}
			if !strings.Contains(strings.ToLower(got.Cause), c.wantWord) {
				t.Errorf("Cause=%q does not mention %q", got.Cause, c.wantWord)
			}
		})
	}
}

// A service that is up now still gets its history read: "it was blocked at
// boot and is only running because you started it by hand" is a different
// answer from "OK", and it is the one that tells the user this will recur.
func TestExplain_RunningAfterAPastFailureIsNotSilent(t *testing.T) {
	got := Explain("windows", true, []Event{
		{Source: "Service Control Manager", ID: 7000,
			Message: "An Application Control policy has blocked this file."},
	})
	if got.Status != Healthy {
		t.Errorf("Status=%v, want Healthy — the service is up now", got.Status)
	}
	if got.Evidence == "" {
		t.Error("the past failure was dropped; the user has no idea it will recur at the next boot")
	}
}

func TestExplain_LinuxFailedUnit(t *testing.T) {
	got := Explain("linux", false, []Event{
		{Source: "systemd", Message: "ActiveState=failed"},
		{Source: "systemd", Message: "Result=exit-code"},
		{Source: "systemd", Message: "ExecMainStatus=1"},
		{Source: "systemd", Message: "NRestarts=3"},
		{Source: "journal", Message: "identity: no enrollment on disk"},
	})

	if got.Status != Failed {
		t.Errorf("Status=%v, want Failed", got.Status)
	}
	if !strings.Contains(got.Cause, "restarted 3 time") {
		t.Errorf("Cause=%q loses the restart count — a crash-loop reads as a one-off", got.Cause)
	}
	if !strings.Contains(got.Evidence, "identity") {
		t.Errorf("Evidence=%q does not quote the journal line", got.Evidence)
	}
}

// Stopped on purpose is not a failure, and must not fail `waired doctor` — but
// the user asking why nothing works still has to be told.
func TestExplain_LinuxDeliberatelyStopped(t *testing.T) {
	got := Explain("linux", false, []Event{
		{Source: "systemd", Message: "ActiveState=inactive"},
		{Source: "systemd", Message: "Result=success"},
	})
	if got.Status != Stopped {
		t.Errorf("Status=%v, want Stopped", got.Status)
	}
	if got.Hint == "" {
		t.Error("no hint for a stopped service")
	}
}

func TestExplain_DarwinNonZeroExit(t *testing.T) {
	got := Explain("darwin", false, []Event{
		{Source: "launchd", Message: "state = not running"},
		{Source: "launchd", Message: "last exit code = 78"},
		{Source: "waired-agent.err.log", Message: "state dir not readable"},
	})
	if got.Status != Failed {
		t.Errorf("Status=%v, want Failed", got.Status)
	}
	if !strings.Contains(got.Cause, "78") {
		t.Errorf("Cause=%q drops the exit status", got.Cause)
	}
}

// TestExplain_DarwinNeverExitedIsNotAnError pins #652: launchd's healthy
// steady state prints `last exit code = (never exited)`, and a string
// compare against "0" let it through into "exited with an error (status
// (never exited))". The observed host showed that warning in the same
// doctor run that reported `✓ management — HTTP 200` and
// `✓ inference engine — ready`.
//
// Product contract from #652, not a record of today's behaviour. The
// literals are the ones launchctl actually printed on macOS 26.5.1.
func TestExplain_DarwinNeverExitedIsNotAnError(t *testing.T) {
	got := Explain("darwin", true, []Event{
		{Source: "launchd", Message: "state = running"},
		{Source: "launchd", Message: "last exit code = (never exited)"},
		{Source: "launchd", Message: "pid = 1234"},
	})
	if got.Status == Failed {
		t.Errorf("Status=%v: a never-exited service is not a failure", got.Status)
	}
	if strings.Contains(got.Cause, "never exited") {
		t.Errorf("Cause=%q pipes launchd's placeholder into a sentence about an exit status", got.Cause)
	}
	if strings.Contains(got.Cause, "exited with an error") {
		t.Errorf("Cause=%q claims an error for a service that is running", got.Cause)
	}
}

func TestExitCode(t *testing.T) {
	cases := []struct {
		in     string
		wantN  int
		wantOK bool
	}{
		{"0", 0, true},
		{"78", 78, true},
		{" 78 ", 78, true},
		{"(never exited)", 0, false},
		{"", 0, false},
		{"unknown", 0, false},
	}
	for _, c := range cases {
		n, ok := exitCode(c.in)
		if n != c.wantN || ok != c.wantOK {
			t.Errorf("exitCode(%q) = (%d, %v), want (%d, %v)", c.in, n, ok, c.wantN, c.wantOK)
		}
	}
}

func TestExplain_DarwinNotRunning(t *testing.T) {
	got := Explain("darwin", false, []Event{
		{Source: "launchd", Message: "state = not running"},
		{Source: "launchd", Message: "last exit code = 0"},
	})
	if got.Status != Stopped {
		t.Errorf("Status=%v, want Stopped", got.Status)
	}
}

// "Down, and I have no idea why" must produce nothing. `waired doctor` already
// prints an unreachable-daemon line; a second finding that explains nothing is
// noise for someone who is already stuck.
func TestExplain_NoEvidenceSaysNothing(t *testing.T) {
	for _, goos := range []string{"windows", "linux", "darwin", "plan9"} {
		t.Run(goos, func(t *testing.T) {
			got := Explain(goos, false, nil)
			if got.Status != Unknown || got.Cause != "" {
				t.Errorf("got %+v, want the zero Result", got)
			}
		})
	}
}

func TestExplain_HealthyAndQuiet(t *testing.T) {
	got := Explain("linux", true, []Event{
		{Source: "systemd", Message: "ActiveState=active"},
		{Source: "systemd", Message: "Result=success"},
	})
	if got.Status != Healthy {
		t.Errorf("Status=%v, want Healthy", got.Status)
	}
	if got.Evidence != "" {
		t.Errorf("Evidence=%q on a healthy service with no history", got.Evidence)
	}
}

// The hints quote start commands. This package keeps its own copies rather
// than importing the SCM/systemd-laden service package, so pin the agreement —
// a hint naming a command that does not work is worse than no hint.
//
// Only the running OS's copy is checkable here (service.StartHint is per-OS),
// but ci.yml runs the unit suite on all three, so all three are covered across
// the matrix.
func TestStartCommandMatchesTheServicePackage(t *testing.T) {
	ours := map[string]string{
		"windows": startCommandWindows,
		"linux":   startCommandLinux,
		"darwin":  startCommandDarwin,
	}[runtime.GOOS]
	if ours == "" {
		t.Skipf("no start command for %s", runtime.GOOS)
	}
	if want := service.StartHint(); ours != want {
		t.Errorf("%s: servicediag says %q, service.StartHint says %q", runtime.GOOS, ours, want)
	}
}
