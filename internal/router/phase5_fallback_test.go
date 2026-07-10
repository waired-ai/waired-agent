package router

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// qwenMulti is a manifest variant of qwen() with both ollama-tag and
// vllm-RepoID variants plus an HF-style alias on the manifest. It
// lets tests assert engine-kind-aware mesh routing and external
// openai-compat fallback (where the LAN endpoint serves the HF id).
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

// externalAdapter is a runtime.Adapter + runtime.ModelLister double
// for testing external fallback without spinning up the openaicompat
// probe loop.
type externalAdapter struct {
	name   string
	models []string
	state  runtime.Health
}

func (e *externalAdapter) Name() string                          { return e.name }
func (e *externalAdapter) BaseURL() string                       { return "http://stub" }
func (e *externalAdapter) Health(context.Context) runtime.Health { return e.state }
func (e *externalAdapter) EnsureRunning(context.Context) error   { return nil }
func (e *externalAdapter) Stop(context.Context) error            { return nil }
func (e *externalAdapter) ListModels() []string                  { return append([]string(nil), e.models...) }

func registryWith(adapters ...runtime.Adapter) *runtime.Registry {
	r := runtime.NewRegistry()
	r.Register(stubAdapter{name: "ollama"})
	for _, a := range adapters {
		r.Register(a)
	}
	return r
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

// TestSelector_MeshFallback_RejectsOpenAICompatTypedPeer: Phase 5
// keeps "openai-compat" as an agent-local-only kind. A peer that
// somehow advertises Type=openai-compat must be ignored by the
// mesh-routing path (defence in depth — the probe does not produce
// this Type today).
func TestSelector_MeshFallback_RejectsOpenAICompatTypedPeer(t *testing.T) {
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
		t.Fatalf("openai-compat peer must not be picked from mesh; got %v", err)
	}
}

// TestSelector_External_PicksReadyAdapter: an external openai-compat
// adapter whose ListModels intersects the manifest's aliases must be
// selected before the mesh fallback runs.
func TestSelector_External_PicksReadyAdapter(t *testing.T) {
	ext := &externalAdapter{
		name:   "openai-compat:lan",
		models: []string{"Qwen/Qwen3-8B-Instruct"},
		state:  runtime.Health{State: runtime.StateReady},
	}
	// Also put a reachable mesh peer in the snapshot — external must win.
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{vllmPeer("peer-V", "Qwen/Qwen3-8B-Instruct", true, false)},
	}
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwenMulti()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWith(ext),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
		AllowExternal:  true,
	})
	sel, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.Runtime != "openai-compat:lan" {
		t.Errorf("Runtime = %q, want openai-compat:lan", sel.Runtime)
	}
	if sel.ExecutionMode != "external" {
		t.Errorf("ExecutionMode = %q, want external", sel.ExecutionMode)
	}
	if sel.EngineModel != "Qwen/Qwen3-8B-Instruct" {
		t.Errorf("EngineModel = %q", sel.EngineModel)
	}
}

// TestSelector_External_SkippedWhenAllowExternalFalse is the loop
// prevention regression: the overlay-side Selector receives the same
// registry but with AllowExternal=false, and must NOT proxy through
// the external endpoint.
func TestSelector_External_SkippedWhenAllowExternalFalse(t *testing.T) {
	ext := &externalAdapter{
		name:   "openai-compat:lan",
		models: []string{"Qwen/Qwen3-8B-Instruct"},
		state:  runtime.Health{State: runtime.StateReady},
	}
	s := NewSelector(Inputs{
		Manifests:     []catalog.Manifest{qwenMulti()},
		LocalState:    emptyState(),
		Hardware:      goodHardware(),
		Runtimes:      registryWith(ext),
		AllowExternal: false,
	})
	_, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if !errors.Is(err, ErrModelNotReady) {
		t.Fatalf("AllowExternal=false must hide external adapters; got %v", err)
	}
}

// TestSelector_External_NotReadyAdapterIgnored: a registered external
// adapter that has not (yet) reached Ready state must not be picked
// — we'd just proxy into a 5xx.
func TestSelector_External_NotReadyAdapterIgnored(t *testing.T) {
	ext := &externalAdapter{
		name:   "openai-compat:lan",
		models: []string{"Qwen/Qwen3-8B-Instruct"},
		state:  runtime.Health{State: runtime.StateStarting},
	}
	s := NewSelector(Inputs{
		Manifests:     []catalog.Manifest{qwenMulti()},
		LocalState:    emptyState(),
		Hardware:      goodHardware(),
		Runtimes:      registryWith(ext),
		AllowExternal: true,
	})
	_, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if !errors.Is(err, ErrModelNotReady) {
		t.Fatalf("Starting-state adapter must not be picked; got %v", err)
	}
}

// TestSelector_External_MatchesByAlias: when the external endpoint
// reports a name listed in manifest.ModelAliases (e.g. an HF id), the
// Selector matches via the alias path.
func TestSelector_External_MatchesByAlias(t *testing.T) {
	// qwenMulti() includes "Qwen/Qwen3-8B-Instruct" as an alias and
	// as variant Source.RepoID — both should match. Use a pure-alias
	// name to exercise the manifest-level path specifically.
	m := qwenMulti()
	m.Variants[1].Source.RepoID = "" // strip the variant-level match
	m.ModelAliases = append(m.ModelAliases, "neural-chat/qwen3-8b")
	ext := &externalAdapter{
		name:   "openai-compat:openai",
		models: []string{"neural-chat/qwen3-8b"},
		state:  runtime.Health{State: runtime.StateReady},
	}
	s := NewSelector(Inputs{
		Manifests:     []catalog.Manifest{m},
		LocalState:    emptyState(),
		Hardware:      goodHardware(),
		Runtimes:      registryWith(ext),
		AllowExternal: true,
	})
	sel, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.Runtime != "openai-compat:openai" || sel.EngineModel != "neural-chat/qwen3-8b" {
		t.Errorf("alias-path Selection wrong: %+v", sel)
	}
	if !strings.Contains(strings.Join(sel.Decision.Reason, " "), "alias") {
		t.Errorf("Decision should record alias match: %v", sel.Decision.Reason)
	}
}

// TestSelector_External_PreferredOverMesh confirms the documented
// priority order: local-ready > external > mesh peers.
func TestSelector_External_PreferredOverMesh(t *testing.T) {
	ext := &externalAdapter{
		name:   "openai-compat:lan",
		models: []string{"Qwen/Qwen3-8B-Instruct"},
		state:  runtime.Health{State: runtime.StateReady},
	}
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{vllmPeer("peer-V", "Qwen/Qwen3-8B-Instruct", true, false)},
	}
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwenMulti()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWith(ext),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
		AllowExternal:  true,
	})
	sel, _ := s.Select(t.Context(), Request{Model: "waired/default"})
	if sel.Runtime != "openai-compat:lan" {
		t.Errorf("external must beat mesh; got Runtime=%q", sel.Runtime)
	}
}

// TestSelector_External_FallsThroughToMesh: when external is enabled
// but no openai-compat adapter is registered, the mesh path still
// runs.
func TestSelector_External_FallsThroughToMesh(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{vllmPeer("peer-V", "Qwen/Qwen3-8B-Instruct", true, false)},
	}
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwenMulti()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
		AllowExternal:  true,
	})
	sel, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.Runtime != "remote:peer-V" {
		t.Errorf("no external adapter → mesh; got %q", sel.Runtime)
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
