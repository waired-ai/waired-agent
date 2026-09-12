package main

import (
	"context"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// THE waired-agent#1301 REGRESSION BAR. PRODUCT CONTRACT (owner, rc6
// review): onboarding is not complete until the benchmarks that follow
// the model download have run.
//
// The control plane derives setup_complete from the rows, so a row that
// is `running` while the measurement runs IS the wait. Measured on the
// reference host: four minutes of saturated GPU after every surface said
// the computer was set up.
func TestPrefillMeasurementSteps_RowPerStage(t *testing.T) {
	cases := []struct {
		name   string
		pr     prefillSetupProgress
		want   string // "" = no row at all
		detail string
	}{
		{
			// Completion-safe direction: a host with no committed model
			// has nothing owed, and an absent row denies nothing.
			name: "no model committed emits no row",
			pr:   prefillSetupProgress{Stage: prefillSetupStageNone},
			want: "",
		},
		{
			name: "owed or under way holds setup open",
			pr:   prefillSetupProgress{Stage: prefillSetupStageMeasuring},
			want: signer.SetupStatusRunning,
		},
		{
			name: "measured",
			pr:   prefillSetupProgress{Stage: prefillSetupStageMeasured},
			want: signer.SetupStatusDone,
		},
		{
			// A host that cannot be measured still serves, and the
			// control plane tolerates a failed benchmark row.
			name:   "failed carries the reason and does not block",
			pr:     prefillSetupProgress{Stage: prefillSetupStageFailed, Detail: "engine prefilled 900 tokens for a 4096-token rung"},
			want:   signer.SetupStatusFailed,
			detail: "engine prefilled 900 tokens for a 4096-token rung",
		},
		{
			// skipped, not failed: nothing about this host failed — the
			// engine was busy every time the measurement came for it.
			name: "budget exhausted is terminal and blameless",
			pr:   prefillSetupProgress{Stage: prefillSetupStageGaveUp},
			want: signer.SetupStatusSkipped,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := prefillMeasurementSteps(tc.pr)
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("want no row, got %+v", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("want exactly one row, got %+v", got)
			}
			if got[0].ID != setupStepPrefillMeasurement {
				t.Errorf("row id = %q, want %q", got[0].ID, setupStepPrefillMeasurement)
			}
			if got[0].Status != tc.want {
				t.Errorf("status = %q, want %q", got[0].Status, tc.want)
			}
			if got[0].ErrorDetail != tc.detail {
				t.Errorf("detail = %q, want %q", got[0].ErrorDetail, tc.detail)
			}
		})
	}
}

// setupPrefillProgress is derived from the measurement's own state rather
// than tracked beside it, so these are the cases where that derivation
// has to be right.
func TestSetupPrefillProgress_DerivesTheStage(t *testing.T) {
	newProvider := func(t *testing.T, variant string) *agentInferenceProvider {
		t.Helper()
		p := &agentInferenceProvider{store: catalog.NewStore(t.TempDir() + "/state.json")}
		if variant != "" {
			if err := p.store.Update(func(s *catalog.State) {
				s.Active = &catalog.ActiveSelection{
					Runtime: catalog.RuntimeOllama, ModelID: "m", VariantID: variant,
				}
			}); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
		return p
	}

	t.Run("no committed model says nothing", func(t *testing.T) {
		p := newProvider(t, "")
		if got := p.setupPrefillProgress().Stage; got != prefillSetupStageNone {
			t.Errorf("stage = %v, want none", got)
		}
	})

	t.Run("owed before the gate is even armed", func(t *testing.T) {
		// The window the whole row exists for: an engine is a moment
		// behind its daemon, so nothing has armed the gate yet and the
		// measurement has not started — and setup must not complete here.
		p := newProvider(t, "q4")
		if got := p.setupPrefillProgress().Stage; got != prefillSetupStageMeasuring {
			t.Errorf("stage = %v, want measuring", got)
		}
	})

	t.Run("a recorded figure for this variant is done", func(t *testing.T) {
		p := newProvider(t, "q4")
		p.SetLastPrefill(PrefillMeasurement{VariantID: "q4", Rungs: []PrefillRung{{Depth: 4096, Tokps: 800}}})
		if got := p.setupPrefillProgress().Stage; got != prefillSetupStageMeasured {
			t.Errorf("stage = %v, want measured", got)
		}
	})

	t.Run("a figure for a different variant is not this one", func(t *testing.T) {
		// A model switch makes the old figure a number for a model this
		// host no longer runs, and the new one is owed.
		p := newProvider(t, "q4")
		p.SetLastPrefill(PrefillMeasurement{VariantID: "q2", Rungs: []PrefillRung{{Depth: 4096, Tokps: 800}}})
		if got := p.setupPrefillProgress().Stage; got != prefillSetupStageMeasuring {
			t.Errorf("stage = %v, want measuring", got)
		}
	})

	t.Run("a recorded failure is reported with its reason", func(t *testing.T) {
		p := newProvider(t, "q4")
		p.SetLastPrefill(PrefillMeasurement{VariantID: "q4", Failed: true, Err: "engine did not answer"})
		pr := p.setupPrefillProgress()
		if pr.Stage != prefillSetupStageFailed || pr.Detail != "engine did not answer" {
			t.Errorf("progress = %+v, want failed with the reason", pr)
		}
	})

	t.Run("the budget makes it terminal", func(t *testing.T) {
		// Without this a host whose engine is always busy would be told
		// it is unfinished for the life of the daemon: two paths give
		// the measurement back without recording anything.
		p := newProvider(t, "q4")
		p.speedMeasureArmedAt.Store(time.Now().Add(-prefillSetupBudget - time.Minute).UnixNano())
		if got := p.setupPrefillProgress().Stage; got != prefillSetupStageGaveUp {
			t.Errorf("stage = %v, want gave_up", got)
		}
	})

	t.Run("inside the budget it is still waiting", func(t *testing.T) {
		p := newProvider(t, "q4")
		p.speedMeasureArmedAt.Store(time.Now().Add(-time.Minute).UnixNano())
		if got := p.setupPrefillProgress().Stage; got != prefillSetupStageMeasuring {
			t.Errorf("stage = %v, want measuring", got)
		}
	})
}

// The gate's stamp is what bounds the row above. Cleared on the way out,
// so a second measurement for a switched model starts its own clock
// rather than inheriting a stamp from the first.
func TestSpeedMeasurementArmedAt_FirstArmingWinsAndIsCleared(t *testing.T) {
	p := &agentInferenceProvider{}
	p.beginSpeedMeasurement()
	first := p.speedMeasureArmedAt.Load()
	if first == 0 {
		t.Fatal("arming did not stamp")
	}
	// Re-arming happens every round on a measurement that keeps yielding;
	// the budget is "how long this host has been trying".
	p.beginSpeedMeasurement()
	if got := p.speedMeasureArmedAt.Load(); got != first {
		t.Errorf("stamp moved on re-arm: %d -> %d", first, got)
	}
	p.endSpeedMeasurement()
	if got := p.speedMeasureArmedAt.Load(); got != 0 {
		t.Errorf("stamp = %d after the gate cleared, want 0", got)
	}
}

// The wiring, not the projection. Both tests above pass with the row
// never appended to the snapshot at all — measured by reverting the
// append and counting: zero tests bit. This is the one that does.
//
// PRODUCT CONTRACT (owner, rc6 review; waired-agent#1301): the row is
// LAST, because it is last in time, and the control plane derives
// setup_complete from the rows — so its presence at `running` is what
// makes onboarding wait.
func TestSetupSnapshot_PrefillRowIsLastAndHoldsSetupOpen(t *testing.T) {
	ctx := context.Background()
	f := &fakeSetupProvider{
		engineInstalled: true,
		engineReady:     true,
		prefillProgress: prefillSetupProgress{Stage: prefillSetupStageMeasuring},
	}
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	r.Apply(ctx, desiredFrame("ollama", "", 0))

	snap := r.snapshot(ctx)
	got := stepByID(t, snap, setupStepPrefillMeasurement)
	if got.Status != signer.SetupStatusRunning {
		t.Fatalf("prefill row = %+v, want running", got)
	}
	if last := snap.Steps[len(snap.Steps)-1]; last.ID != setupStepPrefillMeasurement {
		t.Errorf("last step = %q, want %q — it is last in time, and the wire order is NAVI's render order",
			last.ID, setupStepPrefillMeasurement)
	}
}

// And the absent case through the same path: a host with nothing to say
// about the measurement emits no row, so it denies no completion.
func TestSetupSnapshot_NoPrefillRowWhenNothingIsOwed(t *testing.T) {
	ctx := context.Background()
	f := &fakeSetupProvider{engineInstalled: true, engineReady: true}
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	r.Apply(ctx, desiredFrame("ollama", "", 0))

	for _, st := range r.snapshot(ctx).Steps {
		if st.ID == setupStepPrefillMeasurement {
			t.Fatalf("emitted a prefill row with nothing owed: %+v", st)
		}
	}
}
