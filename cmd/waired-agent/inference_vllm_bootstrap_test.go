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
		parked   bool
		want     string
	}{
		{"nothing recorded", nil, "", false, vllmBootstrapStart},
		{"nothing recorded, stale state ignored", nil, infruntime.StateReady, false, vllmBootstrapStart},
		{"already ready", live, infruntime.StateReady, false, vllmBootstrapSkip},
		// Mid-startup already owns the port, and vLLM's load is minutes on a
		// multi-GB model — the window a double spawn would land in.
		{"still starting", live, infruntime.StateStarting, false, vllmBootstrapSkip},
		{"failed", live, infruntime.StateFailed, false, vllmBootstrapStopFirst},
		{"stopped", live, infruntime.StateStopped, false, vllmBootstrapStopFirst},
		{"never started", live, infruntime.StateNotStarted, false, vllmBootstrapStopFirst},
		{"unknown state", live, "who knows", false, vllmBootstrapStopFirst},

		// The operator's hard stop (#881) wins over every other verdict.
		// The nil row is the one that matters: `waired inference engine
		// stop` has to hold on a host whose bootstrap has not spawned
		// anything yet — the venv install and the weights download are
		// exactly when someone asks for their memory back — and without the
		// latch the download would finish and start an engine they stopped.
		{"parked, nothing recorded", nil, "", true, vllmBootstrapParked},
		{"parked and ready", live, infruntime.StateReady, true, vllmBootstrapParked},
		{"parked and stopped", live, infruntime.StateStopped, true, vllmBootstrapParked},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideVLLMBootstrap(tc.existing, tc.state, tc.parked); got != tc.want {
				t.Errorf("decideVLLMBootstrap(%v, %q, parked=%v) = %q, want %q",
					tc.existing != nil, tc.state, tc.parked, got, tc.want)
			}
		})
	}
}
