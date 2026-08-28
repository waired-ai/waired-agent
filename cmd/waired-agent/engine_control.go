package main

import (
	"context"
	"log/slog"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/management"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// engineController implements management.EngineController: the hard
// engine power axis (#186). It is orthogonal to inferenceController (the
// soft enable/disable gate, engine stays warm) and shareController — this
// one actually stops the engine process to free VRAM/RAM and latches it
// stopped so request traffic can't revive it.
//
// It acts on THIS HOST'S SERVING ENGINE, whichever that is (#881). It used
// to hold an *infruntime.OllamaAdapter, so on a vLLM host the routes still
// resolved and still parked the (idle) ollama adapter: `waired inference
// engine stop` reported success, the mesh advertisement flipped to
// "stopped" so peers stopped routing to the host — and vLLM kept the GPU.
// That is the worst combination available, and on vLLM stopping the process
// is the ONLY way to release the memory, because the pool is reserved at
// start-up and held to process exit.
//
// State is live-only (the ollama adapter's parked flag, the provider's vLLM
// latch); nothing is persisted, so a daemon restart returns to config-driven
// startup. That is deliberate: a hard stop is an operational "free my memory
// now" action, not a policy like the soft toggle's persisted
// desired-inference.
type engineController struct {
	// p is the live provider: which engine serves is a runtime answer, not a
	// boot-time one (adoptEngine can change it after an engine installed
	// after boot is taken on, #339), so the controller has to ask each time
	// rather than capture one adapter.
	p *agentInferenceProvider
	// agentCtx is the daemon's long-lived context. StartEngine spawns the
	// (blocking-until-ready) EnsureRunning against THIS ctx, never the
	// per-request HTTP context, which is cancelled the moment the
	// management handler returns.
	agentCtx context.Context
	logger   *slog.Logger
	// onEngineUp, when set, is called after an operator start brings the
	// engine back. It exists for the #320 warm-up: an unpark returns to a
	// process holding no weights, and nothing else on this path asks for
	// them. Set after construction (like restartOnWedge) so the several
	// test call sites need no new argument. nil is a no-op.
	//
	// The ollama arm only. It is warmServingModel, whose target already
	// declines on a non-ollama engine, and a vLLM engine comes back holding
	// its model by construction — the process cannot be ready without it.
	onEngineUp func()
}

func newEngineController(ctx context.Context, p *agentInferenceProvider, logger *slog.Logger) *engineController {
	return &engineController{p: p, agentCtx: ctx, logger: logger}
}

// StopEngine hard-stops the serving engine and latches it stopped.
// Synchronous — the caller learns the memory was actually freed before the
// HTTP response — but the caller's context bounds only how long IT waits,
// never how long the kill runs (#316). Dropping the request context here
// matters twice: a tray that gives up at 3s must not truncate the SIGTERM
// grace period on Unix, and it must never leave a live engine behind a
// latched "stopped" power state.
func (e *engineController) StopEngine(ctx context.Context) error {
	engine := e.p.servingEngine()
	if e.logger != nil {
		e.logger.Info("engine controller: hard stop requested", "engine", engine)
	}
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), management.EngineStopBudgetFor(engine))
	defer cancel()

	var err error
	if engine == catalog.RuntimeVLLM {
		err = e.stopVLLM(stopCtx)
	} else {
		err = e.p.ollama.Park(stopCtx)
	}
	if e.logger != nil {
		e.logger.Debug("engine controller: hard stop result", "engine", engine, "ok", err == nil)
	}
	return err
}

// stopVLLM latches first and stops second, the order ollama's Park uses —
// and rolls the latch back the same way when the stop fails. Claiming
// "stopped" for a process that may still be alive is the worst of both
// worlds: status lies AND the latch keeps the engine from being revived for
// local and peer traffic alike (#316).
//
// No adapter is a success, not an error: the bootstrap has not spawned
// anything (the venv is installing, or the weights are still downloading),
// so there is no memory to free — and the latch still has to stand, or the
// download would finish and start an engine the operator stopped.
func (e *engineController) stopVLLM(ctx context.Context) error {
	e.p.setVLLMParked(true)
	a := e.p.vllmAdapter()
	if a == nil {
		return nil
	}
	if err := a.Stop(ctx); err != nil {
		e.p.setVLLMParked(false)
		return err
	}
	return nil
}

// StartEngine clears the parked latch and restarts the engine
// asynchronously: bringing an engine up blocks until the readiness probe
// passes (seconds to tens of seconds on ollama, minutes on a multi-GB vLLM
// model), so blocking the HTTP handler on it would hang the tray/CLI. The
// status endpoint reflects "starting" → "ready" as the background work
// progresses.
func (e *engineController) StartEngine(_ context.Context) error {
	// The soft toggle is a persisted policy; this axis is a live operation
	// (#186). A live operation may not quietly overrule a persisted policy,
	// so both engines refuse and the refusal is reported (waired-agent#964).
	//
	// The two arms used to disagree, and both lied about it. The ollama arm
	// called EnsureRunning directly, so `waired inference engine start`
	// brought the engine up on a device somebody had turned local inference
	// off on. The vLLM arm went through requestEngineStart, which returns
	// errInferenceOff — inside a dispatched goroutine, where nobody sees it.
	// The CLI printed "engine start ok." for both.
	if e.p.isInferenceDisabled != nil && e.p.isInferenceDisabled() {
		return errInferenceOff
	}
	if e.p.servingEngine() == catalog.RuntimeVLLM {
		return e.startVLLM()
	}
	e.p.ollama.Unpark()
	// An explicit start is also the documented reset for a crash-recovery
	// give-up (waired-agent#29): the operator has presumably changed
	// something, so let the engine try again. No new endpoint or CLI verb —
	// this is already `waired inference engine start`.
	e.p.ollama.ClearFailure()
	// The provider half of the same reset (waired-agent#1110): the count
	// that decides the latch lives on the provider, and leaving it at the
	// boot path's three strikes meant this start got one attempt, not the
	// three the troubleshooting page promises.
	e.p.resetEngineStrikes()
	if e.logger != nil {
		e.logger.Info("engine controller: start requested", "engine", catalog.RuntimeOllama)
	}
	go func() {
		if err := e.p.ollama.EnsureRunning(e.agentCtx); err != nil {
			if e.logger != nil {
				e.logger.Warn("engine controller: start failed", "err", err)
			}
			return
		}
		if e.logger != nil {
			e.logger.Debug("engine controller: engine running", "mode", string(e.p.ollama.Mode()))
		}
		if e.logger != nil && e.p.ollama.Mode() == infruntime.EngineModeAdopted {
			e.logger.Info("engine controller: adopted orphan bundled ollama (exact pin match)",
				"version", e.p.ollama.EngineVersion())
		}
		if e.onEngineUp != nil {
			e.onEngineUp()
		}
	}()
	return nil
}

// startVLLM clears the latches and hands the start to requestEngineStart
// rather than calling EnsureRunning on the current adapter.
//
// That path is the only one that can build an adapter when there is none —
// the case the provider-side latch exists for — and it re-resolves the venv,
// the weights, tensor parallelism, the KV dtype, the --max-model-len clamp,
// the tool parser and the serve-flag gate, which is the vLLM analogue of the
// ollama arm's ClearFailure reset. It also takes engineOpMu, serialising
// against reconcileEngineServe, and it is already coalesced and dispatched
// on the daemon context.
func (e *engineController) startVLLM() error {
	e.p.setVLLMParked(false)
	// Symmetric with the ollama arm's ClearFailure: an explicit start is the
	// documented reset for the give-up latch (#946).
	if c, ok := e.p.vllmAdapter().(interface{ ClearFailure() }); ok {
		c.ClearFailure()
	}
	// Same provider-side reset the ollama arm does (waired-agent#1110).
	e.p.resetEngineStrikes()
	if e.logger != nil {
		e.logger.Info("engine controller: start requested", "engine", catalog.RuntimeVLLM)
	}
	e.p.requestEngineStart("engine start requested by operator")
	return nil
}

// EngineState reports the live power state plus whether the engine is
// waired-managed (false for adopted ollama orphans — there is no process
// handle, so the power axis cannot actually free memory).
func (e *engineController) EngineState() (management.EnginePowerState, bool) {
	engine := e.p.servingEngine()
	in := enginePowerInputs{
		Engine:        engine,
		StartInFlight: e.p.engineStartInFlight.Load(),
	}
	if engine == catalog.RuntimeVLLM {
		in.Parked = e.p.vllmIsParked()
		if a := e.p.vllmAdapter(); a != nil {
			in.AdapterPresent = true
			in.Health = a.Health(e.agentCtx).State
		}
		return decideEnginePower(in)
	}
	in.AdapterPresent = e.p.ollama != nil
	if e.p.ollama != nil {
		in.Parked = e.p.ollama.IsParked()
		in.Health = e.p.ollama.Health(e.agentCtx).State
		in.OllamaAdopted = e.p.ollama.Mode() == infruntime.EngineModeAdopted
	}
	return decideEnginePower(in)
}
