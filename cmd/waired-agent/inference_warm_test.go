package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// warmEngine is a fake ollama that records the /api/generate warm-up
// requests it receives and serves a scriptable /api/ps.
//
// It records the DECODED body, not just the fact of a call: keep_alive is
// the field the warm-up has to send and the probe callers must not, so a
// fake that dropped it would make the failing case unwritable.
type warmEngine struct {
	mu       sync.Mutex
	resident []string // what /api/ps reports as loaded
	loads    []map[string]any
}

func (e *warmEngine) recorded() []map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]map[string]any(nil), e.loads...)
}

func (e *warmEngine) start(t *testing.T) (host string, port int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ps":
			e.mu.Lock()
			models := make([]map[string]any, 0, len(e.resident))
			for _, n := range e.resident {
				models = append(models, map[string]any{"name": n, "size_vram": 1})
			}
			e.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"models": models})
		case "/api/generate":
			body, _ := io.ReadAll(r.Body)
			var got map[string]any
			_ = json.Unmarshal(body, &got)
			e.mu.Lock()
			e.loads = append(e.loads, got)
			e.resident = append(e.resident, got["model"].(string))
			e.mu.Unlock()
			_, _ = w.Write([]byte(`{"done":true}`))
		default: // /api/tags and the health probe
			_, _ = w.Write([]byte(`{"models":[]}`))
		}
	}))
	t.Cleanup(srv.Close)
	return hostPort(t, srv.URL)
}

// warmProvider wires a provider whose engine is the fake above, with
// modelID active and ready under tag.
func warmProvider(t *testing.T, e *warmEngine, modelID, tag string) *agentInferenceProvider {
	t.Helper()
	host, port := e.start(t)
	a := infruntime.NewOllamaAdapter(infruntime.OllamaConfig{
		Binary: "/fake/ollama", Host: host, Port: port,
		Spawner: &fakeSpawner{}, HTTPClient: &http.Client{},
		HealthInterval: time.Millisecond, HealthSuccess: 1, HealthMaxFails: 5,
	})
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	p := &agentInferenceProvider{
		ollama:   a,
		store:    catalog.NewStore(filepath.Join(t.TempDir(), "state.json")),
		cfg:      agentconfig.InferenceConfig{},
		logger:   slog.New(slog.DiscardHandler),
		agentCtx: context.Background(),
	}
	if modelID != "" {
		if err := p.store.Update(func(s *catalog.State) {
			s.Models[modelID] = catalog.ModelState{
				State: catalog.ModelStateReady, VariantID: "q4", OllamaTag: tag,
			}
			s.Active = &catalog.ActiveSelection{Runtime: catalog.RuntimeOllama, ModelID: modelID}
		}); err != nil {
			t.Fatalf("seed state: %v", err)
		}
	}
	return p
}

// THE #320 REGRESSION BAR, warm half. PRODUCT CONTRACT: the serving
// model is loaded outside a real request, with an explicit keep_alive.
//
// The cold load was the largest single term in first-request TTFT — a
// 22.7 GB model on the reported host — and nothing preloaded it. The
// keep_alive matters on its own: an ADOPTED engine was spawned by a
// previous run, so its OLLAMA_KEEP_ALIVE is not ours to set, and a warm
// that relied on the serve-level variable would be undone minutes later
// on exactly the hosts that cannot be bounced to fix it.
func TestWarmServingModel_LoadsTheActiveTagWithKeepAlive(t *testing.T) {
	e := &warmEngine{}
	p := warmProvider(t, e, "model-a", "a:q4")

	p.warmServingModelNow(context.Background())

	got := e.recorded()
	if len(got) != 1 {
		t.Fatalf("warm-up loads = %d, want exactly 1: %+v", len(got), got)
	}
	if got[0]["model"] != "a:q4" {
		t.Errorf("warmed %q, want the active model's tag a:q4", got[0]["model"])
	}
	if got[0]["keep_alive"] != infruntime.KeepAliveIndefinite {
		t.Errorf("keep_alive = %v, want %q sent explicitly — the serve-level "+
			"variable is not ours to trust on an adopted engine",
			got[0]["keep_alive"], infruntime.KeepAliveIndefinite)
	}
}

// Already resident is the steady state, and the call sites are
// deliberately liberal (every reconcile exit, every boot, every unpark).
// That is only affordable because this costs one /api/ps.
func TestWarmServingModel_SkipsAModelAlreadyResident(t *testing.T) {
	e := &warmEngine{resident: []string{"a:q4"}}
	p := warmProvider(t, e, "model-a", "a:q4")

	p.warmServingModelNow(context.Background())

	if got := e.recorded(); len(got) != 0 {
		t.Fatalf("re-loaded a model already in /api/ps: %+v", got)
	}
}

// A DIFFERENT model being resident is not a reason to skip: that is the
// mid-switch case, and the router is pointing at the new one.
//
// This is also the gap in the pre-#320 behaviour, where the only load
// outside a request was verifyOllamaTuning's — which skips whenever
// /api/ps is non-empty, whatever is in it.
func TestWarmServingModel_LoadsWhenAnotherModelIsResident(t *testing.T) {
	e := &warmEngine{resident: []string{"b:q4"}}
	p := warmProvider(t, e, "model-a", "a:q4")

	p.warmServingModelNow(context.Background())

	got := e.recorded()
	if len(got) != 1 || got[0]["model"] != "a:q4" {
		t.Fatalf("loads = %+v, want one load of a:q4 — a foreign resident model "+
			"must not suppress warming the one that will serve", got)
	}
}

// A pull holds the disk and, on a single-GPU host, the memory the load
// wants; warming into that contention is how a download and a model load
// take each other down. endPull fires a reconcile when the last pull
// leaves, and that reconcile warms — so this defers, it does not drop.
func TestWarmTarget_DeclinesWhileAPullIsInFlight(t *testing.T) {
	e := &warmEngine{}
	p := warmProvider(t, e, "model-a", "a:q4")

	if _, ok := p.warmTarget(context.Background()); !ok {
		t.Fatal("precondition: the target must resolve with no pull running")
	}

	p.pullMu.Lock()
	p.pullsInFlight = map[string]*pullJob{"model-b": {}}
	p.pullMu.Unlock()

	if _, ok := p.warmTarget(context.Background()); ok {
		t.Error("warmed while another model was downloading")
	}
}

// A parked engine is an operator's explicit "free my memory now"; the
// warm-up must not be the thing that quietly refills it.
func TestWarmTarget_DeclinesWhileParked(t *testing.T) {
	e := &warmEngine{}
	p := warmProvider(t, e, "model-a", "a:q4")

	if err := p.ollama.Park(context.Background()); err != nil {
		t.Fatalf("Park: %v", err)
	}
	if _, ok := p.warmTarget(context.Background()); ok {
		t.Error("warmed a parked engine, re-allocating memory the operator freed")
	}
}

// Nothing active means a fresh install whose model is still downloading.
// There is no tag to load, and inventing one (the first tag the engine
// happens to have) would warm a model the router is not pointing at.
func TestWarmTarget_DeclinesWithNoActiveModel(t *testing.T) {
	e := &warmEngine{}
	p := warmProvider(t, e, "", "")

	if _, ok := p.warmTarget(context.Background()); ok {
		t.Error("resolved a warm target with no active selection")
	}
}

// Single-flight: the load is minutes on a cold multi-GB model and the
// engine serves one at a time, so a trigger arriving mid-load must drop
// rather than queue. Records today's behaviour AND the contract — the
// call sites are liberal precisely because re-entry is cheap.
func TestWarmServingModel_IsSingleFlight(t *testing.T) {
	e := &warmEngine{}
	p := warmProvider(t, e, "model-a", "a:q4")

	p.warmInFlight.Store(true) // a load is already running
	p.warmServingModel()

	if got := e.recorded(); len(got) != 0 {
		t.Fatalf("a second warm-up stacked on one already in flight: %+v", got)
	}
	if !p.warmInFlight.Load() {
		t.Error("the dropped call cleared the in-flight flag it did not set")
	}
}
