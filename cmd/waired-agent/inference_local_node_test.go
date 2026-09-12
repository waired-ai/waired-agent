package main

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// TestBuildSelectorWith_WiresTheLocalReading is the test without which the
// whole of waired-agent#1302 is dead code in production while every router
// test stays green: the ordering is gated on Inputs.LocalNode being
// non-nil, and nothing but this call sets it.
//
// CLAUDE.md §Test discipline: "a `var xFn = realFn` seam needs a table test
// on `realFn`, or the real one is never called by any test."
func TestBuildSelectorWith_WiresTheLocalReading(t *testing.T) {
	p := newClaudeSelectorProvider(t, func() inferencemesh.Snapshot {
		return inferencemesh.Snapshot{SelfDeviceID: "dev_self"}
	})
	in := p.selectorInputs(t.Context(), state.RoutingPreference{}, false)
	if in.LocalNode == nil {
		t.Fatal("Inputs.LocalNode is nil — the auto arm falls back to the pre-#1302 branch and the ordering never runs")
	}
	if in.TieBreak == nil {
		t.Fatal("Inputs.TieBreak is nil — three computers ranking the same tied candidates all pick the same one (waired-agent#1303 S4)")
	}

	// And the OVERLAY posture must not carry either: a request that arrived
	// FROM a peer is served by this device's engine because that peer's
	// router already chose it, and re-ranking here would let a serving node
	// hand the work on.
	base := p.baseRouterInputs(t.Context())
	if base.LocalNode != nil {
		t.Error("baseRouterInputs carries LocalNode; the overlay-side Selector would start re-ranking peer-arriving requests")
	}
	if base.MeshSnapshotFn != nil {
		t.Error("baseRouterInputs carries MeshSnapshotFn; loop prevention depends on it being nil")
	}
}

// TestLocalNodeFrom is the decision behind the seam: which of the facts
// gathered from the live host make this device a routing candidate, and
// under what description.
//
// Product contract, ratifying source waired-agent#1302. The three
// "not serving" shapes are the reason the auto arm falls through instead of
// refusing: an empty reading means this device could not describe itself,
// which is not the same as being unable to serve.
func TestLocalNodeFrom(t *testing.T) {
	full := localFacts{
		serving:        true,
		modelID:        "qwen3.8-9b-instruct",
		deviceID:       "dev_self",
		displayName:    "this-computer",
		runtime:        "ollama",
		engineTag:      "qwen3.8:9b-q4_K_M",
		variantID:      "q4-gguf",
		pendingModelID: "qwen3.5-4b",
		contextWindow:  200_704,
		capacity:       2,
		capacityUsed:   1,
	}

	t.Run("a serving host describes itself", func(t *testing.T) {
		ln := localNodeFrom(full)
		if !ln.Serving {
			t.Fatalf("Serving = false: %+v", ln)
		}
		if ln.DeviceID != "dev_self" || ln.DisplayName != "this-computer" {
			t.Errorf("identity = %q / %q", ln.DeviceID, ln.DisplayName)
		}
		// The join key against the want set, and the same value
		// narrowPublishedModels advertises to peers.
		if ln.EngineTag != "qwen3.8:9b-q4_K_M" || ln.ModelID != "qwen3.8-9b-instruct" {
			t.Errorf("model = %q / tag = %q", ln.ModelID, ln.EngineTag)
		}
		if ln.Capacity != 2 || ln.CapacityUsed != 1 {
			t.Errorf("capacity = %d/%d, want 2/1 — the congestion divisor", ln.CapacityUsed, ln.Capacity)
		}
		// Through a switch this device is still serving the OLD model,
		// which is what the reason line needs to say.
		if ln.PendingModelID != "qwen3.5-4b" {
			t.Errorf("PendingModelID = %q", ln.PendingModelID)
		}
	})

	for _, c := range []struct {
		name string
		mut  func(*localFacts)
	}{
		{"the engine is not ready", func(f *localFacts) { f.serving = false }},
		{"no active model", func(f *localFacts) { f.modelID = "" }},
		{"no device id yet (before the first network map)", func(f *localFacts) { f.deviceID = "" }},
		{"no engine tag yet (before the active selection is recorded)", func(f *localFacts) { f.engineTag = "" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := full
			c.mut(&f)
			ln := localNodeFrom(f)
			if ln.Serving {
				t.Errorf("Serving = true: %+v", ln)
			}
			if ln.DeviceID != "" {
				t.Errorf("a non-candidate carries an identity: %+v", ln)
			}
		})
	}
}

// TestLocalNodeForRouting_GathersNothingWhenTheEngineIsDown is the thin
// gathering half, on the one shape a test host can reach: no engine, so
// nothing is read and nothing is claimed.
func TestLocalNodeForRouting_GathersNothingWhenTheEngineIsDown(t *testing.T) {
	p := newClaudeSelectorProvider(t, func() inferencemesh.Snapshot {
		return inferencemesh.Snapshot{SelfDeviceID: "dev_self"}
	})
	// This fixture has no engine adapter, so EngineReady is false — the
	// same shape a host has before its bootstrap reaches the spawn.
	if ready, _ := p.EngineReady(); ready {
		t.Fatal("precondition: this fixture is supposed to have no live engine")
	}
	if ln := p.localNodeForRouting(); ln.Serving {
		t.Errorf("Serving = true with no live engine: %+v", ln)
	}

	var nilProv *agentInferenceProvider
	if ln := nilProv.localNodeForRouting(); ln.Serving {
		t.Errorf("a nil provider claimed to be serving: %+v", ln)
	}
}

// TestRoutingTieBreak keeps the randomiser inside its contract: the result
// is a valid index, and n<=1 is answered without asking for randomness.
func TestRoutingTieBreak(t *testing.T) {
	if got := routingTieBreak(0); got != 0 {
		t.Errorf("routingTieBreak(0) = %d, want 0", got)
	}
	if got := routingTieBreak(1); got != 0 {
		t.Errorf("routingTieBreak(1) = %d, want 0", got)
	}
	for range 200 {
		if got := routingTieBreak(3); got < 0 || got > 2 {
			t.Fatalf("routingTieBreak(3) = %d, want [0,3)", got)
		}
	}
}
