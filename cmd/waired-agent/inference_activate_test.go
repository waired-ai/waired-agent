package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

	// Note the scope of the "auto" assertions below: this provider has no
	// preferencePath, so there genuinely IS no prior selection to honour.
	// What decided_by should be when there IS one is
	// TestBundledActivationRecord / TestActivateBundledIfUnset_HonoursTheOperatorChoice.
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

// TestBundledActivationRecord table-tests the decision on its own, below
// the store and the file: the three answers differ only in what a person
// at this machine had already said.
//
// Product contract (waired-ai/waired-agent#783): an activation may not
// report "no prior selection" when there is one.
func TestBundledActivationRecord(t *testing.T) {
	for _, tc := range []struct {
		name       string
		chosen     string
		modelID    string
		wantBy     string
		wantReason string
	}{
		{
			name: "nobody chose anything", chosen: "", modelID: "bundled",
			wantBy: "auto", wantReason: "bundled model auto-activated on first run (no prior selection)",
		},
		{
			// The takeover path: the picker preselects the recommended
			// model, which is what BundledModelID names, so the operator's
			// pick and the bundled model are the SAME id.
			name: "the operator chose this very model", chosen: "bundled", modelID: "bundled",
			wantBy: "user", wantReason: "preferred-model switch applied (model ready)",
		},
		{
			// Gap-filling while the chosen model downloads — a real auto
			// decision, but not one taken in the absence of a selection.
			name: "the operator chose something else", chosen: "chosen", modelID: "bundled",
			wantBy: "auto", wantReason: "bundled model activated while the chosen model is not ready",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			by, reason := bundledActivationRecord(tc.chosen, tc.modelID)
			if by != tc.wantBy || reason != tc.wantReason {
				t.Errorf("got (%q, %q), want (%q, %q)", by, reason, tc.wantBy, tc.wantReason)
			}
		})
	}
}

// TestActivateBundledIfUnset_HonoursTheOperatorChoice runs the real thing
// against a real preferred-model.json, so the seam being tested is the one
// that was missing: activateBundledIfUnset never opened the file at all.
func TestActivateBundledIfUnset_HonoursTheOperatorChoice(t *testing.T) {
	newProvider := func(t *testing.T, pref *agentconfig.Preference) *agentInferenceProvider {
		t.Helper()
		dir := t.TempDir()
		store := catalog.NewStore(filepath.Join(dir, "state.json"))
		if err := store.Update(func(s *catalog.State) {
			s.Models = map[string]catalog.ModelState{
				"bundled": {State: catalog.ModelStateReady, VariantID: "q4"},
			}
		}); err != nil {
			t.Fatalf("seed store: %v", err)
		}
		prefPath := filepath.Join(dir, "preferred-model.json")
		if pref != nil {
			if err := agentconfig.SavePreference(prefPath, *pref); err != nil {
				t.Fatalf("save preference: %v", err)
			}
		}
		return &agentInferenceProvider{
			store:          store,
			cfg:            agentconfig.InferenceConfig{BundledModelID: "bundled"},
			preferencePath: prefPath,
			logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
	}

	t.Run("an operator preference for this model is recorded as the operator's", func(t *testing.T) {
		p := newProvider(t, &agentconfig.Preference{
			ModelID: "bundled", Source: agentconfig.PreferenceSourceOperator,
		})
		p.activateBundledIfUnset("bundled", "q4")
		st, _ := p.store.Load()
		if st.Active == nil {
			t.Fatal("Active nil after activating a ready bundled model")
		}
		if st.Active.DecidedBy != "user" {
			t.Errorf("DecidedBy = %q, want user — a person chose this model", st.Active.DecidedBy)
		}
		for _, r := range st.Active.DecisionReason {
			if strings.Contains(r, "no prior selection") {
				t.Errorf("DecisionReason = %v, must not claim there was no prior selection", st.Active.DecisionReason)
			}
		}
	})

	t.Run("an applied instruction is not a local choice", func(t *testing.T) {
		// source:"desired" is the setup reconciler writing what the control
		// plane asked for. Nobody at this machine answered, so the
		// activation is still an auto one (#647's provenance rule).
		p := newProvider(t, &agentconfig.Preference{
			ModelID: "bundled", Source: agentconfig.PreferenceSourceDesired,
		})
		p.activateBundledIfUnset("bundled", "q4")
		st, _ := p.store.Load()
		if st.Active == nil || st.Active.DecidedBy != "auto" {
			t.Errorf("Active = %+v, want an auto activation", st.Active)
		}
	})

	t.Run("a record written before Source existed carries no claim", func(t *testing.T) {
		p := newProvider(t, &agentconfig.Preference{ModelID: "bundled"})
		p.activateBundledIfUnset("bundled", "q4")
		st, _ := p.store.Load()
		if st.Active == nil || st.Active.DecidedBy != "auto" {
			t.Errorf("Active = %+v, want an auto activation", st.Active)
		}
	})
}

// TestBenchDescribes pins which stored measurements count as evidence
// about the model a host is serving.
//
// Product contract (waired-ai/waired-agent#783): the floor comparison may
// not judge one model by another model's rate.
func TestBenchDescribes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		bench  BenchResult
		active string
		want   bool
	}{
		{name: "measured the model that is serving", bench: BenchResult{ModelID: "a"}, active: "a", want: true},
		{name: "measured a different model", bench: BenchResult{ModelID: "b"}, active: "a", want: false},
		{
			// A cache entry or a build predating the field. Every reader
			// treated these as evidence before, and withholding the
			// recommendation would trade a stale number for none at all.
			name: "unlabelled", bench: BenchResult{}, active: "a", want: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := benchDescribes(tc.bench, tc.active); got != tc.want {
				t.Errorf("benchDescribes(%+v, %q) = %v, want %v", tc.bench, tc.active, got, tc.want)
			}
		})
	}
}

// TestActivationRemeasuresTheNewModel: activating a model the measurement
// on file does not describe starts a fresh one.
//
// Without it the takeover path leaves a host serving a model nothing ever
// measured — init exits while the pull is still running, so the only
// result on file belongs to whatever was serving before, and every asking
// surface reads it as this model's (waired-ai/waired-agent#783). The
// daemon still does not ACT on the verdict: stepping a host down needs
// consent it cannot ask for.
func TestActivationRemeasuresTheNewModel(t *testing.T) {
	newProvider := func(t *testing.T, runs chan<- string) *agentInferenceProvider {
		t.Helper()
		store := catalog.NewStore(filepath.Join(t.TempDir(), "state.json"))
		if err := store.Update(func(s *catalog.State) {
			s.Models = map[string]catalog.ModelState{
				"bundled": {State: catalog.ModelStateReady, VariantID: "q4"},
			}
		}); err != nil {
			t.Fatalf("seed store: %v", err)
		}
		p := &agentInferenceProvider{
			store:    store,
			cfg:      agentconfig.InferenceConfig{BundledModelID: "bundled"},
			profiler: cpuSwapProfiler(t),
			logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		p.benchRun = func(context.Context) BenchResult {
			runs <- "ran"
			return BenchResult{TokensPerSec: 80, Capacity: 2, ModelID: "bundled", Outcome: benchOutcomeMeasured}
		}
		return p
	}

	t.Run("a model no result on file describes is measured", func(t *testing.T) {
		runs := make(chan string, 4)
		p := newProvider(t, runs)
		// What is on file belongs to a model this host no longer serves.
		p.SetLastBench(BenchResult{TokensPerSec: 12, Capacity: 1, ModelID: "previous", Outcome: benchOutcomeMeasured})

		done := p.remeasureForActiveModel("bundled")
		if done == nil {
			t.Fatal("no benchmark started for a model nothing on file describes")
		}

		select {
		case <-runs:
		case <-time.After(10 * time.Second):
			t.Fatal("the run started but never reached the measurement")
		}
		// The run is detached; wait for it, or it writes into the temp
		// directory after this subtest has returned.
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("the benchmark run never completed")
		}
	})

	t.Run("nothing measured yet still gets a run", func(t *testing.T) {
		// The takeover path's own shape: the boot benchmark ran before any
		// model existed, so what is on file is a skip, not a verdict.
		runs := make(chan string, 4)
		p := newProvider(t, runs)
		p.SetLastBench(BenchResult{Capacity: 0, Outcome: benchOutcomeSkipped})

		done := p.remeasureForActiveModel("bundled")
		if done == nil {
			t.Fatal("a skipped run stood the measurement down")
		}
		<-runs
		<-done
	})

	t.Run("a real measurement of this model is left alone", func(t *testing.T) {
		runs := make(chan string, 4)
		p := newProvider(t, runs)
		p.SetLastBench(BenchResult{TokensPerSec: 80, Capacity: 2, ModelID: "bundled", Outcome: benchOutcomeMeasured})

		if done := p.remeasureForActiveModel("bundled"); done != nil {
			t.Fatal("re-measured a model the result on file already describes")
		}

		select {
		case <-runs:
			t.Fatal("a benchmark ran anyway")
		case <-time.After(200 * time.Millisecond):
		}
	})

	// Where the trigger is NOT. The boot activations
	// (activateBundledIfReady, bootstrapPreferredModel) reach main.go's own
	// benchmark a moment later, orchestrated with the cache and the engine
	// claim that run needs; starting a second one from under them would
	// race it for the engine rather than measure anything sooner. Hanging
	// it on the activation instead detached a goroutine into every caller
	// of those two, which is how the darwin leg noticed.
	t.Run("activation alone starts nothing", func(t *testing.T) {
		runs := make(chan string, 4)
		p := newProvider(t, runs)
		p.SetLastBench(BenchResult{TokensPerSec: 12, Capacity: 1, ModelID: "previous", Outcome: benchOutcomeMeasured})

		p.activateBundledIfUnset("bundled", "q4")

		select {
		case <-runs:
			t.Fatal("activation started a benchmark of its own")
		case <-time.After(200 * time.Millisecond):
		}
	})
}

// TestRunPullJob_ReMeasuresTheModelItJustMadeActive proves the pull path
// actually REACHES remeasureForActiveModel.
//
// It is here because the condition guarding that call was narrowed twice
// while getting it right, and after the second narrowing no test in the
// package reached the call at all. A draft that DEADLOCKED the daemon
// there — blocking on a benchmark that waits for a quiet engine, from
// inside the very function whose deferred endPull is what lets the engine
// go quiet — passed the entire suite on the strength of that absence.
//
// The comment beside the call explains why it must not block. A comment is
// not executable, and the next person to doubt it will try blocking and
// see everything green. This is what fails instead.
func TestRunPullJob_ReMeasuresTheModelItJustMadeActive(t *testing.T) {
	r := newBlockingRunner(t)
	p := bounceProvider(t, r)
	p.cfg.BundledModelID = "model-a" // so the completed pull activates it
	p.profiler = cpuSwapProfiler(t)

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	p.benchRun = func(context.Context) BenchResult {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return BenchResult{TokensPerSec: 55, Capacity: 1, ModelID: "model-a", Outcome: benchOutcomeMeasured}
	}

	if _, err := p.PullModel(context.Background(), "model-a"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	r.awaitStarted(t)
	r.releaseAll()
	p.waitForPulls()

	if got := p.activeModelID(); got != "model-a" {
		t.Fatalf("active model after the pull = %q, want model-a — the fixture no "+
			"longer sets up the transition this test is about", got)
	}
	select {
	case <-entered:
	case <-time.After(waitBackstop):
		t.Fatal("the pull that made model-a active never reached the re-measurement: " +
			"the trigger's condition no longer matches the path it guards")
	}

	// Join the run and let it finish, so nothing is still writing into the
	// temp directory when this test returns.
	done := p.startBenchmarkJob(0)
	close(release)
	waitDone(t, done)
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
// a missing one attempts a re-pull (here blocked by a manifest with no
// variant at all — the dispatch path must not panic or touch Active).
// allow_pull=false used to be the cheap way to block it; since #338 it
// refuses before the dispatch, which would leave the subtest asserting
// something its name does not describe.
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
			cfg:       agentconfig.InferenceConfig{PreferredModelID: "pref-model", AllowPull: true},
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
