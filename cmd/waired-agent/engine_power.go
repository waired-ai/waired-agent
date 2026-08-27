package main

import (
	"context"
	"fmt"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/management"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// The hard engine power axis (#186), for whichever engine this host serves
// with (#881).
//
// Everything in this file is untagged so the vLLM half is table-tested on the
// windows and darwin legs too, where infruntime.VLLMAdapter does not exist at
// all (CLAUDE.md §Test discipline: put the seam below the behaviour under
// test, the initStateDirMode model).

// setVLLMParked records the operator's engine-power latch for the vLLM
// engine, and vllmIsParked reads it.
//
// The latch lives on the provider rather than on the adapter — the opposite
// of ollama, whose latch is a field on its long-lived OllamaAdapter — because
// it has to outlive any one adapter:
//
//   - bootstrapVLLM builds a FRESH VLLMAdapter on every call
//     (inference_vllm_linux.go) and registers it over the previous entry, so
//     a latch stored on the adapter is discarded by the next bootstrap;
//   - there is no adapter at all while the venv installs or the weights
//     download, which is an hour on a host whose servingEngine() already says
//     vllm — and "before first start" is precisely a state ollama's Park
//     supports and this axis must too;
//   - bootstrapVLLM holds engineOpMu ACROSS that download, so StopEngine
//     cannot take the same mutex without blocking for the whole of it. A park
//     can therefore always land after a bootstrap's own check, and only a
//     latch the freshly built adapter reads LIVE (through VLLMConfig.Parked)
//     can refuse the spawn that follows.
//
// Live-only, like ollama's: nothing is persisted, so a daemon restart returns
// to config-driven startup. A hard stop is an operational "free my memory
// now", not a policy.
func (p *agentInferenceProvider) setVLLMParked(v bool) {
	if p == nil {
		return
	}
	p.vllmParked.Store(v)
}

func (p *agentInferenceProvider) vllmIsParked() bool {
	return p != nil && p.vllmParked.Load()
}

// servingAdapter is the adapter for the engine this host actually serves
// from, or nil when that engine has no adapter yet (a vLLM host whose
// bootstrap has not reached the spawn).
//
// The nil-check placement is load-bearing and predates this function: a nil
// *OllamaAdapter stored into the interface would compare non-nil, so the
// assignment happens INSIDE the check, never around it.
func (p *agentInferenceProvider) servingAdapter() infruntime.Adapter {
	if p == nil {
		return nil
	}
	if p.servingEngine() == catalog.RuntimeVLLM {
		return p.vllmAdapter()
	}
	if p.ollama != nil {
		return p.ollama
	}
	return nil
}

// enginePowerInputs is what decideEnginePower needs, gathered from the live
// host by engineController.EngineState.
type enginePowerInputs struct {
	// Engine is servingEngine(): catalog.RuntimeOllama or RuntimeVLLM.
	Engine string
	// Parked is the operator's latch for that engine.
	Parked bool
	// AdapterPresent is false only on a vLLM host whose bootstrap has not
	// built an adapter yet. The ollama adapter always exists.
	AdapterPresent bool
	// Health is the adapter's Health().State, "" when there is no adapter.
	Health string
	// StartInFlight is engineStartInFlight: a bootstrap this process
	// dispatched has not finished. On vLLM that covers the weights download,
	// which is the long half.
	StartInFlight bool
	// OllamaAdopted is Mode() == EngineModeAdopted: an orphan of a previous
	// run, which waired holds no process handle for.
	OllamaAdopted bool
}

// decideEnginePower answers the power state and whether waired manages the
// engine, i.e. whether stop/start apply at all.
//
// managed is false only for an ADOPTED ollama orphan, where there is no
// process handle and the axis genuinely cannot free memory. It is always true
// for vLLM: there is no adoption path for that engine (a foreign vLLM on the
// port makes the spawn fail rather than being adopted), and because the latch
// lives on the provider the axis applies even before an adapter exists.
// Answering false there would make the management handler 409 the stop —
// reproducing #881's shape, a surface reporting a state the system does not
// honour.
func decideEnginePower(in enginePowerInputs) (management.EnginePowerState, bool) {
	// managed is per-engine; the answer below is not (waired-agent#964).
	// The two arms used to disagree on the same facts: ollama let
	// StateFailed, StateStopped and StateNotStarted fall through to
	// "running", vLLM answered "stopped" for all three, and neither had a
	// way to say "not running, and nobody asked for that".
	managed := true
	if in.Engine != catalog.RuntimeVLLM {
		managed = !in.OllamaAdopted
	}
	switch {
	case in.Parked:
		// The operator's own stop outranks whatever the engine is doing:
		// an engine parked while it happened to be failing was stopped on
		// purpose, and reporting the failure would send someone after a
		// fault that is a setting.
		return management.EnginePowerStopped, managed
	case in.Health == infruntime.StateStarting:
		return management.EnginePowerStarting, managed
	case in.Health == infruntime.StateFailed:
		return management.EnginePowerFailed, managed
	case !in.AdapterPresent:
		// vLLM only — the ollama adapter always exists. A start was asked
		// for and the bootstrap is still resolving the venv or downloading
		// weights; reporting "stopped" would have an operator press start
		// again on a host that is already pulling 40 GB.
		if in.StartInFlight {
			return management.EnginePowerStarting, managed
		}
		return management.EnginePowerStopped, managed
	case in.Health == infruntime.StateReady:
		return management.EnginePowerRunning, managed
	case in.StartInFlight:
		// Stopped or never started, with a start in flight: the bounce a
		// model switch makes, and the window between a bootstrap's Stop and
		// its next spawn.
		return management.EnginePowerStarting, managed
	default:
		// Stopped, never started, or a state this build does not know. Not
		// "running": an engine that is not up has not got the memory, which
		// is the question this axis answers.
		return management.EnginePowerStopped, managed
	}
}

// engineIsParked reports the operator's hard stop for the engine this host
// serves with. Every surface that used to read p.ollama.IsParked() directly
// has to go through here: on a vLLM host that read answered for an adapter
// that was not serving, so an ollama park flipped the whole subsystem to
// "stopped" — and peers stopped routing to a host that was still answering
// (#944).
func (p *agentInferenceProvider) engineIsParked() bool {
	if p == nil {
		return false
	}
	if p.servingEngine() == catalog.RuntimeVLLM {
		return p.vllmIsParked()
	}
	return p.ollama != nil && p.ollama.IsParked()
}

// onVLLMEngineStartFailed is the VLLMConfig.OnStartFailed handler: a start
// attempt ended without the engine serving (waired-agent#1026).
//
// The ollama sibling (onEngineStartFailed) has existed since #310; vLLM had
// the callback declared and nothing wired to it. The consequence is the one
// #310 names: a start that never reaches Ready never reaches markUnhealthy,
// so no strike is charged and FailureLatched() stays false — and on vLLM
// every later trigger (a gateway request, a desired-state apply, the
// wizard's benchmark) re-enters bootstrapVLLM, which retries three times and
// gives up again. On a host whose port was taken that was an unbounded loop
// that no surface could report, because "the engine gave up" was a state the
// engine could not reach.
//
// Deliberately does NOT schedule a restart, for the reason its ollama
// sibling gives: bootstrapVLLM has already spent its own retry budget, and
// throwing another start at it from here builds the respawn storm the
// give-up latch exists to end. All this owes the system is a verdict.
func (p *agentInferenceProvider) onVLLMEngineStartFailed(detail string) {
	if p == nil || p.servingEngine() != catalog.RuntimeVLLM || p.vllmIsParked() {
		return
	}
	n := p.recordEngineStrike()
	if n <= engineRecoveryMaxAttempts {
		if p.logger != nil {
			p.logger.Warn("vllm engine did not start; leaving the retry to the caller",
				"attempt", n, "max", engineRecoveryMaxAttempts)
		}
		return
	}
	if p.logger != nil {
		p.logger.Error("vllm engine repeatedly failed to start; automatic restart disabled",
			"attempts", n, "window", engineRecoveryStableFor)
	}
	if l, ok := p.vllmAdapter().(interface{ LatchFailed(string) }); ok {
		l.LatchFailed(fmt.Sprintf(
			"engine failed to start %d times within %s; automatic restart disabled — see the engine log, "+
				"then `waired inference engine start` (or switch model) to retry\n%s",
			n, engineRecoveryStableFor, detail))
	}
}

// onVLLMEngineUnhealthy is the VLLMConfig.OnUnhealthy handler: the adapter
// has found its engine dead and moved to StateFailed (#946).
//
// The ollama sibling's policy, on the same budget — three attempts at
// 0s/15s/60s, then a give-up latch that says so instead of respawning
// forever. It shares recordEngineStrike deliberately: a host serves with one
// engine at a time, so "this host's engine keeps dying" is one budget, not
// two, and a switch between engines is exactly when a fresh start is wanted
// anyway.
//
// The restart goes through requestEngineStart, not EnsureRunning on the dead
// adapter: a vLLM engine that died may have died of its own sizing, and that
// path re-resolves the venv, the weights and the tuning before spawning.
func (p *agentInferenceProvider) onVLLMEngineUnhealthy(detail string) {
	if p == nil || p.servingEngine() != catalog.RuntimeVLLM || p.vllmIsParked() {
		// Parked is the operator's own stop; charging it as a crash would
		// let `waired inference engine stop` spend the recovery budget.
		return
	}
	n := p.recordEngineStrike()
	if n > engineRecoveryMaxAttempts {
		if p.logger != nil {
			p.logger.Error("vllm engine crashed repeatedly; automatic restart disabled",
				"crashes", n, "window", engineRecoveryStableFor)
		}
		if l, ok := p.vllmAdapter().(interface{ LatchFailed(string) }); ok {
			l.LatchFailed(fmt.Sprintf(
				"engine crashed %d times within %s; automatic restart disabled — see the engine log, "+
					"then `waired inference engine start` (or switch model) to retry\n%s",
				n, engineRecoveryStableFor, detail))
		}
		return
	}
	delay := engineRecoveryBackoff(n)
	if p.logger != nil {
		p.logger.Warn("vllm engine died; scheduling restart",
			"crash", n, "max", engineRecoveryMaxAttempts, "in", delay)
	}
	ctx := p.agentCtx
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		if delay > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
		}
		p.requestEngineStart("vllm engine crash recovery")
	}()
}
