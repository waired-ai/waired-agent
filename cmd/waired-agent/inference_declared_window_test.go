package main

import (
	"path/filepath"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// TestDeclaredContextWindow covers the field the mesh routes on
// (signer.InferenceState.ContextWindow, waired#1031). Every case here is
// a way the node could have advertised a window it does not serve, which
// is the failure this field exists to make impossible.
func TestDeclaredContextWindow(t *testing.T) {
	manifests := []catalog.Manifest{
		{ModelID: "big", ContextLength: 262144},
		{ModelID: "huge", ContextLength: 1048576},
		{ModelID: "small", ContextLength: 131072},
	}
	newProv := func(t *testing.T, tuning infruntime.ModelTuning, active string) *agentInferenceProvider {
		t.Helper()
		a := newTestAdapter(t)
		if tuning != (infruntime.ModelTuning{}) {
			a.SetAppliedTuning(tuning)
		}
		store := catalog.NewStore(filepath.Join(t.TempDir(), "state.json"))
		if active != "" {
			if err := store.Update(func(s *catalog.State) {
				if s.Models == nil {
					s.Models = map[string]catalog.ModelState{}
				}
				s.Models[active] = catalog.ModelState{State: catalog.ModelStateReady, VariantID: "v"}
				s.Active = &catalog.ActiveSelection{
					Runtime: catalog.RuntimeOllama, ModelID: active, VariantID: "v", DecidedBy: "user"}
			}); err != nil {
				t.Fatalf("seed store: %v", err)
			}
		}
		return &agentInferenceProvider{manifests: manifests, ollama: a, store: store}
	}

	t.Run("applied window at the 200k mode", func(t *testing.T) {
		p := newProv(t, infruntime.ModelTuning{ModelID: "big", ContextLength: 200704, WindowFits: true}, "big")
		if got := p.DeclaredContextWindow(); got != 200704 {
			t.Errorf("got %d, want 200704", got)
		}
	})

	t.Run("a wider applied window is declared as it is", func(t *testing.T) {
		// The mode is a floor to compare against, not a value to round to:
		// a peer serving 262144 can take 200k traffic AND is worth knowing
		// about for anything that wants more.
		p := newProv(t, infruntime.ModelTuning{ModelID: "big", ContextLength: 262144, WindowFits: true}, "big")
		if got := p.DeclaredContextWindow(); got != 262144 {
			t.Errorf("got %d, want 262144", got)
		}
	})

	t.Run("a sub-200k window declares nothing", func(t *testing.T) {
		// The tuner no longer trims for spill (waired-agent#587), so this
		// is the safety net: were a sub-rung window ever applied again,
		// publishing it would advertise a window nobody can route on, and
		// publishing the intended one would be a lie. Saying nothing is
		// the only safe answer.
		p := newProv(t, infruntime.ModelTuning{ModelID: "big", ContextLength: 150000, WindowFits: true}, "big")
		if got := p.DeclaredContextWindow(); got != 0 {
			t.Errorf("got %d, want 0 (below the smallest declarable window)", got)
		}
	})

	t.Run("a forced rung declares the window it serves", func(t *testing.T) {
		// PRODUCT CONTRACT (waired-ai/waired-agent#657; owner ruling of
		// 2026-08-11 recorded on the waired-ai/waired window-contract
		// decision of 2026-08-02). This subtest asserted the opposite
		// until then: a rung the sizing could not be shown to hold
		// (WindowFits false — the forced lowest rung of
		// waired-agent#587, or the verify pass's spill latch) declared
		// nothing.
		//
		// It is served, so it is declared. Spill costs decode speed, not
		// window size, and withholding the window for a speed problem
		// made a host that answers real 200k requests invisible to the
		// mesh at every session size — the admin page then rendered that
		// silence as "takes no Claude Code sessions" one row above a
		// measured 12 s coding turn. Waired does not force state on a
		// device: a machine the operator chose to run a model on is
		// published on their own inference network, spilling or not.
		//
		// Speed-based exclusion belongs to a consumer that can see speed
		// (HostSpeed reaches the control plane, and is stripped from the
		// served NetworkMap by design), not to the agent withholding a
		// true fact. The size guard above still stands.
		p := newProv(t, infruntime.ModelTuning{ModelID: "big", ContextLength: 200704, WindowFits: false}, "big")
		if got := p.DeclaredContextWindow(); got != 200704 {
			t.Errorf("got %d, want 200704 (served, therefore declared)", got)
		}
	})

	t.Run("a spilling host below the smallest window still declares nothing", func(t *testing.T) {
		// The two guards are independent, and only the size one survives
		// #657: a forced rung that is ALSO under the smallest declarable
		// window is the case where a 200k session would truly be
		// truncated (waired-agent#623), so it stays silent.
		p := newProv(t, infruntime.ModelTuning{ModelID: "big", ContextLength: 150000, WindowFits: false}, "big")
		if got := p.DeclaredContextWindow(); got != 0 {
			t.Errorf("got %d, want 0 (below the smallest declarable window)", got)
		}
	})

	t.Run("cold engine declares nothing", func(t *testing.T) {
		// This is where DeclaredContextWindow and ContextWindowFor part
		// company: with no applied tuning the latter falls back to the
		// window the tuner AIMS for, which is exactly the optimism a
		// declaration may not be built on.
		p := newProv(t, infruntime.ModelTuning{}, "big")
		if got := p.DeclaredContextWindow(); got != 0 {
			t.Errorf("got %d, want 0 (nothing has tuned yet)", got)
		}
		if got := p.ContextWindowFor("big"); got == 0 {
			t.Error("ContextWindowFor must still answer for the guard; the two differ on purpose")
		}
	})

	t.Run("tuning for a different model declares nothing", func(t *testing.T) {
		p := newProv(t, infruntime.ModelTuning{ModelID: "other", ContextLength: 262144}, "big")
		if got := p.DeclaredContextWindow(); got != 0 {
			t.Errorf("got %d, want 0 (stale tuning for another model)", got)
		}
	})

	t.Run("never claims past the model's own window", func(t *testing.T) {
		// A tuning above native is a misconfiguration, not a capability —
		// and "small" cannot reach a declarable window at all, so capping
		// first is what makes the answer 0 instead of 262144.
		p := newProv(t, infruntime.ModelTuning{ModelID: "small", ContextLength: 262144, WindowFits: true}, "small")
		if got := p.DeclaredContextWindow(); got != 0 {
			t.Errorf("got %d, want 0 (native window is 131072)", got)
		}
	})

	t.Run("the 1M mode", func(t *testing.T) {
		p := newProv(t, infruntime.ModelTuning{ModelID: "huge", ContextLength: hostfit.ServingWindow1M, WindowFits: true}, "huge")
		if got := p.DeclaredContextWindow(); got != hostfit.ServingWindow1M {
			t.Errorf("got %d, want %d", got, hostfit.ServingWindow1M)
		}
	})

	t.Run("no active model declares nothing", func(t *testing.T) {
		p := newProv(t, infruntime.ModelTuning{ModelID: "big", ContextLength: 262144}, "")
		if got := p.DeclaredContextWindow(); got != 0 {
			t.Errorf("got %d, want 0 (nothing selected)", got)
		}
	})
}
