package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
)

// TestExitPlanFor pins the numbers themselves. PRODUCT CONTRACT: these are
// a shell interface — install.sh and install.ps1 branch on them, and a
// change here is a change to what those scripts tell the user.
//
// 3 rather than 1 because with only 0 and 1 an installer's two options are
// both wrong: 0 is what let it print "🎉 Waired is installed." over a
// device whose engine never came up, and 1 would have it report a sign-in
// that plainly succeeded as "sign-in did not complete". 130 is the Ctrl-C
// path's (setup_executor.go) and stays a real interruption.
func TestExitPlanFor(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		want      int
		wantPrint bool
	}{
		{name: "nothing went wrong", err: nil, want: 0},
		{
			// Non-zero AND silent, which is the unusual pair: the closing
			// box is the user-facing account of this outcome, and a
			// "waired: ..." line under it reads as a second problem.
			name: "signed in, but local AI is down",
			err:  errLocalAIDown, want: exitLocalAIDown, wantPrint: false,
		},
		{
			// The caller returns the sentinel bare today, but an error that
			// grew context on the way out must still land on the same code.
			name: "the same outcome, wrapped",
			err:  fmt.Errorf("finishing setup: %w", errLocalAIDown),
			want: exitLocalAIDown, wantPrint: false,
		},
		{
			name: "any other failure",
			err:  errors.New("login timed out waiting for the daemon"),
			want: 1, wantPrint: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, printErr := exitPlanFor(tc.err)
			if got != tc.want {
				t.Errorf("exitPlanFor(%v) code = %d, want %d", tc.err, got, tc.want)
			}
			if printErr != tc.wantPrint {
				t.Errorf("exitPlanFor(%v) printErr = %v, want %v", tc.err, printErr, tc.wantPrint)
			}
		})
	}
	// The one thing a table of names cannot say: 3 must not collide with a
	// code that already means something else on this command.
	if exitLocalAIDown == 0 || exitLocalAIDown == 1 || exitLocalAIDown == 130 {
		t.Errorf("exitLocalAIDown = %d, which already means something else", exitLocalAIDown)
	}
}

// The closing box is the last thing `waired init` says, and until #310 it
// was chosen by asking only whether the engine could be INSTALLED. On the
// rc7 host the install worked, the engine then would not run, and the run
// ended on "everything completed successfully!" over a device with no
// local AI at all.
//
// Product contract. The negative cases are the point as much as the
// positive one: not-ready is the honest answer on plenty of hosts where
// nothing is wrong, so only a STATED engine fault may change the box.
func TestPrintDaemonSummaryBoxPicksTheOutcomeItCanDefend(t *testing.T) {
	// Substrings, not whole lines: box() pads and frames its content, and
	// emoji are dropped when the terminal cannot render them.
	const (
		celebration  = "everything completed successfully"
		needsInstall = "local AI still needs installing"
		notRunning   = "local AI isn't running"
		notAnswering = "local AI is not answering yet"
		startsOff    = "local AI starts off on this computer"
		installsOff  = "engine installs are turned off here"
	)
	slow := func() *management.HostSpeedStatus {
		return &management.HostSpeedStatus{
			TurnSeconds: 66.9, BudgetSeconds: 45, TurnedInferenceOff: true,
		}
	}

	cases := []struct {
		name    string
		summary daemonSummary
		want    string
		absent  []string
		// wantExit is the process exit code the same outcome produces.
		// Same table as the box on purpose: the two are derived from one
		// struct by one rule, and splitting them across two tables is how
		// they would drift into disagreeing about the same host (#310).
		wantExit int
	}{
		{
			name:    "everything landed",
			summary: daemonSummary{accountEmail: "someone@example.test"},
			want:    celebration,
			absent:  []string{needsInstall, notRunning},
		},
		{
			name:     "the engine could not be installed (#188)",
			summary:  daemonSummary{engineErr: errors.New("download: 403")},
			want:     needsInstall,
			absent:   []string{celebration},
			wantExit: exitLocalAIDown,
		},
		{
			// #310, the case that had no box of its own. The install box
			// would be wrong here: it points at the command that installs
			// an engine, and this host already has one.
			name:     "the engine installed and would not stay up (#310)",
			summary:  daemonSummary{engineFailure: "ollama: process exited during startup: signal: killed"},
			want:     notRunning,
			absent:   []string{celebration, needsInstall},
			wantExit: exitLocalAIDown,
		},
		{
			// Both true: the install never produced an engine, so there is
			// nothing for the #310 wording to describe.
			name: "an install failure outranks a wait that saw no engine",
			summary: daemonSummary{
				engineErr:     errors.New("download: 403"),
				engineFailure: "ollama: not reachable",
			},
			want:     needsInstall,
			absent:   []string{celebration, notRunning},
			wantExit: exitLocalAIDown,
		},
		{
			// #552, the third case that had no box of its own. The engine
			// installed, stayed up, took the model — and returned HTTP 500
			// on the test generation. The run still ended on the
			// celebration, three lines after saying so.
			name:     "the engine ran and could not answer (#29/#552)",
			summary:  daemonSummary{benchFailed: true},
			want:     notAnswering,
			absent:   []string{celebration, needsInstall, notRunning},
			wantExit: exitLocalAIDown,
		},
		{
			// Order: an engine that never stayed up cannot have had a
			// benchmark reach it, so #310's wording is the one that
			// describes this host.
			name: "an engine that would not stay up outranks the benchmark",
			summary: daemonSummary{
				engineFailure: "ollama: process exited during startup: signal: killed",
				benchFailed:   true,
			},
			want:     notRunning,
			absent:   []string{celebration, notAnswering},
			wantExit: exitLocalAIDown,
		},
		{
			// NEGATIVE CONTROL, and the one that matters most here. A
			// benchmark that was SKIPPED is not a benchmark that failed:
			// routing-only nodes and hosts pointed at an external
			// endpoint return Capacity=0 and never benchmark by design
			// (#203). Keying the box on "no measurement" instead of on a
			// stated failure would warn every one of them.
			name:    "a skipped benchmark is not a failed one",
			summary: daemonSummary{accountEmail: "someone@example.test", bench: benchmarkOutcome{}},
			want:    celebration,
			absent:  []string{notAnswering, notRunning},
		},
		{
			// NEGATIVE CONTROL. A gateway-only host answers `disabled` and
			// the wait returns not-ready by design. Keying the box on
			// "the wait did not reach ready" instead of on a stated fault
			// would hand these operators a warning about a machine that is
			// doing exactly what they configured.
			name:    "inference is simply switched off",
			summary: daemonSummary{accountEmail: "someone@example.test"},
			want:    celebration,
			absent:  []string{notRunning},
		},
		{
			// waired#1099. The measurement left local AI off, so the
			// ordinary box's two claims — "everything completed
			// successfully" and "Local inference is live via the
			// waired-agent daemon" — are both false on this host. Nothing
			// FAILED, so it is not one of the fault boxes either, and the
			// exit code stays 0: #465's off is a default with a working
			// opt-in, and an installer must not read it as a bad install
			// (waired-ai/waired#1056).
			name: "the measurement left local AI off",
			summary: daemonSummary{
				accountEmail: "someone@example.test",
				hostSpeed:    slow(),
			},
			want:   startsOff,
			absent: []string{celebration, notRunning, notAnswering, "is live"},
		},
		{
			// Order: a real fault outranks the measurement. An engine that
			// would not stay up was never measured usefully, and telling
			// that operator their machine is slow points at the wrong
			// thing.
			name: "an engine that would not stay up outranks the measurement",
			summary: daemonSummary{
				engineFailure: "ollama: process exited during startup: signal: killed",
				hostSpeed:     slow(),
			},
			want:     notRunning,
			absent:   []string{startsOff, celebration},
			wantExit: exitLocalAIDown,
		},
		{
			// A figure on a host that CLEARED the budget is just a figure.
			// TurnedInferenceOff is the claim, not the number.
			name: "a fast host keeps the ordinary box, with its speed on it",
			summary: daemonSummary{
				accountEmail: "someone@example.test",
				hostSpeed:    &management.HostSpeedStatus{TurnSeconds: 4.5, BudgetSeconds: 45},
			},
			want:   celebration,
			absent: []string{startsOff, notRunning},
		},
		{
			// #551. The same engineErr field as the #188 row above, and a
			// different outcome, because the operator asked for this: the
			// installer ran --skip-ollama, or WAIRED_NO_OLLAMA is set on
			// the host. Nothing failed, so nothing warns and the exit code
			// stays 0 — the pairing this table exists to keep honest.
			//
			// `celebration` is absent as well as the three faults: this
			// host has no local AI, so "everything completed successfully
			// — local inference is live" would be the #310 shape one
			// reason along.
			name: "engine installs are turned off on this host (#551)",
			summary: daemonSummary{
				accountEmail: "someone@example.test",
				engineErr:    fmt.Errorf("%w (%s)", errEngineOptOut, "WAIRED_NO_OLLAMA"),
			},
			want:   installsOff,
			absent: []string{celebration, needsInstall, notRunning, notAnswering, startsOff},
		},
		{
			// PRECEDENCE against waired#1099's box, and the reason this row
			// exists rather than being left to the switch's order: both are
			// "local AI is off and nothing failed", so a reader could take
			// either. They differ in the REMEDY, and only one of them is
			// true here — `waired inference on` cannot produce local AI on a
			// host that will not install an engine, so pointing at it would
			// send the operator round a loop. The opt-out wins.
			name: "the opt-out outranks the measurement, because their remedies differ",
			summary: daemonSummary{
				accountEmail: "someone@example.test",
				engineErr:    fmt.Errorf("%w (%s)", errEngineOptOut, "WAIRED_NO_OLLAMA"),
				hostSpeed:    slow(),
			},
			want:   installsOff,
			absent: []string{startsOff, celebration},
		},
		{
			// Unreachable today — no engine was installed, so nothing can
			// be down — and pinned anyway, because that is the assumption
			// engineOptOut is built on. If a stated fault ever does turn
			// up beside the sentinel, this must NOT become the arm that
			// swallows it: the run keeps its warn box and its exit 3, and
			// only which warn box is a question of the existing ordering.
			name: "a stated fault beside the opt-out is still a fault",
			summary: daemonSummary{
				engineErr:     fmt.Errorf("%w (%s)", errEngineOptOut, "WAIRED_NO_OLLAMA"),
				engineFailure: "ollama: process exited during startup: signal: killed",
			},
			want:     needsInstall,
			absent:   []string{celebration, installsOff},
			wantExit: exitLocalAIDown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			printDaemonSummaryBox(&out, tc.summary)
			got := out.String()
			if !strings.Contains(got, tc.want) {
				t.Errorf("expected %q in the summary, got: %q", tc.want, got)
			}
			for _, a := range tc.absent {
				if strings.Contains(got, a) {
					t.Errorf("did not expect %q in the summary, got: %q", a, got)
				}
			}
			// The exit code the installers branch on, from the same
			// outcome. The zero rows are the load-bearing ones: a
			// gateway-only host must keep exiting 0, or install.sh starts
			// warning about machines that are doing exactly what they
			// were configured to do.
			if got, _ := exitPlanFor(tc.summary.exitErr()); got != tc.wantExit {
				t.Errorf("exit code = %d, want %d", got, tc.wantExit)
			}
		})
	}
}
