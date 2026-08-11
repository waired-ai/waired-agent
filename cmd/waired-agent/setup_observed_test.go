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
// has been chosen and downloaded.
func observedHost() *fakeSetupProvider {
	return &fakeSetupProvider{
		engineInstalled: true,
		engineReady:     true,
		preferred:       "qwen3.5-4b",
		modelState:      catalog.ModelStateReady,
	}
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
	for _, s := range p.Steps {
		if s.Status != signer.SetupStatusDone && s.Status != signer.SetupStatusSkipped {
			t.Errorf("step %q = %q; the completion rule reads every row", s.ID, s.Status)
		}
	}
	// Nobody claimed the lease, so there is no driver to report. It must
	// NOT read as the browser: that derivation exists because a desired
	// state is a browser's implicit claim, and there is no desired state
	// here (waired-agent#645).
	if p.Driver != "" {
		t.Errorf("driver = %q with nothing driving, want none", p.Driver)
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

	t.Run("engine but nothing chosen to serve", func(t *testing.T) {
		f := observedHost()
		f.preferred = ""
		if p := newObservedReconciler(t, f).snapshot(context.Background()); p != nil {
			t.Fatalf("snapshot = %+v, want nil — no model has been chosen", p)
		}
	})
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
