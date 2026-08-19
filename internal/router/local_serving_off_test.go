package router

import (
	"errors"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// LocalServingOff is the operator's local-inference toggle, and a host
// that never installed an engine (waired-agent#829). It used to be a
// middleware in front of the whole gateway, which meant an engine-less
// node could not reach the mesh at all. Here it removes the local
// candidate and nothing else.

func offSelectorWith(snap *inferencemesh.Snapshot, mode state.RoutingMode) *Selector {
	in := Inputs{
		Manifests:       []catalog.Manifest{qwen()},
		LocalState:      readyState(), // weights ARE ready — the toggle is what stops us
		Hardware:        goodHardware(),
		Runtimes:        registryWithOllama(),
		LocalServingOff: true,
		RoutingMode:     mode,
	}
	if snap != nil {
		in.MeshSnapshotFn = func() inferencemesh.Snapshot { return *snap }
	}
	return NewSelector(in)
}

func TestLocalServingOff_MeshStillServes(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{mkPeer("peer-B", "qwen3:8b-q4_K_M", true, false)},
	}
	sel, err := offSelectorWith(&snap, "").Select(t.Context(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("a host with local inference off must still reach the mesh: %v", err)
	}
	if sel.ExecutionMode != "remote" || sel.Runtime != "remote:peer-B" {
		t.Fatalf("ExecutionMode=%q Runtime=%q, want remote / remote:peer-B", sel.ExecutionMode, sel.Runtime)
	}
}

// The local candidate really is gone: with the toggle off and a ready
// model on disk, auto must not pick this host.
func TestLocalServingOff_LocalCandidateIsWithdrawn(t *testing.T) {
	sel, err := offSelectorWith(nil, "").Select(t.Context(), Request{Model: "waired/default"})
	if err == nil {
		t.Fatalf("ready weights must not serve while local inference is off; got %+v", sel)
	}
	if !errors.Is(err, ErrLocalInferenceOff) {
		t.Fatalf("err = %v, want ErrLocalInferenceOff", err)
	}
}

// Tripwire for the assertion above: without the toggle the very same
// inputs serve locally, so the test is measuring the toggle and not an
// unrelated hole in the fixture.
func TestLocalServingOff_SameInputsServeLocallyWhenOn(t *testing.T) {
	s := NewSelector(Inputs{
		Manifests:  []catalog.Manifest{qwen()},
		LocalState: readyState(),
		Hardware:   goodHardware(),
		Runtimes:   registryWithOllama(),
	})
	sel, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.ExecutionMode != "local" {
		t.Fatalf("ExecutionMode = %q, want local", sel.ExecutionMode)
	}
}

// The toggle is named as the reason, not the weights: "not in ready
// state on disk" about a model that IS ready sends the operator to
// `waired models`, and the fix is `waired inference on`.
func TestLocalServingOff_NamesTheToggleNotTheWeights(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode state.RoutingMode
	}{
		{"auto", ""},
		{"local-only", state.RoutingModeLocalOnly},
		{"peer-preferred", state.RoutingModePeerPreferred},
	} {
		t.Run(tc.name, func(t *testing.T) {
			empty := inferencemesh.Snapshot{}
			_, err := offSelectorWith(&empty, tc.mode).Select(t.Context(), Request{Model: "waired/default"})
			if !errors.Is(err, ErrLocalInferenceOff) {
				t.Fatalf("err = %v, want ErrLocalInferenceOff", err)
			}
			if strings.Contains(err.Error(), "ready state on disk") {
				t.Errorf("error blames the weights: %q", err)
			}
		})
	}
}

// peer-only and pinned never consulted the local candidate, so the
// toggle must not change what they return — a pin that is down is still
// a pin that is down.
func TestLocalServingOff_LeavesMeshOnlyModesAlone(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{mkPeer("peer-B", "qwen3:8b-q4_K_M", true, false)},
	}
	in := Inputs{
		Manifests:          []catalog.Manifest{qwen()},
		LocalState:         readyState(),
		Hardware:           goodHardware(),
		Runtimes:           registryWithOllama(),
		LocalServingOff:    true,
		MeshSnapshotFn:     func() inferencemesh.Snapshot { return snap },
		RoutingMode:        state.RoutingModePinned,
		PinnedPeerDeviceID: "peer-GONE",
	}
	_, err := NewSelector(in).Select(t.Context(), Request{Model: "waired/default"})
	if !errors.Is(err, ErrPinnedPeerUnreachable) {
		t.Fatalf("err = %v, want ErrPinnedPeerUnreachable", err)
	}
}
