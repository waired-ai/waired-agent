package router

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// Inputs.PublicOnly narrows one selection to other people's machines — the
// "Waired public share" /model entry (waired-agent#901).
//
// PIN: product contract — owner ruling 2026-08-20. Two halves, and the second
// is the one worth having a test for: it NARROWS and never widens, so the
// consumer's standing Public Share posture still decides what is admissible.

func TestPublicOnly_DropsOwnNetworkPeers(t *testing.T) {
	s, _, _ := publicSelector(t, allowAll(),
		mkPublicPeer(publicPeerDeviceID, publicPeerAlias, "qwen3:8b-q4_K_M"),
		mkPeer("dev_own00000001", "qwen3:8b-q4_K_M", true, false))
	s.in.PublicOnly = true
	s.in.RoutingMode = state.RoutingModePeerOnly

	cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 5)
	if err != nil {
		t.Fatalf("SelectK: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want only the public one: %+v", len(cands), cands)
	}
	if cands[0].PeerID != publicPeerDeviceID {
		t.Errorf("candidate = %q, want the public machine", cands[0].PeerID)
	}
	// Own peers are not runners-up here: they are the thing that was excluded.
	for _, f := range cands[0].Decision.Fallback {
		if strings.Contains(f.Runtime, "dev_own00000001") {
			t.Errorf("an own-network peer survived as a fallback: %+v", f)
		}
	}
}

// Without the flag nothing moves — own-network peers still outrank public ones
// (Team Share routing order), which is the behaviour this must not disturb.
func TestPublicOnly_OffLeavesTheOrderAlone(t *testing.T) {
	s, _, _ := publicSelector(t, allowAll(),
		mkPublicPeer(publicPeerDeviceID, publicPeerAlias, "qwen3:8b-q4_K_M"),
		mkPeer("dev_own00000001", "qwen3:8b-q4_K_M", true, false))

	cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 5)
	if err != nil {
		t.Fatalf("SelectK: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("got %d candidates, want both", len(cands))
	}
	if cands[0].PeerID != "dev_own00000001" {
		t.Errorf("first candidate = %q, want the own-network peer to still win", cands[0].PeerID)
	}
}

// The half that makes this safe. The entry respects the standing posture
// rather than overriding it, so a posture that admits nothing still admits
// nothing — selecting the entry cannot reach a machine the operator never
// consented to.
func TestPublicOnly_CannotWidenPastThePosture(t *testing.T) {
	// INVERTED by waired-agent#1201. This asserted only `err != nil`, and
	// that is how the wrong sentence shipped: every way of admitting
	// nothing produced the same error, and the message reported the
	// operator's own switch as "no public machine is reachable right now".
	// Each cause now has to name its own switch.
	t.Run("posture off admits nothing at all", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			policy PublicPolicy
			want   string
		}{
			{
				name:   "never consented",
				policy: PublicPolicy{Mode: PublicModeExplicit, Main: true, Sub: true},
				want:   "warning has not been accepted",
			},
			{
				name:   "consented and switched off",
				policy: PublicPolicy{Mode: PublicModeOff, Consented: true, Main: true, Sub: true},
				want:   "set not to use other people's public machines",
			},
			{
				name:   "every traffic class switched off",
				policy: PublicPolicy{Mode: PublicModeExplicit, Consented: true},
				want:   "both main-agent and sub-agent turns",
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				s, _, _ := publicSelector(t, tc.policy,
					mkPublicPeer(publicPeerDeviceID, publicPeerAlias, "qwen3:8b-q4_K_M"))
				s.in.PublicOnly = true
				s.in.RoutingMode = state.RoutingModePeerOnly

				_, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 5)
				if err == nil {
					t.Fatal("public-only with the posture off must not select a public machine")
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Errorf("err = %v, want it to name the switch (%q)", err, tc.want)
				}
				if strings.Contains(err.Error(), "reachable") {
					t.Errorf("err = %v, must not report the operator's own switch as unreachability", err)
				}
			})
		}
	})

	// "Two" was the count before waired-agent#1201 added the arms that
	// name a switch; these two remain the world-state ones.
	t.Run("the two ways of finding nothing are told apart", func(t *testing.T) {
		// Nobody is lending a machine.
		none, _, _ := publicSelector(t, allowAll(),
			mkPeer("dev_own00000001", "qwen3:8b-q4_K_M", true, false))
		none.in.PublicOnly = true
		none.in.RoutingMode = state.RoutingModePeerOnly
		_, err := none.SelectK(t.Context(), Request{Model: "waired/default"}, 5)
		if err == nil || !strings.Contains(err.Error(), "no public machine is reachable") {
			t.Errorf("err = %v, want it to say no public machine is reachable", err)
		}

		// One is lending, but the posture says only when it is better.
		worse, _, _ := publicSelector(t, PublicPolicy{Mode: PublicModeAuto, Consented: true, Main: true, Sub: true},
			mkPublicPeer(publicPeerDeviceID, publicPeerAlias, "qwen3:8b-q4_K_M"),
			mkPeer("dev_own00000001", "qwen3:8b-q4_K_M", true, false))
		worse.in.PublicOnly = true
		worse.in.RoutingMode = state.RoutingModePeerOnly
		_, err = worse.SelectK(t.Context(), Request{Model: "waired/default"}, 5)
		if err == nil || !strings.Contains(err.Error(), "only when it beats this one") {
			t.Errorf("err = %v, want it to name the posture rather than unavailability", err)
		}
	})

	t.Run("posture auto still applies its tier comparison", func(t *testing.T) {
		// auto admits a public candidate only when its tier strictly beats
		// the consumer's own best. With an own node present and no tier
		// advantage, the public one is not admitted — and public-only has
		// removed the own one, so the request finds nothing. That is the
		// posture working, not the entry failing.
		s, _, _ := publicSelector(t, PublicPolicy{Mode: PublicModeAuto, Consented: true, Main: true, Sub: true},
			mkPublicPeer(publicPeerDeviceID, publicPeerAlias, "qwen3:8b-q4_K_M"),
			mkPeer("dev_own00000001", "qwen3:8b-q4_K_M", true, false))
		s.in.PublicOnly = true
		s.in.RoutingMode = state.RoutingModePeerOnly

		if _, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 5); err == nil {
			t.Fatal("auto's tier comparison must still be able to refuse")
		}
	})
}

// A public machine's real identifiers must not reach a reason line just
// because this request asked for one (spec §8.5).
func TestPublicOnly_StillNamesThePseudonym(t *testing.T) {
	stranger := mkPublicPeer(publicPeerDeviceID, publicPeerAlias, "qwen3:8b-q4_K_M")
	stranger.DeviceName = "stranger-workstation"
	s, _, _ := publicSelector(t, allowAll(), stranger)
	s.in.PublicOnly = true
	s.in.RoutingMode = state.RoutingModePeerOnly

	cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 5)
	if err != nil {
		t.Fatalf("SelectK: %v", err)
	}
	joined := strings.Join(cands[0].Decision.Reason, "\n")
	for _, leak := range []string{"stranger-workstation", publicPeerDeviceID} {
		if strings.Contains(joined, leak) {
			t.Errorf("reason leaks %q:\n%s", leak, joined)
		}
	}
	if !strings.Contains(joined, publicPeerAlias) {
		t.Errorf("reason does not name the machine at all:\n%s", joined)
	}
}
