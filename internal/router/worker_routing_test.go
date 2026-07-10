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

func TestWorkerRouting_Pinned_PeerLacksModelSoftFallback(t *testing.T) {
	// Pin reachable, but serves a different model. Per user-confirmed
	// spec decision, this is a SOFT fallback: route to another peer that
	// does serve the model. Pin emphasises "use this GPU machine", not
	// "use this exact (peer, model)" tuple.
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
