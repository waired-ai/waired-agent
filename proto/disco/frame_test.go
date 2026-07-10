package disco

import (
	"bytes"
	"net/netip"
	"testing"
)

func TestFrameRoundtripSTUNRequest(t *testing.T) {
	in := &Frame{
		Type:         TypeSTUNRequest,
		SrcDeviceID:  "dev_abc",
		HasNonce:     true,
		Nonce:        [NonceSize]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		HasTimestamp: true,
		Timestamp:    1234567890123,
		HMACTag:      bytes.Repeat([]byte{0xab}, HMACTagSize),
	}
	raw, err := in.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	out, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if out.Type != in.Type ||
		out.SrcDeviceID != in.SrcDeviceID ||
		!out.HasNonce || out.Nonce != in.Nonce ||
		!out.HasTimestamp || out.Timestamp != in.Timestamp ||
		!bytes.Equal(out.HMACTag, in.HMACTag) {
		t.Fatalf("roundtrip mismatch: got %+v", out)
	}
}

func TestFrameRoundtripSTUNResponse(t *testing.T) {
	addr := netip.MustParseAddrPort("203.0.113.7:51820")
	in := &Frame{
		Type:         TypeSTUNResponse,
		HasNonce:     true,
		Nonce:        [NonceSize]byte{0xde, 0xad, 0xbe, 0xef},
		HasTimestamp: true,
		Timestamp:    1700000000000,
		HasObserved:  true,
		ObservedAddr: addr,
		HMACTag:      bytes.Repeat([]byte{0xcd}, HMACTagSize),
	}
	raw, err := in.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	out, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !out.HasObserved || out.ObservedAddr != addr {
		t.Fatalf("ObservedAddr roundtrip: got %v, want %v", out.ObservedAddr, addr)
	}
}

func TestFrameRoundtripCallMeMaybeWithCandidates(t *testing.T) {
	v4 := netip.MustParseAddrPort("198.51.100.10:51820")
	v6 := netip.MustParseAddrPort("[2001:db8::1]:51820")
	in := &Frame{
		Type:          TypeCallMeMaybe,
		SrcDeviceID:   "dev_initiator",
		DstDeviceID:   "dev_responder",
		HasNonce:      true,
		Nonce:         [NonceSize]byte{0x42},
		HasTimestamp:  true,
		Timestamp:     1700000000000,
		CandidateList: []netip.AddrPort{v4, v6},
		Ed25519Sig:    bytes.Repeat([]byte{0x07}, Ed25519SigSize),
	}
	raw, err := in.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	out, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out.CandidateList) != 2 || out.CandidateList[0] != v4 || out.CandidateList[1] != v6 {
		t.Fatalf("candidate list roundtrip: got %v", out.CandidateList)
	}
	if out.SrcDeviceID != "dev_initiator" || out.DstDeviceID != "dev_responder" {
		t.Fatalf("src/dst roundtrip: got src=%q dst=%q", out.SrcDeviceID, out.DstDeviceID)
	}
	if !bytes.Equal(out.Ed25519Sig, in.Ed25519Sig) {
		t.Fatalf("Ed25519Sig mismatch")
	}
}

func TestSignedPrefixHMAC(t *testing.T) {
	in := &Frame{
		Type:         TypeSTUNRequest,
		SrcDeviceID:  "dev",
		HasNonce:     true,
		Nonce:        [NonceSize]byte{1, 2, 3},
		HasTimestamp: true,
		Timestamp:    1000,
		HMACTag:      bytes.Repeat([]byte{0xff}, HMACTagSize),
	}
	raw, err := in.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	prefix, err := SignedPrefix(raw)
	if err != nil {
		t.Fatalf("SignedPrefix: %v", err)
	}
	// Prefix must be everything before the HMAC TLV's tag byte.
	// Encoding the same frame with HMACTag cleared then probing length:
	bare := *in
	bare.HMACTag = nil
	bareRaw, err := bare.Encode()
	if err != nil {
		t.Fatalf("Encode bare: %v", err)
	}
	if !bytes.Equal(prefix, bareRaw) {
		t.Fatalf("SignedPrefix should equal frame without auth TLV.\n  got:  %x\n  want: %x", prefix, bareRaw)
	}
}

func TestSignedPrefixEd25519(t *testing.T) {
	in := &Frame{
		Type:         TypeProbe,
		SrcDeviceID:  "a",
		DstDeviceID:  "b",
		HasNonce:     true,
		Nonce:        [NonceSize]byte{0xaa},
		HasTimestamp: true,
		Timestamp:    1,
		CandidateID:  "local-v4-0",
		Ed25519Sig:   bytes.Repeat([]byte{0x01}, Ed25519SigSize),
	}
	raw, err := in.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	prefix, err := SignedPrefix(raw)
	if err != nil {
		t.Fatalf("SignedPrefix: %v", err)
	}
	if !bytes.HasPrefix(raw, prefix) {
		t.Fatalf("prefix is not a true prefix of raw")
	}
	// The slice cut off should equal the encoded TLV header (3 B) +
	// the 64 B sig.
	if len(raw)-len(prefix) != 3+Ed25519SigSize {
		t.Fatalf("trailing auth TLV size mismatch: got %d, want %d",
			len(raw)-len(prefix), 3+Ed25519SigSize)
	}
}

func TestDecodeRejectsBadMagic(t *testing.T) {
	raw := []byte("XXXXXX\x01\x00")
	if _, err := Decode(raw); err == nil {
		t.Fatal("expected bad-magic error")
	}
}

func TestDecodeRejectsTruncatedTLV(t *testing.T) {
	// Header OK, body claims 100-byte string but only 5 follow.
	raw := append([]byte{}, Magic[:]...)
	raw = append(raw, byte(TypeSTUNRequest), 0)
	raw = append(raw, byte(TagSrcDeviceID), 0x00, 0x64, 'a', 'b', 'c', 'd', 'e')
	if _, err := Decode(raw); err == nil {
		t.Fatal("expected truncated-TLV error")
	}
}

func TestDecodeRejectsAuthNotLast(t *testing.T) {
	// Manually craft: header + HMACTag TLV (32B) + a SrcDeviceID TLV
	// after it (illegal).
	raw := append([]byte{}, Magic[:]...)
	raw = append(raw, byte(TypeSTUNRequest), 0)
	raw = append(raw, byte(TagHMACTag), 0x00, 0x20)
	raw = append(raw, bytes.Repeat([]byte{1}, HMACTagSize)...)
	raw = append(raw, byte(TagSrcDeviceID), 0x00, 0x03, 'a', 'b', 'c')
	if _, _, err := findAuthTLV(raw); err == nil {
		t.Fatal("expected auth-not-last error from findAuthTLV")
	}
}

func TestEncodeRejectsCandidateListOverflow(t *testing.T) {
	in := &Frame{Type: TypeCallMeMaybe}
	for i := 0; i < MaxCandidateListLen+1; i++ {
		in.CandidateList = append(in.CandidateList, netip.MustParseAddrPort("1.2.3.4:51820"))
	}
	if _, err := in.Encode(); err == nil {
		t.Fatal("expected MaxCandidateListLen overflow error")
	}
}

func TestEncodeRejectsInvalidHMACSize(t *testing.T) {
	in := &Frame{
		Type:    TypeSTUNRequest,
		HMACTag: []byte{0x01, 0x02, 0x03}, // wrong size
	}
	if _, err := in.Encode(); err == nil {
		t.Fatal("expected hmac size error")
	}
}

func TestAddrV4V6Roundtrip(t *testing.T) {
	cases := []netip.AddrPort{
		netip.MustParseAddrPort("0.0.0.0:0"),
		netip.MustParseAddrPort("255.255.255.255:65535"),
		netip.MustParseAddrPort("[::]:0"),
		netip.MustParseAddrPort("[2001:db8::1]:443"),
	}
	for _, c := range cases {
		enc, err := encodeAddr(c)
		if err != nil {
			t.Fatalf("encodeAddr(%v): %v", c, err)
		}
		got, err := decodeAddr(enc)
		if err != nil {
			t.Fatalf("decodeAddr(%v): %v", c, err)
		}
		if got != c {
			t.Fatalf("roundtrip %v != %v", got, c)
		}
	}
}
