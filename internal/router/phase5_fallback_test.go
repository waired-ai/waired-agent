package router

import (
	"errors"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// qwenMulti is a manifest variant of qwen() with both ollama-tag and
// vllm-RepoID variants plus an HF-style alias on the manifest. It
// lets tests assert engine-kind-aware mesh routing: which manifest
// field a peer's advertised model name is compared against depends on
// the engine kind that peer declares.
func qwenMulti() catalog.Manifest {
	return catalog.Manifest{
		ModelID:       "qwen3-8b-instruct",
		ModelAliases:  []string{"waired/default", "Qwen/Qwen3-8B-Instruct"},
		ContextLength: 8192,
		Capabilities:  []string{"chat"},
		Runtime:       catalog.RuntimePolicy{Preferred: catalog.RuntimeOllama},
		Variants: []catalog.Variant{
			{
				VariantID:      "q4-gguf",
				Format:         catalog.FormatOllamaTag,
				RuntimeSupport: []string{catalog.RuntimeOllama},
				MinRAMGB:       12,
				Source:         catalog.VariantSource{Type: catalog.SourceOllama, Tag: "qwen3:8b-q4_K_M"},
				QualityTier:    50,
			},
			{
				VariantID:      "awq-vllm",
				Format:         catalog.FormatSafetensors,
				RuntimeSupport: []string{catalog.RuntimeVLLM},
				MinVRAMMB:      8000,
				Source:         catalog.VariantSource{Type: catalog.SourceHuggingFace, RepoID: "Qwen/Qwen3-8B-Instruct"},
				QualityTier:    60,
			},
		},
	}
}

func vllmPeer(deviceID, repoID string, reachable, stale bool) inferencemesh.PeerView {
	return inferencemesh.PeerView{
		DeviceID:   deviceID,
		DeviceName: deviceID,
		OverlayIP:  "100.96.0.20",
		Stale:      stale,
		InferenceState: &signer.InferenceState{
			Reachable: reachable,
			Type:      signer.InferenceTypeVLLM,
			Models:    []string{repoID},
			LastCheck: "2026-05-09T18:00:00Z",
		},
	}
}

// TestSelector_MeshFallback_AcceptsVLLMPeer: Phase 5 extends mesh
// matching to vllm peers via Source.RepoID. Before Phase 5 this
// would have been rejected outright.
func TestSelector_MeshFallback_AcceptsVLLMPeer(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{vllmPeer("peer-V", "Qwen/Qwen3-8B-Instruct", true, false)},
	}
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwenMulti()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
	})
	sel, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.Runtime != "remote:peer-V" {
		t.Fatalf("Runtime = %q, want remote:peer-V", sel.Runtime)
	}
	if sel.EngineModel != "Qwen/Qwen3-8B-Instruct" {
		t.Errorf("EngineModel = %q, want Qwen/Qwen3-8B-Instruct", sel.EngineModel)
	}
	if sel.VariantID != "awq-vllm" {
		t.Errorf("VariantID = %q, want awq-vllm (vllm variant)", sel.VariantID)
	}
}

// TestSelector_MeshFallback_RejectsVLLMPeerWithOllamaTag: a peer
// declaring Type=vllm but listing an ollama-tag-style name must not
// match — engine-kind dictates which manifest field to compare against.
func TestSelector_MeshFallback_RejectsVLLMPeerWithOllamaTag(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{vllmPeer("peer-V", "qwen3:8b-q4_K_M", true, false)},
	}
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwenMulti()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
	})
	_, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if !errors.Is(err, ErrModelNotReady) {
		t.Fatalf("vllm peer with ollama tag must not match; got %v", err)
	}
}

// TestSelector_MeshFallback_RejectsUnknownTypedPeer: mesh matching
// compares a peer's advertised model names against the manifest field
// its engine kind implies, so a peer declaring a kind this agent does
// not know is ignored rather than matched by some default. The kind
// used here, "openai-compat", is one no agent produces since #490 —
// which is exactly the shape a stale or hostile snapshot would carry.
func TestSelector_MeshFallback_RejectsUnknownTypedPeer(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{{
			DeviceID: "peer-X",
			InferenceState: &signer.InferenceState{
				Reachable: true,
				Type:      "openai-compat",
				Models:    []string{"Qwen/Qwen3-8B-Instruct"},
				LastCheck: "2026-05-09T18:00:00Z",
			},
		}},
	}
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwenMulti()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
	})
	_, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if !errors.Is(err, ErrModelNotReady) {
		t.Fatalf("unknown-kind peer must not be picked from mesh; got %v", err)
	}
}

// TestVariantWantSets exercises the helper directly.
func TestVariantWantSets(t *testing.T) {
	ollama, vllm := variantWantSets(qwenMulti())
	if _, ok := ollama["qwen3:8b-q4_K_M"]; !ok {
		t.Errorf("ollama set missing tag: %v", ollama)
	}
	if _, ok := vllm["Qwen/Qwen3-8B-Instruct"]; !ok {
		t.Errorf("vllm set missing repo id: %v", vllm)
	}
	if _, ok := ollama["Qwen/Qwen3-8B-Instruct"]; ok {
		t.Errorf("HF id leaked into ollama set: %v", ollama)
	}
}

// silence unused warnings in case hardware import isn't otherwise
// needed during edits.
var _ = hardware.Profile{}
