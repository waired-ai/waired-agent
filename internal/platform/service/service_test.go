package service

import (
	"runtime"
	"strings"
	"testing"
)

// PRODUCT CONTRACT. These are the commands we print to users and render in
// the tray when the agent is down; getting one wrong tells someone to run
// something that does nothing.
//
// The darwin case is the reason this test exists. StartHint used to be four
// per-OS definitions, with a fifth copy in internal/gui/tray/hint_darwin.go.
// #520 moved macOS from a per-user LaunchAgent to a system LaunchDaemon and
// updated one of them; the tray's copy went on telling users to kickstart
// `gui/$(id -u)/com.waired.agent` — a job that had ceased to exist. No test
// named any of the strings and no CI leg runs on macOS, so nothing went red.
// One untagged function plus this table is what makes that impossible.
func TestStartHintFor(t *testing.T) {
	cases := map[string]string{
		"windows": "Start-Service waired-agent",
		"linux":   "sudo systemctl start waired-agent",
		"darwin":  "sudo launchctl kickstart -k system/com.waired.agent",
		// An OS with no service backend (service_stub.go) has no honest
		// answer. Callers must treat "" as "say nothing", not print an empty
		// command.
		"plan9": "",
	}
	for goos, want := range cases {
		t.Run(goos, func(t *testing.T) {
			if got := StartHintFor(goos); got != want {
				t.Errorf("StartHintFor(%q) = %q, want %q", goos, got, want)
			}
		})
	}
}

// The darwin hint has to name the same launchd domain and label that
// darwinManager.Start kickstarts, or the hint is a command that starts
// something else (or nothing).
func TestStartHintForDarwinMatchesTheSystemDomainJob(t *testing.T) {
	got := StartHintFor("darwin")
	if !strings.Contains(got, "system/"+DarwinLabel) {
		t.Errorf("darwin hint %q does not name the system-domain job %q", got, DarwinLabel)
	}
	if strings.Contains(got, "gui/") {
		t.Errorf("darwin hint %q still names the pre-#520 per-user LaunchAgent domain", got)
	}
}

func TestStartHintUsesTheRunningOS(t *testing.T) {
	if got, want := StartHint(), StartHintFor(runtime.GOOS); got != want {
		t.Errorf("StartHint() = %q, want the %s entry %q", got, runtime.GOOS, want)
	}
}
