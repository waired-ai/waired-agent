package service

import (
	"runtime"
	"testing"
)

// PRODUCT CONTRACT (waired-agent#684): all three supervisors bring the
// agent back after RestartRequestedExitCode, and the table says which of
// them can tell that exit apart from a crash.
//
// The issue this pins: exit 17 was wired into the systemd unit only, and
// nothing in the tree recorded what the other two did. It was not "the
// other two stay down" — launchd restarts on any non-zero exit, and the
// Windows path restarted too, by exiting 1 from inside the service
// process so the SCM saw a hard crash. "Green on Linux, wrong elsewhere,
// and nothing says so" is the failure mode CLAUDE.md §Cross-OS parity
// exists for (waired#746–#758), and it already hid one bug (#656/#670).
//
// Untagged, so the three answers are checked on the Linux `unit tests`
// leg — the only required one. The per-OS renderers are pinned by their
// own tests on legs that are not.
func TestRestartOnExitFor(t *testing.T) {
	cases := map[string]RestartOnExit{
		"linux": {
			Restarts:  true,
			Named:     true,
			Mechanism: "systemd SuccessExitStatus=17 + RestartForceExitStatus=17",
		},
		"darwin": {
			Restarts:  true,
			Named:     false,
			Mechanism: "launchd KeepAlive{SuccessfulExit=false} — any non-zero exit",
		},
		"windows": {
			Restarts:  true,
			Named:     true,
			Mechanism: "SCM ServiceSpecificExitCode=17 + recovery actions on non-crash failures",
		},
		// An OS with no service backend (service_stub.go) has no
		// supervisor, so there is nothing to restart it and nothing to
		// name. The zero value is the honest answer.
		"plan9": {},
	}
	for goos, want := range cases {
		t.Run(goos, func(t *testing.T) {
			if got := RestartOnExitFor(goos); got != want {
				t.Errorf("RestartOnExitFor(%q) =\n  %+v\nwant\n  %+v", goos, got, want)
			}
		})
	}
}

// Records today's behaviour: darwin is the one OS that cannot name the
// code. Stated as its own assertion because it is the reason the Named
// field exists — a reader who only sees three `Restarts: true` rows would
// conclude the three are equivalent, and they are not.
func TestRestartOnExitFor_OnlyDarwinCannotNameTheCode(t *testing.T) {
	for _, goos := range []string{"linux", "windows"} {
		if !RestartOnExitFor(goos).Named {
			t.Errorf("%s should be able to name exit %d", goos, RestartRequestedExitCode)
		}
	}
	if RestartOnExitFor("darwin").Named {
		t.Errorf("launchd has no per-exit-code KeepAlive key; darwin cannot name exit %d",
			RestartRequestedExitCode)
	}
}

// PRODUCT CONTRACT (waired-agent#684): the intent is recorded before the
// process is taken down, so no exit path can observe the shutdown without
// also seeing why.
//
// The ordering is the whole point on Windows: svcHandler.Execute reads the
// flag when run() returns, and run() returns *because* RequestRestart
// cancelled it. A flag set afterwards would always be too late.
func TestRequestRestart_RecordsTheIntentBeforeTakingTheDaemonDown(t *testing.T) {
	restartRequested.Store(false)
	t.Cleanup(func() { restartRequested.Store(false) })

	if RestartRequested() {
		t.Fatal("the intent is set before anything asked for a restart")
	}

	// The mechanism takes the process down on every OS, so the ordering
	// is observed through the seam rather than by running one.
	seen := false
	requestRestart(func() { seen = RestartRequested() })

	if !seen {
		t.Error("the per-OS mechanism ran before the intent was recorded")
	}
	if !RestartRequested() {
		t.Error("RestartRequested() is false after a restart request")
	}
}

// Records today's behaviour: this host's own answer is a real one. A new
// GOOS reaching the default arm would ship a build whose supervisor
// contract is blank, and CLAUDE.md §Cross-OS parity wants that visible
// rather than assumed.
func TestRestartOnExitFor_ThisHostHasAnAnswer(t *testing.T) {
	if got := RestartOnExitFor(runtime.GOOS); got.Mechanism == "" {
		t.Errorf("%s has no entry: %+v", runtime.GOOS, got)
	}
}
