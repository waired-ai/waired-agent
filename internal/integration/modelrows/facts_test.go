package modelrows

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
	"github.com/waired-ai/waired-agent/proto/hostfit"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// peerView is a computer serving tag at the 200k tier, which every computer
// that answers a Waired row declares (waired-agent#1396).
func peerView(name, deviceID, tag string, reachable bool) inferencemesh.PeerView {
	return inferencemesh.PeerView{
		DeviceID:   deviceID,
		DeviceName: name,
		InferenceState: &signer.InferenceState{
			Reachable:     reachable,
			Type:          signer.InferenceTypeOllama,
			Models:        []string{tag},
			ActiveModel:   "qwen3.5-4b",
			ContextWindow: hostfit.ServingWindow200k,
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
		// The peers that cannot answer are still in the facts, marked, since
		// their names decide which ids need a hash. They get no row.
		var peerRows []string
		for _, r := range Rows(f) {
			if claudecode.IsPeerDirectiveID(r.ID) {
				peerRows = append(peerRows, r.ID)
			}
		}
		if len(peerRows) != 1 || peerRows[0] != "waired/peer-linux-gpu" {
			t.Errorf("peer rows = %v, want only the serving one", peerRows)
		}
		for _, p := range f.Peers {
			if p.NotServing != (p.DisplayID != "linux-gpu") {
				t.Errorf("peer %q NotServing = %v", p.DisplayID, p.NotServing)
			}
		}
		if f.Peers[0].Model != "qwen3.5-4b" {
			t.Errorf("model = %q, want the catalog id rather than the engine tag", f.Peers[0].Model)
		}
	})

	// PRODUCT CONTRACT, ratifying source: owner decision 2026-09-16 on
	// waired-agent#1396. Every Waired row is at least a 200k session, so a
	// computer that declares less, or nothing, cannot answer any of them, and
	// a row naming it is a menu entry whose selection fails.
	t.Run("a computer under the 200k tier gets no row", func(t *testing.T) {
		narrow := peerView("small-box", "dev_n", "qwen3.5:4b", true)
		narrow.InferenceState.ContextWindow = 131072
		silent := peerView("quiet-box", "dev_q", "qwen3.5:4b", true)
		silent.InferenceState.ContextWindow = 0
		f := FactsFromSnapshot(&inferencemesh.Snapshot{
			Peers: []inferencemesh.PeerView{narrow, peerView("linux-gpu", "dev_a", "qwen3.5:4b", true), silent},
		}, 5, false)
		var peerRows []string
		for _, r := range Rows(f) {
			if claudecode.IsPeerDirectiveID(r.ID) {
				peerRows = append(peerRows, r.ID)
			}
		}
		if len(peerRows) != 1 || peerRows[0] != "waired/peer-linux-gpu" {
			t.Errorf("peer rows = %v, want only the computer at the 200k tier", peerRows)
		}
		// Still in the facts, for the ids: a session that picked the row keeps
		// the id, and the refusal names the computer.
		if len(f.Peers) != 3 {
			t.Errorf("peers = %+v, want all three kept for id hashing", f.Peers)
		}
		if got, ok := PeerForDirective([]inferencemesh.PeerView{narrow, silent}, "waired/peer-small-box"); !ok || got.DeviceID != "dev_n" {
			t.Errorf("the id of a computer under the tier resolved to %q, %v", got.DeviceID, ok)
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
		silent.InferenceState.ContextWindow = 0
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

// PIN: product contract — the id a row is offered under resolves to that
// row's computer and no other (waired-agent#325; waired#1370 review). Rows and
// PeerForDirective must agree for every row, whatever the names.
func TestPeerForDirective(t *testing.T) {
	team := func(v inferencemesh.PeerView, owner string) inferencemesh.PeerView {
		v.Grant = &signer.PeerGrant{ID: "g_" + v.DeviceID, Kind: "team", Role: "provider", DisplayName: owner}
		return v
	}
	ownIdle := peerView("studio-mac", "dev_own", "qwen3.5:4b", false)
	peers := []inferencemesh.PeerView{
		team(peerView("studio-mac", "dev_tanaka", "qwen3.5:4b", true), "田中"),
		team(peerView("studio-mac", "dev_sato", "qwen3.5:4b", true), "佐藤"),
		ownIdle,
		team(peerView("strix-halo-box", "dev_mail1", "qwen3.5:4b", true), "alice.example@example.com"),
		team(peerView("strix-halo-box", "dev_mail2", "qwen3.5:4b", true), "alice.example@example.org"),
		peerView("studio-mac-2", "dev_literal2", "qwen3.5:4b", true),
		peerView("linux-gpu", "dev_gpu", "qwen3.5:4b", true),
	}
	f := FactsFromSnapshot(&inferencemesh.Snapshot{Peers: peers}, 10, false)

	t.Run("every offered row resolves to its own computer", func(t *testing.T) {
		rows := claudecode.PeerDirectiveModels(f.Peers, 10)
		if len(rows) != 6 {
			t.Fatalf("got %d peer rows, want the 6 serving peers: %+v", len(rows), rows)
		}
		var serving []inferencemesh.PeerView
		for _, p := range peers {
			if inferencemesh.PeerServing(p) {
				serving = append(serving, p)
			}
		}
		for i, r := range rows {
			got, ok := PeerForDirective(peers, r.ID)
			if !ok {
				t.Errorf("row %q resolves to nothing", r.ID)
				continue
			}
			if got.DeviceID != serving[i].DeviceID {
				t.Errorf("row %q (%s) resolves to %s", r.ID, r.DisplayName, got.DeviceID)
			}
		}
	})

	t.Run("the bare slug no longer names any of the same-named computers", func(t *testing.T) {
		if got, ok := PeerForDirective(peers, "waired/peer-studio-mac"); ok {
			t.Errorf("waired/peer-studio-mac resolved to %s", got.DeviceID)
		}
	})

	t.Run("an old -2 id names only a computer called that", func(t *testing.T) {
		got, ok := PeerForDirective(peers, "waired/peer-studio-mac-2")
		if !ok || got.DeviceID != "dev_literal2" {
			t.Errorf("resolved to %q, %v; want the computer named studio-mac-2", got.DeviceID, ok)
		}
		without := append([]inferencemesh.PeerView(nil), peers[:5]...)
		if got, ok := PeerForDirective(without, "waired/peer-studio-mac-2"); ok {
			t.Errorf("with no studio-mac-2 on the mesh, the old ordinal id resolved to %s", got.DeviceID)
		}
	})

	t.Run("a computer that stopped answering is still the one its id names", func(t *testing.T) {
		ids := claudecode.PeerDirectiveIDs(f.Peers)
		idle := 0
		for i, p := range f.Peers {
			if !p.NotServing {
				continue
			}
			idle++
			got, ok := PeerForDirective(peers, ids[i])
			if !ok || got.DeviceID != "dev_own" {
				t.Errorf("id %q resolved to %q, %v; want your own idle studio-mac", ids[i], got.DeviceID, ok)
			}
		}
		if idle != 1 {
			t.Errorf("the facts hold %d idle peers, want your own idle studio-mac", idle)
		}
	})

	t.Run("a public machine's hash does not come from its device id", func(t *testing.T) {
		pub := peerView("stranger", "dev_stranger", "qwen3.5:4b", true)
		pub.Grant = &signer.PeerGrant{ID: "g_pub", Kind: "public", Role: "provider", Pseudonym: "guest-a7f3"}
		pub2 := pub
		pub2.DeviceID = "dev_stranger2"
		pub2.Grant = &signer.PeerGrant{ID: "g_pub2", Kind: "public", Role: "provider", Pseudonym: "guest-a7f3"}
		facts, _ := peerFacts([]inferencemesh.PeerView{pub, pub2})
		if facts[0].Key != "g_pub" || facts[1].Key != "g_pub2" {
			t.Errorf("keys = %q, %q; want the grant ids", facts[0].Key, facts[1].Key)
		}
	})
}
