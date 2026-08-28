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
		latched  bool
		want     string
	}{
		{"nothing recorded", nil, "", false, false, vllmBootstrapStart},
		{"nothing recorded, stale state ignored", nil, infruntime.StateReady, false, false, vllmBootstrapStart},
		{"already ready", live, infruntime.StateReady, false, false, vllmBootstrapSkip},
		// Mid-startup already owns the port, and vLLM's load is minutes on a
		// multi-GB model — the window a double spawn would land in.
		{"still starting", live, infruntime.StateStarting, false, false, vllmBootstrapSkip},
		{"failed", live, infruntime.StateFailed, false, false, vllmBootstrapStopFirst},
		{"stopped", live, infruntime.StateStopped, false, false, vllmBootstrapStopFirst},
		{"never started", live, infruntime.StateNotStarted, false, false, vllmBootstrapStopFirst},
		{"unknown state", live, "who knows", false, false, vllmBootstrapStopFirst},

		// The operator's hard stop (#881) wins over every other verdict.
		// The nil row is the one that matters: `waired inference engine
		// stop` has to hold on a host whose bootstrap has not spawned
		// anything yet — the venv install and the weights download are
		// exactly when someone asks for their memory back — and without the
		// latch the download would finish and start an engine they stopped.
		{"parked, nothing recorded", nil, "", true, false, vllmBootstrapParked},
		{"parked and ready", live, infruntime.StateReady, true, false, vllmBootstrapParked},
		{"parked and stopped", live, infruntime.StateStopped, true, false, vllmBootstrapParked},

		// THE #1109 BAR. A latched adapter reads StateFailed, so before
		// this arm existed it fell to stop_first — and stop_first is
		// followed by a fresh NewVLLMAdapter, which the latch does not
		// survive. Every ordinary trigger (crash recovery, turning
		// inference on, a control-plane frame) therefore cleared the latch
		// that exists to stop the respawn storm #310 described, while the
		// ollama arm refused those same triggers by name. Asking here is
		// the only place it CAN be asked: EnsureRunning's own
		// ErrEngineUnrecoverable guard runs after the replacement.
		{"gave up", live, infruntime.StateFailed, false, true, vllmBootstrapGaveUp},
		// A latch is not a state: Stop() clears Health with no give-up
		// guard, so a latched engine that was then bounced reads stopped.
		{"gave up, then stopped", live, infruntime.StateStopped, false, true, vllmBootstrapGaveUp},
		// The operator's hard stop still outranks it — they asked for the
		// memory back, and that is the more specific instruction.
		{"parked beats gave up", live, infruntime.StateFailed, true, true, vllmBootstrapParked},
		// No adapter means no latch to read; a latched flag with none is
		// not a state this can be in, and start is the safe answer.
		{"nothing recorded cannot be latched", nil, "", false, false, vllmBootstrapStart},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := decideVLLMBootstrap(tc.existing, tc.state, tc.parked, tc.latched)
			if got != tc.want {
				t.Errorf("decideVLLMBootstrap(%v, %q, parked=%v, latched=%v) = %q, want %q",
					tc.existing != nil, tc.state, tc.parked, tc.latched, got, tc.want)
			}
		})
	}
}
