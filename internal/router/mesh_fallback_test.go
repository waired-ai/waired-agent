package router

import (
	"errors"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// emptyState is a State with no models cached locally — Step 3
// rejects everything as ErrModelNotReady, so the mesh fallback (or
// loop-prevention nil-snapshot) is exercised.
func emptyState() catalog.State {
	return catalog.State{
		Version:   catalog.StateVersion,
		Models:    map[string]catalog.ModelState{},
		Endpoints: map[string]catalog.EndpointState{},
	}
}

func mkPeer(deviceID, tag string, reachable, stale bool) inferencemesh.PeerView {
	return inferencemesh.PeerView{
		DeviceID:   deviceID,
		DeviceName: deviceID,
		OverlayIP:  "100.96.0.10",
		Stale:      stale,
		InferenceState: &signer.InferenceState{
			Reachable: reachable,
			Type:      signer.InferenceTypeOllama,
			Models:    []string{tag},
			LastCheck: "2026-05-09T18:00:00Z",
		},
	}
}

func TestSelector_MeshFallback_HappyPath(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			mkPeer("peer-B", "qwen3:8b-q4_K_M", true, false),
		},
	}
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     emptyState(), // local model not ready
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
	})
	sel, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.Runtime != "remote:peer-B" {
		t.Fatalf("Runtime = %q, want remote:peer-B", sel.Runtime)
	}
	if sel.ExecutionMode != "remote" {
		t.Fatalf("ExecutionMode = %q, want remote", sel.ExecutionMode)
	}
	if sel.EngineModel != "qwen3:8b-q4_K_M" {
		t.Fatalf("EngineModel = %q", sel.EngineModel)
	}
	// EndpointID encodes the peer for traceability.
	if !strings.Contains(sel.EndpointID, "peer_b") {
		t.Fatalf("EndpointID must encode the peer; got %q", sel.EndpointID)
	}
}

func TestSelector_MeshFallback_NilSnapshotPreservesLocalOnly(t *testing.T) {
	// Loop prevention: overlay-side Selector receives nil
	// MeshSnapshotFn and must NOT recurse to a peer.
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: nil,
	})
	_, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if !errors.Is(err, ErrModelNotReady) {
		t.Fatalf("nil snapshot must keep local-only behaviour; got %v", err)
	}
}

func TestSelector_MeshFallback_PrefersLocalWhenReady(t *testing.T) {
	// Even with a peer offering the engine tag, a local-ready model
	// keeps the local path winning. Phase 4 only kicks in when local
	// is not ready (the Step 3 fall-through).
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{mkPeer("peer-B", "qwen3:8b-q4_K_M", true, false)},
	}
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     readyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
	})
	sel, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.ExecutionMode != "local" {
		t.Fatalf("local-ready must prefer local; got %q", sel.ExecutionMode)
	}
}

func TestSelector_MeshFallback_RejectsStalePeer(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{mkPeer("peer-B", "qwen3:8b-q4_K_M", true, true)},
	}
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
	})
	_, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if !errors.Is(err, ErrModelNotReady) {
		t.Fatalf("stale peer must not be picked; got %v", err)
	}
}

func TestSelector_MeshFallback_RejectsUnreachablePeer(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{mkPeer("peer-B", "qwen3:8b-q4_K_M", false, false)},
	}
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
	})
	_, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if !errors.Is(err, ErrModelNotReady) {
		t.Fatalf("unreachable peer must not be picked; got %v", err)
	}
}

func TestSelector_MeshFallback_RejectsTagMismatch(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{mkPeer("peer-B", "llama3.1:8b", true, false)},
	}
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
	})
	_, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if !errors.Is(err, ErrModelNotReady) {
		t.Fatalf("non-matching engine tag must not be picked; got %v", err)
	}
}

func TestSelector_MeshFallback_DeterministicTieBreak(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			mkPeer("peer-Z", "qwen3:8b-q4_K_M", true, false),
			mkPeer("peer-A", "qwen3:8b-q4_K_M", true, false),
			mkPeer("peer-M", "qwen3:8b-q4_K_M", true, false),
		},
	}
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
	})
	sel, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.Runtime != "remote:peer-A" {
		t.Fatalf("tie-break must pick lowest device id; got %q", sel.Runtime)
	}
}

func TestSelector_MeshFallback_RejectsVLLMPeerForOllamaOnlyManifest(t *testing.T) {
	// Phase 5: vLLM peers DO participate in mesh routing, but they
	// must match against variant.Source.RepoID (the HF id vLLM
	// serves), not the ollama-tag form. qwen() in this test has only
	// an ollama-tag variant — so the vllm-typed peer carrying an
	// ollama-tag string has no matching variant to land on.
	// (See phase5_fallback_test.go for the positive vLLM cases.)
	state := &signer.InferenceState{
		Reachable: true,
		Type:      signer.InferenceTypeVLLM,
		Models:    []string{"qwen3:8b-q4_K_M"},
		LastCheck: "2026-05-09T18:00:00Z",
	}
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{{
			DeviceID:       "peer-vllm",
			InferenceState: state,
		}},
	}
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
	})
	_, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if !errors.Is(err, ErrModelNotReady) {
		t.Fatalf("vLLM peer with ollama-tag must not match an ollama-only manifest; got %v", err)
	}
}
