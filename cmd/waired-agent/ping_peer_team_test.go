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
