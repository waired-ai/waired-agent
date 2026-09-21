package router

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/proto/hostfit"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// A custom model under 200,704 tokens is reachable by the coding-agent rows
// (owner ruling 5 on waired-ai/waired#1473: no minimum window), carrying its
// own window so the overflow guard answers a larger prompt; a computer that
// meets 200,704 is preferred, and the 1M floor stays a floor. Product
// contract: that ruling.

func smallCustomPeer() inferencemesh.PeerView {
	p := customPeer(false)
	p.InferenceState.ContextWindow = 0 // declares nothing below 200,704
	p.InferenceState.CustomModelWindow = 40960
	return p
}

func bundledPeerAt200k() inferencemesh.PeerView {
	return inferencemesh.PeerView{
		DeviceID: "peer-bundled", DeviceName: "peer-bundled",
		InferenceState: &signer.InferenceState{
			Reachable: true, Type: signer.InferenceTypeOllama, Models: []string{"qwen3:8b-q4_K_M"},
			LastCheck: "2026-09-22T00:00:00Z", ContextWindow: hostfit.ServingWindow200k,
		},
	}
}

func windowSelector(peers ...inferencemesh.PeerView) *Selector {
	snap := inferencemesh.Snapshot{Peers: peers}
	return NewSelector(Inputs{
		Manifests:       []catalog.Manifest{qwen(), customTiny()},
		LocalState:      catalog.State{Version: catalog.StateVersion, Models: map[string]catalog.ModelState{}},
		Hardware:        goodHardware(),
		Runtimes:        registryWithOllama(),
		LocalServingOff: true,
		MeshSnapshotFn:  func() inferencemesh.Snapshot { return snap },
	})
}

func TestCustomModelUnderTheCodingWindow(t *testing.T) {
	t.Run("named, it is reached with its own window", func(t *testing.T) {
		sel, err := windowSelector(smallCustomPeer()).Select(t.Context(),
			Request{Model: "custom-tiny-0123abcd", MinContextWindow: hostfit.ServingWindow200k})
		if err != nil || sel.ExecutionMode != "remote" || sel.ContextWindow != 40960 {
			t.Fatalf("sel=%+v err=%v, want the peer at 40,960", sel, err)
		}
	})
	t.Run("unnamed, a computer at 200,704 is preferred", func(t *testing.T) {
		sel, err := windowSelector(smallCustomPeer(), bundledPeerAt200k()).Select(t.Context(),
			Request{Model: DefaultModelAlias, MinContextWindow: hostfit.ServingWindow200k})
		if err != nil || sel.Runtime != "remote:peer-bundled" {
			t.Fatalf("sel=%+v err=%v, want the computer at 200,704", sel, err)
		}
	})
	t.Run("unnamed, it answers when nothing at 200,704 can", func(t *testing.T) {
		sel, err := windowSelector(smallCustomPeer()).Select(t.Context(),
			Request{Model: DefaultModelAlias, MinContextWindow: hostfit.ServingWindow200k})
		if err != nil || sel.Runtime != "remote:peer-custom" || sel.ContextWindow != 40960 {
			t.Fatalf("sel=%+v err=%v", sel, err)
		}
	})
	t.Run("the 1M floor stays a floor", func(t *testing.T) {
		if sel, err := windowSelector(smallCustomPeer()).Select(t.Context(),
			Request{Model: "custom-tiny-0123abcd", MinContextWindow: hostfit.ServingWindow1M}); err == nil {
			t.Fatalf("a 40,960-token model answered a 1M row: %+v", sel)
		}
	})
	t.Run("a catalog model declaring nothing is still refused", func(t *testing.T) {
		p := bundledPeerAt200k()
		p.InferenceState.ContextWindow = 0
		p.InferenceState.CustomModelWindow = 40960 // not a custom model: ignored
		if sel, err := windowSelector(p).Select(t.Context(),
			Request{Model: "qwen3-8b-instruct", MinContextWindow: hostfit.ServingWindow200k}); err == nil {
			t.Fatalf("a catalog model under the floor answered: %+v", sel)
		}
	})
}
