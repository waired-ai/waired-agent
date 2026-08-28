package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/management"
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

// errInferenceOff means local inference is turned off on this device.
// Like errEngineNotInstalled it is not a failure and leaves the latch
// open: the state is a setting, and the setting can change without a
// restart (#465).
var errInferenceOff = fmt.Errorf("%w: local inference is off on this computer, so the engine was not started — turn it on with `waired inference on`", management.ErrEngineStartRefused)

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

// decideEngineStartNow resolves decideEngineStart's inputs against this
// host, right now, and returns the answer. The rule itself lives in an
// untagged file so it stays table-testable; this is only the gathering.
func (p *agentInferenceProvider) decideEngineStartNow(ctx context.Context) string {
	current := p.servingEngine()
	reChosen, reChoiceOK := "", false
	if p.engineChoice != nil {
		reChosen, reChoiceOK = p.engineChoice(ctx)
	}
	return decideEngineStart(current, reChosen, reChoiceOK, p.engineIsUp(ctx))
}

// engineIsUp reports whether the engine this process is currently set to
// serve has a process up. StateStarting counts alongside StateReady for
// the same reason decideVLLMBootstrap counts it: an engine mid-startup
// already owns its port, and a vLLM load is minutes on a multi-GB model.
func (p *agentInferenceProvider) engineIsUp(ctx context.Context) bool {
	var a infruntime.Adapter
	if p.servingEngine() == catalog.RuntimeVLLM {
		a = p.vllmAdapter()
	} else if p.ollama != nil {
		// Assigned inside the nil check, not around it: a nil
		// *OllamaAdapter stored in the interface would compare non-nil.
		a = p.ollama
	}
	if a == nil {
		return false
	}
	switch a.Health(ctx).State {
	case infruntime.StateReady, infruntime.StateStarting:
		return true
	}
	return false
}

// adoptEngine records a change of serving engine. A no-op when the answer
// is the engine already set, which is every call on a converged host.
//
// The stale-ActiveSelection clear mirrors the boot-time engine switch in
// startInferenceSubsystem: the previous engine's model is not something
// the new engine can serve, and leaving it recorded would have the
// bootstrap try. Clearing it lets activateBundledIfUnset — which only
// fills an EMPTY slot — commit the new engine's model instead.
//
// Only reachable with nothing serving: decideEngineStart returns the
// current engine whenever one is up, and chooseEngine answers "persisted"
// (i.e. no change) whenever the recorded Active is still viable. So this
// never discards a selection something is running on.
func (p *agentInferenceProvider) adoptEngine(engine, reason string) {
	if engine == "" || engine == p.servingEngine() {
		return
	}
	was := p.servingEngine()
	p.setServingEngine(engine)
	if p.logger != nil {
		p.logger.Info("adopting a different serving engine", "was", was, "now", engine, "reason", reason)
	}
	if p.store == nil {
		return
	}
	if err := p.store.Update(func(s *catalog.State) {
		if s.Active != nil && s.Active.Runtime == was {
			s.Active = nil
		}
	}); err != nil && p.logger != nil {
		p.logger.Warn("engine adopt: clearing the previous engine's active selection failed", "err", err)
	}
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

	if err := p.startEngineAndBootstrap(ctx, reason); err != nil {
		p.logOnChange(&p.lastStartDecline, "engine start did not complete",
			err.Error(), "trigger", reason)
	}
}

// logOnChange writes msg at Info the first time detail is seen in slot, and
// again whenever detail changes — never on a repeat. Callers pace their own
// slot (agentInferenceProvider.lastReChoice / lastStartDecline).
//
// Info rather than Debug, and deduped rather than rate-limited: the thing
// worth seeing is that a trigger fired and what it decided, which is a
// handful of lines per boot on a converging host and exactly one line on a
// host that is stuck. #778 is what a stuck host looks like when those lines
// are Debug — indistinguishable from a trigger that never fired.
func (p *agentInferenceProvider) logOnChange(slot *atomic.Pointer[string], msg, detail string, args ...any) {
	if p == nil || p.logger == nil || slot == nil {
		return
	}
	if prev := slot.Load(); prev != nil && *prev == detail {
		return
	}
	slot.Store(&detail)
	p.logger.Info(msg, append([]any{"detail", detail}, args...)...)
}

// startEngineAndBootstrap brings the serving engine up and runs the
// post-start bootstrap. It is the former boot goroutine body, made
// re-entrant so an engine installed after boot is adopted without a
// service restart (#304 for the ollama binary, #339 for a vLLM venv).
//
// Both engines enter here since #339. They then diverge completely — vLLM
// needs the weights on disk before it can start and has no post-start tail
// at all, whereas `ollama serve` starts model-agnostic and pays the tail
// below — so the vLLM arm returns rather than falling through.
//
// Two latches on the ollama arm, on purpose:
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
	if p == nil {
		return errEngineNotInstalled
	}
	// Local inference turned off is a runtime state now, not an unbuilt
	// subsystem (#465): the management routes, the onboarding capability
	// and the tray's inference group all have to survive it, so the
	// engine is where "off" has to bite. Checked before the latch below,
	// so turning it back on still runs the tail.
	//
	// It now covers vLLM too. The old boot goroutine forked to
	// bootstrapVLLM before reaching any of this, so a device with local
	// inference off still spawned a multi-gigabyte engine — the one thing
	// #465/#507 turning it off is supposed to prevent.
	if p.isInferenceDisabled != nil && p.isInferenceDisabled() {
		return errInferenceOff
	}

	// Which engine, asked live rather than read off the boot decision
	// (#339). FIRST, before anything ollama-specific: the checks below
	// are that engine's preconditions, and a vLLM host has to reach its
	// own arm without being measured against them.
	switch action := p.decideEngineStartNow(ctx); action {
	case engineStartVLLM:
		p.adoptEngine(catalog.RuntimeVLLM, reason)
		// The same mutex the ollama arm takes: bootstrapVLLM stops and
		// spawns a subprocess, and nothing else may be bouncing an engine
		// while it does.
		p.engineOpMu.Lock()
		defer p.engineOpMu.Unlock()
		// Safe to call repeatedly (#337/#510): a live engine is left
		// alone, a dead one is stopped before this call spawns over it.
		// That idempotency is what allows this path to be re-entrant at
		// all, which is the whole of #339.
		p.bootstrapVLLM(ctx)
		return nil
	case engineStartOllama:
		p.adoptEngine(catalog.RuntimeOllama, reason)
	default:
		// engineStartNone, and any engine kind added later that reaches
		// here without an arm of its own. Refusing is the safe answer for
		// both, and it is not a failure: the caller's latch stays open so
		// the next trigger asks again.
		return errEngineNotInstalled
	}

	if p.ollama == nil {
		return errEngineNotInstalled
	}
	// AllowPull is deliberately NOT consulted here (#338). It means "do
	// not download model weights", and gating the START on it — the
	// pre-#304 boot goroutine's `binary == "" || !cfg.AllowPull`, carried
	// over verbatim by the #304 rewrite — left a host whose weights were
	// already on disk unable to serve them at all. Nothing reported it
	// either: hasUsableEngine reads the BINARY, not the process, so
	// subsystemState saw a usable engine and fell through to
	// awaiting_model while `ollama serve` was not running. The refusal
	// lives on the pull dispatchers instead (bootstrapPreferredModel,
	// bundledPrePullTarget, maybePreCache), with PullModel refusing every
	// caller as the backstop. The switch that stops an engine is the one
	// directly above (#465); `waired inference engine stop` is the other.
	//
	// vLLM never had the gate at all — bootstrapVLLM refuses only the
	// weights DOWNLOAD (inference_vllm_linux.go) — so #338 made the two
	// engines agree rather than giving vLLM something new.
	//
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

	// A fresh run of the loop supersedes whatever the last one ended on:
	// while these attempts are in flight the honest answer is "still
	// trying", not the previous verdict (waired-agent#1093).
	p.clearEngineStartExhausted()

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
		// The diagnosis, on the state rather than only in a log line the
		// desktop never shows (waired-agent#1069). The vLLM bootstrap has
		// done this since #1026; this is the ollama half. It matters in
		// exactly the window where nothing else speaks: these attempts can
		// all fail without exceeding the recovery budget, so no latch is
		// set and no give-up message is composed.
		hint := p.engineFailureDiagnosis(catalog.RuntimeOllama, ensureErr.Error())
		if p.logger != nil {
			p.logger.Error("ollama did not become ready after retries; local inference unavailable until restart",
				"attempts", engineEnsureAttempts, "err", ensureErr, "hint", hint)
		}
		p.ollama.SetStartFailureReason(hint)
		// And on the provider, where a reader that must not be fooled by
		// a transient probe can find it. The wizard's engine row asks only
		// about a give-up, and this loop gives up without ever latching:
		// it spends three strikes against a budget of four, so the row
		// reported the engine install DONE over an engine that could not
		// start (waired-agent#1093). Read back rather than recomposed so
		// the row and runtimes[].last_error quote the same bytes.
		p.noteEngineStartExhausted(p.ollama.Health(ctx).LastErr)
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
	// #320: load the serving model so the first request does not. Deferred
	// rather than called at the end so it survives an early return being
	// added back — the case it was written for was an untuned boot plan
	// (`!p.bootPlan.tuned`, i.e. the fresh install), which used to return
	// before finalizeOllamaServeTuning, whose /api/ps side effect was the
	// only thing warming anything. When this boot dispatched a pull
	// instead, the model is not on disk yet and the warm declines;
	// endPull's reconcile picks it up when the download lands.
	defer p.warmServingModel()
	if p.ollama.Mode() == infruntime.EngineModeAdopted && p.logger != nil {
		p.logger.Info("adopted orphan bundled ollama (exact pin match)",
			"version", p.ollama.EngineVersion())
	}
	// EVERY restart this tail performs happens FIRST, before a single byte
	// is asked for (#359). `ollama pull` is a client of `ollama serve`, so
	// the two steps below — which stop and restart the engine — used to
	// kill the downloads this same function had just dispatched, and the
	// job recorded the model `failed` for a reason that had nothing to do
	// with it. Two restarts are possible (a backend fallback and a tuning
	// degrade), so the boot tail could burn two of the job's three attempts
	// on its own; and the harm latched, because engineBootstrapOnce runs
	// this tail exactly once per process.
	//
	// Neither step depends on the dispatch: the probe reads /api/ps and
	// falls back to the first tag /api/tags already serves, and the verify
	// reads p.bootPlan, frozen at startInferenceSubsystem time. A pull
	// dispatched a few lines earlier had not finished either, so what they
	// observe is unchanged — only the order is.
	//
	// #290: for hosts with a fallback backend (Strix Halo Linux: ROCm
	// then Vulkan), verify the GPU actually engaged and switch to the
	// next backend if it didn't, so the host never runs on CPU silently
	// while a working GPU path exists. Conservative: an inconclusive
	// probe keeps the preferred backend.
	//
	// No longer gated on Probes() (#70). A host with only one GPU backend
	// cannot be moved to a better one, but it can still be MISLABELLED —
	// a detected GPU that fails to engage kept reporting cuda / vulkan /
	// metal while inference ran on the CPU. resolveBackendWithProbe now
	// decides for itself what a plan's verdict may change: a restart
	// where there is a fallback, the label alone where there is not.
	// "" means the probe declined to decide (a provider with no boot
	// plan); the seed from startInferenceSubsystem stands rather than
	// being cleared.
	if resolved := resolveBackendWithProbe(ctx, p.ollama, p.bootPlan.backend, p.ollama.BaseURL(), &http.Client{}, p.logger); resolved != "" {
		p.ollama.SetResolvedBackend(resolved)
	}
	// #621: verify the exported serve tuning against the running engine
	// and degrade once on positive evidence (silent f16 fallback /
	// spill). Ordered after the backend probe so the two never interleave
	// restarts. An untuned boot plan (`!p.bootPlan.tuned`) is the
	// fresh-install case and has nothing to verify.
	//
	// A CONDITION, not the early return it used to be: the model dispatch
	// moved below it, and a `return` here would skip the download this
	// whole function exists to start.
	if p.bootPlan.tuned {
		// #621 post-spawn tuning
		// verification, shared with the in-process reconcile (#812).
		p.finalizeOllamaServeTuning(ctx, p.bootPlan.tune,
			p.bootPlan.tuneManifest, p.bootPlan.tuneVariant, p.bootPlan.tuneTag)
	}
	// How fast one coding-agent turn is on this host (#496), taken here
	// because this is the only line every install path reaches with a
	// serving engine. It used to be taken by the two paths that needed it
	// — the bundled pre-pull and the model apply — and the browser wizard
	// reaches neither before the operator has to choose, so the majority
	// install path had no figure at the moment it mattered (waired#1099).
	//
	// BELOW the two steps above, which restart the engine, and above the
	// dispatch, which does not: a measurement is a request to the engine,
	// so a restart underneath it discards three minutes of work. #359
	// records the same ordering for the downloads below.
	//
	// Returns immediately; the work is on pullsWG.
	p.startHostSpeedMeasurement(ctx)
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
	} else {
		// Finish a preferred-model switch interrupted by its own restart
		// (issue #347) is bootstrapPreferredModel's job above; this is the
		// fresh-install pre-pull of the hardware auto-select.
		//
		// The decision is taken here and now — including committing weights
		// that are already on disk as Active — but the DOWNLOAD is held
		// until setup has had its say, so a host being set up from a browser
		// does not fetch the fallback alongside the model the operator is in
		// the middle of choosing (#379).
		//
		// UNCONDITIONAL, where this used to read `else if cfg.PullOnStartup`
		// (#526). Committing weights is the target's first job and the
		// download only its second, so that gate suppressed both: a host the
		// install-time selector had told not to download — which is what
		// applyBundledSelection writes on the disk-short verdict, i.e. the
		// host most likely to be reusing weights it already has — never
		// committed the ones sitting right there. The pull_on_startup
		// refusal moved down beside allow_pull's, inside the target and
		// below the activation, where both read as what they are: reasons
		// not to DOWNLOAD.
		if modelID, ok := p.bundledPrePullTarget(ctx); ok {
			p.holdBundledPrePull(ctx, modelID)
		}
	}
}
