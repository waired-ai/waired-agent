package main

import (
	"context"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/inference"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// Team share spec §10.2: a teammate's computer is shown as "<device>
// (<owner>)" and its device id never is, so that label is how an operator
// names it — and a device name alone still works when it is unambiguous.
// Record of today's behaviour for the matching order; the no-device-id
// rule is the spec's.
func TestAgentPinger_TeammateResolvesByLabel(t *testing.T) {
	teammate := func(name, id, ip, owner string) signer.NetworkMapPeer {
		return signer.NetworkMapPeer{DeviceName: name, DeviceID: id, OverlayIP: ip,
			Grant: &signer.PeerGrant{Kind: signer.GrantKindTeam, Role: signer.GrantRoleBoth, DisplayName: owner}}
	}

	p := &fakeOverlayPinger{resp: inference.PingResponse{OK: true}}
	pinger := pingerWithPeers(p, teammate("studio-mac", "dev_team_a", "100.87.131.5", "Alice"))
	if _, err := pinger.PingPeer(context.Background(), "studio-mac"); err != nil {
		t.Fatalf("PingPeer by an unambiguous device name: %v", err)
	}

	p = &fakeOverlayPinger{resp: inference.PingResponse{OK: true}}
	pinger = pingerWithPeers(p,
		teammate("studio-mac", "dev_team_a", "100.87.131.5", "Alice"),
		teammate("studio-mac", "dev_team_b", "100.87.131.9", "Bob"),
	)
	_, err := pinger.PingPeer(context.Background(), "studio-mac")
	if err == nil {
		t.Fatal("two teammates' same-named machines resolved instead of being refused")
	}
	for _, id := range []string{"dev_team_a", "dev_team_b"} {
		if strings.Contains(err.Error(), id) {
			t.Errorf("the ambiguity error printed a teammate's device id: %v", err)
		}
	}
	if !strings.Contains(err.Error(), "studio-mac (Bob)") {
		t.Errorf("the ambiguity error does not offer the labels to choose from: %v", err)
	}
	if _, err := pinger.PingPeer(context.Background(), "studio-mac (Bob)"); err != nil {
		t.Fatalf("PingPeer by the team label: %v", err)
	}
	if p.gotAddr.String() != "100.87.131.9" {
		t.Errorf("dialled %v, want Bob's machine at 100.87.131.9", p.gotAddr)
	}
}

// Team share spec §10.2 / public share spec §8.5: the remote answers a
// ping with its own DeviceID, and `waired ping` printed it verbatim as
// device_from_peer — another account's device id on a CLI surface, for
// a teammate and for a public machine alike. A grant peer is named by its
// label; one of this account's machines keeps its device id.
func TestAgentPinger_DeviceFromPeerNeverShowsAnotherAccountsID(t *testing.T) {
	cases := []struct {
		name string
		peer signer.NetworkMapPeer
		ask  string
		want string
	}{
		{
			name: "teammate",
			peer: signer.NetworkMapPeer{DeviceName: "studio-mac", DeviceID: "dev_team_a", OverlayIP: "100.87.131.5",
				Grant: &signer.PeerGrant{ID: "grant_t", Kind: signer.GrantKindTeam, Role: signer.GrantRoleBoth, DisplayName: "Alice"}},
			ask:  "studio-mac (Alice)",
			want: "studio-mac (Alice)",
		},
		{
			name: "public machine",
			peer: signer.NetworkMapPeer{DeviceName: "pub-node-b21c", DeviceID: "dev_stranger", OverlayIP: "100.87.131.6",
				Grant: &signer.PeerGrant{ID: "grant_p", Kind: signer.GrantKindPublic, Role: signer.GrantRoleProvider, Pseudonym: "pub-node-b21c"}},
			ask:  "pub-node-b21c",
			want: "pub-node-b21c",
		},
		{
			name: "own machine keeps its device id",
			peer: signer.NetworkMapPeer{DeviceName: "linux-gpu", DeviceID: "dev_own", OverlayIP: "100.87.131.7"},
			ask:  "linux-gpu",
			want: "dev_own",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The fake answers the way the real overlay /waired/v1/ping
			// does: with the remote's own DeviceID.
			p := &fakeOverlayPinger{resp: inference.PingResponse{OK: true, Device: tc.peer.DeviceID}}
			res, err := pingerWithPeers(p, tc.peer).PingPeer(context.Background(), tc.ask)
			if err != nil {
				t.Fatalf("PingPeer: %v", err)
			}
			if res.DeviceFromPeer != tc.want {
				t.Errorf("device_from_peer = %q, want %q", res.DeviceFromPeer, tc.want)
			}
		})
	}
}

// One teammate with two machines of the same name gives two identical
// labels, and "use one of those names" would loop. The message says
// what does work instead, and still prints no device id.
func TestAgentPinger_SameTeammateSameNameSaysRename(t *testing.T) {
	twin := func(id, ip string) signer.NetworkMapPeer {
		return signer.NetworkMapPeer{DeviceName: "studio-mac", DeviceID: id, OverlayIP: ip,
			Grant: &signer.PeerGrant{Kind: signer.GrantKindTeam, Role: signer.GrantRoleProvider, DisplayName: "Alice"}}
	}
	p := &fakeOverlayPinger{resp: inference.PingResponse{OK: true}}
	_, err := pingerWithPeers(p, twin("dev_team_a", "100.87.131.5"), twin("dev_team_b", "100.87.131.9")).
		PingPeer(context.Background(), "studio-mac (Alice)")
	if err == nil || !strings.Contains(err.Error(), "rename one") {
		t.Fatalf("err = %v, want the rename hint", err)
	}
	if strings.Contains(err.Error(), "dev_team_") {
		t.Fatalf("err = %v leaks a teammate's device id", err)
	}
}
