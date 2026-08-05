package main

import (
	"testing"

	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// decideVLLMBootstrap is what makes bootstrapVLLM safe to call twice. Before
// it, a second call registered a fresh adapter over the previous one and
// spawned a second process on the same port while the first was still alive
// (#337, and the prerequisite #339 names for adopting a late venv).
//
// A record of today's behaviour turned into a regression bar, not a product
// contract: no ratifying source states these transitions, only #339's Proposed
// item 1 ("unregister/stop the previous adapter, or refuse when one is already
// registered and healthy"), which this reads as "refuse while up, replace
// otherwise".
//
// Untagged on purpose: bootstrapVLLM itself is Linux-only, so a decision left
// inline there would be untestable on the darwin and windows legs.
func TestDecideVLLMBootstrap(t *testing.T) {
	live := fakeAdapter{name: "vllm"}

	tests := []struct {
		name     string
		existing infruntime.Adapter
		state    string
		want     string
	}{
		{"nothing recorded", nil, "", vllmBootstrapStart},
		{"nothing recorded, stale state ignored", nil, infruntime.StateReady, vllmBootstrapStart},
		{"already ready", live, infruntime.StateReady, vllmBootstrapSkip},
		// Mid-startup already owns the port, and vLLM's load is minutes on a
		// multi-GB model — the window a double spawn would land in.
		{"still starting", live, infruntime.StateStarting, vllmBootstrapSkip},
		{"failed", live, infruntime.StateFailed, vllmBootstrapStopFirst},
		{"stopped", live, infruntime.StateStopped, vllmBootstrapStopFirst},
		{"never started", live, infruntime.StateNotStarted, vllmBootstrapStopFirst},
		{"unknown state", live, "who knows", vllmBootstrapStopFirst},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideVLLMBootstrap(tc.existing, tc.state); got != tc.want {
				t.Errorf("decideVLLMBootstrap(%v, %q) = %q, want %q",
					tc.existing != nil, tc.state, got, tc.want)
			}
		})
	}
}
