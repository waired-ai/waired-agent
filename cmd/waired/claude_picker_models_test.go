package main

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
	"github.com/waired-ai/waired-agent/proto/signer"
)

func ids(models []claudecode.GatewayCacheModel) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.ID)
	}
	return out
}

func has(models []claudecode.GatewayCacheModel, id string) bool {
	for _, m := range models {
		if m.ID == id {
			return true
		}
	}
	return false
}

// PIN: product contract — the local row is dropped on a computer with no AI
// engine by owner ruling (2026-08-20, waired-ai/waired#1223: the request-only
// node must be able to pick something other than local). The rest is a record
// of today's rendering.
func TestPickerModels(t *testing.T) {
	peers := []claudecode.PeerFact{
		{DisplayID: "linux-gpu", Model: "qwen3.5-35b-a3b"},
		{DisplayID: "studio-mac", Model: "qwen3.5-4b"},
	}

	t.Run("fixed entries, then one row per peer", func(t *testing.T) {
		got := pickerModels(pickerModelFacts{
			engineUsable: true, publicShareOn: true, peers: peers, peerLimit: 5,
		})
		want := append(ids(claudecode.DirectiveCacheModels()),
			"claude-waired-peer-linux-gpu", "claude-waired-peer-studio-mac")
		if strings.Join(ids(got), ",") != strings.Join(want, ",") {
			t.Errorf("ids =\n %v\nwant\n %v", ids(got), want)
		}
	})

	// PIN: product contract — owner ruling 2026-08-20 (waired-agent#901).
	// The picker cannot render a row as disabled, so a host that has not
	// enabled Public Share must not be shown a choice it cannot take.
	t.Run("the public entry is absent until Public Share is on", func(t *testing.T) {
		off := pickerModels(pickerModelFacts{engineUsable: true, peers: peers, peerLimit: 5})
		if has(off, claudecode.DirectiveModelPublic) {
			t.Error("a host with Public Share off is offered someone else's computer")
		}
		// Everything else is unaffected — this removes one row, not a family.
		for _, id := range []string{
			claudecode.DirectiveModelPeer, claudecode.DirectiveModelLocal,
			claudecode.DirectiveModelAuto, "claude-waired-peer-linux-gpu",
		} {
			if !has(off, id) {
				t.Errorf("%q must survive with Public Share off", id)
			}
		}
		on := pickerModels(pickerModelFacts{engineUsable: true, publicShareOn: true, peerLimit: 0})
		if !has(on, claudecode.DirectiveModelPublic) {
			t.Error("a host with Public Share on is not offered it")
		}
	})

	t.Run("no engine here drops the local row and nothing else", func(t *testing.T) {
		got := pickerModels(pickerModelFacts{engineUsable: false, publicShareOn: true, peers: peers, peerLimit: 5})
		if has(got, claudecode.DirectiveModelLocal) {
			t.Error("a computer with no engine still offers to run the model itself")
		}
		// The point of dropping it is that the OTHER choices remain.
		for _, id := range []string{
			claudecode.DirectiveModelPeer,
			claudecode.DirectiveModelAuto,
			claudecode.DirectiveModelAuto1M,
			"claude-waired-peer-linux-gpu",
		} {
			if !has(got, id) {
				t.Errorf("%q must survive on an engine-less computer", id)
			}
		}
	})

	t.Run("peers off leaves the fixed entries exactly as they were", func(t *testing.T) {
		got := pickerModels(pickerModelFacts{engineUsable: true, publicShareOn: true, peerLimit: 0})
		if strings.Join(ids(got), ",") != strings.Join(ids(claudecode.DirectiveCacheModels()), ",") {
			t.Errorf("ids = %v, want the fixed table unchanged", ids(got))
		}
	})

	// waired-agent#1037: picking a real Anthropic model in /model now reaches
	// the real Anthropic API on its own, and says which model answers besides.
	// The row said the same thing with less information, and the picker folds
	// at about four Waired rows.
	t.Run("the retired cloud row is not written into the picker", func(t *testing.T) {
		got := pickerModels(pickerModelFacts{engineUsable: true, publicShareOn: true, peers: peers, peerLimit: 5})
		if has(got, claudecode.DirectiveModelCloud) {
			t.Error("the cloud row is back in the picker; it is routed for the sessions that hold it, not offered")
		}
	})

	// WriteGatewayCache refuses an empty list, and that refusal is what keeps a
	// stale file in place. The fixed prefix is what guarantees we never hand it
	// an empty one.
	t.Run("never empty, even with no engine and no peers", func(t *testing.T) {
		if got := pickerModels(pickerModelFacts{}); len(got) == 0 {
			t.Error("an empty list would leave the previous cache in place, dead entries and all")
		}
	})
}

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

func TestPickerFactsFromSnapshot(t *testing.T) {
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
		f := pickerFactsFromSnapshot(snap, 5)
		if len(f.peers) != 1 || f.peers[0].DisplayID != "linux-gpu" {
			t.Errorf("peers = %+v, want only the serving one", f.peers)
		}
		if f.peers[0].Model != "qwen3.5-4b" {
			t.Errorf("model = %q, want the catalog id rather than the engine tag", f.peers[0].Model)
		}
	})

	t.Run("a public machine is named by its pseudonym, never its device name", func(t *testing.T) {
		stranger := peerView("stranger-workstation", "dev_pub", "qwen3.5:4b", true)
		stranger.Grant = &signer.PeerGrant{ID: "g1", Kind: "public", Role: "provider", Pseudonym: "guest-a7f3"}
		nameless := peerView("another-stranger", "dev_pub2", "qwen3.5:4b", true)
		nameless.Grant = &signer.PeerGrant{ID: "g2", Kind: "public", Role: "provider"}

		f := pickerFactsFromSnapshot(&inferencemesh.Snapshot{
			Peers: []inferencemesh.PeerView{stranger, nameless},
		}, 5)
		if len(f.peers) != 1 {
			t.Fatalf("peers = %+v, want only the one with a pseudonym", f.peers)
		}
		if f.peers[0].DisplayID != "guest-a7f3" {
			t.Errorf("DisplayID = %q, want the pseudonym", f.peers[0].DisplayID)
		}
		for _, p := range f.peers {
			if strings.Contains(p.DisplayID, "stranger") {
				t.Errorf("a stranger's machine name reached the picker: %q", p.DisplayID)
			}
		}
	})

	t.Run("this computer's engine decides the local row", func(t *testing.T) {
		with := &inferencemesh.Snapshot{Self: peerView("me", "dev_self", "qwen3.5:4b", true)}
		if !pickerFactsFromSnapshot(with, 0).engineUsable {
			t.Error("an engine is present; the local row belongs")
		}
		// Stopped, not absent: still this computer's engine.
		stopped := &inferencemesh.Snapshot{Self: peerView("me", "dev_self", "qwen3.5:4b", false)}
		if !pickerFactsFromSnapshot(stopped, 0).engineUsable {
			t.Error("a stopped engine must not make the local row flicker away")
		}
		none := &inferencemesh.Snapshot{Self: inferencemesh.PeerView{DeviceID: "dev_self"}}
		if pickerFactsFromSnapshot(none, 0).engineUsable {
			t.Error("no engine at all means the local row names something impossible")
		}
		typeNone := &inferencemesh.Snapshot{Self: peerView("me", "dev_self", "", true)}
		typeNone.Self.InferenceState.Type = "none"
		if pickerFactsFromSnapshot(typeNone, 0).engineUsable {
			t.Error(`engine type "none" is the reported form of the same thing`)
		}
	})

	// A failed READ must not be read as a fact about this computer.
	t.Run("no snapshot keeps the local row", func(t *testing.T) {
		f := pickerFactsFromSnapshot(nil, 5)
		if !f.engineUsable {
			t.Error("a mesh read that failed must not remove the local entry")
		}
		if len(f.peers) != 0 {
			t.Errorf("peers = %+v, want none", f.peers)
		}
	})
}
