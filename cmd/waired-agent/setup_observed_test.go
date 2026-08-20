package main

import (
	"context"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// The report a host makes about itself when the control plane never sent
// it an instruction (waired-agent#646).
//
// `waired init` run from a terminal writes its answers to this daemon and
// nowhere else — the desired columns belong to the management API — so
// before this the device published nothing at all, the completion rule saw
// no steps, and NAVI's model card stayed shut on every CLI-installed node.

// observedHost is a provider scripted as a machine somebody set up from a
// terminal: the engine is on disk, it is the one being served, and a model
// has been chosen, downloaded and activated.
func observedHost() *fakeSetupProvider {
	return &fakeSetupProvider{
		engineInstalled: true,
		engineReady:     true,
		preferred:       "qwen3.5-4b",
		activeModel:     "qwen3.5-4b",
		modelState:      catalog.ModelStateReady,
	}
}

// autoSelectedHost is the machine waired-agent#753 and #756 describe: the
// same finished host, except that NOBODY WAS EVER ASKED which model to
// run. The engine is installed and serving, the bundled model was pulled
// by the daemon's own auto-selection and is answering requests — and
// preferred-model.json does not exist, because it records a choice and no
// choice was made.
//
// Two journeys arrive here. `waired init --non-interactive` skips the
// model picker outright (#753, all three platforms), and on macOS the
// installer registers the LaunchDaemon before running init, so the auto-
// pull has already started by the time the interactive picker looks and
// the picker steps aside for a host that already has model history (#756).
func autoSelectedHost() *fakeSetupProvider {
	f := observedHost()
	f.preferred = ""
	return f
}

// newObservedReconciler is a reconciler that has folded a frame carrying NO
// instruction — the frame every terminal-installed device gets, forever.
func newObservedReconciler(t *testing.T, f *fakeSetupProvider) *setupReconciler {
	t.Helper()
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	r.now = newFakeClock().now
	r.Apply(context.Background(), desiredFrame("", "", 0))
	return r
}

// The headline: a machine that was set up from a terminal says so, and
// every row it reports is finished — which is what the control plane's
// completion rule needs to see (§7: at least one step, and all of them
// done or skipped).
func TestObservedSetupReportsTheFinishedRows(t *testing.T) {
	r := newObservedReconciler(t, observedHost())

	p := r.snapshot(context.Background())
	if p == nil {
		t.Fatal("snapshot = nil on a host with an engine and a chosen model — the control plane learns nothing")
	}
	if got := stepByID(t, p, setupStepEngineInstall).Status; got != signer.SetupStatusDone {
		t.Errorf("engine_install = %q, want done", got)
	}
	if got := stepByID(t, p, setupStepModelPull).Status; got != signer.SetupStatusDone {
		t.Errorf("model_pull = %q, want done", got)
	}
	assertCompletableDocument(t, p)
	// INVERTED by waired-agent#790: this used to assert no driver at all.
	// Nobody holds the lease, but a host that can describe itself with no
	// instruction is one `waired init` set up — the terminal writes its
	// answers to this daemon and nowhere else. It must NOT read as the
	// browser: that derivation exists because a desired state is the
	// browser's implicit claim, and there is none here (waired-agent#645).
	if p.Driver != signer.SetupDriverTerminal {
		t.Errorf("driver = %q on a host that set itself up from a terminal, want terminal", p.Driver)
	}
	// The measurement is the terminal's own and has no generation from the
	// control plane, so it gets no row — an unfinished one would hold the
	// whole report open.
	if hasStepID(p, setupStepBenchmark) {
		t.Error("a benchmark row appeared with no generation asked for")
	}
}

// The two populations that must stay silent. Both would otherwise open an
// engine row for an install nobody asked for, and the second one is every
// device in a fleet that has only ever enrolled.
func TestObservedSetupStaysSilentWithNothingToReport(t *testing.T) {
	t.Run("no engine on this host", func(t *testing.T) {
		f := observedHost()
		f.engineInstalled, f.engineReady = false, false
		if p := newObservedReconciler(t, f).snapshot(context.Background()); p != nil {
			t.Fatalf("snapshot = %+v, want nil — nothing is installed here", p)
		}
	})

	t.Run("engine but nothing chosen and nothing serving", func(t *testing.T) {
		// Both model signals absent, stated explicitly: since #753 the
		// report falls back from the chosen model to the served one, so
		// clearing only the preference no longer describes this
		// population and an implicit zero value would stop testing it.
		f := observedHost()
		f.preferred, f.activeModel = "", ""
		if p := newObservedReconciler(t, f).snapshot(context.Background()); p != nil {
			t.Fatalf("snapshot = %+v, want nil — no model has been chosen and none is being served", p)
		}
	})
}

// waired-agent#753 / #756: a host nobody asked is still a host that is
// serving something, and serving is the observation this report is for.
//
// Both issues land here. The device works — engine installed, model
// resident, requests answered — and before this it published a document
// with zero steps, which the completion rule can never accept. NAVI then
// showed it as a computer that never finished setting up, and because the
// model card is gated on that rule, the model could never be changed from
// the console on exactly the hosts that never open a browser.
func TestObservedSetupFallsBackToTheServingModel(t *testing.T) {
	r := newObservedReconciler(t, autoSelectedHost())

	p := r.snapshot(context.Background())
	if p == nil {
		t.Fatal("snapshot = nil on a host serving a model nobody chose — the control plane learns nothing (#753)")
	}
	if got := stepByID(t, p, setupStepEngineInstall).Status; got != signer.SetupStatusDone {
		t.Errorf("engine_install = %q, want done", got)
	}
	if got := stepByID(t, p, setupStepModelPull).Status; got != signer.SetupStatusDone {
		t.Errorf("model_pull = %q, want done", got)
	}
	assertCompletableDocument(t, p)
}

// The order of the two model signals, pinned. A choice that has not
// converged yet must still name the target, or the row reports the
// OUTGOING model as done and the wizard's progress bar tracks a download
// that already finished. Without this test an implementation that reads
// the served model first passes every other case in this file.
func TestObservedSetupPrefersTheChosenModelOverTheServingOne(t *testing.T) {
	f := autoSelectedHost()
	f.preferred = "qwen3.5-4b"   // chosen, still downloading
	f.activeModel = "qwen3.5-2b" // still answering with the old one
	f.modelState = catalog.ModelStateDownloading
	f.modelCompleted, f.modelTotal = 512, 4096

	step := stepByID(t, newObservedReconciler(t, f).snapshot(context.Background()), setupStepModelPull)
	if step.Status != signer.SetupStatusRunning {
		t.Fatalf("model_pull = %+v, want running — the chosen model is still downloading", step)
	}
	// The id the row was built from, which is the whole point: asking about
	// the served model would report the finished download of the model the
	// operator is switching AWAY from.
	f.mu.Lock()
	asked := append([]string(nil), f.modelStateAsked...)
	f.mu.Unlock()
	for _, id := range asked {
		if id == "qwen3.5-2b" {
			t.Fatalf("the row was built from the served model; asked = %v, want the chosen one", asked)
		}
	}
	if len(asked) == 0 || asked[0] != "qwen3.5-4b" {
		t.Errorf("asked = %v, want the chosen model", asked)
	}
}

// The #586 answers are facts about the QUESTION, not about the machine.
// A host that answered "no model" and is nonetheless serving one is
// serving it, and reporting silence about a computer that is answering
// requests is the defect this fallback exists to remove.
//
// Recorded as today's behaviour, not as a ratified rule: no owner ruling
// covers the combination, and it is only reachable at all because
// handleNoModelSelected persists the answer without clearing state.Active.
func TestObservedSetupReportsTheServingModelAfterTheNoneAnswer(t *testing.T) {
	// A "none" record names no model, so it reaches the reconciler as an
	// empty preference — exactly like the never-asked host above.
	r := newObservedReconciler(t, autoSelectedHost())
	if p := r.snapshot(context.Background()); p == nil {
		t.Fatal("snapshot = nil on a host that is answering requests")
	}
}

// The invariant the whole design rests on: an observation is a report,
// never an instruction. If it leaked into r.desired the reconciler would
// start converging a host onto a model nobody asked for, and SetupState
// would serve the executor a desired value the control plane never sent.
func TestObservedSetupDoesNotBecomeDesiredState(t *testing.T) {
	r := newObservedReconciler(t, autoSelectedHost())
	if p := r.snapshot(context.Background()); p == nil {
		t.Fatal("snapshot = nil; the rest of this test would be vacuous")
	}
	if r.desired != (setupDesired{}) {
		t.Errorf("r.desired = %+v after an observed snapshot, want empty", r.desired)
	}
	if got := r.SetupState(context.Background()).DesiredModelID; got != "" {
		t.Errorf("SetupState desired model = %q, want none — the control plane sent no instruction", got)
	}
}

// The acted-on "don't run local AI here" answer owns the whole report
// (#597): that host reports the inference_off row and nothing else, and
// engine and model rows synthesised beside it would contradict the row the
// completion rule reads.
//
// Tested at the function rather than through a snapshot because the control
// plane never clears a desired value — so an acted-off record always
// arrives with the instruction that produced it still in force, and the
// reconciler takes the instruction path. This pins the guard for the day
// that stops being true.
func TestObservedSetupYieldsToTheLocalAIOffAnswer(t *testing.T) {
	r := newSetupReconciler(observedHost(), nil, "dev-1", nil, quietLogger())
	if _, ok := r.observedSetup(context.Background(), signer.DesiredInferenceOff, state.SetupIntegrations{}); ok {
		t.Fatal("observedSetup described an engine and a model on a host told not to run local AI")
	}
	if _, ok := r.observedSetup(context.Background(), "", state.SetupIntegrations{}); !ok {
		t.Fatal("observedSetup reported nothing for a set-up host with no local-AI answer on record")
	}
}

// An authored instruction is the operator's and stays the authority. The
// observed values are deliberately not a second opinion: with a desired
// state in force, the rows describe THAT, even when this host is serving
// something else.
func TestObservedSetupNeverOverridesAnInstruction(t *testing.T) {
	f := observedHost()
	f.preferred = "qwen3.5-2b" // demoted locally, as in waired-agent#647
	r, _ := leasedReconciler(t, f, "ollama", "qwen3.5-4b")

	p := r.snapshot(context.Background())
	if p == nil {
		t.Fatal("snapshot = nil with a desired state in force")
	}
	// The browser's implicit claim, unchanged: a desired state exists and
	// nobody took the lease.
	if p.Driver != signer.SetupDriverBrowser {
		t.Errorf("driver = %q, want browser — the instruction is the claim", p.Driver)
	}
	if r.desired.modelID != "qwen3.5-4b" {
		t.Errorf("desired model = %q, want the control plane's instruction", r.desired.modelID)
	}
}

// --- the coding-tools row on a host with no instruction ---

// No record, no row. The engine and model rows are re-derived from the disk
// on every snapshot; the coding tools live in a user's home and in
// root-owned managed settings, which the daemon deliberately never reads,
// so the executor's record is the only evidence there is (waired-agent#312).
// waired-agent#791: the row a FAILED terminal apply reports.
//
// The reporting hole had two halves. The CLI dropped the failure
// (reportTerminalIntegrations), and even had it not, this projection only
// opened the row when there was an instruction or a persisted success —
// and a failure is neither. So the step existed in no state at all, and
// the control plane's completion rule, which reads only the steps that
// were reported, was reached over a step the operator had just watched
// fail.
func TestObservedSetupReportsAFailedCodingToolsRow(t *testing.T) {
	dir := t.TempDir()
	f := observedHost()
	f.stateDir = dir
	r := newObservedReconciler(t, f)

	r.NoteExecutor(context.Background(), management.SetupExecutorRequest{
		Attached: true, Elevated: true,
		Step:  management.SetupStepIntegration,
		Phase: management.SetupExecutorPhaseFailed,
		Error: "open /home/u/.claude: permission denied",
	})

	step := stepByID(t, r.snapshot(context.Background()), setupStepIntegration)
	if step.Status != signer.SetupStatusFailed {
		t.Fatalf("step = %+v, want failed", step)
	}
	if step.ErrorCode != signer.SetupErrorPermissionDenied {
		t.Errorf("error_code = %q, want permission_denied", step.ErrorCode)
	}

	// The other half of the ruling: report it, do not record it. A
	// persisted failure would outlive the `waired link --force all` that
	// repairs it, which is what
	// docs/decisions/20260802/1757-setup-integration-persisted-front-loaded.md
	// refuses. Product contract, and that decision is its source.
	rec, err := state.ReadSetupIntegrations(dir)
	if err != nil {
		t.Fatalf("reading the record: %v", err)
	}
	if len(rec.Targets) != 0 {
		t.Fatalf("record = %+v after a failure, want nothing written", rec)
	}
}

// The failure has to survive `waired init` exiting, or the row is red for
// the few seconds between the report and the process ending and green
// again afterwards. Release clears the lease, the install claim and the
// driver claim; it does not clear the step reports.
func TestObservedSetupFailedCodingToolsRowSurvivesTheRelease(t *testing.T) {
	f := observedHost()
	f.stateDir = t.TempDir()
	r := newObservedReconciler(t, f)
	r.NoteExecutor(context.Background(), management.SetupExecutorRequest{
		Attached: true, Elevated: true,
		Step:  management.SetupStepIntegration,
		Phase: management.SetupExecutorPhaseFailed,
		Error: "claude-code: permission denied",
	})
	r.NoteExecutor(context.Background(), management.SetupExecutorRequest{Attached: false})

	step := stepByID(t, r.snapshot(context.Background()), setupStepIntegration)
	if step.Status != signer.SetupStatusFailed {
		t.Fatalf("step = %+v after the executor released, want failed", step)
	}
}

// A record of today's behaviour, NOT a product contract: the failure lives
// in this process's memory only, so a service restart takes it with it and
// the row goes back to absent. That is the direct consequence of not
// persisting failures, and it is bounded by the repair path — a later
// `waired init` or `waired link --force all` reports the success that
// replaces it.
func TestObservedSetupFailedCodingToolsRowIsGoneAfterARestart(t *testing.T) {
	dir := t.TempDir()
	f := observedHost()
	f.stateDir = dir
	r := newObservedReconciler(t, f)
	r.NoteExecutor(context.Background(), management.SetupExecutorRequest{
		Attached: true, Elevated: true,
		Step:  management.SetupStepIntegration,
		Phase: management.SetupExecutorPhaseFailed,
		Error: "claude-code: permission denied",
	})

	restarted := observedHost()
	restarted.stateDir = dir
	if hasStepID(newObservedReconciler(t, restarted).snapshot(context.Background()), setupStepIntegration) {
		t.Fatal("the failed row came back on a fresh daemon; failures are not persisted")
	}
}

func TestObservedSetupOmitsTheCodingToolsRowWithoutARecord(t *testing.T) {
	if hasStepID(newObservedReconciler(t, observedHost()).snapshot(context.Background()), setupStepIntegration) {
		t.Fatal("a coding-tools row appeared with nothing recorded")
	}
}

// The terminal's own apply reports the row, and the daemon has no
// instruction to read the target names from — so it takes them from the
// report. This is the pair that closes the loop for a CLI-installed node.
func TestObservedSetupTakesTheCodingToolsFromTheExecutorReport(t *testing.T) {
	f := observedHost()
	f.stateDir = t.TempDir()
	r := newObservedReconciler(t, f)

	r.NoteExecutor(context.Background(), management.SetupExecutorRequest{
		Attached: true, Elevated: true,
		Step:               management.SetupStepIntegration,
		Phase:              management.SetupExecutorPhaseDone,
		IntegrationTargets: []string{signer.IntegrationOpenClaw, signer.IntegrationClaudeCode},
	})

	step := stepByID(t, r.snapshot(context.Background()), setupStepIntegration)
	if step.Status != signer.SetupStatusDone {
		t.Fatalf("integration = %+v, want done", step)
	}
	// Persisted, so the row survives the service restart that used to walk
	// a finished device back to "nobody has run the setup command here".
	rec, err := state.ReadSetupIntegrations(f.stateDir)
	if err != nil {
		t.Fatalf("reading the record: %v", err)
	}
	if !rec.Covers([]string{signer.IntegrationClaudeCode, signer.IntegrationOpenClaw}) {
		t.Fatalf("record = %+v, want both targets", rec)
	}
}

// The record has to survive this process, or the row is only right until
// the next service restart — the waired-agent#312 failure, one surface over.
func TestObservedSetupCodingToolsRowSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	f := observedHost()
	f.stateDir = dir
	r := newObservedReconciler(t, f)
	r.NoteExecutor(context.Background(), management.SetupExecutorRequest{
		Attached: true, Elevated: true,
		Step:               management.SetupStepIntegration,
		Phase:              management.SetupExecutorPhaseDone,
		IntegrationTargets: []string{signer.IntegrationClaudeCode},
	})

	// A fresh reconciler over the same state dir is what a daemon restart
	// looks like: no lease, no memory of one, only the record on disk.
	fresh := observedHost()
	fresh.stateDir = dir
	step := stepByID(t, newObservedReconciler(t, fresh).snapshot(context.Background()), setupStepIntegration)
	if step.Status != signer.SetupStatusDone {
		t.Fatalf("integration = %+v after a restart, want done", step)
	}
}

// An instruction still wins. The executor applies what SetupState served
// it, so that value is the authority whenever there is one — the reported
// list exists for the case where there is none, and must not become a
// second, disagreeing source for the case where there is.
func TestExecutorReportedTargetsYieldToTheInstruction(t *testing.T) {
	dir := t.TempDir()
	f := observedHost()
	f.stateDir = dir
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	r.now = newFakeClock().now
	r.Apply(context.Background(), integrationFrame(&[]string{signer.IntegrationClaudeCode}))

	r.NoteExecutor(context.Background(), management.SetupExecutorRequest{
		Attached: true, Elevated: true,
		Step:               management.SetupStepIntegration,
		Phase:              management.SetupExecutorPhaseDone,
		IntegrationTargets: []string{signer.IntegrationOpenClaw},
	})

	rec, err := state.ReadSetupIntegrations(dir)
	if err != nil {
		t.Fatalf("reading the record: %v", err)
	}
	if len(rec.Targets) != 1 || rec.Targets[0] != signer.IntegrationClaudeCode {
		t.Fatalf("record = %+v, want the instruction's target alone", rec)
	}
}

func TestValidIntegrationTargets(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"nothing reported", nil, nil},
		{
			// Sorted and de-duplicated, so the record on disk is stable
			// across writes whatever order the executor sent.
			name: "sorted and deduplicated",
			in:   []string{signer.IntegrationOpenClaw, signer.IntegrationClaudeCode, signer.IntegrationOpenClaw},
			want: []string{signer.IntegrationClaudeCode, signer.IntegrationOpenClaw},
		},
		{
			// Same tolerance the control plane's instruction gets: a CLI
			// newer or older than the daemon it is driving is the ordinary
			// state for the seconds around an upgrade.
			name: "unknown ids are dropped",
			in:   []string{"cursor", signer.IntegrationOpenClaw},
			want: []string{signer.IntegrationOpenClaw},
		},
		{
			// A retired id takes the same road, which is the whole migration
			// plan for waired-agent#333.
			name: "retired ids are dropped",
			in:   []string{signer.IntegrationOpenCode},
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := validIntegrationTargets(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("validIntegrationTargets = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("validIntegrationTargets = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// The regression bar for the `waired link` repair report
// (waired-agent#791). It rides the executor route because that is where
// the step record lives, and it must be inert for everything else on it.
//
// Each field below is one way the ordinary lease post would have gone
// wrong for an unprivileged repair running beside a live `waired init`:
// executorElevated outlives the lease so engine_install can still report
// permission_denied, installClaimed is what stops a second elevated engine
// install, and the driver claim is the terminal's.
func TestStepOnlyReportLeavesTheLeaseAlone(t *testing.T) {
	two := []string{signer.IntegrationOpenClaw, signer.IntegrationClaudeCode}
	f := &fakeSetupProvider{stateDir: t.TempDir()}
	r, _ := leasedReconciler(t, f, "ollama", "")
	r.Apply(context.Background(), integrationFrame(&two))

	// An elevated `waired init` is mid-install and owns the terminal.
	r.NoteExecutor(context.Background(), management.SetupExecutorRequest{
		Attached: true, Elevated: true, Driver: signer.SetupDriverTerminal,
		Phase: management.SetupExecutorPhaseInstalling, Engine: "ollama",
	})
	before := r.SetupState(context.Background())
	if before.InstallClaimed != "ollama" || !before.ExecutorElevated {
		t.Fatalf("fixture did not take: %+v", before)
	}

	// An ordinary non-root `waired link --force all` finishes elsewhere on
	// the machine and says so.
	r.NoteExecutor(context.Background(), management.SetupExecutorRequest{
		StepOnly: true,
		Step:     management.SetupStepIntegration,
		Phase:    management.SetupExecutorPhaseDone,
		IntegrationTargets: []string{
			signer.IntegrationClaudeCode, signer.IntegrationOpenClaw,
		},
	})

	after := r.SetupState(context.Background())
	if after.InstallClaimed != "ollama" {
		t.Errorf("InstallClaimed = %q, want ollama — a repair must not release the engine claim", after.InstallClaimed)
	}
	if !after.ExecutorElevated {
		t.Error("ExecutorElevated went false; engine_install would now report permission_denied")
	}
	if got := r.snapshot(context.Background()).Driver; got != signer.SetupDriverTerminal {
		t.Errorf("driver = %q, want terminal — the repair claimed a setup it is not driving", got)
	}
	if got := stepByID(t, r.snapshot(context.Background()), setupStepEngineInstall).Status; got != signer.SetupStatusRunning {
		t.Errorf("engine_install = %q, want running — the install report was overwritten", got)
	}

	// ...and the row it IS about did move.
	if got := stepByID(t, r.snapshot(context.Background()), setupStepIntegration).Status; got != signer.SetupStatusDone {
		t.Fatalf("integration = %q, want done", got)
	}
}

// A repair on a host with no instruction of its own — the terminal-driven
// case — writes the record, which is what makes the row survive a restart.
func TestStepOnlyReportPersistsTheRepair(t *testing.T) {
	dir := t.TempDir()
	f := observedHost()
	f.stateDir = dir
	r := newObservedReconciler(t, f)
	r.NoteExecutor(context.Background(), management.SetupExecutorRequest{
		Attached: true, Elevated: true,
		Step:  management.SetupStepIntegration,
		Phase: management.SetupExecutorPhaseFailed,
		Error: "claude-code: permission denied",
	})
	if got := stepByID(t, r.snapshot(context.Background()), setupStepIntegration).Status; got != signer.SetupStatusFailed {
		t.Fatalf("integration = %q before the repair, want failed", got)
	}

	r.NoteExecutor(context.Background(), management.SetupExecutorRequest{
		StepOnly: true,
		Step:     management.SetupStepIntegration,
		Phase:    management.SetupExecutorPhaseDone,
		IntegrationTargets: []string{
			signer.IntegrationClaudeCode, signer.IntegrationOpenClaw,
		},
	})
	if got := stepByID(t, r.snapshot(context.Background()), setupStepIntegration).Status; got != signer.SetupStatusDone {
		t.Fatalf("integration = %q after the repair, want done", got)
	}

	rec, err := state.ReadSetupIntegrations(dir)
	if err != nil {
		t.Fatalf("reading the record: %v", err)
	}
	if len(rec.Targets) != 2 {
		t.Fatalf("record = %+v, want both adapters", rec)
	}

	// The repair is what a restart reads back, so the row stays green.
	restarted := observedHost()
	restarted.stateDir = dir
	fresh := newObservedReconciler(t, restarted)
	if got := stepByID(t, fresh.snapshot(context.Background()), setupStepIntegration).Status; got != signer.SetupStatusDone {
		t.Fatalf("integration = %q on a fresh daemon, want done", got)
	}
}

// The release path is unchanged: only a step-only report is exempt from
// it, and a plain detached post still ends the lease.
func TestDetachedReportStillReleasesTheLease(t *testing.T) {
	f := &fakeSetupProvider{}
	r, _ := leasedReconciler(t, f, "ollama", "")
	r.NoteExecutor(context.Background(), management.SetupExecutorRequest{
		Attached: true, Elevated: true, Driver: signer.SetupDriverTerminal,
		Phase: management.SetupExecutorPhaseInstalling, Engine: "ollama",
	})
	r.NoteExecutor(context.Background(), management.SetupExecutorRequest{Attached: false})

	if got := r.SetupState(context.Background()).InstallClaimed; got != "" {
		t.Errorf("InstallClaimed = %q after a release, want none", got)
	}
	if got := r.snapshot(context.Background()).Driver; got != signer.SetupDriverBrowser {
		t.Errorf("driver = %q after a release, want the browser derivation back", got)
	}
}
