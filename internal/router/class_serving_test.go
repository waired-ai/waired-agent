package router

import (
	"errors"
	"testing"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// mkPeerWithServing builds an unlimited-capacity, reachable peer carrying the
// admin Claude serving-eligibility exclusions (CP-folded into ExcludeMain /
// ExcludeSub), so selection turns purely on the per-class filter and the
// deviceID tie-break.
func mkPeerWithServing(deviceID, tag string, excludeMain, excludeSub bool) inferencemesh.PeerView {
	return inferencemesh.PeerView{
		DeviceID:   deviceID,
		DeviceName: deviceID,
		Stale:      false,
		InferenceState: &signer.InferenceState{
			Reachable:   true,
			Type:        signer.InferenceTypeOllama,
			Models:      []string{tag},
			LastCheck:   "2026-05-14T18:00:00Z",
			ExcludeMain: excludeMain,
			ExcludeSub:  excludeSub,
		},
	}
}

// TestSelector_ClassMain_SkipsMainExcludedPeer: peer-A is ineligible for main
// and has the lower deviceID (so the deterministic tie-break would pick it),
// but a main-class request must route past it to peer-Z.
func TestSelector_ClassMain_SkipsMainExcludedPeer(t *testing.T) {
	s := weightedSelector(nil, nil,
		mkPeerWithServing("peer-A", "qwen3:8b-q4_K_M", true, false),
		mkPeerWithServing("peer-Z", "qwen3:8b-q4_K_M", false, false),
	)
	sel, err := s.Select(t.Context(), Request{Model: "waired/default", Class: state.ClaudeClassMain})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.Runtime != "remote:peer-Z" {
		t.Errorf("main-excluded peer-A must be skipped for a main request; got %q", sel.Runtime)
	}
	sel.Release()
}

// TestSelector_ClassSub_KeepsMainExcludedPeer is the mirror: the same peer-A
// only excludes MAIN, so it remains eligible for a sub-class request and (lower
// deviceID) wins — proving the filter is per-class, not all-or-nothing.
func TestSelector_ClassSub_KeepsMainExcludedPeer(t *testing.T) {
	s := weightedSelector(nil, nil,
		mkPeerWithServing("peer-A", "qwen3:8b-q4_K_M", true, false),
		mkPeerWithServing("peer-Z", "qwen3:8b-q4_K_M", false, false),
	)
	sel, err := s.Select(t.Context(), Request{Model: "waired/default", Class: state.ClaudeClassSub})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.Runtime != "remote:peer-A" {
		t.Errorf("sub request must still reach a main-only-excluded peer; got %q", sel.Runtime)
	}
	sel.Release()
}

// TestSelector_EmptyClass_Unfiltered confirms general (non-Claude) inference —
// empty Class — is never filtered by the serving toggles: a peer marked
// ineligible for main still serves an unclassified request.
func TestSelector_EmptyClass_Unfiltered(t *testing.T) {
	s := weightedSelector(nil, nil,
		mkPeerWithServing("peer-A", "qwen3:8b-q4_K_M", true, true),
	)
	sel, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.Runtime != "remote:peer-A" {
		t.Errorf("empty-class request must be unfiltered; got %q", sel.Runtime)
	}
	sel.Release()
}

// TestSelector_ClassMain_AllExcluded_NotReady: when the only matching peer is
// ineligible for the request's class, the mesh has no candidate and the caller
// gets ErrModelNotReady (no local engine in this fixture).
func TestSelector_ClassMain_AllExcluded_NotReady(t *testing.T) {
	s := weightedSelector(nil, nil,
		mkPeerWithServing("peer-A", "qwen3:8b-q4_K_M", true, false),
	)
	_, err := s.Select(t.Context(), Request{Model: "waired/default", Class: state.ClaudeClassMain})
	if !errors.Is(err, ErrModelNotReady) {
		t.Fatalf("want ErrModelNotReady when every matching peer is class-excluded; got %v", err)
	}
}
