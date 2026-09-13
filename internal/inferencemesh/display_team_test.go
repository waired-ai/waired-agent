package inferencemesh

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// PRODUCT CONTRACT (team share spec §9 and §10.2; owner ruling
// 2026-09-13 recorded in waired#1370): a teammate's computer is shown by
// its device name and its owner's real name, carried in
// PeerGrant.DisplayName — never by its device identifier, and never
// through the pseudonym path public grants use.
func TestTeamPeerDisplay(t *testing.T) {
	team := func(deviceName, owner string) func(*PeerView) {
		return func(p *PeerView) {
			p.DeviceID = "dev_teammate_real_id"
			p.DeviceName = deviceName
			p.Grant = &signer.PeerGrant{ID: "grant_t", Kind: signer.GrantKindTeam, Role: "provider", DisplayName: owner}
		}
	}
	tests := []struct {
		name   string
		p      PeerView
		want   string
		wantOK bool
	}{
		{"device and owner", peer(team("studio-mac", "Alice Example")), "studio-mac (Alice Example)", true},
		{"owner only", peer(team("", "Alice Example")), "Alice Example", true},
		{"device only", peer(team("studio-mac", "")), "studio-mac", true},
		{"neither", peer(team("", "")), "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for fn, get := range map[string]func(PeerView) (string, bool){
				"PeerDisplayID":   PeerDisplayID,
				"PeerDisplayName": PeerDisplayName,
			} {
				got, ok := get(tt.p)
				if got != tt.want || ok != tt.wantOK {
					t.Errorf("%s = (%q, %v), want (%q, %v)", fn, got, ok, tt.want, tt.wantOK)
				}
				if strings.Contains(got, tt.p.DeviceID) {
					t.Errorf("%s leaked the device id: %q", fn, got)
				}
			}
			label := PeerDisplayLabel(tt.p)
			if tt.wantOK && label != tt.want {
				t.Errorf("PeerDisplayLabel = %q, want %q", label, tt.want)
			}
			if !tt.wantOK && label != TeamPeerFallbackLabel {
				t.Errorf("PeerDisplayLabel = %q, want the team fallback %q", label, TeamPeerFallbackLabel)
			}
		})
	}
}

// A team grant never reads the pseudonym, even if one were present: the
// two fields keep one meaning each (team share spec §9).
func TestTeamPeerDisplay_IgnoresPseudonym(t *testing.T) {
	p := peer(func(p *PeerView) {
		p.DeviceName = "studio-mac"
		p.Grant = &signer.PeerGrant{Kind: signer.GrantKindTeam, Role: "provider", DisplayName: "Alice", Pseudonym: "guest-a7f3"}
	})
	if got, _ := PeerDisplayID(p); got != "studio-mac (Alice)" {
		t.Errorf("PeerDisplayID = %q, want the team label", got)
	}
}

func TestGrantKindPredicates(t *testing.T) {
	for _, tc := range []struct {
		name         string
		g            *signer.PeerGrant
		team, public bool
	}{
		{"nil", nil, false, false},
		{"public", &signer.PeerGrant{Kind: "public"}, false, true},
		{"team", &signer.PeerGrant{Kind: "team"}, true, false},
		{"unknown", &signer.PeerGrant{Kind: "org"}, false, false},
		{"empty", &signer.PeerGrant{}, false, false},
	} {
		if got := IsTeamGrant(tc.g); got != tc.team {
			t.Errorf("%s: IsTeamGrant = %v, want %v", tc.name, got, tc.team)
		}
		if got := IsPublicGrant(tc.g); got != tc.public {
			t.Errorf("%s: IsPublicGrant = %v, want %v", tc.name, got, tc.public)
		}
	}
}
