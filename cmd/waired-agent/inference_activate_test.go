package main

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
)

// TestActivateBundledIfUnset guards the BUG3 fix: a fresh install commits no
// ActiveSelection (MigrateInPlace only synthesises one on a v1→v2 carry-over),
// so the freshly-pulled bundled model must be auto-activated — otherwise the
// agent stays in subsystem_state "awaiting_model" (EngineReady=false → the
// boot benchmark POSTs an empty model and 400s, /inference/benchmark 425s)
// even though the engine serves on demand.
func TestActivateBundledIfUnset(t *testing.T) {
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
		return &agentInferenceProvider{
			store:  store,
			cfg:    agentconfig.InferenceConfig{BundledModelID: "bundled"},
			logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
	}

	t.Run("commits active when bundled model ready and none set", func(t *testing.T) {
		p := newProvider(t, func(s *catalog.State) {
			s.Models["bundled"] = catalog.ModelState{State: catalog.ModelStateReady, VariantID: "q4"}
		})
		p.activateBundledIfUnset("bundled", "q4")
		st, _ := p.store.Load()
		if st.Active == nil {
			t.Fatal("Active still nil after activating a ready bundled model (BUG3 regression)")
		}
		if st.Active.ModelID != "bundled" || st.Active.VariantID != "q4" {
			t.Errorf("Active = %s/%s, want bundled/q4", st.Active.ModelID, st.Active.VariantID)
		}
		if st.Active.DecidedBy != "auto" {
			t.Errorf("DecidedBy = %q, want auto", st.Active.DecidedBy)
		}
	})

	t.Run("falls back to the model's recorded variant when arg empty", func(t *testing.T) {
		p := newProvider(t, func(s *catalog.State) {
			s.Models["bundled"] = catalog.ModelState{State: catalog.ModelStateReady, VariantID: "q4"}
		})
		p.activateBundledIfUnset("bundled", "")
		st, _ := p.store.Load()
		if st.Active == nil || st.Active.VariantID != "q4" {
			t.Errorf("Active = %+v, want variant q4 from ModelState", st.Active)
		}
	})

	t.Run("no-op when bundled model not ready", func(t *testing.T) {
		p := newProvider(t, func(s *catalog.State) {
			s.Models["bundled"] = catalog.ModelState{State: catalog.ModelStateDownloading, VariantID: "q4"}
		})
		p.activateBundledIfUnset("bundled", "q4")
		st, _ := p.store.Load()
		if st.Active != nil {
			t.Errorf("Active = %+v, want nil (model not ready)", st.Active)
		}
	})

	t.Run("does not override an existing active selection", func(t *testing.T) {
		p := newProvider(t, func(s *catalog.State) {
			s.Models["bundled"] = catalog.ModelState{State: catalog.ModelStateReady, VariantID: "q4"}
			s.Active = &catalog.ActiveSelection{
				Runtime: catalog.RuntimeOllama, ModelID: "other", VariantID: "x", DecidedBy: "user",
			}
		})
		p.activateBundledIfUnset("bundled", "q4")
		st, _ := p.store.Load()
		if st.Active == nil || st.Active.ModelID != "other" {
			t.Errorf("Active = %+v, want unchanged 'other'", st.Active)
		}
	})
}

// TestActivatePreferredIfNeeded guards the issue #347 reconcile: the
// /preferred-model handler persisted the choice and restarted the agent,
// but nothing ever wrote state.Active afterwards, so the daemon came
// back up serving the old model forever.
func TestActivatePreferredIfNeeded(t *testing.T) {
	manifests := []catalog.Manifest{
		{ModelID: "pref-model", ModelAliases: []string{"waired/pref"}},
		{ModelID: "other-model"},
	}
	newProvider := func(t *testing.T, preferred string, seed func(*catalog.State)) *agentInferenceProvider {
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
		return &agentInferenceProvider{
			store:     store,
			cfg:       agentconfig.InferenceConfig{PreferredModelID: preferred},
			manifests: manifests,
			logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
	}

	t.Run("replaces the existing active selection once the preferred model is ready", func(t *testing.T) {
		p := newProvider(t, "pref-model", func(s *catalog.State) {
			s.Models["pref-model"] = catalog.ModelState{State: catalog.ModelStateReady, VariantID: "q4"}
			s.Active = &catalog.ActiveSelection{
				Runtime: catalog.RuntimeOllama, ModelID: "old-model", VariantID: "q4", DecidedBy: "auto",
			}
		})
		p.activatePreferredIfNeeded("pref-model", "q4")
		st, _ := p.store.Load()
		if st.Active == nil || st.Active.ModelID != "pref-model" {
			t.Fatalf("Active = %+v, want pref-model (issue #347 regression)", st.Active)
		}
		if st.Active.DecidedBy != "user" {
			t.Errorf("DecidedBy = %q, want user", st.Active.DecidedBy)
		}
	})

	t.Run("resolves an alias preference to its model id", func(t *testing.T) {
		p := newProvider(t, "waired/pref", func(s *catalog.State) {
			s.Models["pref-model"] = catalog.ModelState{State: catalog.ModelStateReady, VariantID: "q4"}
		})
		p.activatePreferredIfNeeded("pref-model", "")
		st, _ := p.store.Load()
		if st.Active == nil || st.Active.ModelID != "pref-model" || st.Active.VariantID != "q4" {
			t.Errorf("Active = %+v, want pref-model/q4 via alias", st.Active)
		}
	})

	t.Run("an unrelated pull does not hijack the active slot", func(t *testing.T) {
		p := newProvider(t, "pref-model", func(s *catalog.State) {
			s.Models["other-model"] = catalog.ModelState{State: catalog.ModelStateReady, VariantID: "q4"}
			s.Active = &catalog.ActiveSelection{
				Runtime: catalog.RuntimeOllama, ModelID: "old-model", VariantID: "q4", DecidedBy: "auto",
			}
		})
		p.activatePreferredIfNeeded("other-model", "q4")
		st, _ := p.store.Load()
		if st.Active == nil || st.Active.ModelID != "old-model" {
			t.Errorf("Active = %+v, want unchanged old-model", st.Active)
		}
	})

	t.Run("no-op when no preference is set", func(t *testing.T) {
		p := newProvider(t, "", func(s *catalog.State) {
			s.Models["pref-model"] = catalog.ModelState{State: catalog.ModelStateReady, VariantID: "q4"}
		})
		p.activatePreferredIfNeeded("pref-model", "q4")
		st, _ := p.store.Load()
		if st.Active != nil {
			t.Errorf("Active = %+v, want nil", st.Active)
		}
	})

	t.Run("no-op while the preferred model is still downloading", func(t *testing.T) {
		p := newProvider(t, "pref-model", func(s *catalog.State) {
			s.Models["pref-model"] = catalog.ModelState{State: catalog.ModelStateDownloading, VariantID: "q4"}
		})
		p.activatePreferredIfNeeded("pref-model", "q4")
		st, _ := p.store.Load()
		if st.Active != nil {
			t.Errorf("Active = %+v, want nil (model not ready)", st.Active)
		}
	})

	t.Run("keeps an active selection that already points at the preferred model", func(t *testing.T) {
		p := newProvider(t, "pref-model", func(s *catalog.State) {
			s.Models["pref-model"] = catalog.ModelState{State: catalog.ModelStateReady, VariantID: "q4"}
			s.Active = &catalog.ActiveSelection{
				Runtime: catalog.RuntimeOllama, ModelID: "pref-model", VariantID: "q4", DecidedBy: "user",
				DecisionReason: []string{"original"},
			}
		})
		p.activatePreferredIfNeeded("pref-model", "q4")
		st, _ := p.store.Load()
		if st.Active == nil || len(st.Active.DecisionReason) != 1 || st.Active.DecisionReason[0] != "original" {
			t.Errorf("Active = %+v, want untouched original selection", st.Active)
		}
	})
}

// TestBootstrapPreferredModel covers the boot-time half of the #347
// reconcile: a Ready preferred model is committed without a pull, and
// a missing one attempts a re-pull (here blocked by allow_pull=false —
// the dispatch path must not panic or touch Active).
func TestBootstrapPreferredModel(t *testing.T) {
	manifests := []catalog.Manifest{{ModelID: "pref-model"}}

	t.Run("activates a preferred model that is already on disk", func(t *testing.T) {
		store := catalog.NewStore(filepath.Join(t.TempDir(), "state.json"))
		if err := store.Update(func(s *catalog.State) {
			s.Models = map[string]catalog.ModelState{
				"pref-model": {State: catalog.ModelStateReady, VariantID: "q4"},
			}
		}); err != nil {
			t.Fatalf("seed store: %v", err)
		}
		p := &agentInferenceProvider{
			store:     store,
			cfg:       agentconfig.InferenceConfig{PreferredModelID: "pref-model"},
			manifests: manifests,
			logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		p.bootstrapPreferredModel(t.Context())
		st, _ := p.store.Load()
		if st.Active == nil || st.Active.ModelID != "pref-model" {
			t.Errorf("Active = %+v, want pref-model", st.Active)
		}
	})

	t.Run("missing model attempts a re-pull and leaves Active alone", func(t *testing.T) {
		store := catalog.NewStore(filepath.Join(t.TempDir(), "state.json"))
		p := &agentInferenceProvider{
			store:     store,
			cfg:       agentconfig.InferenceConfig{PreferredModelID: "pref-model", AllowPull: false},
			manifests: manifests,
			logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		p.bootstrapPreferredModel(t.Context())
		st, _ := p.store.Load()
		if st.Active != nil {
			t.Errorf("Active = %+v, want nil", st.Active)
		}
	})
}
