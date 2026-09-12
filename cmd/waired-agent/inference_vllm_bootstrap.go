package main

import (
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// What bootstrapVLLM should do when it finds an adapter already recorded.
const (
	// vllmBootstrapStart: nothing is recorded, so spawn the engine.
	vllmBootstrapStart = "start"
	// vllmBootstrapSkip: one is recorded and up, so leave it alone.
	vllmBootstrapSkip = "skip"
	// vllmBootstrapStopFirst: one is recorded but not up, so stop it before
	// spawning — a fresh Register over a live subprocess orphans it while it
	// still holds the GPU and the port.
	vllmBootstrapStopFirst = "stop_first"
	// vllmBootstrapParked: the operator hard-stopped this host's engine, so
	// nothing may bring one up until they start it again (#881). Wins over
	// every other verdict, including "nothing is recorded" — the latch lives
	// on the provider precisely so it can answer before an adapter exists.
	vllmBootstrapParked = "parked"
	// vllmBootstrapGaveUp: automatic recovery has given up on this engine
	// and only an explicit start clears that. The other latch this function
	// has to answer before it decides to spawn (waired-agent#1109).
	vllmBootstrapGaveUp = "gave_up"
	// vllmBootstrapProbeHoldsTheCard: the host-speed probe has an engine of
	// its own up on the probe model, on this host's vLLM port
	// (waired-agent#1298). Spawning over it would collide on the port and
	// charge the serving engine a start failure for something it did not
	// do; the probe asks for a start on its way out, so this is a wait
	// rather than a dead end.
	//
	// NOT "the exclusive measurement claim is held", which is also true
	// during a boot benchmark or a prefill measurement — those run against
	// the serving engine and a restart is allowed to interrupt them
	// (docs/decisions/20260809/1726-benchmark-yields-to-engine-restarts.md).
	// This is specifically "a second engine is up".
	vllmBootstrapProbeHoldsTheCard = "probe_engine_up"
)

// decideVLLMBootstrap answers what bootstrapVLLM should do, given the adapter
// already recorded (nil when none) and that adapter's last health state.
//
// bootstrapVLLM was written on the assumption that it runs at most once: each
// call builds a fresh VLLMAdapter, registers it OVER the previous entry, and
// spawns a second process on the same port, orphaning the first while it is
// still alive. `vllmBootstrapOnce` keeps that from happening today, but it
// guards the call site rather than the function. This is the function-level
// half, and the prerequisite #339 names for adopting a venv installed after
// boot.
//
// The decision lives in this untagged file rather than inline in the
// Linux-only bootstrapVLLM so it is table-testable on every OS (CLAUDE.md
// §Test discipline: put the seam below the behaviour under test).
//
// StateStarting counts as "up" alongside StateReady: an engine mid-startup
// already owns the port, and vLLM's load is minutes on a multi-GB model, so
// treating it as absent is exactly the double-spawn this prevents.
//
// parked is the operator's hard stop (#881) and is checked FIRST, before the
// nil case: `waired inference engine stop` has to hold on a host that has not
// bootstrapped yet — the venv install and the weights download are exactly
// when someone asks for their memory back, and there is no adapter then. The
// adapter refuses request traffic on its own (VLLMConfig.Parked); this is the
// arm that refuses the daemon's own start triggers.
//
// latched is the give-up latch, and it has to be asked HERE rather than left
// to EnsureRunning's ErrEngineUnrecoverable guard, because by the time that
// guard runs the latch is gone: a latched adapter reads StateFailed, which
// fell to stop_first, and stop_first is followed by a fresh NewVLLMAdapter
// that the latch does not survive. So every ordinary trigger — a crash
// recovery, turning inference on, a control-plane frame — quietly cleared
// the latch that exists to stop a respawn storm (#310), while the ollama arm
// refused the same triggers by name (engine_bootstrap.go). The documented
// reset stays the explicit `waired inference engine start`, which clears the
// latch itself before it gets here (waired-agent#1109).
func decideVLLMBootstrap(existing infruntime.Adapter, state string, parked, latched, probeEngineUp bool) string {
	if parked {
		return vllmBootstrapParked
	}
	if latched {
		return vllmBootstrapGaveUp
	}
	// After the two latches and before anything that spawns: the probe's
	// engine is a real process holding the port, so this is about what can
	// physically start, not about what should.
	if probeEngineUp {
		return vllmBootstrapProbeHoldsTheCard
	}
	if existing == nil {
		return vllmBootstrapStart
	}
	switch state {
	case infruntime.StateReady, infruntime.StateStarting:
		return vllmBootstrapSkip
	default:
		return vllmBootstrapStopFirst
	}
}
