package router

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// ExcludeUnpinned is the account's switch for whether the rows that name
// no computer (Waired / Waired peer) may land on a custom model, resolved
// per recipient by the control plane (waired-ai/waired#1473 ruling 5:
// on by default at 200,704 tokens or more, off below; the team switch is
// the importer's and the team owner or an admin can stop it). Product
// contract: that ruling. A request that names the model, or a pin to the
// computer, is not one of those rows.

const customTag = "hf.co/acme/tiny-GGUF:Q4_K_M"

func customTiny() catalog.Manifest {
	return catalog.Manifest{
		ModelID: "custom-tiny-0123abcd", DisplayName: "Tiny", ContextLength: 262144,
		Capabilities: []string{"chat"},
		Runtime:      catalog.RuntimePolicy{Preferred: catalog.RuntimeOllama},
		Variants: []catalog.Variant{{
			VariantID: "q4-k-m", Format: catalog.FormatOllamaTag, RuntimeSupport: []string{catalog.RuntimeOllama},
			Source: catalog.VariantSource{Type: "ollama", Tag: customTag},
		}},
		ManualOnly: "imported", Provenance: catalog.ProvenanceCustom,
	}
}

func customPeer(exclude bool) inferencemesh.PeerView {
	return inferencemesh.PeerView{
		DeviceID: "peer-custom", DeviceName: "peer-custom",
		InferenceState: &signer.InferenceState{
			Reachable: true, Type: signer.InferenceTypeOllama, Models: []string{customTag},
			LastCheck: "2026-09-22T00:00:00Z", ExcludeUnpinned: exclude,
		},
	}
}

func unpinnedSelector(exclude bool, mode state.RoutingMode, pin string) *Selector {
	snap := inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{customPeer(exclude)}}
	return NewSelector(Inputs{
		Manifests:          []catalog.Manifest{qwen(), customTiny()},
		LocalState:         catalog.State{Version: catalog.StateVersion, Models: map[string]catalog.ModelState{}},
		Hardware:           goodHardware(),
		Runtimes:           registryWithOllama(),
		LocalServingOff:    true,
		MeshSnapshotFn:     func() inferencemesh.Snapshot { return snap },
		RoutingMode:        mode,
		PinnedPeerDeviceID: pin,
	})
}

func TestExcludeUnpinned(t *testing.T) {
	for _, tc := range []struct {
		name    string
		exclude bool
		model   string
		mode    state.RoutingMode
		pin     string
		served  bool
	}{
		{"unnamed row, switch on", false, DefaultModelAlias, "", "", true},
		{"unnamed row, switch off", true, DefaultModelAlias, "", "", false},
		{"the model named", true, "custom-tiny-0123abcd", "", "", true},
		{"the computer pinned", true, DefaultModelAlias, state.RoutingModePinned, "peer-custom", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sel, err := unpinnedSelector(tc.exclude, tc.mode, tc.pin).Select(t.Context(), Request{Model: tc.model})
			got := err == nil && sel.ExecutionMode == "remote"
			if got != tc.served {
				t.Errorf("served=%v (sel=%+v err=%v), want %v", got, sel, err, tc.served)
			}
		})
	}
}

// This computer's own custom model follows the same switch
// (InferenceState.ExcludeUnpinned on its own map entry): a request that
// names neither a model nor a computer does not land on it when the switch
// is off; one that names the model does. Found on real hardware
// (2026-09-22): only peers were filtered.
func TestExcludeUnpinned_ThisComputer(t *testing.T) {
	s := NewSelector(Inputs{Manifests: []catalog.Manifest{qwen(), customTiny()}})
	o, v := wantSetsFor(s.in.Manifests)
	w := meshWant{ollama: o, vllm: v}
	ln := LocalNode{DeviceID: "dev_self", Serving: true, Runtime: catalog.RuntimeOllama,
		EngineTag: customTag, ModelID: "custom-tiny-0123abcd", ContextWindow: 262144, ExcludeUnpinned: true}
	if _, ok, _ := s.buildLocalCandidate(ln, 0, w, true); ok {
		t.Error("an unnamed request landed on this computer's custom model with the switch off")
	}
	if _, ok, _ := s.buildLocalCandidate(ln, 0, w, false); !ok {
		t.Error("a request naming the model was refused")
	}
	ln.ExcludeUnpinned = false
	if _, ok, _ := s.buildLocalCandidate(ln, 0, w, true); !ok {
		t.Error("the switch on, and the unnamed request still refused")
	}
}
