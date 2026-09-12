package main

import (
	"context"
	"net/http"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// residencyWarmRetry paces the residency maintainer. The warm-up is
// best-effort and may fail — a broken engine, a model the runner will
// not load — and the maintainer runs on the 5 s probe tick, so without
// a pace a permanently failing load becomes a permanent retry storm
// against the engine everything else is trying to talk to.
//
// A minute is long enough that a failing host is left alone and short
// enough that a host which lost its weights for a recoverable reason
// (an engine bounce, a probe eviction) is warm again well inside a
// human's idea of "just now".
const residencyWarmRetry = time.Minute

// warmBudget bounds one warm-up attempt. Generous on purpose: the whole
// point is the cold load of a multi-GB model, which is minutes on a slow
// disk. It matches probeLoadTimeout — the same request, sent for a
// different reason — with room for the /api/ps look first.
const warmBudget = 4 * time.Minute

// warmServingModel loads the active model into (V)RAM in the background
// so the first real request does not pay for it.
//
// The cold load is the single largest term in first-request TTFT: on the
// host waired-agent#320 was reported from it was a 22.7 GB model, and a
// coding agent's opening prompt landed on an engine that had not touched
// the weights yet. Nothing preloaded them. `OLLAMA_KEEP_ALIVE=60m`
// prevents REPEAT unloads, never the first load, and the boot benchmark
// — the only thing that warmed anything — returns early on a cache hit
// and short-circuits entirely when no model is active, which is exactly
// the fresh-install case.
//
// Until now the only load outside a real request was a side effect of
// verifyOllamaTuning, which loads solely to make /api/ps meaningful and
// skips it whenever a model — any model, including the wrong one — is
// already resident. Boot spawns on an untuned plan, adopts, unparks and
// no-op reconciles all miss it.
//
// Best-effort by construction: every failure is logged and swallowed.
// Nothing downstream may depend on the model being warm, because a
// request that arrives first will simply load it the slow way, exactly
// as before.
func (p *agentInferenceProvider) warmServingModel() {
	if p == nil || p.ollama == nil {
		return
	}
	// Single-flight. A boot that also reconciles, or two reconciles in
	// quick succession, would otherwise stack multi-minute loads on an
	// engine that can only serve one at a time.
	if !p.warmInFlight.CompareAndSwap(false, true) {
		return
	}
	p.warmStartedAt.Store(p.nowForWarm().UnixNano())
	// Detached from whatever asked for it: this outlives the reconcile or
	// the bootstrap that triggered it, and no caller should block on a
	// cold load. Cancelled with the agent, not with the request.
	wctx := p.backgroundCtx()
	go func() {
		defer func() {
			p.warmEndedAt.Store(p.nowForWarm().UnixNano())
			p.warmStartedAt.Store(0)
			p.warmInFlight.Store(false)
		}()
		p.warmServingModelNow(wctx)
	}()
}

// warmServingModelNow is the synchronous body, factored out so tests can
// drive it without a goroutine.
func (p *agentInferenceProvider) warmServingModelNow(ctx context.Context) {
	tag, ok := p.warmTarget(ctx)
	if !ok {
		return
	}
	client := &http.Client{}
	baseURL := p.ollama.BaseURL()
	// Already resident? /api/ps is cheap and the load is not. This is also
	// what makes the call sites free to be liberal: the steady state is a
	// 10 ms probe.
	var ps psResponse
	if err := getJSON(ctx, client, baseURL+"/api/ps", probeHTTPTimeout, &ps); err == nil {
		for _, m := range ps.Models {
			if m.Name == tag {
				return
			}
		}
	}
	// Not while this host is being measured. A warm-up is a multi-GB load
	// like any other, and under infruntime.MaxResidentModels loading it
	// evicts the probe — the boot path fires this and the host-speed
	// measurement from the same bootstrap tail, so they raced by
	// construction (waired-agent#703).
	//
	// Skipped rather than queued, because a skip costs nothing here:
	// ensureHostSpeedMeasured calls this again on its way out, after
	// releasing the claim, precisely so the model it evicted comes back
	// (waired-agent#320). Everything else that warms is a reconcile or a
	// bootstrap that will warm again.
	release, ok := p.claimEngineExclusive()
	if !ok {
		p.logger.Debug("serving model warm-up skipped: this host is being measured", "model", tag)
		return
	}
	defer release()

	wctx, cancel := context.WithTimeout(ctx, warmBudget)
	defer cancel()
	start := time.Now()
	// Send keep_alive explicitly rather than relying on the serve-level
	// variable: an ADOPTED engine was spawned by a previous run and its
	// environment is not ours, so a warm that trusted OLLAMA_KEEP_ALIVE
	// would be undone minutes later on the very hosts that cannot be
	// bounced to fix it.
	if err := loadOllamaModel(wctx, client, baseURL, tag, p.keepAlive()); err != nil {
		if p.logger != nil {
			p.logger.Info("warm-up load did not complete; the first request will pay for it",
				"model", tag, "err", err, "after", time.Since(start).Round(time.Second))
		}
		return
	}
	if p.logger != nil {
		p.logger.Info("serving model warmed",
			"model", tag, "took", time.Since(start).Round(time.Second))
	}
}

// warmTarget reports the engine-native tag to warm, and whether warming
// is appropriate at all right now.
//
// Split out from the load so the whole decision is one testable function
// rather than a chain of early returns around an HTTP call.
func (p *agentInferenceProvider) warmTarget(ctx context.Context) (string, bool) {
	// Only ollama. vLLM's own startup loads the weights before it reports
	// ready, so there is nothing to warm there.
	if p.servingEngine() != catalog.RuntimeOllama {
		return "", false
	}
	// A pull holds the disk and, on a single-GPU host, the memory the load
	// wants. Warming into that contention is how a competing download and
	// a model load take each other down, so defer: endPull fires a
	// reconcile when the last pull finishes, and that reconcile warms.
	p.pullMu.Lock()
	pulling := len(p.pullsInFlight) > 0
	p.pullMu.Unlock()
	if pulling {
		return "", false
	}
	if p.ollama.IsParked() {
		return "", false
	}
	if p.ollama.Health(ctx).State != infruntime.StateReady {
		return "", false
	}
	// Warm what will actually serve — Active's tag — not the tuning
	// target. The two can differ mid-switch, and loading the model the
	// router is not pointing at would evict the one it is.
	//
	// Read p.store. This used to have to say so, because the benchmark's
	// engineModelForActive opened its own store at the process-wide default
	// path; every reader of the Active selection is on the provider's store
	// now (waired-agent#1206), so there is one source of truth to be on.
	st, err := p.store.Load()
	if err != nil || st.Active == nil || st.Active.Runtime != catalog.RuntimeOllama {
		return "", false
	}
	ms, ok := st.Models[st.Active.ModelID]
	if !ok || ms.State != catalog.ModelStateReady {
		return "", false // weights not on disk: nothing to load
	}
	if ms.OllamaTag == "" {
		return "", false // no engine-native name to ask for
	}
	return ms.OllamaTag, true
}

// nowForWarm is p.now with the production default, so the elapsed figure
// is injectable in a test without reaching for the wall clock. Windows'
// clock has a 15.6 ms granularity, so nothing here may derive an ORDER
// from two readings — only a duration, which is what the surfaces show.
func (p *agentInferenceProvider) nowForWarm() time.Time {
	if p != nil && p.now != nil {
		return p.now()
	}
	return time.Now()
}

// ModelLoading is inference.Config.ModelLoadingFn: is a load of the
// weights into memory in flight, and for how many seconds.
//
// The warm-up is the only thing in the product that loads weights
// outside a request (see the doc on warmServingModel), so its latch is
// the whole answer. A load driven by a request needs no signal here —
// the requester is already waiting on it and knows.
//
// Seconds rather than a finer unit because it feeds a human-facing line
// and a peer's decision to look elsewhere, and neither improves with
// milliseconds. Never negative: an injected clock that runs backwards
// reads as 0 rather than as a warm-up that started in the future.
func (p *agentInferenceProvider) ModelLoading() (bool, int64) {
	if p == nil || !p.warmInFlight.Load() {
		return false, 0
	}
	started := p.warmStartedAt.Load()
	if started == 0 {
		return true, 0
	}
	secs := int64(p.nowForWarm().Sub(time.Unix(0, started)).Seconds())
	if secs < 0 {
		secs = 0
	}
	return true, secs
}

// maintainResidency is the safety valve for making residency a term of
// readiness (waired-agent#1307).
//
// Once "the weights are not in memory" means "do not send this node
// work", the product needs something that puts them back. Before this,
// nothing did: the warm-up fired at four moments — boot, a reconcile, an
// operator engine start, and after the host-speed probe evicted the
// model — and if residency was lost at any other time, the next real
// request was what reloaded it. That was survivable while residency
// decided nothing. It is not survivable now, because the request that
// used to do the reloading is exactly the request that would no longer
// be routed here. Without this, the term would be a one-way door: a node
// that lost its weights once would never be admitted again.
//
// Liberal by construction, which is what warmServingModel's own doc
// licenses: it single-flights, declines while a pull holds the disk,
// declines while the host-speed measurement holds the engine, declines
// on a parked or unready engine, and returns in about ten milliseconds
// when the model is already there.
func (p *agentInferenceProvider) maintainResidency() {
	if p == nil || p.ollama == nil {
		return
	}
	// Only this engine has a residency to lose. A ready vLLM process
	// holds its weights for its whole life (see vllmResident), so there
	// is nothing here to put back.
	if p.servingEngine() != catalog.RuntimeOllama {
		return
	}
	if res := p.ollama.Residency(); !res.Observed || res.Resident() {
		// Not observed is not "cold": the ratified reading of an
		// unobserved residency is that we have not looked, and acting on
		// it would load a model on the strength of a probe that has not
		// run yet.
		return
	}
	if ended := p.warmEndedAt.Load(); ended != 0 &&
		p.nowForWarm().Sub(time.Unix(0, ended)) < residencyWarmRetry {
		return
	}
	p.warmServingModel()
}
