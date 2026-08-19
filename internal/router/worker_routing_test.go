package router

import (
	"errors"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// Worker-pin routing modes: 4-mode locality-filter behaviour matrix.
// These tests cover the manual-routing override described in
// docs/records/20260518/1530-routing-peer-pin-spec.md.
//
// Helpers (qwen / readyState / emptyState / goodHardware /
// registryWithOllama / mkPeer) come from endpoint_router_test.go and
// mesh_fallback_test.go.

// --- local-only -------------------------------------------------------

func TestWorkerRouting_LocalOnly_LocalReadyPicksLocal(t *testing.T) {
	// Even when a peer offers the model, local-only must use local.
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{mkPeer("peer-B", "qwen3:8b-q4_K_M", true, false)},
	}
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     readyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
		RoutingMode:    state.RoutingModeLocalOnly,
	})
	sel, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.ExecutionMode != "local" {
		t.Errorf("local-only + local-ready: ExecutionMode = %q, want local", sel.ExecutionMode)
	}
}

func TestWorkerRouting_LocalOnly_NotReadyErrors(t *testing.T) {
	// local-only must NOT fall through to mesh even with a reachable peer.
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{mkPeer("peer-B", "qwen3:8b-q4_K_M", true, false)},
	}
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
		RoutingMode:    state.RoutingModeLocalOnly,
	})
	_, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if !errors.Is(err, ErrModelNotReady) {
		t.Fatalf("local-only without local-ready must error ErrModelNotReady, got %v", err)
	}
	if !strings.Contains(err.Error(), "local-only") {
		t.Errorf("error message should mention routing=local-only, got %v", err)
	}
}

// --- peer-preferred ---------------------------------------------------

func TestWorkerRouting_PeerPreferred_PrefersMeshOverLocal(t *testing.T) {
	// Inverts auto: even when local is ready, mesh wins.
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{mkPeer("peer-B", "qwen3:8b-q4_K_M", true, false)},
	}
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     readyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
		RoutingMode:    state.RoutingModePeerPreferred,
	})
	sel, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.ExecutionMode != "remote" {
		t.Errorf("peer-preferred with reachable peer should pick remote, got %q", sel.ExecutionMode)
	}
}

func TestWorkerRouting_PeerPreferred_FallsBackToLocalWhenMeshEmpty(t *testing.T) {
	// No peer offers the model: local-ready must still serve.
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{mkPeer("peer-B", "totally-other-model:7b", true, false)},
	}
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     readyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
		RoutingMode:    state.RoutingModePeerPreferred,
	})
	sel, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.ExecutionMode != "local" {
		t.Errorf("peer-preferred + empty-mesh + local-ready should fall back to local, got %q", sel.ExecutionMode)
	}
}

func TestWorkerRouting_PeerPreferred_ErrorsWhenLocalAndMeshBothEmpty(t *testing.T) {
	snap := inferencemesh.Snapshot{} // no peers
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
		RoutingMode:    state.RoutingModePeerPreferred,
	})
	_, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if !errors.Is(err, ErrModelNotReady) {
		t.Fatalf("peer-preferred with nothing serving must error, got %v", err)
	}
}

// --- peer-only --------------------------------------------------------

func TestWorkerRouting_PeerOnly_PrefersMeshOverLocal(t *testing.T) {
	// Like peer-preferred: a ready local model does not win.
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{mkPeer("peer-B", "qwen3:8b-q4_K_M", true, false)},
	}
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     readyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
		RoutingMode:    state.RoutingModePeerOnly,
	})
	sel, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.ExecutionMode != "remote" {
		t.Errorf("peer-only with a serving peer: ExecutionMode = %q, want remote", sel.ExecutionMode)
	}
}

// TestWorkerRouting_PeerOnly_FailsClosedWhenMeshEmpty pins the PRODUCT
// CONTRACT that separates peer-only from peer-preferred (#327): with no
// mesh candidate it errors even though the local engine could serve.
// Falling back would silently undo the operator's "not on this machine"
// choice — the same failure shape as the Claude surface's silent local
// fallback (#325).
func TestWorkerRouting_PeerOnly_FailsClosedWhenMeshEmpty(t *testing.T) {
	snap := inferencemesh.Snapshot{} // no peers
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     readyState(), // local COULD serve — and must not
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
		RoutingMode:    state.RoutingModePeerOnly,
	})
	_, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if !errors.Is(err, ErrModelNotReady) {
		t.Fatalf("peer-only without a mesh candidate must error ErrModelNotReady, got %v", err)
	}
	if !strings.Contains(err.Error(), "peer-only") {
		t.Errorf("error message should mention routing=peer-only, got %v", err)
	}
}

func TestWorkerRouting_PeerOnly_OverlaySideFailsClosed(t *testing.T) {
	// Defensive, and deliberately the opposite of the pinned case: the
	// overlay-side Selector has MeshSnapshotFn=nil, and "serve it locally
	// instead" is precisely what peer-only forbids.
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     readyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: nil,
		RoutingMode:    state.RoutingModePeerOnly,
	})
	_, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if !errors.Is(err, ErrModelNotReady) {
		t.Fatalf("overlay-side peer-only must fail closed, got %v", err)
	}
	if !strings.Contains(err.Error(), "no mesh snapshot") {
		t.Errorf("error should name the missing snapshot, got %v", err)
	}
}

// --- pinned -----------------------------------------------------------

func TestWorkerRouting_Pinned_HoistsPinToHead(t *testing.T) {
	// Two peers both have the model, pin is the SECOND in deterministic
	// order. Without pinning, peer-A (DeviceID < peer-B in score-then-
	// deviceID order) would win. Pin should force peer-B to position 0.
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			mkPeer("peer-A", "qwen3:8b-q4_K_M", true, false),
			mkPeer("peer-B", "qwen3:8b-q4_K_M", true, false),
		},
	}
	s := NewSelector(Inputs{
		Manifests:          []catalog.Manifest{qwen()},
		LocalState:         readyState(), // local-ready, but pin must override
		Hardware:           goodHardware(),
		Runtimes:           registryWithOllama(),
		MeshSnapshotFn:     func() inferencemesh.Snapshot { return snap },
		RoutingMode:        state.RoutingModePinned,
		PinnedPeerDeviceID: "peer-B",
	})
	sel, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.Runtime != "remote:peer-B" {
		t.Errorf("pin to peer-B should win, got Runtime=%q", sel.Runtime)
	}
}

func TestWorkerRouting_Pinned_PeerNotInSnapshotErrors(t *testing.T) {
	// Pin to a peer that's completely absent. Must surface 503
	// ErrPinnedPeerUnreachable rather than silently fall through.
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{mkPeer("peer-A", "qwen3:8b-q4_K_M", true, false)},
	}
	s := NewSelector(Inputs{
		Manifests:          []catalog.Manifest{qwen()},
		LocalState:         emptyState(),
		Hardware:           goodHardware(),
		Runtimes:           registryWithOllama(),
		MeshSnapshotFn:     func() inferencemesh.Snapshot { return snap },
		RoutingMode:        state.RoutingModePinned,
		PinnedPeerDeviceID: "peer-missing",
	})
	_, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if !errors.Is(err, ErrPinnedPeerUnreachable) {
		t.Fatalf("pinned + peer absent must return ErrPinnedPeerUnreachable, got %v", err)
	}
}

func TestWorkerRouting_Pinned_PeerStaleErrors(t *testing.T) {
	// Pin reachable but the inferencemesh aggregator flagged it stale.
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			mkPeer("peer-A", "qwen3:8b-q4_K_M", true, false), // serves model
			mkPeer("peer-B", "qwen3:8b-q4_K_M", true, true),  // pin, stale
		},
	}
	s := NewSelector(Inputs{
		Manifests:          []catalog.Manifest{qwen()},
		LocalState:         emptyState(),
		Hardware:           goodHardware(),
		Runtimes:           registryWithOllama(),
		MeshSnapshotFn:     func() inferencemesh.Snapshot { return snap },
		RoutingMode:        state.RoutingModePinned,
		PinnedPeerDeviceID: "peer-B",
	})
	_, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if !errors.Is(err, ErrPinnedPeerUnreachable) {
		t.Fatalf("pinned + peer stale must return ErrPinnedPeerUnreachable, got %v", err)
	}
}

// TestWorkerRouting_Pinned_ErrorNamesThePeer pins the PRODUCT CONTRACT that
// a strict-pin failure carries the peer's identity: the gateway has no view
// of the routing preference, so without this the operator gets a 503 and an
// empty peer_id and cannot tell which worker is down (waired-agent#325).
func TestWorkerRouting_Pinned_ErrorNamesThePeer(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{mkPeer("peer-A", "qwen3:8b-q4_K_M", true, false)},
	}
	s := NewSelector(Inputs{
		Manifests:          []catalog.Manifest{qwen()},
		LocalState:         emptyState(),
		Hardware:           goodHardware(),
		Runtimes:           registryWithOllama(),
		MeshSnapshotFn:     func() inferencemesh.Snapshot { return snap },
		RoutingMode:        state.RoutingModePinned,
		PinnedPeerDeviceID: "peer-missing",
	})
	// A request that NAMED a model carries it; one that named none has
	// no model to carry, because a pin picks a node and the node's own
	// model is the answer — naming the requester's default here is the
	// confusion waired-agent#828 removed.
	for _, tc := range []struct{ name, model, wantModelID string }{
		{"named model", "qwen3-8b-instruct", "qwen3-8b-instruct"},
		{"no model named", "waired/default", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Select(t.Context(), Request{Model: tc.model})
			var pin *PinnedPeerUnreachableError
			if !errors.As(err, &pin) {
				t.Fatalf("error = %v, want a *PinnedPeerUnreachableError", err)
			}
			if pin.PeerDisplayID != "peer-missing" {
				t.Errorf("PeerDisplayID = %q, want %q", pin.PeerDisplayID, "peer-missing")
			}
			if pin.ModelID != tc.wantModelID {
				t.Errorf("ModelID = %q, want %q", pin.ModelID, tc.wantModelID)
			}
			// The sentinel must keep matching — every gateway mapping uses it.
			if !errors.Is(err, ErrPinnedPeerUnreachable) {
				t.Errorf("errors.Is(err, ErrPinnedPeerUnreachable) = false for %v", err)
			}
		})
	}
}

// TestWorkerRouting_Pinned_PublicPeerNamedByPseudonym pins the §8.5 rule on
// the new error: the identifier it exposes reaches a header, a log line and
// the 503 body, so a Public Share peer must appear as its grant pseudonym
// and never as its real device id.
func TestWorkerRouting_Pinned_PublicPeerNamedByPseudonym(t *testing.T) {
	// The pin is present but stale, so it fails the strict check while
	// still being in the snapshot — the only case where the grant is
	// visible to the error builder.
	pub := mkPublicPeer("peer-foreign", "quiet-otter", "qwen3:8b-q4_K_M")
	pub.Stale = true
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			mkPeer("peer-A", "qwen3:8b-q4_K_M", true, false),
			pub,
		},
	}
	rec := &recordingRecorder{}
	s := NewSelector(Inputs{
		Manifests:          []catalog.Manifest{qwen()},
		LocalState:         emptyState(),
		Hardware:           goodHardware(),
		Runtimes:           registryWithOllama(),
		MeshSnapshotFn:     func() inferencemesh.Snapshot { return snap },
		RoutingMode:        state.RoutingModePinned,
		PinnedPeerDeviceID: "peer-foreign",
		Recorder:           rec,
	})
	_, err := s.Select(t.Context(), Request{Model: "waired/default"})
	var pin *PinnedPeerUnreachableError
	if !errors.As(err, &pin) {
		t.Fatalf("error = %v, want a *PinnedPeerUnreachableError", err)
	}
	if pin.PeerDisplayID != "quiet-otter" {
		t.Errorf("PeerDisplayID = %q, want the grant pseudonym", pin.PeerDisplayID)
	}
	if strings.Contains(err.Error(), "peer-foreign") {
		t.Errorf("error body leaks the real device id: %v", err)
	}
	// The event emitted beside that error goes to the ring the management
	// API serves whole and to agent.log, so it is a surface under the same
	// rule — and it used to carry the raw pin while the error next to it
	// was named correctly (#739).
	got := rec.pinFailureSnapshot()
	if len(got) != 1 {
		t.Fatalf("pin-failure emits = %d, want 1 (%+v)", len(got), got)
	}
	if got[0].peerID != "quiet-otter" {
		t.Errorf("emitted peer id = %q, want the grant pseudonym", got[0].peerID)
	}
}

// The soft-fallback branch emits the same kind of event from a different
// place, and had the same defect.
func TestWorkerRouting_Pinned_PublicPeerLacksModelEventNamesThePseudonym(t *testing.T) {
	// Reachable and serving, but a model the request did not ask for, so
	// the pin cannot be hoisted and the soft-fallback event fires.
	pub := mkPublicPeer("peer-foreign", "quiet-otter", "some-other:tag")
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			mkPeer("peer-A", "qwen3:8b-q4_K_M", true, false),
			pub,
		},
	}
	rec := &recordingRecorder{}
	s := NewSelector(Inputs{
		Manifests:          []catalog.Manifest{qwen()},
		LocalState:         emptyState(),
		Hardware:           goodHardware(),
		Runtimes:           registryWithOllama(),
		MeshSnapshotFn:     func() inferencemesh.Snapshot { return snap },
		RoutingMode:        state.RoutingModePinned,
		PinnedPeerDeviceID: "peer-foreign",
		Recorder:           rec,
	})
	if _, err := s.Select(t.Context(), Request{Model: "waired/default"}); err != nil {
		t.Fatalf("soft fallback should still select a peer: %v", err)
	}
	got := rec.pinFailureSnapshot()
	if len(got) != 1 || got[0].reason != "lacks_model" {
		t.Fatalf("pin-failure emits = %+v, want one lacks_model", got)
	}
	if got[0].peerID != "quiet-otter" {
		t.Errorf("emitted peer id = %q, want the grant pseudonym", got[0].peerID)
	}
}

func TestWorkerRouting_Pinned_PeerLacksModelSoftFallback(t *testing.T) {
	// Pin reachable, but advertising a model the catalog does not know —
	// the one case left where the request still soft-falls to another
	// peer, because there is no model to serve it with here. A pin
	// running a DIFFERENT CATALOG model now serves the request itself
	// (waired-agent#828, TestNodeFirst_PinWinsOverANamedModel); the
	// 2026-05-19 soft-fallback answer was revised on 2026-08-19.
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			mkPeer("peer-A", "qwen3:8b-q4_K_M", true, false),        // serves model
			mkPeer("peer-B", "totally-other-model:7b", true, false), // pin, different model
		},
	}
	s := NewSelector(Inputs{
		Manifests:          []catalog.Manifest{qwen()},
		LocalState:         emptyState(),
		Hardware:           goodHardware(),
		Runtimes:           registryWithOllama(),
		MeshSnapshotFn:     func() inferencemesh.Snapshot { return snap },
		RoutingMode:        state.RoutingModePinned,
		PinnedPeerDeviceID: "peer-B",
	})
	sel, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("pin-lacks-model should soft-fallback, got err %v", err)
	}
	if sel.Runtime != "remote:peer-A" {
		t.Errorf("soft fallback should pick peer-A, got %q", sel.Runtime)
	}
}

func TestWorkerRouting_Pinned_OverlaySideFallsBackToLocal(t *testing.T) {
	// Defensive: overlay-side Selector has MeshSnapshotFn=nil. Even if
	// pinned mode is somehow set there (shouldn't happen, but the agent
	// is one bug-fix away from forgetting), pinned must NOT loop a peer
	// back through the mesh. Local-ready serves; otherwise ErrModelNotReady.
	s := NewSelector(Inputs{
		Manifests:          []catalog.Manifest{qwen()},
		LocalState:         readyState(),
		Hardware:           goodHardware(),
		Runtimes:           registryWithOllama(),
		MeshSnapshotFn:     nil,
		RoutingMode:        state.RoutingModePinned,
		PinnedPeerDeviceID: "peer-B",
	})
	sel, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.ExecutionMode != "local" {
		t.Errorf("overlay-side pinned must fall back to local, got %q", sel.ExecutionMode)
	}
}

// --- auto (regression) ------------------------------------------------

func TestWorkerRouting_Auto_EmptyModeMatchesAutoExplicit(t *testing.T) {
	// The empty RoutingMode (zero value) must be indistinguishable from
	// state.RoutingModeAuto so callers that don't set the field continue
	// to see the historical behaviour.
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{mkPeer("peer-B", "qwen3:8b-q4_K_M", true, false)},
	}
	want := func(in Inputs) string {
		t.Helper()
		s := NewSelector(in)
		sel, err := s.Select(t.Context(), Request{Model: "waired/default"})
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		return sel.ExecutionMode
	}
	implicit := want(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
		// RoutingMode left zero
	})
	explicit := want(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
		RoutingMode:    state.RoutingModeAuto,
	})
	if implicit != explicit {
		t.Errorf("empty mode vs explicit auto mismatch: implicit=%q explicit=%q", implicit, explicit)
	}
}
