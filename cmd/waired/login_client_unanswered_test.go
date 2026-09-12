package main

import (
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
)

// PRODUCT CONTRACT (waired-agent#1300, owner ruling 2026-09-12): a
// `waired init` whose stdin is empty stops at the first question that
// commits this computer to something, says which flag answers it, and
// exits non-zero.
//
// End to end because the table beside it pins the box and the code from a
// struct, and this is the run that has to produce the struct. The shape
// is the rc6 review's Windows host: ssh with no pty, so stdin is at EOF
// from the first read. Until now that printed "[Y/n] (default: Yes)",
// applied No, ended on "✅ Waired is ready" and exited 0, and the server
// build that ran it had no way to learn that the local inference it was
// installed for was never turned on.
func TestRunInitViaDaemon_EmptyStdinStopsAtTheQuestionAndExitsNonZero(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	shrinkSetupTimers(t)
	owner := scriptStdin("") // ssh with no pty: EOF on the first read
	d := &promptsDaemon{statusSeq: []management.InferenceStatus{readyStatus()}}

	out := runDaemonInit(t, d.server(t).URL, owner,
		daemonInitScenario{noBrowser: true, wantExit: exitNoAnswer})

	for _, want := range []string{
		`No answer on stdin, so "Set up coding-agent integration?" went unanswered.`,
		"--skip-integration",
		"setup stopped at a question nobody answered",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("empty-stdin run missing %q\n---\n%s", want, out)
		}
	}
	for _, unwanted := range []string{
		"Waired is ready — setup is complete",
		// The old account of this run, and the claim #1300 is about: a
		// skip is something somebody decided.
		"Skipped. Set it up anytime with",
	} {
		if strings.Contains(out, unwanted) {
			t.Errorf("empty-stdin run still printed %q\n---\n%s", unwanted, out)
		}
	}
}

// NEGATIVE CONTROL, and the row that keeps the fix off the population it
// must not touch. --non-interactive answers every one of these questions
// before stdin is read, so a host with no terminal at all still finishes
// and still exits 0 — which is what install.sh --non-interactive and
// install.ps1's redirected-stdin detection both rely on.
func TestRunInitViaDaemon_NonInteractiveWithEmptyStdinStillFinishes(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	shrinkSetupTimers(t)
	owner := scriptStdin("")
	d := &promptsDaemon{statusSeq: []management.InferenceStatus{readyStatus()}}

	out := runDaemonInit(t, d.server(t).URL, owner,
		daemonInitScenario{noBrowser: true, nonInteractive: true})

	if strings.Contains(out, "went unanswered") {
		t.Errorf("--non-interactive must answer its own questions\n---\n%s", out)
	}
}

// NEGATIVE CONTROL: an answered question is not an unanswered one. A
// typed "n" declines, which is a decision, and the run ends as it always
// has — on the box for a computer that was asked not to do the thing, and
// on exit 0.
func TestRunInitViaDaemon_ATypedNoIsStillADecision(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	shrinkSetupTimers(t)
	owner := scriptStdin("n\nn\nn\n")
	d := &promptsDaemon{statusSeq: []management.InferenceStatus{readyStatus()}}

	out := runDaemonInit(t, d.server(t).URL, owner, daemonInitScenario{noBrowser: true})

	if strings.Contains(out, "went unanswered") {
		t.Errorf("a typed answer must not read as no answer\n---\n%s", out)
	}
	if !strings.Contains(out, "Skipped. Set it up anytime with") {
		t.Errorf("a declined integration keeps its own account\n---\n%s", out)
	}
}
