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
	// A started engine nobody stops leaves superviseChild parked on a
	// child belonging to a finished test (waired-agent#925).
	stateDir, agentCtx, arm := providerLifetime(t)
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
		store:    catalog.NewStore(filepath.Join(stateDir, "state.json")),
		cfg:      agentconfig.InferenceConfig{},
		logger:   slog.New(slog.DiscardHandler),
		agentCtx: agentCtx,
	}
	arm(p)
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
	// Sent explicitly, AND in a spelling the engine accepts. This used to
	// assert infruntime.KeepAliveIndefinite — the environment variable's
	// grammar — so it certified as correct the one value a live engine
	// answers with 400, and every warm under the default setting failed
	// while this stayed green (waired-agent#927). A fake that accepts any
	// body cannot catch a wire grammar; asserting the property can.
	ka, ok := got[0]["keep_alive"].(string)
	if !ok {
		t.Fatalf("keep_alive = %v, want it sent explicitly — the serve-level "+
			"variable is not ours to trust on an adopted engine", got[0]["keep_alive"])
	}
	d, err := time.ParseDuration(ka)
	if err != nil {
		t.Errorf("keep_alive = %q, which the engine cannot parse: %v", ka, err)
	} else if d >= 0 {
		t.Errorf("keep_alive = %q (%v), want a negative duration for indefinite", ka, d)
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
	// Given back at the end: the fixture's lifetime reads this latch to
	// decide whether work is still in flight, and a borrowed one held
	// past the test looks exactly like a goroutine that never finished.
	defer p.warmInFlight.Store(false)
	p.warmServingModel()

	if got := e.recorded(); len(got) != 0 {
		t.Fatalf("a second warm-up stacked on one already in flight: %+v", got)
	}
	if !p.warmInFlight.Load() {
		t.Error("the dropped call cleared the in-flight flag it did not set")
	}
}

// waitForWarm waits for the detached warm-up goroutine to finish. The
// latch is the signal rather than a sleep: on Windows the clock has a
// 15.6 ms granularity, so a duration-based wait is a coin toss there and
// green everywhere I would run it by hand.
func waitForWarm(t *testing.T, p *agentInferenceProvider) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for p.warmInFlight.Load() {
		if time.Now().After(deadline) {
			t.Fatal("warm-up did not finish within 5s")
		}
		time.Sleep(time.Millisecond)
	}
}

// THE SAFETY VALVE for waired-agent#1307. PRODUCT CONTRACT: once "the
// weights are not in memory" means "do not send this node work",
// something has to put them back.
//
// Before this, four moments warmed — boot, a reconcile, an operator
// engine start, and the host-speed probe's own eviction — and residency
// lost at any other time was restored by the next real request. That
// was survivable while residency decided nothing. It is not survivable
// now: the request that used to do the reloading is precisely the
// request that would no longer be routed here. Without the valve the
// admission term is a one-way door.
func TestMaintainResidency_ReloadsWhatTheEngineLost(t *testing.T) {
	e := &warmEngine{}
	p := warmProvider(t, e, "model-a", "a:q4")
	// EnsureRunning above already recorded "spawned, therefore holding
	// nothing", which is the observation this valve acts on.
	if res := p.ollama.Residency(); !res.Observed || res.Resident() {
		t.Fatalf("fixture: want an observed-cold residency, got %+v", res)
	}

	p.maintainResidency()
	waitForWarm(t, p)

	got := e.recorded()
	if len(got) != 1 {
		t.Fatalf("warm-up loads = %d, want exactly 1: %+v", len(got), got)
	}
	if got[0]["model"] != "a:q4" {
		t.Errorf("reloaded %q, want the active model's tag a:q4", got[0]["model"])
	}
}

// nil is "we have not looked", never "cold"
// (docs/decisions/20260820/0130). Acting on it would load a model on the
// strength of a probe that has not run — and on a host whose engine is
// unreachable, that is a load attempt every probe tick forever.
func TestMaintainResidency_DeclinesOnAnUnobservedResidency(t *testing.T) {
	e := &warmEngine{}
	p := warmProvider(t, e, "model-a", "a:q4")
	p.ollama.SetResidency(infruntime.ModelResidency{}) // never looked

	p.maintainResidency()
	waitForWarm(t, p)

	if got := e.recorded(); len(got) != 0 {
		t.Errorf("loaded on an unobserved residency: %+v", got)
	}
}

// Already resident is the steady state and this runs on the 5 s probe
// tick, so the common path must cost nothing.
func TestMaintainResidency_DeclinesWhenAlreadyResident(t *testing.T) {
	e := &warmEngine{resident: []string{"a:q4"}}
	p := warmProvider(t, e, "model-a", "a:q4")
	p.ollama.SetResidency(infruntime.ModelResidency{Observed: true, Model: "a:q4"})

	p.maintainResidency()
	waitForWarm(t, p)

	if got := e.recorded(); len(got) != 0 {
		t.Errorf("reloaded a model that is already in memory: %+v", got)
	}
}

// A load that keeps failing — a broken engine, a model the runner will
// not accept — must not be restarted every probe tick for the life of
// the process. The pace is measured from when the last attempt ENDED,
// so a slow load is not counted against the next one.
func TestMaintainResidency_PacesRetriesAfterAFailure(t *testing.T) {
	e := &warmEngine{}
	p := warmProvider(t, e, "model-a", "a:q4")

	p.warmEndedAt.Store(time.Now().Add(-residencyWarmRetry / 2).UnixNano())

	p.maintainResidency()
	waitForWarm(t, p)
	if got := e.recorded(); len(got) != 0 {
		t.Fatalf("retried inside the pacing window: %+v", got)
	}

	// Past the window, the valve opens again.
	p.warmEndedAt.Store(time.Now().Add(-residencyWarmRetry - time.Minute).UnixNano())
	p.maintainResidency()
	waitForWarm(t, p)
	if got := e.recorded(); len(got) != 1 {
		t.Errorf("warm-up loads after the window = %d, want 1: %+v", len(got), got)
	}
}

// ModelLoading is what /healthz and `waired status` read to say "this
// node is mid-load" rather than "ready". Seconds are derived from an
// injected clock: two real readings would be a coin toss on Windows,
// whose clock moves in 15.6 ms steps.
func TestModelLoading_ReportsTheLatchAndTheElapsedSeconds(t *testing.T) {
	p := &agentInferenceProvider{}
	if loading, secs := p.ModelLoading(); loading || secs != 0 {
		t.Errorf("idle provider reported loading=%v secs=%d", loading, secs)
	}

	// Relative to the real clock, not an injected one: the warm runs on a
	// detached goroutine, so a test-written clock field would be a data
	// race. The half-second of slack keeps the truncation off a boundary
	// on a host whose clock moves in 15.6 ms steps.
	p.warmInFlight.Store(true)
	p.warmStartedAt.Store(time.Now().Add(-17500 * time.Millisecond).UnixNano())

	loading, secs := p.ModelLoading()
	if !loading {
		t.Error("loading = false while the warm latch is held")
	}
	if secs != 17 {
		t.Errorf("elapsed = %d s, want 17", secs)
	}

	// A stamp in the future must read as 0, not as a negative age.
	p.warmStartedAt.Store(time.Now().Add(time.Minute).UnixNano())
	if _, secs := p.ModelLoading(); secs != 0 {
		t.Errorf("elapsed = %d s for a stamp in the future, want 0", secs)
	}
}
