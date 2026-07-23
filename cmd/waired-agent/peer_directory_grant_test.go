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

	// The whole grant annotation rides along (waired#824): the serving
	// gate chain classifies public consumers on Kind/Role.
	if foreign.Grant == nil || foreign.Grant.ID != "grant_1" || foreign.Grant.Kind != "public" || foreign.Grant.Role != "consumer" {
		t.Fatalf("foreign Grant = %+v, want the full PeerGrant copied", foreign.Grant)
	}
	if !foreign.IsPublicConsumer() {
		t.Fatal("foreign IsPublicConsumer() = false, want true")
	}
	if own.Grant != nil || own.IsPublicConsumer() {
		t.Fatalf("own peer: Grant=%+v IsPublicConsumer=%v, want nil/false", own.Grant, own.IsPublicConsumer())
	}
	if own.IsForeignGrantPeer() {
		t.Fatal("own peer: IsForeignGrantPeer() = true, want false")
	}
	if !foreign.IsForeignGrantPeer() {
		t.Fatal("foreign peer: IsForeignGrantPeer() = false, want true")
	}
}

// TestPeerDirectory_ProviderRoleGrantPeer pins the classification the
// serving-side grantRoleGate depends on (waired#896): a peer we CONSUME
// from is indexed with its real machine key (the WG peering is
// bidirectional and it must be able to answer us), but it resolves as a
// foreign grant peer that is NOT a public consumer — the identity the
// gate refuses on the inbound path.
func TestPeerDirectory_ProviderRoleGrantPeer(t *testing.T) {
	d := newPeerDirectory()
	_, b64 := mustKeyB64(t)
	d.Update(&signer.NetworkMap{
		Peers: []signer.NetworkMapPeer{
			{
				DeviceID: "dev-foreign-provider", OverlayIP: "100.99.0.4", MachinePublicKey: b64,
				Grant: &signer.PeerGrant{ID: "grant_2", Kind: "public", Role: "provider", Pseudonym: "pub-node-11"},
			},
		},
	})

	p, ok := d.LookupByOverlayIP(netip.MustParseAddr("100.99.0.4"))
	if !ok {
		t.Fatal("provider-role grant peer not indexed")
	}
	if !p.IsForeignGrantPeer() {
		t.Fatal("IsForeignGrantPeer() = false, want true")
	}
	if p.IsPublicConsumer() {
		t.Fatal("IsPublicConsumer() = true, want false — this peer serves US, not the other way round")
	}
	if got := p.DisplayName(); got != "pub-node-11" {
		t.Fatalf("DisplayName = %q, want pseudonym", got)
	}
}
