package main

import (
	"path/filepath"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/router"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// TestContextWindowFor exercises the #623 effective-window helper:
// min(native, host-sustainable), the EffectiveContextFloor fallback when the
// host window is unknown, dynamic-alias / unknown-id resolution to the active
// model, and the fail-open 0.
func TestContextWindowFor(t *testing.T) {
	manifests := []catalog.Manifest{
		{ModelID: "big", ModelAliases: []string{"waired/default"}, ContextLength: 262144},
		{ModelID: "small", ContextLength: 131072},
	}
	newProv := func(t *testing.T, tuning infruntime.ModelTuning, active string) *agentInferenceProvider {
		t.Helper()
		a := newTestAdapter(t, false)
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
				s.Active = &catalog.ActiveSelection{Runtime: catalog.RuntimeOllama, ModelID: active, VariantID: "v", DecidedBy: "user"}
			}); err != nil {
				t.Fatalf("seed store: %v", err)
			}
		}
		return &agentInferenceProvider{manifests: manifests, ollama: a, store: store}
	}

	t.Run("min of native and host", func(t *testing.T) {
		p := newProv(t, infruntime.ModelTuning{ModelID: "big", ContextLength: 131072}, "")
		if got := p.ContextWindowFor("big"); got != 131072 {
			t.Errorf("got %d, want 131072 (min of native 262144 / host 131072)", got)
		}
	})

	t.Run("mismatched host tuning ignored → floor", func(t *testing.T) {
		p := newProv(t, infruntime.ModelTuning{ModelID: "other", ContextLength: 50000}, "")
		if got := p.ContextWindowFor("big"); got != router.CodingAgentContextFloorTokens {
			t.Errorf("got %d, want %d (floor; tuning for a different model is ignored)", got, router.CodingAgentContextFloorTokens)
		}
	})

	t.Run("sub-floor native, no host → native", func(t *testing.T) {
		p := newProv(t, infruntime.ModelTuning{}, "")
		if got := p.ContextWindowFor("small"); got != 131072 {
			t.Errorf("got %d, want 131072 (EffectiveContextFloor capped at native)", got)
		}
	})

	t.Run("dynamic alias resolves to manifest window", func(t *testing.T) {
		p := newProv(t, infruntime.ModelTuning{ModelID: "big", ContextLength: 200000}, "")
		if got := p.ContextWindowFor("waired/default"); got != 200000 {
			t.Errorf("got %d, want 200000", got)
		}
	})

	t.Run("unknown id, no active → 0 (fail open)", func(t *testing.T) {
		p := newProv(t, infruntime.ModelTuning{}, "")
		if got := p.ContextWindowFor("claude-sonnet-4"); got != 0 {
			t.Errorf("got %d, want 0", got)
		}
	})

	t.Run("unknown id resolves to active model", func(t *testing.T) {
		p := newProv(t, infruntime.ModelTuning{ModelID: "big", ContextLength: 150000}, "big")
		if got := p.ContextWindowFor("claude-sonnet-4"); got != 150000 {
			t.Errorf("got %d, want 150000 (resolved to active 'big')", got)
		}
	})
}
