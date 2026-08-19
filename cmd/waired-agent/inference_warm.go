package main

import (
	"context"
	"net/http"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

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
	// Detached from whatever asked for it: this outlives the reconcile or
	// the bootstrap that triggered it, and no caller should block on a
	// cold load. Cancelled with the agent, not with the request.
	wctx := p.backgroundCtx()
	go func() {
		defer p.warmInFlight.Store(false)
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
	// Read p.store, not engineModelForActive: that helper opens its own
	// store at the process-wide default path, which is right for the boot
	// benchmark (it runs before a provider exists) and wrong here, where
	// the provider's own store is the source of truth every other reader
	// on this path uses.
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
