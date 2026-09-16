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

// TestSelector_TierSkipsAPeerThatDeclaresNothing: a peer declaring 0 is not
// an answer to a tier. It used to be kept, for agents predating the field —
// and that kept every computer serving under the smallest declarable window,
// which declares 0 rather than a smaller number, so a gpt-oss host answered
// the 200k row (waired-agent#1395).
func TestSelector_TierSkipsAPeerThatDeclaresNothing(t *testing.T) {
	for _, floor := range []int{hostfit.ServingWindow200k, hostfit.ServingWindow1M} {
		s := weightedSelector(nil, nil, mkPeerWithWindow("peer-A", "qwen3:8b-q4_K_M", 0))
		_, err := s.Select(t.Context(), Request{
			Model: "waired/default", MinContextWindow: floor,
		})
		e, ok := WindowFloor(err)
		if !ok {
			t.Fatalf("floor %d: err = %v, want a *WindowFloorError", floor, err)
		}
		if e.Need != floor || e.Local || e.Public {
			t.Errorf("floor %d: got %+v", floor, *e)
		}
		if !errors.Is(err, ErrNoEndpointForWindow) {
			t.Errorf("floor %d: err = %v does not match ErrNoEndpointForWindow", floor, err)
		}
	}
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
// less, and the Claude surface turns that into an error naming the window.
// Answering locally-but-smaller here is precisely the lie waired#1031
// removes.
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
	// so the error must not read as "nobody has this model". It used to be
	// the generic mesh miss, which named neither (waired-agent#1395).
	if errors.Is(err, ErrModelNotFound) || errors.Is(err, ErrModelNotReady) {
		t.Errorf("err = %v, want a window-shaped failure", err)
	}
	if !errors.Is(err, ErrNoEndpointForWindow) {
		t.Errorf("err = %v, want ErrNoEndpointForWindow", err)
	}
}
