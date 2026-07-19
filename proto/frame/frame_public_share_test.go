package frame

import (
	"bytes"
	"testing"
)

// TestEncryptedPacketWithoutDstNetworkID_KeepsWireForm pins the §10
// compatibility property: a same-network packet (DstNetworkID unset)
// encodes to bytes with no dst_network_id key, i.e. exactly the
// pre-v0.2.0 frame, so relays and agents that predate the field see an
// unchanged stream.
func TestEncryptedPacketWithoutDstNetworkID_KeepsWireForm(t *testing.T) {
	raw, err := Encode(EncryptedPacket{
		Version:     Version,
		NetworkID:   "wn_1",
		SrcDeviceID: "dev_a",
		DstDeviceID: "dev_b",
		Payload:     "AAAA",
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if bytes.Contains(raw, []byte("dst_network_id")) {
		t.Fatalf("same-network packet unexpectedly carries dst_network_id:\n%s", raw)
	}
}

// TestEncryptedPacketWithDstNetworkID_RoundTrip covers the
// cross-network forwarding path: the destination network survives
// Encode/Decode intact.
func TestEncryptedPacketWithDstNetworkID_RoundTrip(t *testing.T) {
	pkt := EncryptedPacket{
		Version:      Version,
		NetworkID:    "wn_consumer",
		SrcDeviceID:  "dev_a",
		DstDeviceID:  "dev_b",
		DstNetworkID: "wn_provider",
		Payload:      "AAAA",
	}
	raw, err := Encode(pkt)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, typ, err := Decode(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if typ != TypeEncryptedPacket {
		t.Fatalf("decoded type %q, want %q", typ, TypeEncryptedPacket)
	}
	got, ok := decoded.(EncryptedPacket)
	if !ok {
		t.Fatalf("decoded %T, want EncryptedPacket", decoded)
	}
	if got.DstNetworkID != "wn_provider" {
		t.Fatalf("DstNetworkID = %q, want %q", got.DstNetworkID, "wn_provider")
	}
}
