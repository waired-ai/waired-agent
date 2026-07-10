package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net/netip"
	"testing"

	"github.com/waired-ai/waired-agent/proto/signer"
)

func mustKeyB64(t *testing.T) (ed25519.PublicKey, string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	return pub, base64.StdEncoding.EncodeToString(pub)
}

func TestPeerDirectory_Update_PopulatesIndex(t *testing.T) {
	d := newPeerDirectory()
	keyA, b64A := mustKeyB64(t)
	_, b64B := mustKeyB64(t)
	nm := &signer.NetworkMap{
		Peers: []signer.NetworkMapPeer{
			{DeviceID: "dev-A", OverlayIP: "100.96.0.10", MachinePublicKey: b64A},
			{DeviceID: "dev-B", OverlayIP: "100.96.0.11", MachinePublicKey: b64B},
		},
	}
	d.Update(nm)

	got, ok := d.LookupByOverlayIP(netip.MustParseAddr("100.96.0.10"))
	if !ok || got.DeviceID != "dev-A" {
		t.Fatalf("dev-A: ok=%v got=%+v", ok, got)
	}
	if string(got.MachineKey) != string(keyA) {
		t.Fatalf("dev-A key mismatch")
	}
	if got, ok := d.LookupByOverlayIP(netip.MustParseAddr("100.96.0.11")); !ok || got.DeviceID != "dev-B" {
		t.Fatalf("dev-B: ok=%v got=%+v", ok, got)
	}
	if _, ok := d.LookupByOverlayIP(netip.MustParseAddr("100.96.0.99")); ok {
		t.Fatalf("unknown IP must miss")
	}
}

func TestPeerDirectory_Update_ReplacesWholesale(t *testing.T) {
	d := newPeerDirectory()
	_, b64A := mustKeyB64(t)
	d.Update(&signer.NetworkMap{Peers: []signer.NetworkMapPeer{
		{DeviceID: "dev-A", OverlayIP: "100.96.0.10", MachinePublicKey: b64A},
	}})

	// Next frame removes dev-A entirely (e.g. revoked) and adds dev-C.
	_, b64C := mustKeyB64(t)
	d.Update(&signer.NetworkMap{Peers: []signer.NetworkMapPeer{
		{DeviceID: "dev-C", OverlayIP: "100.96.0.12", MachinePublicKey: b64C},
	}})

	if _, ok := d.LookupByOverlayIP(netip.MustParseAddr("100.96.0.10")); ok {
		t.Fatalf("removed peer must drop out")
	}
	if got, ok := d.LookupByOverlayIP(netip.MustParseAddr("100.96.0.12")); !ok || got.DeviceID != "dev-C" {
		t.Fatalf("new peer not visible: ok=%v got=%+v", ok, got)
	}
}

func TestPeerDirectory_SkipsBadEntries(t *testing.T) {
	d := newPeerDirectory()
	_, b64Good := mustKeyB64(t)
	d.Update(&signer.NetworkMap{Peers: []signer.NetworkMapPeer{
		// Missing overlay IP.
		{DeviceID: "no-ip", OverlayIP: "", MachinePublicKey: b64Good},
		// Unparseable overlay IP.
		{DeviceID: "bad-ip", OverlayIP: "not-an-ip", MachinePublicKey: b64Good},
		// Missing public key.
		{DeviceID: "no-key", OverlayIP: "100.96.0.20", MachinePublicKey: ""},
		// Malformed base64.
		{DeviceID: "bad-key", OverlayIP: "100.96.0.21", MachinePublicKey: "not!base64"},
		// Wrong-length key.
		{DeviceID: "short", OverlayIP: "100.96.0.22", MachinePublicKey: base64.StdEncoding.EncodeToString([]byte("too-short"))},
	}})
	for _, ip := range []string{"100.96.0.20", "100.96.0.21", "100.96.0.22"} {
		if _, ok := d.LookupByOverlayIP(netip.MustParseAddr(ip)); ok {
			t.Fatalf("%s should have been skipped", ip)
		}
	}
}

func TestPeerDirectory_NilNetworkMap_NoPanic(t *testing.T) {
	d := newPeerDirectory()
	d.Update(nil)
	if _, ok := d.LookupByOverlayIP(netip.MustParseAddr("100.96.0.10")); ok {
		t.Fatalf("nil update must leave directory empty")
	}
}
