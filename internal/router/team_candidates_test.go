package router

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// PRODUCT CONTRACT (team share spec §6.1; owner rulings recorded for
// implementation in waired#1370): a teammate's node is in the same pool
// as this account's own nodes — own == team > public — and none of the
// Public Share consumer policy (use mode, minimum tier, auto comparison)
// applies to it. It is named "<device> (<owner>)", never by device id.

const (
	teamPeerDeviceID = "dev_teammate0000001"
	teamPeerName     = "studio-mac"
	teamPeerOwner    = "Alice Example"
)

// mkTeamPeer builds a teammate's node injected under a Team Share grant.
func mkTeamPeer(deviceID, tag, role string) inferencemesh.PeerView {
	p := mkPeer(deviceID, tag, true, false)
	p.DeviceName = teamPeerName
	p.Grant = &signer.PeerGrant{
		ID:          "grant_team0001",
		Kind:        signer.GrantKindTeam,
		Role:        role,
		DisplayName: teamPeerOwner,
	}
	return p
}

// teamSelector is publicSelectorWith with Public Share turned off, so any
// candidate that appears came in on the team path.
func teamSelector(t *testing.T, policy PublicPolicy, peers ...inferencemesh.PeerView) *Selector {
	t.Helper()
	s, _, _ := publicSelectorWith(t, policy, qwenTier(50), peers...)
	return s
}

func TestTeamCandidate_AdmittedWithoutPublicPolicy(t *testing.T) {
	for _, role := range []string{signer.GrantRoleProvider, signer.GrantRoleBoth} {
		t.Run(role, func(t *testing.T) {
			// Zero PublicPolicy fails the public gate closed; the team
			// node must not care.
			s := teamSelector(t, PublicPolicy{}, mkTeamPeer(teamPeerDeviceID, "qwen3:8b-q4_K_M", role))
			cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 3)
			if err != nil || len(cands) != 1 {
				t.Fatalf("team node with Public Share off: cands=%d err=%v, want 1 candidate", len(cands), err)
			}
			want := teamPeerName + " (" + teamPeerOwner + ")"
			if cands[0].PeerDisplayID != want {
				t.Errorf("PeerDisplayID = %q, want %q", cands[0].PeerDisplayID, want)
			}
			for _, r := range cands[0].Decision.Reason {
				if strings.Contains(r, teamPeerDeviceID) {
					t.Errorf("reason leaks the teammate's device id: %q", r)
				}
			}
			sel, ok := cands[0].Commit()
			if !ok {
				t.Fatal("Commit failed")
			}
			if strings.Contains(sel.EndpointID, teamPeerDeviceID) || sel.PeerDisplayID != want {
				t.Errorf("Selection names the teammate as %q / %q", sel.EndpointID, sel.PeerDisplayID)
			}
		})
	}
}

// A teammate that only consumes from us is not a routing target, and a
// public-kind grant can never be read as a team one.
func TestTeamCandidate_ConsumerOnlyAndKindChecks(t *testing.T) {
	consumer := mkTeamPeer(teamPeerDeviceID, "qwen3:8b-q4_K_M", signer.GrantRoleConsumer)
	s := teamSelector(t, allowAll(), consumer)
	if _, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 3); err == nil {
		t.Fatal("a teammate that only consumes from us was admitted as a routing target")
	}

	publicBoth := mkPublicPeer(publicPeerDeviceID, publicPeerAlias, "qwen3:8b-q4_K_M")
	publicBoth.Grant.Role = signer.GrantRoleBoth
	s = teamSelector(t, allowAll(), publicBoth)
	if _, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 3); err == nil {
		t.Fatal(`a public grant with the team-only role "both" was admitted`)
	}
}

// own == team > public: a team node outranks a public one even when the
// public node would win every other key, and ranks against own nodes by
// the ordinary keys.
func TestTeamCandidate_Tier(t *testing.T) {
	team := mkTeamPeer(teamPeerDeviceID, "qwen3:8b-q4_K_M", signer.GrantRoleProvider)
	public := mkPublicPeer(publicPeerDeviceID, publicPeerAlias, "qwen3:8b-q4_K_M")
	public.InferenceState.Priority = 1 // High

	t.Run("team above public", func(t *testing.T) {
		s := teamSelector(t, allowAll(), public, team)
		cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 5)
		if err != nil || len(cands) != 2 {
			t.Fatalf("cands=%d err=%v, want 2", len(cands), err)
		}
		if cands[0].PeerID != teamPeerDeviceID {
			t.Fatalf("public node outranked the team node: cands[0] = %q", cands[0].PeerID)
		}
	})

	t.Run("team and own share one tier", func(t *testing.T) {
		own := mkPeer("dev_own00000001", "qwen3:8b-q4_K_M", true, false)
		teamHigh := mkTeamPeer(teamPeerDeviceID, "qwen3:8b-q4_K_M", signer.GrantRoleProvider)
		teamHigh.InferenceState.Priority = 1 // High
		s := teamSelector(t, PublicPolicy{}, own, teamHigh)
		cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 5)
		if err != nil || len(cands) != 2 {
			t.Fatalf("cands=%d err=%v, want 2", len(cands), err)
		}
		// Spec §6.1: the teammate's High beats our own Middle, because it
		// is the same pool; nothing corrects for ownership.
		if cands[0].PeerID != teamPeerDeviceID {
			t.Fatalf("own node outranked a higher-priority team node: cands[0] = %q", cands[0].PeerID)
		}
	})
}

// "Waired public share" asked for Public Share providers; a teammate's
// node is not one.
func TestTeamCandidate_ExcludedFromPublicOnly(t *testing.T) {
	snap := inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{
		mkTeamPeer(teamPeerDeviceID, "qwen3:8b-q4_K_M", signer.GrantRoleProvider),
	}}
	policy := allowAll()
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwenTier(50)},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
		PublicPolicyFn: func() PublicPolicy { return policy },
		PublicOnly:     true,
	})
	if cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 3); err == nil && len(cands) > 0 {
		t.Fatalf("public-only admitted a team node: %+v", cands[0].PeerID)
	}
}

// ownBestTier counts a teammate's node with this account's own, and a
// team provider does not satisfy the "is there a public grant" demand
// check.
func TestTeamProvider_OwnBaselineAndDemand(t *testing.T) {
	team := mkTeamPeer(teamPeerDeviceID, "qwen3:8b-q4_K_M", signer.GrantRoleBoth)
	snap := inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{team}}
	s := NewSelector(Inputs{Manifests: []catalog.Manifest{qwenTier(50)}, LocalState: emptyState()})
	if got := s.ownBestTier(snap); got != 50 {
		t.Fatalf("ownBestTier = %d, want 50 from the team node", got)
	}
	if snapshotHasPublicProvider(snap) {
		t.Fatal("a team provider read as a held public grant")
	}
	consumerOnly := mkTeamPeer(teamPeerDeviceID, "qwen3:8b-q4_K_M", signer.GrantRoleConsumer)
	if got := s.ownBestTier(inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{consumerOnly}}); got != 0 {
		t.Fatalf("ownBestTier = %d, want 0 — a teammate that only consumes from us serves nothing here", got)
	}
}

// A pinned teammate is named by its label, not by the pin (another
// account's device id).
func TestPinDisplayID_TeamPeer(t *testing.T) {
	snap := inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{
		mkTeamPeer(teamPeerDeviceID, "qwen3:8b-q4_K_M", signer.GrantRoleProvider),
	}}
	got := pinDisplayID(snap, teamPeerDeviceID, "")
	if got != teamPeerName+" ("+teamPeerOwner+")" {
		t.Fatalf("pinDisplayID = %q, want the team label", got)
	}
}

// A pinned teammate who has left the map (stopped sharing, taken out of
// the pool, left the team) is named by the label recorded when the pin
// was set, never by the pin — which is another account's device id and
// used to reach the error body, the X-Waired-Inference-Peer header and
// the event ring.
func TestPinDisplayID_AbsentGrantPinUsesSavedLabel(t *testing.T) {
	saved := teamPeerName + " (" + teamPeerOwner + ")"
	if got := pinDisplayID(inferencemesh.Snapshot{}, teamPeerDeviceID, saved); got != saved {
		t.Fatalf("pinDisplayID = %q, want the saved label %q", got, saved)
	}
	if got := pinDisplayLabel(inferencemesh.Snapshot{}, teamPeerDeviceID, saved); strings.Contains(got, teamPeerDeviceID) {
		t.Fatalf("pinDisplayLabel = %q leaks the teammate's device id", got)
	}
	// A pin written by an agent predating the recorded name is one of
	// your own machines, and keeps its device id.
	if got := pinDisplayID(inferencemesh.Snapshot{}, "dev_own", ""); got != "dev_own" {
		t.Fatalf("legacy own pin = %q, want its device id", got)
	}
}

// End to end through SelectK: a pin whose teammate has gone from the
// snapshot fails with an error that names the saved label.
func TestPinnedAbsentTeammateErrorNamesTheLabel(t *testing.T) {
	saved := teamPeerName + " (" + teamPeerOwner + ")"
	s := &Selector{in: Inputs{
		RoutingMode:         state.RoutingModePinned,
		PinnedPeerDeviceID:  teamPeerDeviceID,
		PinnedPeerDisplayID: saved,
	}}
	err := s.pinUnreachable(inferencemesh.Snapshot{}, "qwen3-8b-instruct")
	if err == nil || strings.Contains(err.Error(), teamPeerDeviceID) || !strings.Contains(err.Error(), saved) {
		t.Fatalf("pinUnreachable error = %v, want the saved label and no device id", err)
	}
}
