package modelrows

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
	"github.com/waired-ai/waired-agent/proto/hostfit"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// PIN: product contract — the rows, their "[1m]" twins and the window each
// states are the same on Claude Code's /model and on the OpenAI-dialect
// listing, and a row states exactly the window routing guarantees for it
// (owner decision 2026-09-16, waired-agent#1395): 1M for a twin and 200k for
// every other row, whatever the computer behind it holds (owner decision
// 2026-09-16, waired-agent#1396).
func TestRows_StateTheWindowRoutingGuarantees(t *testing.T) {
	f := Facts{
		LocalServes:    true,
		LocalWindow:    262144,
		PeerWindow1M:   true,
		PublicShareOn:  true,
		PublicWindow1M: true,
		PeerLimit:      5,
		Peers: []claudecode.PeerFact{
			{DisplayID: "big-box", Key: "dev-1", Model: "deepseek-v4-flash", Window1M: true, ContextWindow: hostfit.ServingWindow1M},
			{DisplayID: "quiet-box", Key: "dev-2", Model: "gpt-oss-20b"},
		},
	}
	type want struct {
		id     string
		window int
		tier1M bool
	}
	wants := []want{
		{claudecode.DirectiveModelAny, hostfit.ServingWindow200k, false},
		{claudecode.Tier1M(claudecode.DirectiveModelAny), hostfit.ServingWindow1M, true},
		// This computer serves 262144: its row is still a 200k session, and
		// it earns no twin.
		{claudecode.DirectiveModelLocal, hostfit.ServingWindow200k, false},
		{claudecode.DirectiveModelPeer, hostfit.ServingWindow200k, false},
		{claudecode.Tier1M(claudecode.DirectiveModelPeer), hostfit.ServingWindow1M, true},
		{claudecode.DirectiveModelPublic, hostfit.ServingWindow200k, false},
		// The public row has a twin now: a public machine's window rides the
		// network map like any other peer's.
		{claudecode.Tier1M(claudecode.DirectiveModelPublic), hostfit.ServingWindow1M, true},
		// A computer that holds 1M: its row is a 200k session like any other,
		// and its twin is the 1M one.
		{"waired/peer-big-box", hostfit.ServingWindow200k, false},
		{claudecode.Tier1M("waired/peer-big-box"), hostfit.ServingWindow1M, true},
		// Rows states what the row is; whether a computer that declares
		// nothing gets a row at all is FactsFromSnapshot's decision.
		{"waired/peer-quiet-box", hostfit.ServingWindow200k, false},
	}
	got := Rows(f)
	if len(got) != len(wants) {
		ids := make([]string, len(got))
		for i, r := range got {
			ids[i] = r.ID
		}
		t.Fatalf("rows = %v, want %d rows", ids, len(wants))
	}
	for i, w := range wants {
		r := got[i]
		if r.ID != w.id || r.ContextWindow != w.window || r.Tier1M != w.tier1M {
			t.Errorf("row %d = {%q window=%d tier1M=%v}, want {%q window=%d tier1M=%v}",
				i, r.ID, r.ContextWindow, r.Tier1M, w.id, w.window, w.tier1M)
		}
	}
}

func TestFactsFromSnapshot_WhoEarnsATwin(t *testing.T) {
	wide := func(name, id string) inferencemesh.PeerView {
		p := peerView(name, id, "qwen3.5:4b", true)
		p.InferenceState.ContextWindow = hostfit.ServingWindow1M
		return p
	}

	t.Run("turning the per-computer rows off keeps the twins peers earn", func(t *testing.T) {
		f := FactsFromSnapshot(&inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{wide("big-box", "dev_w")}}, 0, false)
		if !f.PeerWindow1M {
			t.Error("with the per-computer rows at 0, the peer twins went too (waired-agent#1395)")
		}
		for _, r := range Rows(f) {
			if claudecode.IsPeerDirectiveID(r.ID) {
				t.Errorf("a per-computer row %q with the limit at 0", r.ID)
			}
		}
	})

	t.Run("a computer switched off for main conversations earns none", func(t *testing.T) {
		p := wide("big-box", "dev_w")
		p.InferenceState.ExcludeMain = true
		f := FactsFromSnapshot(&inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{p}}, 5, false)
		if f.PeerWindow1M || f.Peers[0].Window1M {
			t.Errorf("facts = %+v: a twin whose only computer refuses main conversations", f)
		}
	})

	public := func() inferencemesh.PeerView {
		p := wide("stranger", "dev_pub")
		p.Grant = &signer.PeerGrant{ID: "g1", Kind: signer.GrantKindPublic, Role: "provider", Pseudonym: "guest-a7f3"}
		return p
	}

	t.Run("a public machine with Public Share off gets no row and earns no twin", func(t *testing.T) {
		f := FactsFromSnapshot(&inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{public()}}, 5, false)
		if f.PeerWindow1M || f.PublicWindow1M {
			t.Errorf("facts = %+v: a twin behind a machine this computer may not use", f)
		}
		if len(f.Peers) != 1 || !f.Peers[0].NotServing {
			t.Fatalf("peers = %+v, want the machine kept for id hashing but not serving", f.Peers)
		}
		for _, r := range Rows(f) {
			if r.ID == "waired/peer-guest-a7f3" {
				t.Error("a row for a public machine while Public Share is off")
			}
		}
	})

	t.Run("with Public Share on it earns the public twin, not the any-computer one", func(t *testing.T) {
		f := FactsFromSnapshot(&inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{public()}}, 5, true)
		if !f.PublicWindow1M {
			t.Error("the public row got no twin with a 1M public machine admitted")
		}
		if f.PeerWindow1M {
			t.Error("a public machine earned the twin of the rows that route to your own computers")
		}
	})
}
