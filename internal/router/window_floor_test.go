package router

import (
	"errors"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// The refusals waired-agent#1395 added, one per way a floor or a pin filter
// ends a selection.

// pinnedWindowFleet pins "pin" (declaring pinWindow) beside "other", which
// serves the same model and holds a 1M window — so the rest of the mesh
// COULD have answered, and a pin that fell through would be observable.
func pinnedWindowFleet(pinWindow int) Inputs {
	snap := inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{
		mkPeerWithWindow("other", "qwen3:8b-q4_K_M", hostfit.ServingWindow1M),
		mkPeerWithWindow("pin", "qwen3:8b-q4_K_M", pinWindow),
	}}
	return Inputs{
		Manifests:          []catalog.Manifest{qwen()},
		LocalState:         emptyState(),
		Hardware:           goodHardware(),
		Runtimes:           registryWithOllama(),
		MeshSnapshotFn:     func() inferencemesh.Snapshot { return snap },
		RoutingMode:        state.RoutingModePinned,
		PinnedPeerDeviceID: "pin",
	}
}

func TestPinDeclined_TheWindowFloorDoesNotSubstituteThePin(t *testing.T) {
	for _, declared := range []int{0, hostfit.ServingWindow200k} {
		_, err := NewSelector(pinnedWindowFleet(declared)).Select(t.Context(),
			Request{Model: "waired/default", MinContextWindow: hostfit.ServingWindow1M})
		e, ok := PinnedPeerDeclined(err)
		if !ok {
			t.Fatalf("declared %d: err = %v, want a *PinnedPeerDeclinedError — the pin must not fall through to the 1M peer beside it", declared, err)
		}
		if e.Reason != PinDeclinedWindow || e.Need != hostfit.ServingWindow1M || e.Declared != declared {
			t.Errorf("declared %d: got %+v", declared, *e)
		}
		if e.PeerDisplayID != "pin" {
			t.Errorf("declared %d: PeerDisplayID = %q, want the pin", declared, e.PeerDisplayID)
		}
		// The pin's refusal is the answer; the window wrapper must not
		// replace it with a sentence about every computer.
		if _, isWindow := WindowFloor(err); isWindow {
			t.Errorf("declared %d: the pin refusal was rewrapped as a mesh-wide window error: %v", declared, err)
		}
	}
}

// The pin is the only peer serving anything: the empty-candidate branch
// (the other of the two pin paths) must refuse the same way.
func TestPinDeclined_AloneOnTheMesh(t *testing.T) {
	in := pinnedWindowFleet(hostfit.ServingWindow200k)
	snap := inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{
		mkPeerWithWindow("pin", "qwen3:32b-q4_K_M", hostfit.ServingWindow200k),
	}}
	in.Manifests = []catalog.Manifest{qwen(), bigQwen()}
	in.MeshSnapshotFn = func() inferencemesh.Snapshot { return snap }
	_, err := NewSelector(in).Select(t.Context(),
		Request{Model: "qwen3-8b-instruct", MinContextWindow: hostfit.ServingWindow1M})
	if e, ok := PinnedPeerDeclined(err); !ok || e.Reason != PinDeclinedWindow {
		t.Fatalf("err = %v, want a window decline", err)
	}
}

func TestPinDeclined_ServingSwitchesAreNamed(t *testing.T) {
	for _, tc := range []struct {
		class, want string
	}{
		{state.ClaudeClassMain, PinDeclinedServeMain},
		{state.ClaudeClassSub, PinDeclinedServeSub},
	} {
		in := pinnedWindowFleet(hostfit.ServingWindow200k)
		snap := in.MeshSnapshotFn()
		snap.Peers[1].InferenceState.ExcludeMain = true
		snap.Peers[1].InferenceState.ExcludeSub = true
		in.MeshSnapshotFn = func() inferencemesh.Snapshot { return snap }
		_, err := NewSelector(in).Select(t.Context(), Request{Model: "waired/default", Class: tc.class})
		e, ok := PinnedPeerDeclined(err)
		if !ok || e.Reason != tc.want {
			t.Errorf("class %q: err = %v, want reason %q", tc.class, err, tc.want)
		}
	}
}

// A model row naming the computer refuses a pin running nothing the catalog
// knows; a `waired worker` pin keeps decision 1900's fallthrough
// (TestWorkerRouting_Pinned_PeerLacksModelSoftFallback holds that half).
func TestPinDeclined_StrictPinRefusesAnUnknownModel(t *testing.T) {
	in := pinnedWindowFleet(hostfit.ServingWindow200k)
	snap := in.MeshSnapshotFn()
	snap.Peers[1].InferenceState.Models = []string{"totally-other-model:7b"}
	in.MeshSnapshotFn = func() inferencemesh.Snapshot { return snap }

	in.PinnedStrict = true
	_, err := NewSelector(in).Select(t.Context(), Request{Model: "waired/default"})
	if e, ok := PinnedPeerDeclined(err); !ok || e.Reason != PinDeclinedUnknownModel {
		t.Fatalf("strict: err = %v, want an unknown_model decline", err)
	}

	in.PinnedStrict = false
	sel, err := NewSelector(in).Select(t.Context(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("worker pin: %v — decision 1900 keeps the fallthrough", err)
	}
	if sel.Runtime != "remote:other" {
		t.Errorf("worker pin: Runtime = %q, want the peer it fell through to", sel.Runtime)
	}
	sel.Release()
}

func TestWindowFloor_LocalOnlyNamesThisComputer(t *testing.T) {
	for _, declared := range []int{0, 262144} {
		s := NewSelector(Inputs{
			Manifests:          []catalog.Manifest{qwen()},
			LocalState:         readyState(),
			Hardware:           goodHardware(),
			Runtimes:           registryWithOllama(),
			RoutingMode:        state.RoutingModeLocalOnly,
			LocalContextWindow: func() int { return declared },
		})
		_, err := s.Select(t.Context(), Request{Model: "waired/default", MinContextWindow: hostfit.ServingWindow1M})
		e, ok := WindowFloor(err)
		if !ok || !e.Local || e.LocalWindow != declared || e.Need != hostfit.ServingWindow1M {
			t.Errorf("declared %d: err = %v (%+v), want a local window refusal", declared, err, e)
		}
	}
}

// With the ranked auto arm, this device is dropped by the floor like any
// peer, and with no peer left the miss is the window's — not "this model is
// not ready here", which is false.
func TestWindowFloor_AutoArmWithNothingLeft(t *testing.T) {
	snap := inferencemesh.Snapshot{SelfDeviceID: "self"}
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     readyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
		LocalNode: func() LocalNode {
			return LocalNode{DeviceID: "self", Serving: true, Runtime: catalog.RuntimeOllama,
				EngineTag: "qwen3:8b-q4_K_M", ModelID: "qwen3-8b-instruct", VariantID: "q4-gguf"}
		},
	})
	_, err := s.Select(t.Context(), Request{Model: "waired/default", MinContextWindow: hostfit.ServingWindow200k})
	e, ok := WindowFloor(err)
	if !ok || e.Local {
		t.Fatalf("err = %v, want a (non-local) window refusal", err)
	}
	if errors.Is(err, ErrModelNotReady) {
		t.Errorf("err = %v still reads as a model that is not ready", err)
	}
}

// A busy mesh is a wait that ends; it is not rewritten as a window refusal
// because some other peer was under the floor.
func TestWindowFloor_LeavesABusyMeshAlone(t *testing.T) {
	tracker := NewInFlightTracker()
	release, _ := tracker.Acquire("big", 1)
	defer release()
	big := mkPeerWithWindow("big", "qwen3:8b-q4_K_M", hostfit.ServingWindow200k)
	big.InferenceState.Capacity = 1
	s := weightedSelector(tracker, nil, big, mkPeerWithWindow("small", "qwen3:8b-q4_K_M", 0))
	_, err := s.Select(t.Context(), Request{Model: "waired/default", MinContextWindow: hostfit.ServingWindow200k})
	if !errors.Is(err, ErrAllPeersOverloaded) {
		t.Fatalf("err = %v, want ErrAllPeersOverloaded", err)
	}
	if _, ok := WindowFloor(err); ok {
		t.Errorf("a busy mesh was reported as a window refusal: %v", err)
	}
}

// Both floors removed something: the operator's own size floor is named
// outermost, and the window is still in the chain.
func TestWindowFloor_SizeFloorStaysOutermost(t *testing.T) {
	s := weightedSelector(nil, nil,
		mkPeerWithWindow("sized-out", "qwen3:8b-q4_K_M", hostfit.ServingWindow200k),
		mkPeerWithWindow("windowed-out", "qwen3:8b-q4_K_M", 0),
	)
	s.in.MinModelSize = hostfit.ModelSizeLarge
	_, err := s.Select(t.Context(), Request{Model: "waired/default", MinContextWindow: hostfit.ServingWindow200k})
	if !BelowModelSizeFloor(err) {
		t.Fatalf("err = %v, want the size floor outermost", err)
	}
	if !errors.Is(err, ErrNoEndpointForWindow) {
		t.Errorf("err = %v lost the window refusal it wraps", err)
	}
}

func TestWindowFloor_PublicOnlySaysPublic(t *testing.T) {
	pub := mkPublicPeer("peer-foreign", "quiet-otter", "qwen3:8b-q4_K_M")
	pub.InferenceState.ContextWindow = 131072
	snap := inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{pub}}
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
		RoutingMode:    state.RoutingModePeerOnly,
		PublicOnly:     true,
		PublicPolicyFn: func() PublicPolicy { return allowAll() },
	})
	_, err := s.Select(t.Context(), Request{Model: "waired/default", MinContextWindow: hostfit.ServingWindow200k})
	e, ok := WindowFloor(err)
	if !ok || !e.Public {
		t.Fatalf("err = %v, want a public window refusal", err)
	}
}
