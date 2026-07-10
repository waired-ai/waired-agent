package main

import (
	"path/filepath"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// ActiveModelID backs the Claude-intercept model mapping (#600): unlike
// EngineReady it must report the committed selection even while the model
// is still pulling/loading, so the router (not the resolver) owns the
// precise not-ready error.
func TestActiveModelID(t *testing.T) {
	newProvider := func(t *testing.T, seed func(*catalog.State)) *agentInferenceProvider {
		t.Helper()
		store := catalog.NewStore(filepath.Join(t.TempDir(), "state.json"))
		if seed != nil {
			if err := store.Update(func(s *catalog.State) {
				if s.Models == nil {
					s.Models = map[string]catalog.ModelState{}
				}
				seed(s)
			}); err != nil {
				t.Fatalf("seed store: %v", err)
			}
		}
		return &agentInferenceProvider{store: store}
	}

	t.Run("no active selection", func(t *testing.T) {
		p := newProvider(t, nil)
		if id, ok := p.ActiveModelID(); ok || id != "" {
			t.Errorf("ActiveModelID = (%q, %v), want (\"\", false)", id, ok)
		}
	})

	t.Run("committed selection", func(t *testing.T) {
		p := newProvider(t, func(s *catalog.State) {
			s.Models["qwen3-8b-instruct"] = catalog.ModelState{State: catalog.ModelStateReady, VariantID: "q4"}
			s.Active = &catalog.ActiveSelection{
				Runtime: catalog.RuntimeOllama, ModelID: "qwen3-8b-instruct", VariantID: "q4", DecidedBy: "user",
			}
		})
		if id, ok := p.ActiveModelID(); !ok || id != "qwen3-8b-instruct" {
			t.Errorf("ActiveModelID = (%q, %v), want (qwen3-8b-instruct, true)", id, ok)
		}
	})

	t.Run("committed but not ready still resolves", func(t *testing.T) {
		p := newProvider(t, func(s *catalog.State) {
			s.Models["qwen3-8b-instruct"] = catalog.ModelState{State: catalog.ModelStateDownloading, VariantID: "q4"}
			s.Active = &catalog.ActiveSelection{
				Runtime: catalog.RuntimeOllama, ModelID: "qwen3-8b-instruct", VariantID: "q4", DecidedBy: "auto",
			}
		})
		if id, ok := p.ActiveModelID(); !ok || id != "qwen3-8b-instruct" {
			t.Errorf("ActiveModelID = (%q, %v), want the mid-pull model resolvable", id, ok)
		}
	})
}
