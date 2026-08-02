package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// engineController implements management.EngineController: the hard
// engine power axis (#186). It is orthogonal to inferenceController (the
// soft enable/disable gate, engine stays warm) and shareController — this
// one actually stops `ollama serve` to free VRAM/RAM and latches it
// stopped so request traffic can't revive it.
//
// State is live-only (the OllamaAdapter's parked flag); nothing is
// persisted, so a daemon restart returns to config-driven startup. That
// is deliberate: a hard stop is an operational "free my memory now"
// action, not a policy like the soft toggle's persisted desired-inference.
type engineController struct {
	ollama *infruntime.OllamaAdapter
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
	onEngineUp func()
}

func newEngineController(ctx context.Context, ollama *infruntime.OllamaAdapter, logger *slog.Logger) *engineController {
	return &engineController{ollama: ollama, agentCtx: ctx, logger: logger}
}

// engineStopBudget bounds a hard stop end to end: the adapter's graceful
// grace period plus its post-kill reap, both StopTimeout (5s by default),
// plus headroom. It is deliberately larger than any client budget — the
// kill runs to completion regardless of who is still listening.
const engineStopBudget = 15 * time.Second

// StopEngine hard-stops the engine (SIGTERM→SIGKILL) and latches it
// parked. Synchronous — the caller learns the memory was actually freed
// before the HTTP response — but the caller's context bounds only how
// long IT waits, never how long the kill runs (#316). Dropping the
// request context here matters twice: a tray that gives up at 3s must not
// truncate the SIGTERM grace period on Unix, and it must never leave a
// live engine behind a latched "stopped" power state.
func (e *engineController) StopEngine(ctx context.Context) error {
	if e.logger != nil {
		e.logger.Info("engine controller: hard stop requested")
	}
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), engineStopBudget)
	defer cancel()
	err := e.ollama.Park(stopCtx)
	if e.logger != nil {
		e.logger.Debug("engine controller: hard stop result", "ok", err == nil)
	}
	return err
}

// StartEngine clears the parked latch and restarts the engine
// asynchronously: EnsureRunning blocks until the readiness probe passes
// (seconds to tens of seconds), so blocking the HTTP handler on it would
// hang the tray/CLI. The status endpoint reflects "starting" → "ready"
// as the background spawn progresses.
func (e *engineController) StartEngine(_ context.Context) error {
	e.ollama.Unpark()
	// An explicit start is also the documented reset for a crash-recovery
	// give-up (waired-agent#29): the operator has presumably changed
	// something, so let the engine try again. No new endpoint or CLI verb —
	// this is already `waired inference engine start`.
	e.ollama.ClearFailure()
	if e.logger != nil {
		e.logger.Info("engine controller: start requested")
	}
	go func() {
		if err := e.ollama.EnsureRunning(e.agentCtx); err != nil {
			if e.logger != nil {
				e.logger.Warn("engine controller: start failed", "err", err)
			}
			return
		}
		if e.logger != nil {
			e.logger.Debug("engine controller: engine running", "mode", string(e.ollama.Mode()))
		}
		if e.logger != nil && e.ollama.Mode() == infruntime.EngineModeAdopted {
			e.logger.Info("engine controller: adopted orphan bundled ollama (exact pin match)",
				"version", e.ollama.EngineVersion())
		}
		if e.onEngineUp != nil {
			e.onEngineUp()
		}
	}()
	return nil
}

// EngineState reports the live power state plus whether the engine is
// waired-managed (false in reuse mode and for adopted orphans — in
// both cases there is no process handle, so the power axis cannot
// actually free memory).
func (e *engineController) EngineState() (management.EnginePowerState, bool) {
	managed := !e.ollama.Borrowed() && e.ollama.Mode() != infruntime.EngineModeAdopted
	switch {
	case e.ollama.IsParked():
		return management.EnginePowerStopped, managed
	case e.ollama.Health(e.agentCtx).State == infruntime.StateStarting:
		return management.EnginePowerStarting, managed
	default:
		return management.EnginePowerRunning, managed
	}
}
