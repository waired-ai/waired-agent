package main

import (
	"context"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/proto/signer"
)

type applyArgs struct{ capacity, parallel, publicCapacity int }

// TestStreamingAppliesSelfCapacity verifies the network-map stream loop applies
// the CP's effective per-device settings (nm.Self.InferenceState) to the overlay
// listener via the applyConcurrency callback, and leaves them untouched on
// frames whose Self carries no InferenceState. It also pins the fix for the
// benchmark-vs-override conflation: a Self carrying only a (benchmark) Capacity
// must pass parallel=0 so the agent never restarts its engine on a default host.
func TestStreamingAppliesSelfCapacity(t *testing.T) {
	eng := &fakeEngine{}
	prov := &agentProvider{}
	rec := newReconciler(eng, prov, quietLogger(), nil, cmmTestConfig())

	got := make(chan applyArgs, 4)
	applyConcurrency := func(capacity, parallel, publicCapacity int) {
		got <- applyArgs{capacity, parallel, publicCapacity}
	}

	// Frame 1 carries an effective capacity of 5 with NO admin override
	// (DesiredParallel 0 = the benchmark case); frame 2 has no engine state
	// (Self.InferenceState nil) and must NOT trigger a call.
	frames := make(chan *signer.NetworkMap, 2)
	frames <- &signer.NetworkMap{
		Self: signer.NetworkMapPeer{
			DeviceID:       "self",
			InferenceState: &signer.InferenceState{Reachable: true, Type: signer.InferenceTypeOllama, Capacity: 5},
		},
	}
	frames <- &signer.NetworkMap{
		Self: signer.NetworkMapPeer{DeviceID: "self"},
	}
	close(frames)

	errs := make(chan error)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// streaming returns once frames is closed.
	streaming(ctx, quietLogger(), rec, nil, nil, nil, applyConcurrency, frames, errs)

	select {
	case a := <-got:
		if a.capacity != 5 {
			t.Fatalf("applyConcurrency capacity = %d, want 5", a.capacity)
		}
		if a.parallel != 0 {
			t.Fatalf("applyConcurrency parallel = %d, want 0 (benchmark capacity must not drive parallelism)", a.parallel)
		}
	case <-time.After(time.Second):
		t.Fatalf("applyConcurrency was never called for the frame with Self.InferenceState")
	}
	// The nil-InferenceState frame must not have produced a second call.
	select {
	case a := <-got:
		t.Fatalf("applyConcurrency called again with %+v; nil Self.InferenceState must leave settings untouched", a)
	default:
	}
}
