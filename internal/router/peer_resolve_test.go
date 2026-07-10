package router

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// bigQwen is a second, stronger manifest so tests can assert the
// strongest-model pick when a peer advertises several catalog models.
func bigQwen() catalog.Manifest {
	return catalog.Manifest{
		ModelID:       "qwen3-32b-instruct",
		ContextLength: 32768,
		Capabilities:  []string{"chat"},
		Runtime:       catalog.RuntimePolicy{Preferred: catalog.RuntimeOllama},
		Variants: []catalog.Variant{{
			VariantID:        "q4-gguf",
			Format:           catalog.FormatOllamaTag,
			RuntimeSupport:   []string{catalog.RuntimeOllama},
			ParamCount:       32,
			QuantizationTier: 4,
			Source:           catalog.VariantSource{Type: "ollama", Tag: "qwen3:32b-q4_K_M"},
		}},
	}
}

func vllmManifest() catalog.Manifest {
	return catalog.Manifest{
		ModelID:      "qwen3-8b-vllm",
		Capabilities: []string{"chat"},
		Runtime:      catalog.RuntimePolicy{Preferred: catalog.RuntimeVLLM},
		Variants: []catalog.Variant{{
			VariantID:      "bf16",
			RuntimeSupport: []string{catalog.RuntimeVLLM},
			Source:         catalog.VariantSource{Type: "hf", RepoID: "Qwen/Qwen3-8B-Instruct"},
		}},
	}
}

func TestResolveModelForPeer_OllamaTagMatch(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			mkPeer("peer-B", "qwen3:8b-q4_K_M", true, false),
		},
	}
	m, ok := ResolveModelForPeer([]catalog.Manifest{qwen()}, snap, "peer-B")
	if !ok {
		t.Fatal("expected a match for peer-B's advertised ollama tag")
	}
	if m.ModelID != "qwen3-8b-instruct" {
		t.Fatalf("ModelID = %q, want qwen3-8b-instruct", m.ModelID)
	}
}

func TestResolveModelForPeer_PicksStrongestWhenMultipleMatch(t *testing.T) {
	peer := mkPeer("peer-B", "qwen3:8b-q4_K_M", true, false)
	peer.InferenceState.Models = []string{"qwen3:8b-q4_K_M", "qwen3:32b-q4_K_M"}
	snap := inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{peer}}

	// Catalog order deliberately lists the small model first: the pick
	// must be score-based (ParamCount×QuantizationTier), not
	// first-in-catalog, because the tiering use case is "route main to
	// the strong model on that node".
	m, ok := ResolveModelForPeer([]catalog.Manifest{qwen(), bigQwen()}, snap, "peer-B")
	if !ok {
		t.Fatal("expected a match")
	}
	if m.ModelID != "qwen3-32b-instruct" {
		t.Fatalf("ModelID = %q, want the strongest match qwen3-32b-instruct", m.ModelID)
	}
}

func TestResolveModelForPeer_VLLMRepoIDMatch(t *testing.T) {
	peer := inferencemesh.PeerView{
		DeviceID: "peer-V",
		InferenceState: &signer.InferenceState{
			Reachable: true,
			Type:      signer.InferenceTypeVLLM,
			Models:    []string{"Qwen/Qwen3-8B-Instruct"},
		},
	}
	snap := inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{peer}}
	m, ok := ResolveModelForPeer([]catalog.Manifest{qwen(), vllmManifest()}, snap, "peer-V")
	if !ok {
		t.Fatal("expected a vllm RepoID match")
	}
	if m.ModelID != "qwen3-8b-vllm" {
		t.Fatalf("ModelID = %q, want qwen3-8b-vllm", m.ModelID)
	}
}

func TestResolveModelForPeer_UnusablePeers(t *testing.T) {
	cases := []struct {
		name string
		snap inferencemesh.Snapshot
		id   string
	}{
		{"absent", inferencemesh.Snapshot{}, "peer-B"},
		{"stale", inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{
			mkPeer("peer-B", "qwen3:8b-q4_K_M", true, true)}}, "peer-B"},
		{"unreachable", inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{
			mkPeer("peer-B", "qwen3:8b-q4_K_M", false, false)}}, "peer-B"},
		{"no inference state", inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{
			{DeviceID: "peer-B"}}}, "peer-B"},
		{"engine none", inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{
			{DeviceID: "peer-B", InferenceState: &signer.InferenceState{
				Reachable: true, Type: signer.InferenceTypeNone}}}}, "peer-B"},
		{"no catalog match", inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{
			mkPeer("peer-B", "unknown:tag", true, false)}}, "peer-B"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := ResolveModelForPeer([]catalog.Manifest{qwen()}, tc.snap, tc.id); ok {
				t.Fatal("expected no match")
			}
		})
	}
}

func TestResolveModelForPeer_EmptyTypeDefaultsToOllama(t *testing.T) {
	peer := mkPeer("peer-B", "qwen3:8b-q4_K_M", true, false)
	peer.InferenceState.Type = ""
	snap := inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{peer}}
	if _, ok := ResolveModelForPeer([]catalog.Manifest{qwen()}, snap, "peer-B"); !ok {
		t.Fatal("empty engine type must default to ollama (mirrors buildMeshCandidates)")
	}
}

func TestResolveModelForPeer_DeterministicTieBreak(t *testing.T) {
	// Two manifests with identical scores matching the same peer: the
	// lexicographically-smaller ModelID must win, every time.
	a, b := qwen(), qwen()
	a.ModelID = "aaa-model"
	b.ModelID = "bbb-model"
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{mkPeer("peer-B", "qwen3:8b-q4_K_M", true, false)},
	}
	for range 5 {
		m, ok := ResolveModelForPeer([]catalog.Manifest{b, a}, snap, "peer-B")
		if !ok || m.ModelID != "aaa-model" {
			t.Fatalf("tie-break must pick aaa-model deterministically; got %q ok=%v", m.ModelID, ok)
		}
	}
}
