package wgnet

import (
	"encoding/json"
	"strings"
	"testing"
)

// Internal-package test: newEncryptedPacket is the single frame
// constructor for both relay send paths (WG payloads and disco), so
// pinning it here covers the §10 DstNetworkID stamping for both.

func stampTestBind() *MultiplexBind {
	return &MultiplexBind{
		selfDeviceID:  "dev_self",
		selfNetworkID: "net_self",
		selfNodePub:   "self-node-pub",
	}
}

func TestNewEncryptedPacketStampsForeignPeers(t *testing.T) {
	b := stampTestBind()
	b.SetPeerNetworks(map[string]string{
		"dev_foreign": "net_other",
		"dev_weird":   "net_self", // must NOT stamp (same as self)
	})

	pkt := b.newEncryptedPacket("dev_foreign", "nk", []byte("payload"))
	if pkt.DstNetworkID != "net_other" {
		t.Fatalf("DstNetworkID = %q, want net_other", pkt.DstNetworkID)
	}
	if pkt.NetworkID != "net_self" || pkt.SrcDeviceID != "dev_self" || pkt.DstDeviceID != "dev_foreign" {
		t.Fatalf("frame identity fields: %+v", pkt)
	}

	// Same-network entry (defensive) and unknown peers: no stamp.
	if pkt := b.newEncryptedPacket("dev_weird", "nk", nil); pkt.DstNetworkID != "" {
		t.Fatalf("self-network entry stamped: %q", pkt.DstNetworkID)
	}
	if pkt := b.newEncryptedPacket("dev_same_net", "nk", nil); pkt.DstNetworkID != "" {
		t.Fatalf("unknown peer stamped: %q", pkt.DstNetworkID)
	}
}

// TestNewEncryptedPacketSameNetworkWireBytes pins the compatibility
// property: frames to same-network peers must not grow a
// dst_network_id key (omitempty), so pre-#822 relays and agents see
// byte-identical wire forms.
func TestNewEncryptedPacketSameNetworkWireBytes(t *testing.T) {
	b := stampTestBind()
	b.SetPeerNetworks(map[string]string{"dev_foreign": "net_other"})

	same, err := json.Marshal(b.newEncryptedPacket("dev_same", "nk", []byte("x")))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(same), "dst_network_id") {
		t.Fatalf("same-network frame carries dst_network_id: %s", same)
	}
	foreign, err := json.Marshal(b.newEncryptedPacket("dev_foreign", "nk", []byte("x")))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(foreign), `"dst_network_id":"net_other"`) {
		t.Fatalf("foreign frame missing stamp: %s", foreign)
	}
}

func TestSetPeerNetworksReplacesAndClears(t *testing.T) {
	b := stampTestBind()
	b.SetPeerNetworks(map[string]string{"a": "net_1"})
	if got := b.peerNetworkFor("a"); got != "net_1" {
		t.Fatalf("peerNetworkFor(a) = %q", got)
	}
	// Wholesale replace drops old entries.
	b.SetPeerNetworks(map[string]string{"b": "net_2"})
	if got := b.peerNetworkFor("a"); got != "" {
		t.Fatalf("stale entry survived replace: %q", got)
	}
	// Empty/nil clears.
	b.SetPeerNetworks(nil)
	if got := b.peerNetworkFor("b"); got != "" {
		t.Fatalf("entry survived clear: %q", got)
	}
	// The caller's map is copied, not aliased.
	src := map[string]string{"c": "net_3"}
	b.SetPeerNetworks(src)
	src["c"] = "net_mutated"
	if got := b.peerNetworkFor("c"); got != "net_3" {
		t.Fatalf("registry aliased the caller's map: %q", got)
	}
}
