package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// Every reader of "what is this host serving" is on the provider's store
// (waired-agent#1206).
//
// There used to be six free functions — probeTargetForActive,
// engineModelForActive, variantIDForActive, modelIDForActive,
// variantSHAForActive, activeEngineTagsForActive — each opening its own
// catalog at catalog.DefaultStatePath(). That path resolves
// paths.StateDir(paths.AutoDetect), which ignores --state-dir, so on a
// daemon started with one they could answer about a different host
// entirely. It also made them untestable: anything reaching them read the
// state file of whatever machine the test ran on, which is the
// machine-global dependency CLAUDE.md §Test discipline says to seal.
//
// These tests exist because the conversion is what makes them writable.

// activeReaderProvider is a provider with nothing but a store — the
// smallest thing these readers need.
func activeReaderProvider(t *testing.T) *agentInferenceProvider {
	t.Helper()
	return &agentInferenceProvider{
		store:  catalog.NewStore(filepath.Join(t.TempDir(), "state.json")),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func seedActive(t *testing.T, p *agentInferenceProvider, runtime, modelID, variantID string, ms catalog.ModelState) {
	t.Helper()
	if err := p.store.Update(func(s *catalog.State) {
		s.Active = &catalog.ActiveSelection{Runtime: runtime, ModelID: modelID, VariantID: variantID}
		if s.Models == nil {
			s.Models = map[string]catalog.ModelState{}
		}
		s.Models[modelID] = ms
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// TestActiveEngineModel is the engine-native name the benchmark sends.
func TestActiveEngineModel(t *testing.T) {
	t.Run("no store at all", func(t *testing.T) {
		if got := (*agentInferenceProvider)(nil).activeEngineModel(); got != "" {
			t.Errorf("activeEngineModel on a nil provider = %q, want empty", got)
		}
		if got := (&agentInferenceProvider{}).activeEngineModel(); got != "" {
			t.Errorf("activeEngineModel with no store = %q, want empty", got)
		}
	})

	t.Run("no active selection", func(t *testing.T) {
		p := activeReaderProvider(t)
		if got := p.activeEngineModel(); got != "" {
			t.Errorf("activeEngineModel = %q, want empty so the benchmark short-circuits", got)
		}
	})

	t.Run("ollama wants the tag", func(t *testing.T) {
		p := activeReaderProvider(t)
		seedActive(t, p, catalog.RuntimeOllama, "m", "q4",
			catalog.ModelState{OllamaTag: "m:8b-q4_K_M", VariantID: "q4"})
		if got := p.activeEngineModel(); got != "m:8b-q4_K_M" {
			t.Errorf("activeEngineModel = %q, want the ollama tag", got)
		}
	})

	t.Run("vLLM wants the HF repo id", func(t *testing.T) {
		p := activeReaderProvider(t)
		seedActive(t, p, catalog.RuntimeVLLM, "m", "safetensors",
			catalog.ModelState{HFRepo: "acme/m", VariantID: "safetensors"})
		if got := p.activeEngineModel(); got != "acme/m" {
			t.Errorf("activeEngineModel = %q, want the HF repo id", got)
		}
	})

	t.Run("falls back to the catalog id when the engine name is missing", func(t *testing.T) {
		p := activeReaderProvider(t)
		seedActive(t, p, catalog.RuntimeOllama, "m", "q4", catalog.ModelState{VariantID: "q4"})
		if got := p.activeEngineModel(); got != "m" {
			t.Errorf("activeEngineModel = %q, want the catalog id", got)
		}
	})
}

// TestActiveEngineTags: the pair the probe loop enforces "1 agent = 1
// model" with. Both halves resolve to the same tag today (the #642 derived
// batch model that made them differ was retired by waired-agent#1079) —
// pinned so a future divergence is a decision, not a surprise.
func TestActiveEngineTags(t *testing.T) {
	t.Run("nothing to read", func(t *testing.T) {
		for _, p := range []*agentInferenceProvider{nil, {}} {
			a, s := p.activeEngineTags()
			if a != "" || s != "" {
				t.Errorf("activeEngineTags = (%q, %q), want two empties", a, s)
			}
		}
	})

	t.Run("no active selection", func(t *testing.T) {
		p := activeReaderProvider(t)
		if a, s := p.activeEngineTags(); a != "" || s != "" {
			t.Errorf("activeEngineTags = (%q, %q), want two empties", a, s)
		}
	})

	t.Run("an active ollama selection", func(t *testing.T) {
		p := activeReaderProvider(t)
		seedActive(t, p, catalog.RuntimeOllama, "m", "q4",
			catalog.ModelState{OllamaTag: "m:8b", VariantID: "q4"})
		a, s := p.activeEngineTags()
		if a != "m:8b" || s != "m:8b" {
			t.Errorf("activeEngineTags = (%q, %q), want both m:8b", a, s)
		}
	})

	t.Run("a variant the state disagrees with resolves to nothing", func(t *testing.T) {
		// activeEngineTag's own rule: a ModelState recording a different
		// variant than Active names is a host mid-switch, and advertising
		// either tag would be a claim about a model it is not serving.
		p := activeReaderProvider(t)
		seedActive(t, p, catalog.RuntimeOllama, "m", "q4",
			catalog.ModelState{OllamaTag: "m:8b", VariantID: "q8"})
		if a, s := p.activeEngineTags(); a != "" || s != "" {
			t.Errorf("activeEngineTags = (%q, %q) for a torn selection, want two empties", a, s)
		}
	})
}

// TestActiveVariantSHA: empty on every condition that would make the boot
// benchmark's cache key a guess.
func TestActiveVariantSHA(t *testing.T) {
	if got := (*agentInferenceProvider)(nil).activeVariantSHA(); got != "" {
		t.Errorf("activeVariantSHA on a nil provider = %q, want empty", got)
	}
	p := activeReaderProvider(t)
	if got := p.activeVariantSHA(); got != "" {
		t.Errorf("activeVariantSHA with no Active = %q, want empty", got)
	}
	// A model the bundled catalog does not carry: empty rather than a
	// digest of nothing, which is what benchCacheDisabledReason reports as
	// "the active model is not in this build's catalog".
	seedActive(t, p, catalog.RuntimeOllama, "not-in-any-catalog", "q4", catalog.ModelState{})
	if got := p.activeVariantSHA(); got != "" {
		t.Errorf("activeVariantSHA for an uncatalogued model = %q, want empty", got)
	}
}

// TestRunBenchmarkJob_EngineLessHostRecordsNothing is the regression this
// refactor could have introduced, pinned.
//
// PRODUCT CONTRACT (waired-agent#1206, and setup_desired.go's own words).
// RunBootBenchmark answers `skipped` for an absent engine, BEFORE its
// EngineReady gate. `skipped` is a recorded ending, and setup_desired.go
// says what recording one costs: "a recorded ending at the requested
// generation satisfies the guard below forever. The measurement then
// never runs and the wizard shows a finished speed check with no figure
// in it."
//
// While the engine target always answered ollama, an engine-less host
// reached the EngineReady gate instead and ended `engine_not_ready`,
// which is NOT recorded — so the reconciler asked again. Now that the
// target can say "no engine", the job has to decline before
// RunBootBenchmark can turn that into a recorded ending.
func TestRunBenchmarkJob_EngineLessHostRecordsNothing(t *testing.T) {
	// No ollamaUsable / vllmUsable: this host has no engine installed, so
	// probeTarget answers (none, 0) — the state the guard exists for.
	p := activeReaderProvider(t)
	p.profiler = cpuSwapProfiler(t)
	p.cfg = agentconfig.InferenceConfig{}

	done := make(chan struct{})
	p.runBenchmarkJob(7, done)
	select {
	case <-done:
	case <-time.After(waitBackstop):
		t.Fatal("runBenchmarkJob left its done channel open; a joiner would wait forever")
	}

	st, err := p.store.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st.LastBenchmark != nil {
		t.Errorf("a host with no engine recorded a benchmark: %+v\n"+
			"a recorded ending at the requested generation satisfies the setup guard forever,\n"+
			"and the wizard then shows a finished speed check with no figure in it", st.LastBenchmark)
	}
}

// TestEngineListening dials the REAL predicate, which nothing did before.
//
// `ensureHostMemoryMeasured` takes it as a parameter and every test passes
// a fake, so the production function was never called by anything —
// exactly the shape CLAUDE.md §Test discipline names ("a `var xFn =
// realFn` seam needs a table test on realFn, or the real one is never
// called by any test").
//
// PRODUCT CONTRACT (waired-agent#1206) for the both-ports part: the
// question is whether an engine waired does NOT manage is holding memory,
// so an answer derived from waired's own engine choice looks at the wrong
// port. It used to ask the engine target, which said ollama on a host with
// no selection — so a foreign vLLM on 9479 was never seen.
func TestEngineListening(t *testing.T) {
	listener := func(t *testing.T) int {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		t.Cleanup(srv.Close)
		_, port := hostPort(t, srv.URL)
		return port
	}
	// A port nothing is on. Taken by binding and releasing, so it is free
	// now and was not guessed.
	freePort := func(t *testing.T) int {
		t.Helper()
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		port := l.Addr().(*net.TCPAddr).Port
		_ = l.Close()
		return port
	}

	t.Run("nothing on either port", func(t *testing.T) {
		cfg := agentconfig.InferenceConfig{OllamaPort: freePort(t), VLLMPort: freePort(t)}
		if engineListening(cfg)() {
			t.Error("reported an engine with nothing listening on either port")
		}
	})

	t.Run("something on the ollama port", func(t *testing.T) {
		cfg := agentconfig.InferenceConfig{OllamaPort: listener(t), VLLMPort: freePort(t)}
		if !engineListening(cfg)() {
			t.Error("missed a listener on the ollama port")
		}
	})

	t.Run("something on the vLLM port", func(t *testing.T) {
		// The case the old rule could not see: it asked which engine was
		// active, got ollama for a host with no selection, and dialled
		// 9475 while the memory was held on 9479.
		cfg := agentconfig.InferenceConfig{OllamaPort: freePort(t), VLLMPort: listener(t)}
		if !engineListening(cfg)() {
			t.Error("missed a listener on the vLLM port — the #1206 blind spot")
		}
	})
}

// TestProbeTarget_IsTheOnlyEngineTargetReader guards the collapse itself:
// the probe target is a provider method, so a caller cannot reach it
// without holding the store it reads.
func TestProbeTarget_IsTheOnlyEngineTargetReader(t *testing.T) {
	p := activeReaderProvider(t)
	p.setServingEngine(catalog.RuntimeOllama)
	// An Active selection says nothing about whether the engine is
	// installed — which is the whole distinction the collapse rests on.
	seedActive(t, p, catalog.RuntimeOllama, "m", "q4",
		catalog.ModelState{OllamaTag: "m:8b", VariantID: "q4"})
	kind, port := p.probeTarget(agentconfig.InferenceConfig{})
	if kind != signer.InferenceTypeNone || port != 0 {
		t.Errorf("probeTarget = (%q, %d) on a host with an Active selection and no engine\n"+
			"installed, want (none, 0): Active records what was chosen, not what is on disk",
			kind, port)
	}
	_ = context.Background()
}
