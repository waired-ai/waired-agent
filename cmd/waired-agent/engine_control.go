package main

import (
	"context"
	"log/slog"

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
}

func newEngineController(ctx context.Context, ollama *infruntime.OllamaAdapter, logger *slog.Logger) *engineController {
	return &engineController{ollama: ollama, agentCtx: ctx, logger: logger}
}

// StopEngine hard-stops the engine (SIGTERM→SIGKILL) and latches it
// parked. Synchronous and bounded by the adapter's StopTimeout — the
// caller learns the memory was actually freed before the HTTP response.
func (e *engineController) StopEngine(ctx context.Context) error {
	if e.logger != nil {
		e.logger.Info("engine controller: hard stop requested")
	}
	return e.ollama.Park(ctx)
}

// StartEngine clears the parked latch and restarts the engine
// asynchronously: EnsureRunning blocks until the readiness probe passes
// (seconds to tens of seconds), so blocking the HTTP handler on it would
// hang the tray/CLI. The status endpoint reflects "starting" → "ready"
// as the background spawn progresses.
func (e *engineController) StartEngine(_ context.Context) error {
	e.ollama.Unpark()
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
		if e.logger != nil && e.ollama.Mode() == infruntime.EngineModeAdopted {
			e.logger.Info("engine controller: adopted orphan bundled ollama (exact pin match)",
				"version", e.ollama.EngineVersion())
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
