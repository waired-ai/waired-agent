package main

import (
	"net/netip"
	"testing"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// TestPeerDirectory_GrantPseudonym pins the §8.5 identity plumbing: a
// grant-tagged foreign peer resolves to a PeerIdentity carrying the
// grant pseudonym, and DisplayName prefers it over the real DeviceID
// so serving-side logs never print foreign device identifiers.
func TestPeerDirectory_GrantPseudonym(t *testing.T) {
	d := newPeerDirectory()
	_, b64A := mustKeyB64(t)
	_, b64B := mustKeyB64(t)
	d.Update(&signer.NetworkMap{
		Peers: []signer.NetworkMapPeer{
			{DeviceID: "dev-own", OverlayIP: "100.64.0.2", MachinePublicKey: b64A},
			{
				DeviceID: "dev-foreign", OverlayIP: "100.99.0.3", MachinePublicKey: b64B,
				Grant: &signer.PeerGrant{ID: "grant_1", Kind: "public", Role: "consumer", Pseudonym: "amber-fox-42"},
			},
		},
	})

	own, ok := d.LookupByOverlayIP(netip.MustParseAddr("100.64.0.2"))
	if !ok || own.Pseudonym != "" {
		t.Fatalf("own peer: ok=%v pseudonym=%q, want empty pseudonym", ok, own.Pseudonym)
	}
	if got := own.DisplayName(); got != "dev-own" {
		t.Fatalf("own DisplayName = %q, want DeviceID", got)
	}

	foreign, ok := d.LookupByOverlayIP(netip.MustParseAddr("100.99.0.3"))
	if !ok || foreign.Pseudonym != "amber-fox-42" {
		t.Fatalf("foreign peer: ok=%v pseudonym=%q", ok, foreign.Pseudonym)
	}
	if got := foreign.DisplayName(); got != "amber-fox-42" {
		t.Fatalf("foreign DisplayName = %q, want pseudonym", got)
	}
}
