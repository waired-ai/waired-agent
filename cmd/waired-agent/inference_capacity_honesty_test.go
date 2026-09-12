package main

import (
	"testing"

	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// TestWarmConversationSlots_IntentIsNotAMeasurement is waired-agent#1303's
// capacity half, as a guard.
//
// Product contract, ratifying source waired-agent#1303: what this host
// advertises as warm conversations is what the model runner is serving, and
// nothing else. Measured on pc-mbp14-m5 and sv-macmini on 2026-09-12: the
// runner was started with `-np 1`, the observation never landed (the macOS
// program path contains a space), and the host advertised the 2 it had
// asked for — so a pinned turn was admitted into a one-slot engine and
// queued behind the peer owner's own turn, silent for 185 s.
func TestWarmConversationSlots_IntentIsNotAMeasurement(t *testing.T) {
	// The measured shape: asked for 2, runner really has 1, observation
	// missing. Every ladder rung below the observation must decline.
	got := warmConversationSlots(signer.InferenceTypeOllama, infruntime.ModelTuning{
		NumParallel:            2,
		RecommendedMaxParallel: 4,
		ContextLength:          200704,
	})
	if got != 0 {
		t.Fatalf("warm slots = %d with no observation, want 0 (not known yet); "+
			"%d would be the parallelism this host ASKED for, which the engine may have reduced", got, got)
	}
}

// TestCapacityFn_UnknownSlotsAdvertiseOne pins where that 0 goes. It must
// never reach the wire: InferenceState.Capacity reads 0 as UNLIMITED, which
// is the opposite of what "not known yet" means.
func TestCapacityFn_UnknownSlotsAdvertiseOne(t *testing.T) {
	prov := &agentInferenceProvider{} // no adapters ⇒ WarmConversationSlots() == 0
	sub := &inferenceSubsystem{provider: prov}
	if got := capacityFn(0, sub)(); got != unmeasuredCapacity {
		t.Fatalf("capacityFn = %d with nothing measured, want unmeasuredCapacity (%d)", got, unmeasuredCapacity)
	}
	// And a boot benchmark figure still wins over the fail-safe, so this
	// change does not lower a host that did measure.
	if got := capacityFn(3, sub)(); got != 3 {
		t.Fatalf("capacityFn = %d with a boot figure of 3, want 3", got)
	}
}
