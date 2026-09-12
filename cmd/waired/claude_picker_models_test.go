package main

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
	"github.com/waired-ai/waired-agent/internal/integration/modelrows"
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
		got := pickerModels(modelrows.Facts{
			LocalServes: true, PublicShareOn: true, Peers: peers, PeerLimit: 5,
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
		off := pickerModels(modelrows.Facts{LocalServes: true, Peers: peers, PeerLimit: 5})
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
		on := pickerModels(modelrows.Facts{LocalServes: true, PublicShareOn: true, PeerLimit: 0})
		if !has(on, claudecode.DirectiveModelPublic) {
			t.Error("a host with Public Share on is not offered it")
		}
	})

	t.Run("no engine here drops the local row and nothing else", func(t *testing.T) {
		got := pickerModels(modelrows.Facts{LocalServes: false, PublicShareOn: true, Peers: peers, PeerLimit: 5})
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
		got := pickerModels(modelrows.Facts{LocalServes: true, PublicShareOn: true, PeerLimit: 0})
		if strings.Join(ids(got), ",") != strings.Join(fixedIDs(), ",") {
			t.Errorf("ids = %v, want the fixed table unchanged", ids(got))
		}
	})

	// waired-agent#1037: picking a real Anthropic model in /model now reaches
	// the real Anthropic API on its own, and says which model answers besides.
	// The row said the same thing with less information, and the picker folds
	// at about four Waired rows.
	t.Run("the retired cloud row is not written into the picker", func(t *testing.T) {
		got := pickerModels(modelrows.Facts{LocalServes: true, PublicShareOn: true, Peers: peers, PeerLimit: 5})
		if has(got, claudecode.LegacyModelCloud) {
			t.Error("the cloud row is back in the picker; it is routed for the sessions that hold it, not offered")
		}
	})

	// An empty lineup and no lineup mean the same thing to Claude Code, and
	// WritePickerLineup removes the key rather than writing an empty one. The
	// fixed prefix is what guarantees the ordinary host never gets there.
	t.Run("never empty, even with no engine and no peers", func(t *testing.T) {
		if got := pickerModels(modelrows.Facts{}); len(got) == 0 {
			t.Error("a host with no engine and no peers is left with no Waired rows at all")
		}
	})

	// waired-agent#1177, found on a real host during rc5: the row was offered
	// on a machine whose operator had turned local inference off, and picking
	// it failed every turn. PIN: product contract.
	t.Run("inference turned off drops the local row, like no engine at all", func(t *testing.T) {
		got := pickerModels(modelrows.Facts{LocalServes: false, PublicShareOn: true, Peers: peers, PeerLimit: 5})
		if has(got, claudecode.DirectiveModelLocal) {
			t.Error("a computer with local inference off still offers to run the model itself")
		}
	})

	// Owner ruling 2026-09-06: a "[1m]" twin is offered where — and only
	// where — a side declares a 1M window. A twin with nothing behind it is a
	// menu entry whose selection fails.
	t.Run("1M twins follow the declared windows", func(t *testing.T) {
		none := pickerModels(modelrows.Facts{LocalServes: true, PublicShareOn: true, PeerLimit: 0})
		for _, id := range ids(none) {
			if strings.Contains(id, "[1m]") {
				t.Errorf("%q offered where nothing declares a 1M window", id)
			}
		}

		wide := []claudecode.PeerFact{{DisplayID: "big-box", Model: "qwen3.5-35b-a3b", Window1M: true}}
		got := pickerModels(modelrows.Facts{
			LocalServes: true, PublicShareOn: true, PeerWindow1M: true,
			Peers: wide, PeerLimit: 5,
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
