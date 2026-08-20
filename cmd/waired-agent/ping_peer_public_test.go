package main

import (
	"context"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// PRODUCT CONTRACT (waired-agent#809, owner ruling 2026-08-21): an
// ambiguity message that lists two public machines names them apart.
//
// The message exists so the operator can pick one of the candidates. Two
// public machines with no pseudonym rendered as "public machine, public
// machine" — the list still refused the name, but the escape hatch it
// points at named neither candidate, so it could not be taken.
//
// The disambiguator comes from the GRANT, never from the device, so the
// test asserts both halves: that the two rows differ, and that neither
// carries a real device identifier.
func TestAgentPinger_TwoPublicMachinesAreNamedApart(t *testing.T) {
	const (
		firstDeviceID  = "dev_stranger0000001"
		secondDeviceID = "dev_stranger0000002"
	)
	p := &fakeOverlayPinger{}
	pinger := pingerWithPeers(p,
		signer.NetworkMapPeer{
			DeviceName: "shared-name", DeviceID: firstDeviceID, OverlayIP: "100.87.131.5",
			Grant: &signer.PeerGrant{ID: "grant_first", Kind: "public", Role: "provider"},
		},
		signer.NetworkMapPeer{
			DeviceName: "shared-name", DeviceID: secondDeviceID, OverlayIP: "100.87.131.9",
			Grant: &signer.PeerGrant{ID: "grant_second", Kind: "public", Role: "provider"},
		},
	)

	_, err := pinger.PingPeer(context.Background(), "shared-name")
	if err == nil {
		t.Fatal("PingPeer resolved an ambiguous name instead of refusing it")
	}
	msg := err.Error()

	for _, id := range []string{firstDeviceID, secondDeviceID} {
		if strings.Contains(msg, id) {
			t.Errorf("the message names a public machine's real device id: %v", err)
		}
	}
	first := inferencemesh.PublicPeerLabelFor("grant_first")
	second := inferencemesh.PublicPeerLabelFor("grant_second")
	// Anti-vacuity: if the two labels ever collapse to one string the
	// assertions below would pass on a message that names neither.
	if first == second {
		t.Fatalf("both grants produced the same label %q, so this test cannot observe "+
			"whether the message tells them apart", first)
	}
	for _, want := range []string{first, second} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message does not contain %q, so the operator cannot pick one: %v", want, err)
		}
	}
}
