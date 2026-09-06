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
			// PRODUCT CONTRACT (waired-agent#794): the daemon's refusal
			// has already been printed in full, and this sentinel carries
			// no message. Printing it emitted a bare "waired: " line
			// under the sentence the operator was meant to read.
			name: "the daemon refused a model switch, and said so itself",
			err:  errModelsUseRefused, want: 1, wantPrint: false,
		},
		{
			name: "the same refusal, wrapped",
			err:  fmt.Errorf("switching model: %w", errModelsUseRefused),
			want: 1, wantPrint: false,
		},
		{
			name: "any other failure",
			err:  errors.New("sign-in timed out waiting for the background service"),
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
// TestRoleGuidanceOnlyWhereItIsTrue pins waired-agent#1051: #756's
// inference-role block makes two claims about the host it prints on —
// that the role came from this hardware, and that there is an engine to
// benchmark, share and power — and it must appear only where both hold.
//
// The subject is printDaemonEnding, not roleGuidanceApplies, for the
// reason waired-agent#1027 gave when it asserted the same thing end to
// end: a table over the predicate alone would stay green with nothing
// printing it. This runs the pair in the order a person sees them.
func TestRoleGuidanceOnlyWhereItIsTrue(t *testing.T) {
	const roleBlock = "Inference role was set from this computer's hardware"
	optOut := func() error { return fmt.Errorf("%w (%s)", errEngineOptOut, "WAIRED_NO_OLLAMA") }

	for _, tc := range []struct {
		name    string
		summary daemonSummary
		want    bool
	}{
		{
			// The host #756 wrote the block for: an engine that installed,
			// stayed up and served, with a role nobody was asked about.
			name: "a serving host is told how to revisit its role",
			want: true,
		},
		{
			// waired-agent#1027. The role came from an answer, and the one
			// command of the five that applies is the closing box's own
			// remedy line.
			name:    "local inference switched off",
			summary: daemonSummary{localInferenceOff: "disabled"},
		},
		{
			name:    "the engine is parked",
			summary: daemonSummary{localInferenceOff: "stopped"},
		},
		{
			// waired-agent#1051, the filed half: #551's host. No role was
			// derived from any hardware — engine installs were turned off
			// by instruction — and three of the five commands describe an
			// engine that was never installed.
			name:    "engine installs are turned off here (#551)",
			summary: daemonSummary{engineErr: optOut()},
		},
		{
			// waired-agent#1051, the half found by checking the other
			// non-serving endings in the same pass: #188's host has no
			// engine either, and printEngineInstallFailure has just named
			// `waired init` two lines above, which this block would name a
			// third time.
			name:    "the engine could not be installed (#188)",
			summary: daemonSummary{engineErr: errors.New("download: 403")},
		},
		{
			// NOT suppressed, and this row is the pin for that: an engine
			// IS installed here, so `waired inference engine stop|start`
			// is the command that acts on the thing that went wrong.
			name:    "an engine that will not stay up keeps the block (#310)",
			summary: daemonSummary{engineFailure: "ollama: exited during startup"},
			want:    true,
		},
		{
			// Likewise: the engine ran, took the model and failed one
			// generation. `waired runtimes benchmark` is exactly the
			// command for that.
			name:    "an engine that will not answer keeps the block (#29)",
			summary: daemonSummary{benchFailed: true},
			want:    true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			printDaemonEnding(&out, tc.summary)
			if got := strings.Contains(out.String(), roleBlock); got != tc.want {
				t.Errorf("role block printed = %v, want %v:\n%s", got, tc.want, out.String())
			}
			// The box is not optional on any of these rows — a
			// suppression that swallowed the ending too would satisfy
			// every `want: false` above.
			if !strings.Contains(out.String(), "Waired is ") {
				t.Errorf("no closing box was printed:\n%s", out.String())
			}
		})
	}
}

func TestPrintDaemonSummaryBoxPicksTheOutcomeItCanDefend(t *testing.T) {
	// Substrings, not whole lines: box() pads and frames its content, and
	// emoji are dropped when the terminal cannot render them.
	const (
		celebration  = "setup is complete"
		needsInstall = "the inference engine still needs installing"
		notRunning   = "local inference isn't running"
		notAnswering = "local inference is not answering yet"
		startsOff    = "local inference starts off on this computer"
		switchedOff  = "local inference is switched off on this computer"
		installsOff  = "engine installs are turned off here"
		settingUp    = "local inference is still setting up here"
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
			// NEGATIVE CONTROL, and the reason this row is not the one
			// below. A summary with nothing stated is a host that served:
			// the wait returning not-ready is not a fault, and keying the
			// box on "the wait did not reach ready" would hand a warning to
			// operators whose machines are doing exactly what they
			// configured.
			name:    "nothing stated is a host that served",
			summary: daemonSummary{accountEmail: "someone@example.test"},
			want:    celebration,
			absent:  []string{notRunning, switchedOff},
		},
		{
			// waired-agent#1027. INVERTS what this row pinned before: it
			// asserted the celebration for "inference is simply switched
			// off" while passing a summary that stated no such thing, so
			// what it actually pinned was the row above. The box really did
			// tell a computer with local inference disabled that "Local
			// inference is live via the waired-agent daemon", three lines
			// under "everything completed successfully", while `waired
			// status` on the same host answered `state: disabled` — the
			// shape docs/decisions/20260821/1420-setup-report-says-what-happened.md
			// rules out.
			//
			// Still not a fault and still exit 0: the operator, or the
			// step-4 decline they gave, is the author of this state.
			name: "local inference switched off is not 'everything completed'",
			summary: daemonSummary{
				accountEmail:      "someone@example.test",
				localInferenceOff: "disabled",
			},
			want:   switchedOff,
			absent: []string{celebration, notRunning, settingUp, noModel, startsOff},
		},
		{
			// The other state that reaches the box, and the reason the
			// remedy is chosen rather than fixed: `waired inference on`
			// does nothing for an engine that was parked (waired-agent#881).
			name: "a parked engine gets the power-switch remedy",
			summary: daemonSummary{
				accountEmail:      "someone@example.test",
				localInferenceOff: "stopped",
			},
			want:   switchedOff,
			absent: []string{celebration},
		},
		{
			// The two "off and nothing failed" endings overlap by
			// construction — a host the measurement switched off also
			// answers `disabled` — so the measurement's box has to win. It
			// is the one that knows WHY, and its remedy goes ahead ANYWAY
			// rather than ANYTIME (waired#1099).
			name: "the measurement's verdict outranks the bare toggle",
			summary: daemonSummary{
				accountEmail:      "someone@example.test",
				localInferenceOff: "disabled",
				hostSpeed: &management.HostSpeedStatus{
					TurnSeconds: 66.9, BudgetSeconds: 45, TurnedInferenceOff: true,
				},
			},
			want:   startsOff,
			absent: []string{switchedOff, celebration},
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
			// NEGATIVE CONTROL for #569, and the row that stops that fix
			// from being written as plain !ready: a host that is not-ready
			// by design leaves pending false and must not be told anything
			// is setting up.
			//
			// waired-agent#1027 INVERTS the box this row expects, and
			// corrects its name. It was called "a host with inference
			// switched off", but the summary states no such thing, so what
			// it pinned was a host that served — which is the row at the
			// top of this table. The switched-off host has its own rows
			// there now, and it must still not read as "still setting up":
			// nothing is on its way here either.
			name:    "not-ready by design is not 'still setting up'",
			summary: daemonSummary{accountEmail: "someone@example.test", modelPending: false},
			want:    celebration,
			absent:  []string{settingUp, notRunning, switchedOff},
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
// waired-agent#1027: the `Model` row is labelled for a model, so its
// value has to name one. Before this it was a bare rate, which read as a
// second speed measurement beside `Speed`.
func TestBenchmarkRowValue(t *testing.T) {
	tests := []struct {
		name  string
		bench benchmarkOutcome
		want  string
	}{
		{
			name:  "the measured model is named",
			bench: benchmarkOutcome{Measured: true, Tokps: 13.4, ModelID: "qwen3.5-9b"},
			want:  "qwen3.5-9b — 13 tok/s",
		},
		{
			// A daemon older than the field sends no name, and the row is
			// then byte-identical to the one it printed before.
			name:  "a daemon that sends no name keeps the old row",
			bench: benchmarkOutcome{Measured: true, Tokps: 58},
			want:  "58 tok/s",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := benchmarkRowValue(tc.bench); got != tc.want {
				t.Errorf("benchmarkRowValue = %q, want %q", got, tc.want)
			}
		})
	}
}

// The toggle and the engine's power switch are separate controls
// (waired-agent#881), so the box that covers both has to name the command
// that actually undoes THIS host's off.
func TestInferenceOffRemedy(t *testing.T) {
	if got := inferenceOffRemedy("disabled"); !strings.Contains(got, "waired inference on") {
		t.Errorf("a disabled toggle was not offered `waired inference on`: %q", got)
	}
	if got := inferenceOffRemedy("stopped"); !strings.Contains(got, "waired inference engine start") {
		t.Errorf("a parked engine was not offered `waired inference engine start`: %q", got)
	}
	// `waired inference on` does nothing for an engine that was parked,
	// and offering it there is the failure this function exists to stop.
	if got := inferenceOffRemedy("stopped"); strings.Contains(got, "waired inference on") {
		t.Errorf("a parked engine was offered the toggle: %q", got)
	}
}

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
