package modelrows

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/proto/signer"
)

func peerView(name, deviceID, tag string, reachable bool) inferencemesh.PeerView {
	return inferencemesh.PeerView{
		DeviceID:   deviceID,
		DeviceName: name,
		InferenceState: &signer.InferenceState{
			Reachable:   reachable,
			Type:        signer.InferenceTypeOllama,
			Models:      []string{tag},
			ActiveModel: "qwen3.5-4b",
		},
	}
}

func TestFactsFromSnapshot(t *testing.T) {
	t.Run("only serving peers get a row", func(t *testing.T) {
		// The picker cannot grey a row out — every gateway row renders
		// identically — so a peer that cannot answer must not be offered.
		unreachable := peerView("sleeping-box", "dev_b", "qwen3.5:4b", false)
		noModel := peerView("empty-box", "dev_c", "", true)
		noModel.InferenceState.Models = nil
		snap := &inferencemesh.Snapshot{
			Self: peerView("me", "dev_self", "qwen3.5:4b", true),
			Peers: []inferencemesh.PeerView{
				peerView("linux-gpu", "dev_a", "qwen3.5:4b", true),
				unreachable,
				noModel,
			},
		}
		f := FactsFromSnapshot(snap, 5, false)
		if len(f.Peers) != 1 || f.Peers[0].DisplayID != "linux-gpu" {
			t.Errorf("peers = %+v, want only the serving one", f.Peers)
		}
		if f.Peers[0].Model != "qwen3.5-4b" {
			t.Errorf("model = %q, want the catalog id rather than the engine tag", f.Peers[0].Model)
		}
	})

	t.Run("a public machine is named by its pseudonym, never its device name", func(t *testing.T) {
		stranger := peerView("stranger-workstation", "dev_pub", "qwen3.5:4b", true)
		stranger.Grant = &signer.PeerGrant{ID: "g1", Kind: "public", Role: "provider", Pseudonym: "guest-a7f3"}
		nameless := peerView("another-stranger", "dev_pub2", "qwen3.5:4b", true)
		nameless.Grant = &signer.PeerGrant{ID: "g2", Kind: "public", Role: "provider"}

		f := FactsFromSnapshot(&inferencemesh.Snapshot{
			Peers: []inferencemesh.PeerView{stranger, nameless},
		}, 5, false)
		if len(f.Peers) != 1 {
			t.Fatalf("peers = %+v, want only the one with a pseudonym", f.Peers)
		}
		if f.Peers[0].DisplayID != "guest-a7f3" {
			t.Errorf("DisplayID = %q, want the pseudonym", f.Peers[0].DisplayID)
		}
		for _, p := range f.Peers {
			if strings.Contains(p.DisplayID, "stranger") {
				t.Errorf("a stranger's machine name reached the picker: %q", p.DisplayID)
			}
		}
	})

	t.Run("a 1M window on a node is what earns its twin", func(t *testing.T) {
		wide := peerView("big-box", "dev_w", "qwen3.5:4b", true)
		wide.InferenceState.ContextWindow = 1048576
		narrow := peerView("small-box", "dev_n", "qwen3.5:4b", true)
		narrow.InferenceState.ContextWindow = 131072

		f := FactsFromSnapshot(&inferencemesh.Snapshot{
			Self:  narrow,
			Peers: []inferencemesh.PeerView{wide, narrow},
		}, 5, false)
		if f.LocalWindow1M {
			t.Error("this computer declares 131072 and still got a 1M twin")
		}
		if !f.PeerWindow1M {
			t.Error("a peer declares 1M and the peer row got no twin")
		}
		for _, p := range f.Peers {
			if (p.DisplayID == "big-box") != p.Window1M {
				t.Errorf("peer %q Window1M = %v", p.DisplayID, p.Window1M)
			}
		}

		// A node that publishes no window declares nothing, which is not the
		// same as declaring a small one — but it earns no twin either.
		silent := peerView("quiet-box", "dev_q", "qwen3.5:4b", true)
		g := FactsFromSnapshot(&inferencemesh.Snapshot{
			Self: silent, Peers: []inferencemesh.PeerView{silent},
		}, 5, false)
		if g.LocalWindow1M || g.PeerWindow1M {
			t.Error("a node that publishes no window was read as declaring 1M")
		}
	})

	// waired-agent#1177. SubsystemStateDisabled is the operator's intent on
	// the wire, and the daemon sets it ahead of engine health.
	t.Run("local inference turned off drops the local row", func(t *testing.T) {
		off := peerView("me", "dev_self", "qwen3.5:4b", true)
		off.InferenceState.SubsystemState = signer.SubsystemStateDisabled
		if FactsFromSnapshot(&inferencemesh.Snapshot{Self: off}, 0, false).LocalServes {
			t.Error("an operator turned local inference off and the row is still offered")
		}
		// And it takes the twin with it, whatever the engine last declared.
		off.InferenceState.ContextWindow = 1048576
		if FactsFromSnapshot(&inferencemesh.Snapshot{Self: off}, 0, false).LocalWindow1M {
			t.Error("a row that is not offered still got a 1M twin")
		}
	})

	t.Run("this computer's engine decides the local row", func(t *testing.T) {
		with := &inferencemesh.Snapshot{Self: peerView("me", "dev_self", "qwen3.5:4b", true)}
		if !FactsFromSnapshot(with, 0, false).LocalServes {
			t.Error("an engine is present; the local row belongs")
		}
		// Stopped, not absent: still this computer's engine.
		stopped := &inferencemesh.Snapshot{Self: peerView("me", "dev_self", "qwen3.5:4b", false)}
		if !FactsFromSnapshot(stopped, 0, false).LocalServes {
			t.Error("a stopped engine must not make the local row flicker away")
		}
		none := &inferencemesh.Snapshot{Self: inferencemesh.PeerView{DeviceID: "dev_self"}} //nolint:staticcheck // reads clearer spelled out
		if FactsFromSnapshot(none, 0, false).LocalServes {
			t.Error("no engine at all means the local row names something impossible")
		}
		typeNone := &inferencemesh.Snapshot{Self: peerView("me", "dev_self", "", true)}
		typeNone.Self.InferenceState.Type = "none"
		if FactsFromSnapshot(typeNone, 0, false).LocalServes {
			t.Error(`engine type "none" is the reported form of the same thing`)
		}
	})

	// A failed READ must not be read as a fact about this computer.
	t.Run("no snapshot keeps the local row", func(t *testing.T) {
		f := FactsFromSnapshot(nil, 5, false)
		if !f.LocalServes {
			t.Error("a mesh read that failed must not remove the local entry")
		}
		if len(f.Peers) != 0 {
			t.Errorf("peers = %+v, want none", f.Peers)
		}
	})
}
