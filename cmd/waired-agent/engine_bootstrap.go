package main

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/platform/proclist"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// engineBootstrapPlan carries the boot-computed inputs the engine-start
// sequence needs and the provider cannot re-derive. Frozen at
// startInferenceSubsystem time.
//
// The engine BINARY is deliberately not in here. Capturing it once at
// boot is what made an engine installed afterwards invisible for the
// life of the process (#304); startEngineAndBootstrap asks
// p.ollamaUsable — the live, state-dir-aware resolver — instead.
//
// tuned is false whenever the BOOT-TIME engine decision was not ollama,
// which is exactly the fresh-install case. Those spawns get their tuning
// from the adapter's spawn-time fallback resolver (#624) instead, which
// re-reads state on every spawn.
type engineBootstrapPlan struct {
	backend      infruntime.BackendPlan
	tuned        bool
	tune         ollamaTuning
	tuneTag      string
	tuneManifest catalog.Manifest
	tuneVariant  catalog.Variant
}

// errEngineNotInstalled means there is no engine binary to start yet.
// Not a failure: the caller leaves its latch open so a later trigger
// (an executor reporting the install done, the setup reconciler seeing
// the binary appear) retries.
var errEngineNotInstalled = errors.New("inference: no engine binary installed yet")

// engineEnsureAttempts / engineEnsureBackoff pace the start retry.
// EnsureRunning already waits a full cold-start budget
// (StartupReadyTimeout) per attempt; the retry is insurance against a
// transient spawn failure or a first start that exceeds even that
// budget, so local inference recovers on its own without an agent
// restart.
const engineEnsureAttempts = 3

var engineEnsureBackoff = 10 * time.Second

// requestEngineStart asks, from anywhere, that the engine be started and
// the post-start bootstrap run. Fire-and-forget and coalesced: a second
// caller while one is in flight returns immediately, because both carry
// the same intent (unlike requestEngineReconcile's swapPending, which
// exists to hand a NEW model to the running reconcile).
//
// Always dispatched on p.agentCtx. Every trigger's own context — an HTTP
// request handler, a network-map frame — dies long before a cold start
// finishes.
func (p *agentInferenceProvider) requestEngineStart(reason string) {
	if p == nil {
		return
	}
	ctx := p.agentCtx
	if ctx == nil {
		ctx = context.Background()
	}
	go p.runEngineBootstrap(ctx, reason)
}

// runEngineBootstrap is the guarded entry point. It owns the in-flight
// CAS; startEngineAndBootstrap owns the work.
func (p *agentInferenceProvider) runEngineBootstrap(ctx context.Context, reason string) {
	if p == nil {
		return
	}
	if !p.engineStartInFlight.CompareAndSwap(false, true) {
		return // a start is already running; it does exactly what this caller wants
	}
	defer p.engineStartInFlight.Store(false)

	if err := p.startEngineAndBootstrap(ctx, reason); err != nil && p.logger != nil {
		p.logger.Debug("engine start did not complete", "reason", reason, "err", err)
	}
}

// startEngineAndBootstrap brings the ollama engine up and runs the
// post-start bootstrap. It is the former boot goroutine body, made
// re-entrant so an engine installed after boot is adopted without a
// service restart (#304).
//
// Two latches, on purpose:
//
//   - the engine START may run again on any later trigger. EnsureRunning
//     is single-flight and re-resolves the binary on every call, so a
//     repeat is cheap and is what adopts a late install.
//   - the BOOTSTRAP TAIL (bundled/preferred model, backend probe, tuning
//     verify) runs at most once per process, latched by
//     engineBootstrapOnce. The pulls it dispatches are deduped now
//     (#305), but the probe and the tuning verify both STOP AND RESTART
//     the engine, so re-running the tail on every trigger would bounce a
//     serving engine — and fail any download in flight against it.
func (p *agentInferenceProvider) startEngineAndBootstrap(ctx context.Context, reason string) error {
	if p == nil || p.ollama == nil {
		return errEngineNotInstalled
	}
	cfg := p.effectiveCfg()
	if !cfg.AllowPull {
		return errPullsDisabled
	}
	// The live resolver, not a boot-time snapshot: this is the check that
	// lets a binary installed after boot be seen at all (#304).
	if p.ollamaUsable == nil || !p.ollamaUsable() {
		return errEngineNotInstalled
	}

	// Serialise against the other owner of engine restarts
	// (reconcileEngineServe). Both the backend probe and the tuning
	// verify below stop and restart the engine; before #304 this ran only
	// in the first seconds of process life, so the overlap never came up.
	p.engineOpMu.Lock()
	defer p.engineOpMu.Unlock()

	if p.logger != nil {
		p.logger.Debug("starting engine", "reason", reason)
	}

	var ensureErr error
	for attempt := 1; attempt <= engineEnsureAttempts; attempt++ {
		if ensureErr = p.ollama.EnsureRunning(ctx); ensureErr == nil {
			break
		}
		// A parked engine is the operator's `waired inference engine
		// stop`, and a given-up engine is waired-agent#29's crash latch.
		// Neither clears on its own and neither should be cleared from
		// here — the documented reset is an explicit `waired inference
		// engine start`. Retrying them would only burn the backoff, and
		// clearing the give-up latch from a repeating trigger is how the
		// macOS respawn storm in #310 gets built.
		if errors.Is(ensureErr, infruntime.ErrEngineParked) ||
			errors.Is(ensureErr, infruntime.ErrEngineUnrecoverable) {
			if p.logger != nil {
				p.logger.Debug("engine start refused by a latch; leaving it set",
					"reason", reason, "err", ensureErr)
			}
			return ensureErr
		}
		if p.logger != nil {
			p.logger.Warn("ollama EnsureRunning failed",
				"attempt", attempt, "max", engineEnsureAttempts, "err", ensureErr)
		}
		if attempt == engineEnsureAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt) * engineEnsureBackoff):
		}
	}
	if ensureErr != nil {
		if p.logger != nil {
			p.logger.Error("ollama did not become ready after retries; local inference unavailable until restart",
				"attempts", engineEnsureAttempts, "err", ensureErr)
		}
		return ensureErr
	}

	if !p.engineBootstrapOnce.CompareAndSwap(false, true) {
		return nil // engine is up; the tail already ran this process
	}
	p.bootstrapAfterEngineStart(ctx)
	return nil
}

// bootstrapAfterEngineStart is everything the boot goroutine did once the
// engine was serving. Runs at most once per process, under engineOpMu.
func (p *agentInferenceProvider) bootstrapAfterEngineStart(ctx context.Context) {
	cfg := p.effectiveCfg()
	if p.ollama.Mode() == infruntime.EngineModeAdopted && p.logger != nil {
		p.logger.Info("adopted orphan bundled ollama (exact pin match)",
			"version", p.ollama.EngineVersion())
	}
	// The operator's model comes FIRST, and the hardware auto-select is
	// only the fallback for a host with no operator model to serve (#306).
	// Both used to be dispatched here back to back, and #305's registry is
	// keyed by model_id alone, so two DIFFERENT ids never deduped: rc7
	// downloaded a 9 GB model the daemon picked for itself alongside the
	// 44 GB one chosen in the wizard, and on a 16 GB CI runner the pair
	// reached the OOM killer and took the engine down.
	//
	// Ordering rather than a "does a preference exist?" gate, deliberately:
	// three bundled manifests ship with no ollama-servable variant at all,
	// so a preference that merely resolves in the catalog is not proof that
	// anything will be downloaded — and engineBootstrapOnce latches this
	// tail exactly once, so a wrongly suppressed fallback never re-arms.
	// bootstrapPreferredModel reports whether it actually took the model on.
	if p.bootstrapPreferredModel(ctx) {
		// Its weights may still be on disk from an earlier run, and this is
		// the only caller of activateBundledIfUnset on the boot path.
		// Skipping it wholesale would leave Active nil for the hours the
		// chosen model downloads — EngineReady() false, benchmarks 425ing,
		// Status() reporting awaiting_model — on a host with a working
		// model already on disk.
		p.activateBundledIfReady(ctx)
	} else if cfg.PullOnStartup {
		// Finish a preferred-model switch interrupted by its own restart
		// (issue #347) is bootstrapPreferredModel's job above; this is the
		// fresh-install pre-pull of the hardware auto-select.
		p.bootstrapBundledModel(ctx)
	}
	// #290: for hosts with a fallback backend (Strix Halo Linux: ROCm
	// then Vulkan), verify the GPU actually engaged and switch to the
	// next backend if it didn't, so the host never runs on CPU silently
	// while a working GPU path exists. Conservative: an inconclusive
	// probe keeps the preferred backend.
	if p.bootPlan.backend.Probes() {
		resolved := resolveBackendWithProbe(ctx, p.ollama, p.bootPlan.backend, p.ollama.BaseURL(), &http.Client{}, p.logger)
		p.ollama.SetResolvedBackend(resolved)
	}
	// #621: verify the exported serve tuning against the running engine
	// and degrade once on positive evidence (silent f16 fallback /
	// spill). Ordered after the backend probe so the two never interleave
	// restarts. Reuse mode is read-only — waired cannot restart a process
	// it doesn't own — so a mismatch only records a user-visible warning.
	if !p.bootPlan.tuned {
		return
	}
	if !p.ollama.Borrowed() {
		// #642 derived-batch-model creation + #621 post-spawn tuning
		// verification, shared with the in-process reconcile (#812).
		p.finalizeOllamaServeTuning(ctx, p.bootPlan.tune,
			p.bootPlan.tuneManifest, p.bootPlan.tuneVariant, p.bootPlan.tuneTag)
		return
	}
	hwProfile := p.profiler.Profile(ctx)
	verdict, detail := verifyOllamaTuning(ctx, &http.Client{}, p.ollama.BaseURL(), p.bootPlan.tune, p.bootPlan.tuneTag, hwProfile)
	mt := infruntime.ModelTuning{
		ModelID:   p.bootPlan.tune.ModelID,
		VariantID: p.bootPlan.tune.VariantID,
		Verified:  verdict != tuningInconclusive,
	}
	// #763: surface the reused engine's real request parallelism when its
	// runner can be attributed to this model.
	if verdict != tuningInconclusive {
		if np, ok := observeRunnerParallel(p.bootPlan.tune, proclist.List); ok {
			mt.ObservedNumParallel = np
		}
	}
	if verdict != tuningInconclusive && (detail != "" || verdict != tuningOK) {
		mt.Warning = "reused ollama is not tuned by waired (" + detail +
			"); consider setting OLLAMA_CONTEXT_LENGTH / OLLAMA_KV_CACHE_TYPE on your ollama service"
		if p.logger != nil {
			p.logger.Warn("reused ollama tuning check", "detail", detail)
		}
	}
	p.ollama.SetAppliedTuning(mt)
}
