package main

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
	"github.com/waired-ai/waired-agent/proto/signer"
)

func ids(rows []claudecode.PickerRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Model)
	}
	return out
}

func has(rows []claudecode.PickerRow, id string) bool {
	for _, r := range rows {
		if r.Model == id {
			return true
		}
	}
	return false
}

// fixedIDs is the fixed table's ids with no twins, which is what a host that
// declares no 1M window anywhere offers.
func fixedIDs() []string {
	out := make([]string, 0, 4)
	for _, d := range claudecode.DirectiveModels() {
		out = append(out, d.ID)
	}
	return out
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
			localServes: true, publicShareOn: true, peers: peers, peerLimit: 5,
		})
		want := append(fixedIDs(),
			"waired/peer-linux-gpu", "waired/peer-studio-mac")
		if strings.Join(ids(got), ",") != strings.Join(want, ",") {
			t.Errorf("ids =\n %v\nwant\n %v", ids(got), want)
		}
	})

	// PIN: product contract — owner ruling 2026-08-20 (waired-agent#901).
	// The picker cannot render a row as disabled, so a host that has not
	// enabled Public Share must not be shown a choice it cannot take.
	t.Run("the public entry is absent until Public Share is on", func(t *testing.T) {
		off := pickerModels(pickerModelFacts{localServes: true, peers: peers, peerLimit: 5})
		if has(off, claudecode.DirectiveModelPublic) {
			t.Error("a host with Public Share off is offered someone else's computer")
		}
		// Everything else is unaffected — this removes one row, not a family.
		for _, id := range []string{
			claudecode.DirectiveModelPeer, claudecode.DirectiveModelLocal,
			claudecode.DirectiveModelAny, "waired/peer-linux-gpu",
		} {
			if !has(off, id) {
				t.Errorf("%q must survive with Public Share off", id)
			}
		}
		on := pickerModels(pickerModelFacts{localServes: true, publicShareOn: true, peerLimit: 0})
		if !has(on, claudecode.DirectiveModelPublic) {
			t.Error("a host with Public Share on is not offered it")
		}
	})

	t.Run("no engine here drops the local row and nothing else", func(t *testing.T) {
		got := pickerModels(pickerModelFacts{localServes: false, publicShareOn: true, peers: peers, peerLimit: 5})
		if has(got, claudecode.DirectiveModelLocal) {
			t.Error("a computer with no engine still offers to run the model itself")
		}
		// The point of dropping it is that the OTHER choices remain.
		for _, id := range []string{
			claudecode.DirectiveModelPeer,
			claudecode.DirectiveModelAny,
			"waired/peer-linux-gpu",
		} {
			if !has(got, id) {
				t.Errorf("%q must survive on an engine-less computer", id)
			}
		}
	})

	t.Run("peers off leaves the fixed entries exactly as they were", func(t *testing.T) {
		got := pickerModels(pickerModelFacts{localServes: true, publicShareOn: true, peerLimit: 0})
		if strings.Join(ids(got), ",") != strings.Join(fixedIDs(), ",") {
			t.Errorf("ids = %v, want the fixed table unchanged", ids(got))
		}
	})

	// waired-agent#1037: picking a real Anthropic model in /model now reaches
	// the real Anthropic API on its own, and says which model answers besides.
	// The row said the same thing with less information, and the picker folds
	// at about four Waired rows.
	t.Run("the retired cloud row is not written into the picker", func(t *testing.T) {
		got := pickerModels(pickerModelFacts{localServes: true, publicShareOn: true, peers: peers, peerLimit: 5})
		if has(got, claudecode.LegacyModelCloud) {
			t.Error("the cloud row is back in the picker; it is routed for the sessions that hold it, not offered")
		}
	})

	// An empty lineup and no lineup mean the same thing to Claude Code, and
	// WritePickerLineup removes the key rather than writing an empty one. The
	// fixed prefix is what guarantees the ordinary host never gets there.
	t.Run("never empty, even with no engine and no peers", func(t *testing.T) {
		if got := pickerModels(pickerModelFacts{}); len(got) == 0 {
			t.Error("a host with no engine and no peers is left with no Waired rows at all")
		}
	})

	// waired-agent#1177, found on a real host during rc5: the row was offered
	// on a machine whose operator had turned local inference off, and picking
	// it failed every turn. PIN: product contract.
	t.Run("inference turned off drops the local row, like no engine at all", func(t *testing.T) {
		got := pickerModels(pickerModelFacts{localServes: false, publicShareOn: true, peers: peers, peerLimit: 5})
		if has(got, claudecode.DirectiveModelLocal) {
			t.Error("a computer with local inference off still offers to run the model itself")
		}
	})

	// Owner ruling 2026-09-06: a "[1m]" twin is offered where — and only
	// where — a side declares a 1M window. A twin with nothing behind it is a
	// menu entry whose selection fails.
	t.Run("1M twins follow the declared windows", func(t *testing.T) {
		none := pickerModels(pickerModelFacts{localServes: true, publicShareOn: true, peerLimit: 0})
		for _, id := range ids(none) {
			if strings.Contains(id, "[1m]") {
				t.Errorf("%q offered where nothing declares a 1M window", id)
			}
		}

		wide := []claudecode.PeerFact{{DisplayID: "big-box", Model: "qwen3.5-35b-a3b", Window1M: true}}
		got := pickerModels(pickerModelFacts{
			localServes: true, publicShareOn: true, peerWindow1M: true,
			peers: wide, peerLimit: 5,
		})
		for _, id := range []string{
			claudecode.Tier1M(claudecode.DirectiveModelAny),
			claudecode.Tier1M(claudecode.DirectiveModelPeer),
			claudecode.Tier1M("waired/peer-big-box"),
		} {
			if !has(got, id) {
				t.Errorf("%q missing though a peer declares 1M", id)
			}
		}
		// This computer declares nothing, so its own row has no twin even
		// though the any-node row does.
		if has(got, claudecode.Tier1M(claudecode.DirectiveModelLocal)) {
			t.Error("the local row got a 1M twin from a peer's window")
		}
		// Someone else's computer is not asked for a tier: this host learns
		// a public machine's window only when it answers.
		if has(got, claudecode.Tier1M(claudecode.DirectiveModelPublic)) {
			t.Error("the public row got a 1M twin")
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

	t.Run("a 1M window on a node is what earns its twin", func(t *testing.T) {
		wide := peerView("big-box", "dev_w", "qwen3.5:4b", true)
		wide.InferenceState.ContextWindow = 1048576
		narrow := peerView("small-box", "dev_n", "qwen3.5:4b", true)
		narrow.InferenceState.ContextWindow = 131072

		f := pickerFactsFromSnapshot(&inferencemesh.Snapshot{
			Self:  narrow,
			Peers: []inferencemesh.PeerView{wide, narrow},
		}, 5)
		if f.localWindow1M {
			t.Error("this computer declares 131072 and still got a 1M twin")
		}
		if !f.peerWindow1M {
			t.Error("a peer declares 1M and the peer row got no twin")
		}
		for _, p := range f.peers {
			if (p.DisplayID == "big-box") != p.Window1M {
				t.Errorf("peer %q Window1M = %v", p.DisplayID, p.Window1M)
			}
		}

		// A node that publishes no window declares nothing, which is not the
		// same as declaring a small one — but it earns no twin either.
		silent := peerView("quiet-box", "dev_q", "qwen3.5:4b", true)
		g := pickerFactsFromSnapshot(&inferencemesh.Snapshot{
			Self: silent, Peers: []inferencemesh.PeerView{silent},
		}, 5)
		if g.localWindow1M || g.peerWindow1M {
			t.Error("a node that publishes no window was read as declaring 1M")
		}
	})

	// waired-agent#1177. SubsystemStateDisabled is the operator's intent on
	// the wire, and the daemon sets it ahead of engine health.
	t.Run("local inference turned off drops the local row", func(t *testing.T) {
		off := peerView("me", "dev_self", "qwen3.5:4b", true)
		off.InferenceState.SubsystemState = signer.SubsystemStateDisabled
		if pickerFactsFromSnapshot(&inferencemesh.Snapshot{Self: off}, 0).localServes {
			t.Error("an operator turned local inference off and the row is still offered")
		}
		// And it takes the twin with it, whatever the engine last declared.
		off.InferenceState.ContextWindow = 1048576
		if pickerFactsFromSnapshot(&inferencemesh.Snapshot{Self: off}, 0).localWindow1M {
			t.Error("a row that is not offered still got a 1M twin")
		}
	})

	t.Run("this computer's engine decides the local row", func(t *testing.T) {
		with := &inferencemesh.Snapshot{Self: peerView("me", "dev_self", "qwen3.5:4b", true)}
		if !pickerFactsFromSnapshot(with, 0).localServes {
			t.Error("an engine is present; the local row belongs")
		}
		// Stopped, not absent: still this computer's engine.
		stopped := &inferencemesh.Snapshot{Self: peerView("me", "dev_self", "qwen3.5:4b", false)}
		if !pickerFactsFromSnapshot(stopped, 0).localServes {
			t.Error("a stopped engine must not make the local row flicker away")
		}
		none := &inferencemesh.Snapshot{Self: inferencemesh.PeerView{DeviceID: "dev_self"}} //nolint:staticcheck // reads clearer spelled out
		if pickerFactsFromSnapshot(none, 0).localServes {
			t.Error("no engine at all means the local row names something impossible")
		}
		typeNone := &inferencemesh.Snapshot{Self: peerView("me", "dev_self", "", true)}
		typeNone.Self.InferenceState.Type = "none"
		if pickerFactsFromSnapshot(typeNone, 0).localServes {
			t.Error(`engine type "none" is the reported form of the same thing`)
		}
	})

	// A failed READ must not be read as a fact about this computer.
	t.Run("no snapshot keeps the local row", func(t *testing.T) {
		f := pickerFactsFromSnapshot(nil, 5)
		if !f.localServes {
			t.Error("a mesh read that failed must not remove the local entry")
		}
		if len(f.peers) != 0 {
			t.Errorf("peers = %+v, want none", f.peers)
		}
	})
}
