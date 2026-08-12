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
		settingUp    = "local AI is still setting up here"
		noModel      = "no model chosen for this computer"
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
		{
			// #569, and the pairing is the whole point: the box stops
			// claiming a live local AI, and the exit code stays 0. A
			// download that has not finished is not a failed install, and
			// install.sh / install.ps1 branch on this number.
			//
			// `is live` is in the absent list because that sentence — not
			// the title — is the false claim this row exists to stop: on
			// job 93067141684 it was printed over a host whose 6.6 GB
			// model did not arrive until the following init.
			name: "the model was still on its way when init's window closed (#569)",
			summary: daemonSummary{
				accountEmail: "someone@example.test",
				modelPending: true,
			},
			want:   settingUp,
			absent: []string{celebration, needsInstall, notRunning, notAnswering, "is live"},
		},
		{
			// Order: a stated fault outranks "not yet". The wait sets
			// engineFailure and pending on different arms, so this is
			// unreachable today and pinned for the same reason as the
			// opt-out row above — this must never become the arm that
			// swallows a fault.
			name: "an engine that would not stay up outranks a model still arriving",
			summary: daemonSummary{
				engineFailure: "ollama: process exited during startup: signal: killed",
				modelPending:  true,
			},
			want:     notRunning,
			absent:   []string{settingUp, celebration},
			wantExit: exitLocalAIDown,
		},
		{
			// Order against the measurement's box (waired#1099). Both are
			// non-faults, and the remedies decide it: `waired inference
			// on` is actionable on a host the measurement switched off,
			// whereas waiting for a download nobody is running is not.
			name: "the measurement's own box outranks a model still arriving",
			summary: daemonSummary{
				accountEmail: "someone@example.test",
				modelPending: true,
				hostSpeed:    slow(),
			},
			want:   startsOff,
			absent: []string{settingUp, celebration},
		},
		{
			// NEGATIVE CONTROL for #569, and the row that stops the fix
			// from being written as plain !ready. A gateway-only host is
			// not-ready by design, so the wait leaves pending false and
			// this operator keeps the box and the exit code they had.
			name:    "a host with inference switched off is not 'still setting up'",
			summary: daemonSummary{accountEmail: "someone@example.test", modelPending: false},
			want:    celebration,
			absent:  []string{settingUp, notRunning},
		},
		{
			// waired-agent#736. The success box claims "Local inference is
			// live via the waired-agent daemon" unconditionally, so a host
			// that finished setup with nothing to serve must not reach it —
			// and it must not reach the still-setting-up box either, which
			// promises background work nobody started.
			name: "a host with no model chosen gets neither the celebration nor 'still setting up'",
			summary: daemonSummary{
				accountEmail:  "someone@example.test",
				noModelChosen: true,
			},
			want:   noModel,
			absent: []string{celebration, settingUp, notRunning, "is live"},
		},
		{
			// Order. Both are non-faults and they are disjoint by
			// construction — the wait sets them on different arms of the
			// same deadline — so this is unreachable today and pinned so it
			// stays a deliberate choice if that ever changes. "Still
			// arriving" is the more specific claim: something IS coming.
			name: "a model still arriving outranks no model chosen",
			summary: daemonSummary{
				accountEmail:  "someone@example.test",
				modelPending:  true,
				noModelChosen: true,
			},
			want:   settingUp,
			absent: []string{noModel, celebration},
		},
		{
			// A stated fault still outranks it, the same way it outranks
			// pending: this box must never be what swallows an engine that
			// would not stay up.
			name: "an engine that would not stay up outranks no model chosen",
			summary: daemonSummary{
				engineFailure: "ollama: process exited during startup: signal: killed",
				noModelChosen: true,
			},
			want:     notRunning,
			absent:   []string{noModel, celebration},
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

// TestBenchmarkPlanForDoesNotMeasureWhatIsNotThere pins the other half of
// #569: which hosts `waired init` measures at all.
//
// The not-ready row is the new one, and it is not a tidying-up. Before it,
// a host whose model wait had given up fell through to benchmarkWithScanner,
// whose waitForBenchmark re-ran the ENTIRE readiness wait on a fresh
// benchPollDeadline — ten more minutes on the download init had just told
// the operator it was leaving to the background, and then whatever that
// second wait concluded became the run's verdict.
//
// The benchRun row is load-bearing the other way: this must skip the four
// states it names and nothing else, or #133's model-switch offer and the
// throughput figure in the closing box quietly stop happening.
func TestBenchmarkPlanForDoesNotMeasureWhatIsNotThere(t *testing.T) {
	ready := modelWaitResult{ready: true}
	cases := []struct {
		name        string
		setupActive bool
		engineErr   error
		wait        modelWaitResult
		want        benchSkip
	}{
		{
			name: "engine up, model ready — measure it",
			wait: ready, want: benchRun,
		},
		{
			// §4.2: the benchmark prompt reads stdin and can offer to
			// switch the active model, which would race the wizard.
			name: "a browser setup is driving", setupActive: true, wait: ready,
			want: benchSkipSetupDriving,
		},
		{
			name:      "the engine could not be installed (#188)",
			engineErr: errors.New("download: 403"), want: benchSkipNoEngine,
		},
		{
			// #551: no engine either, by instruction rather than by
			// failure. Still nothing to measure.
			name:      "engine installs are turned off (#551)",
			engineErr: fmt.Errorf("%w (%s)", errEngineOptOut, "WAIRED_NO_OLLAMA"),
			want:      benchSkipNoEngine,
		},
		{
			name: "the engine installed and would not stay up (#310)",
			wait: modelWaitResult{engineFailure: "ollama: exited during startup"},
			want: benchSkipEngineDown,
		},
		{
			// #569, the arm this test was written for. pending is what the
			// closing box keys on; the skip keys on !ready, so the quieter
			// not-ready endings are covered by the row below too.
			name: "the model was still downloading when the window closed (#569)",
			wait: modelWaitResult{pending: true},
			want: benchSkipModelNotReady,
		},
		{
			// A gateway-only host: not-ready by design, pending false. It
			// skips too — there is no model to measure — and it is the
			// closing box, not this, that must keep treating it as normal.
			name: "inference is switched off on this host",
			wait: modelWaitResult{},
			want: benchSkipModelNotReady,
		},
		{
			// Order: an install that never produced an engine is the more
			// specific answer than a wait that then found none.
			name:      "no engine outranks the wait that saw none",
			engineErr: errors.New("download: 403"),
			wait:      modelWaitResult{engineFailure: "ollama: exited during startup"},
			want:      benchSkipNoEngine,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := benchmarkPlanFor(tc.setupActive, tc.engineErr, tc.wait); got != tc.want {
				t.Errorf("benchmarkPlanFor() = %v, want %v", got, tc.want)
			}
		})
	}
}
