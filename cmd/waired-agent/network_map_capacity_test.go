package main

import (
	"context"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// TestStreamingAppliesSelfCapacity verifies the network-map stream loop applies
// the CP's effective per-device settings (nm.Self.InferenceState) to the overlay
// listener via the applySelf callback, and leaves them untouched on frames
// whose Self carries no InferenceState. It also pins the fix for the
// benchmark-vs-override conflation: a Self carrying only a (benchmark) Capacity
// must report DesiredParallel=0 so the agent never restarts its engine on a
// default host — and, for waired#825, that the PublicShare toggle echo reaches
// the callback on the same frame.
func TestStreamingAppliesSelfCapacity(t *testing.T) {
	eng := &fakeEngine{}
	prov := &agentProvider{}
	rec := newReconciler(eng, prov, quietLogger(), nil, cmmTestConfig())

	got := make(chan *signer.InferenceState, 4)
	applySelf := func(st *signer.InferenceState) {
		got <- st
	}

	// Frame 1 carries an effective capacity of 5 with NO admin override
	// (DesiredParallel 0 = the benchmark case) and the CP's PublicShare
	// echo; frame 2 has no engine state (Self.InferenceState nil) and
	// must NOT trigger a call.
	frames := make(chan *signer.NetworkMap, 2)
	frames <- &signer.NetworkMap{
		Self: signer.NetworkMapPeer{
			DeviceID: "self",
			InferenceState: &signer.InferenceState{
				Reachable:   true,
				Type:        signer.InferenceTypeOllama,
				Capacity:    5,
				PublicShare: true,
			},
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
	streaming(ctx, quietLogger(), rec, nil, nil, nil, applySelf, nil, frames, errs)

	select {
	case st := <-got:
		if st.Capacity != 5 {
			t.Fatalf("applySelf capacity = %d, want 5", st.Capacity)
		}
		if st.DesiredParallel != 0 {
			t.Fatalf("applySelf parallel = %d, want 0 (benchmark capacity must not drive parallelism)", st.DesiredParallel)
		}
		if !st.PublicShare {
			t.Fatalf("applySelf PublicShare = false, want true (toggle echo must reach the callback)")
		}
	case <-time.After(time.Second):
		t.Fatalf("applySelf was never called for the frame with Self.InferenceState")
	}
	// The nil-InferenceState frame must not have produced a second call.
	select {
	case st := <-got:
		t.Fatalf("applySelf called again with %+v; nil Self.InferenceState must leave settings untouched", st)
	default:
	}
}
