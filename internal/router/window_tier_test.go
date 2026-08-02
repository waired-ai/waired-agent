package router

import (
	"errors"
	"testing"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/proto/hostfit"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// mkPeerWithWindow builds a reachable peer that declares (or withholds)
// a serving context window. 0 is what every agent predating waired#1031
// sends, and it must mean "says nothing", never "serves nothing".
func mkPeerWithWindow(deviceID, tag string, window int) inferencemesh.PeerView {
	return inferencemesh.PeerView{
		DeviceID:   deviceID,
		DeviceName: deviceID,
		InferenceState: &signer.InferenceState{
			Reachable:     true,
			Type:          signer.InferenceTypeOllama,
			Models:        []string{tag},
			LastCheck:     "2026-08-02T12:00:00Z",
			ContextWindow: window,
		},
	}
}

// TestSelector_TierSkipsAPeerThatDeclaresLess is the /model tier as a
// routing rule (waired#1031). Claude Code sized the session from the id
// before the request existed, so a peer serving less is not a worse
// answer — it is a wrong one, and its turn would be truncated.
func TestSelector_TierSkipsAPeerThatDeclaresLess(t *testing.T) {
	s := weightedSelector(nil, nil,
		mkPeerWithWindow("peer-A", "qwen3:8b-q4_K_M", 98304),
		mkPeerWithWindow("peer-Z", "qwen3:8b-q4_K_M", hostfit.ServingWindow200k),
	)
	// peer-A has the lower deviceID, so the deterministic tie-break would
	// otherwise take it.
	sel, err := s.Select(t.Context(), Request{
		Model: "waired/default", MinContextWindow: hostfit.ServingWindow200k,
	})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.Runtime != "remote:peer-Z" {
		t.Errorf("a 98k peer served a 200k tier; got %q", sel.Runtime)
	}
	if sel.ContextWindow != hostfit.ServingWindow200k {
		t.Errorf("Selection.ContextWindow = %d, want the peer's declaration %d — "+
			"the #623 guard downstream reads it", sel.ContextWindow, hostfit.ServingWindow200k)
	}
	sel.Release()
}

// TestSelector_TierKeepsAPeerThatDeclaresNothing is the rolling-upgrade
// case, and the one that decides whether this feature can ship at all. A
// peer running an older agent sends 0. Reading that as a refusal would
// empty the mesh the moment ONE node upgraded.
func TestSelector_TierKeepsAPeerThatDeclaresNothing(t *testing.T) {
	s := weightedSelector(nil, nil, mkPeerWithWindow("peer-A", "qwen3:8b-q4_K_M", 0))
	sel, err := s.Select(t.Context(), Request{
		Model: "waired/default", MinContextWindow: hostfit.ServingWindow1M,
	})
	if err != nil {
		t.Fatalf("Select: %v — a silent peer must stay eligible", err)
	}
	if sel.Runtime != "remote:peer-A" {
		t.Errorf("got %q, want the undeclared peer", sel.Runtime)
	}
	sel.Release()
}

// TestSelector_NoTierIsUnfiltered keeps the rule where it belongs. General
// inference and the local /model pin carry no window demand, and a peer
// declaring a small window must still serve them.
func TestSelector_NoTierIsUnfiltered(t *testing.T) {
	s := weightedSelector(nil, nil, mkPeerWithWindow("peer-A", "qwen3:8b-q4_K_M", 32768))
	sel, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.Runtime != "remote:peer-A" {
		t.Errorf("got %q, want peer-A", sel.Runtime)
	}
	sel.Release()
}

// TestSelector_TierWithNoQualifyingPeerFails is the tier's contract on a
// mesh that cannot honour it: selection fails rather than quietly serving
// less, and the Claude intercept's auto mode turns that into the real
// Anthropic API. Answering locally-but-smaller here is precisely the lie
// waired#1031 removes.
func TestSelector_TierWithNoQualifyingPeerFails(t *testing.T) {
	s := weightedSelector(nil, nil,
		mkPeerWithWindow("peer-A", "qwen3:8b-q4_K_M", hostfit.ServingWindow200k),
	)
	_, err := s.Select(t.Context(), Request{
		Model: "waired/default", MinContextWindow: hostfit.ServingWindow1M,
	})
	if err == nil {
		t.Fatal("a 200k mesh served a 1M tier")
	}
	// The model IS present in the mesh — what is missing is the window —
	// so the error must not read as "nobody has this model".
	if errors.Is(err, ErrModelNotFound) {
		t.Errorf("err = %v, want a window-shaped failure", err)
	}
}
