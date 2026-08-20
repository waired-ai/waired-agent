package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/gateway"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// The "Waired peer" /model entry restricts one conversation to another
// computer, without touching the operator's `waired worker` setting
// (waired-agent#830, owner request on waired-ai/waired#1223).

// PIN: product contract — waired-agent#830 for the mapping, and
// docs/decisions/20260801/1840-tray-routing-split-and-peer-only.md §3 for
// why it is peer-only (fail-closed) rather than peer-preferred.
func TestNodeDirectivePref(t *testing.T) {
	for _, tc := range []struct {
		name      string
		directive string
		want      state.RoutingMode
		wantOK    bool
	}{
		{"the peer id restricts the turn to the mesh", gateway.ModelWairedPeer, state.RoutingModePeerOnly, true},
		{"no directive leaves the operator's preference alone", "", "", false},
		// The local pin resolves to this device without a routing
		// preference; a second mechanism for the same behaviour is how
		// two mechanisms drift.
		{"the local pin is not a node directive", gateway.ModelWairedLocal, "", false},
		{"the auto tiers are routes, not nodes", gateway.ModelWairedAuto, "", false},
		{"an unknown id is not a node directive", "claude-sonnet-5", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, err := nodeDirectivePref(tc.directive, nil)
			if err != nil {
				t.Fatalf("nodeDirectivePref(%q): %v", tc.directive, err)
			}
			if ok != tc.wantOK {
				t.Fatalf("nodeDirectivePref(%q) ok = %v, want %v", tc.directive, ok, tc.wantOK)
			}
			if got.pref.Mode != tc.want {
				t.Errorf("mode = %q, want %q", got.pref.Mode, tc.want)
			}
			if got.pref.PinnedPeerDeviceID != "" {
				t.Errorf("a directive must not carry a pin: %+v", got)
			}
		})
	}
}

// The whole point of carrying the directive per request: it must not
// become an instruction about the machine. The provider's routing
// accessor is the only door to the persisted preference, and this proves
// the directive path never even opens it — a stronger statement than
// "the file did not change", which would also hold if we read and
// rewrote the same value.
func TestClaudeSelector_PeerDirectiveNeverConsultsThePersistedPreference(t *testing.T) {
	snap := peerSnapshot("big:32b")
	p := newClaudeSelectorProvider(t, func() inferencemesh.Snapshot { return snap })
	reads := 0
	p.routing = func() state.RoutingPreference {
		reads++
		return state.RoutingPreference{Mode: state.RoutingModeLocalOnly}
	}
	sel := &claudeSelector{p: p}

	cands, err := sel.SelectK(t.Context(), router.Request{
		Model:         "big-peer",
		Class:         state.ClaudeClassMain,
		NodeDirective: gateway.ModelWairedPeer,
	}, 1)
	if err != nil {
		t.Fatalf("SelectK: %v", err)
	}
	if reads != 0 {
		t.Errorf("the persisted worker preference was read %d times; the directive decides this request alone", reads)
	}
	if len(cands) == 0 || cands[0].ExecutionMode != "remote" {
		t.Fatalf("candidate = %+v, want remote — local-only is the persisted mode and must not win here", cands)
	}
}

// Fail-closed. The local engine is ready and the mesh is empty; a request
// that asked for a peer must say so rather than quietly running here,
// which is the defect waired-agent#325 took out of the pin.
func TestClaudeSelector_PeerDirectiveFailsClosedWithNoPeer(t *testing.T) {
	p := withRouting(newClaudeSelectorProvider(t, func() inferencemesh.Snapshot { return inferencemesh.Snapshot{} }),
		state.RoutingPreference{Mode: state.RoutingModeAuto})
	sel := &claudeSelector{p: p}

	cands, err := sel.SelectK(t.Context(), router.Request{
		Model:         "small-local",
		Class:         state.ClaudeClassMain,
		NodeDirective: gateway.ModelWairedPeer,
	}, 1)
	if err == nil {
		t.Fatalf("a peer directive with no peer must fail, got %+v", cands)
	}
	if !errors.Is(err, router.ErrModelNotReady) {
		t.Fatalf("error = %v, want ErrModelNotReady", err)
	}
}

// PIN: product contract — waired-agent#901, owner ruling 2026-08-20. The
// public entry is peer-only AND public-only, and it respects the standing
// posture rather than overriding it, which is why nothing here touches
// PublicPolicy.
func TestNodeDirectivePref_PublicShare(t *testing.T) {
	got, ok, err := nodeDirectivePref(gateway.ModelWairedPublic, nil)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if got.pref.Mode != state.RoutingModePeerOnly {
		t.Errorf("mode = %q, want peer-only — it must not fall back to this device", got.pref.Mode)
	}
	if !got.publicOnly {
		t.Error("the public entry must narrow the candidate set to public machines")
	}
	if got.pref.PinnedPeerDeviceID != "" {
		t.Errorf("it names a class of machine, not one: %+v", got.pref)
	}
	// The peer entry is its sibling, not the same thing.
	peer, _, _ := nodeDirectivePref(gateway.ModelWairedPeer, nil)
	if peer.publicOnly {
		t.Error(`"Waired peer" must not be restricted to public machines`)
	}
}

// A per-peer entry names one machine. Resolution re-derives each peer's slug
// with the same function that produced the id, so the two cannot disagree
// about what a name reduces to.
//
// PIN: product contract — waired-agent#830, and waired-agent#325 for the
// fail-closed half (an explicit choice of node must not be served elsewhere).
func TestNodeDirectivePref_PerPeer(t *testing.T) {
	named := peerSnapshot("big:32b")
	named.Peers[0].DeviceName = "linux-gpu"

	t.Run("a named peer becomes a pin for this request", func(t *testing.T) {
		pref, ok, err := nodeDirectivePref("claude-waired-peer-linux-gpu", named.Peers)
		if err != nil || !ok {
			t.Fatalf("ok=%v err=%v", ok, err)
		}
		if pref.pref.Mode != state.RoutingModePinned || pref.pref.PinnedPeerDeviceID != "peer-X" {
			t.Errorf("pref = %+v, want a pin to peer-X", pref)
		}
	})

	t.Run("a machine that is gone fails closed", func(t *testing.T) {
		_, ok, err := nodeDirectivePref("claude-waired-peer-retired-box", named.Peers)
		if ok {
			t.Fatal("an id naming nothing on the mesh must not resolve")
		}
		if !errors.Is(err, router.ErrModelNotReady) {
			t.Fatalf("err = %v, want ErrModelNotReady so the client shows a model error", err)
		}
		// Serving it from wherever the operator's preference points, while
		// the client still displays the name the user picked, is the silent
		// substitution #325 removed.
		if !strings.Contains(err.Error(), "retired-box") {
			t.Errorf("the error does not name the computer that is missing: %v", err)
		}
	})

	t.Run("a public machine resolves by pseudonym, not by device name", func(t *testing.T) {
		snap := peerSnapshot("big:32b")
		snap.Peers[0].DeviceName = "stranger-workstation"
		snap.Peers[0].Grant = &signer.PeerGrant{
			ID: "g1", Kind: "public", Role: "provider", Pseudonym: "guest-a7f3",
		}
		if _, ok, _ := nodeDirectivePref("claude-waired-peer-stranger-workstation", snap.Peers); ok {
			t.Error("a stranger's real machine name must not be an addressable id")
		}
		pref, ok, err := nodeDirectivePref("claude-waired-peer-guest-a7f3", snap.Peers)
		if err != nil || !ok {
			t.Fatalf("the pseudonym must resolve: ok=%v err=%v", ok, err)
		}
		if pref.pref.PinnedPeerDisplayID != "guest-a7f3" {
			t.Errorf("PinnedPeerDisplayID = %q, want the pseudonym", pref.pref.PinnedPeerDisplayID)
		}
	})
}

// End to end through the real selector: the named peer serves, and the
// operator's persisted preference is not consulted.
func TestClaudeSelector_PerPeerDirectivePinsThatPeer(t *testing.T) {
	snap := peerSnapshot("big:32b")
	snap.Peers[0].DeviceName = "linux-gpu"
	p := newClaudeSelectorProvider(t, func() inferencemesh.Snapshot { return snap })
	reads := 0
	p.routing = func() state.RoutingPreference {
		reads++
		return state.RoutingPreference{Mode: state.RoutingModeLocalOnly}
	}
	sel := &claudeSelector{p: p}

	cands, err := sel.SelectK(t.Context(), router.Request{
		Model:         "big-peer",
		Class:         state.ClaudeClassMain,
		NodeDirective: "claude-waired-peer-linux-gpu",
	}, 1)
	if err != nil {
		t.Fatalf("SelectK: %v", err)
	}
	if reads != 0 {
		t.Errorf("the persisted preference was read %d times", reads)
	}
	if len(cands) == 0 || cands[0].PeerID != "peer-X" {
		t.Fatalf("candidate = %+v, want the named peer", cands)
	}
}

// Without the directive nothing changes: the operator's preference still
// decides, including when it says local-only.
func TestClaudeSelector_NoDirectiveKeepsTheOperatorsPreference(t *testing.T) {
	snap := peerSnapshot("big:32b")
	p := withRouting(newClaudeSelectorProvider(t, func() inferencemesh.Snapshot { return snap }),
		state.RoutingPreference{Mode: state.RoutingModeLocalOnly})
	sel := &claudeSelector{p: p}

	cands, err := sel.SelectK(t.Context(), router.Request{Model: "small-local", Class: state.ClaudeClassMain}, 1)
	if err != nil {
		t.Fatalf("SelectK: %v", err)
	}
	if len(cands) == 0 || cands[0].ExecutionMode != "local" {
		t.Fatalf("candidate = %+v, want local", cands)
	}
}
