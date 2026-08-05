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
func decideVLLMBootstrap(existing infruntime.Adapter, state string) string {
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
