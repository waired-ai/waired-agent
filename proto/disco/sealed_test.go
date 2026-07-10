package disco

import (
	"crypto/rand"
	"net/netip"
	"testing"

	"golang.org/x/crypto/curve25519"
)

// keypair generates a fresh curve25519 keypair for tests. Returns
// (priv, pub) so callers can match the EncodeSealed / DecodeSealed
// argument shape directly.
func keypair(t *testing.T) (priv, pub [NodeKeySize]byte) {
	t.Helper()
	if _, err := rand.Read(priv[:]); err != nil {
		t.Fatalf("rand priv: %v", err)
	}
	// X25519 clamping: standard curve25519 scalar massaging so
	// X25519(priv, Basepoint) yields a valid point.
	priv[0] &= 248
	priv[31] &= 127
	priv[31] |= 64
	out, err := curve25519.X25519(priv[:], curve25519.Basepoint)
	if err != nil {
		t.Fatalf("X25519 derive pub: %v", err)
	}
	copy(pub[:], out)
	return priv, pub
}

func TestSealedRoundtripProbe(t *testing.T) {
	aPriv, aPub := keypair(t)
	bPriv, bPub := keypair(t)

	in := &Frame{
		Type:         TypeProbe,
		SrcDeviceID:  "dev_a",
		DstDeviceID:  "dev_b",
		CandidateID:  "local-v4-0",
		HasNonce:     true,
		Nonce:        [NonceSize]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		HasTimestamp: true,
		Timestamp:    1234567890123,
	}
	raw, err := EncodeSealed(in, aPriv, aPub, bPub)
	if err != nil {
		t.Fatalf("EncodeSealed: %v", err)
	}
	if len(raw) < SealedHeaderSize+AEADTagSize {
		t.Fatalf("encoded too short: %d", len(raw))
	}
	if !IsSealed(raw) {
		t.Fatalf("IsSealed returned false on freshly-encoded sealed frame")
	}

	out, src, err := DecodeSealed(raw, bPriv, bPub)
	if err != nil {
		t.Fatalf("DecodeSealed: %v", err)
	}
	if src != aPub {
		t.Fatalf("srcNodeKey mismatch")
	}
	if out.Type != in.Type ||
		out.SrcDeviceID != in.SrcDeviceID ||
		out.DstDeviceID != in.DstDeviceID ||
		out.CandidateID != in.CandidateID ||
		!out.HasNonce || out.Nonce != in.Nonce ||
		!out.HasTimestamp || out.Timestamp != in.Timestamp {
		t.Fatalf("roundtrip mismatch: got %+v", out)
	}
}

func TestSealedRoundtripPongWithObserved(t *testing.T) {
	aPriv, aPub := keypair(t)
	bPriv, bPub := keypair(t)
	addr := netip.MustParseAddrPort("[2001:db8::1]:51820")

	in := &Frame{
		Type:         TypePong,
		SrcDeviceID:  "dev_b",
		DstDeviceID:  "dev_a",
		HasObserved:  true,
		ObservedAddr: addr,
		HasNonce:     true,
		Nonce:        [NonceSize]byte{0xab},
		HasTimestamp: true,
		Timestamp:    1700000000000,
	}
	raw, err := EncodeSealed(in, bPriv, bPub, aPub)
	if err != nil {
		t.Fatalf("EncodeSealed: %v", err)
	}
	out, src, err := DecodeSealed(raw, aPriv, aPub)
	if err != nil {
		t.Fatalf("DecodeSealed: %v", err)
	}
	if src != bPub {
		t.Fatalf("src mismatch")
	}
	if !out.HasObserved || out.ObservedAddr != addr {
		t.Fatalf("ObservedAddr roundtrip: got %v", out.ObservedAddr)
	}
}

func TestSealedRoundtripCallMeMaybeWithCandidates(t *testing.T) {
	aPriv, aPub := keypair(t)
	bPriv, bPub := keypair(t)
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
	}
	raw, err := EncodeSealed(in, aPriv, aPub, bPub)
	if err != nil {
		t.Fatalf("EncodeSealed: %v", err)
	}
	out, _, err := DecodeSealed(raw, bPriv, bPub)
	if err != nil {
		t.Fatalf("DecodeSealed: %v", err)
	}
	if len(out.CandidateList) != 2 || out.CandidateList[0] != v4 || out.CandidateList[1] != v6 {
		t.Fatalf("candidate list roundtrip: got %v", out.CandidateList)
	}
}

// TestSealedDecodeRejectsTamper flips a byte in the ciphertext and
// asserts AEAD open fails (Poly1305 authentication catches it).
func TestSealedDecodeRejectsCiphertextTamper(t *testing.T) {
	aPriv, aPub := keypair(t)
	bPriv, bPub := keypair(t)

	raw, err := EncodeSealed(&Frame{Type: TypeProbe, SrcDeviceID: "a"}, aPriv, aPub, bPub)
	if err != nil {
		t.Fatalf("EncodeSealed: %v", err)
	}
	tampered := append([]byte(nil), raw...)
	tampered[len(tampered)-1] ^= 0x01
	if _, _, err := DecodeSealed(tampered, bPriv, bPub); err == nil {
		t.Fatal("expected AEAD open to reject tampered ciphertext")
	}
}

// TestSealedDecodeRejectsHeaderTamper flips a byte in the AD region.
// AEAD MUST reject because AD is included in the Poly1305 computation.
func TestSealedDecodeRejectsHeaderTamper(t *testing.T) {
	aPriv, aPub := keypair(t)
	bPriv, bPub := keypair(t)

	raw, err := EncodeSealed(&Frame{Type: TypeProbe}, aPriv, aPub, bPub)
	if err != nil {
		t.Fatalf("EncodeSealed: %v", err)
	}
	tampered := append([]byte(nil), raw...)
	// Flip a bit inside the AEAD nonce region (part of the AD).
	tampered[7+NodeKeySize+1] ^= 0x01
	if _, _, err := DecodeSealed(tampered, bPriv, bPub); err == nil {
		t.Fatal("expected AEAD to reject header tamper")
	}
}

// TestSealedDecodeRejectsWrongReceiver: ECDH yields a different shared,
// HKDF → AEAD key mismatch → Poly1305 fails.
func TestSealedDecodeRejectsWrongReceiver(t *testing.T) {
	aPriv, aPub := keypair(t)
	_, bPub := keypair(t)
	cPriv, cPub := keypair(t)

	raw, err := EncodeSealed(&Frame{Type: TypeProbe}, aPriv, aPub, bPub)
	if err != nil {
		t.Fatalf("EncodeSealed: %v", err)
	}
	if _, _, err := DecodeSealed(raw, cPriv, cPub); err == nil {
		t.Fatal("expected AEAD open to fail with wrong receiver key")
	}
}

func TestSealedRejectsBadMagic(t *testing.T) {
	priv, pub := keypair(t)
	raw := make([]byte, SealedHeaderSize+AEADTagSize)
	copy(raw, []byte("XXXXXX"))
	raw[6] = SealedVersion
	if _, _, err := DecodeSealed(raw, priv, pub); err == nil {
		t.Fatal("expected bad-magic error")
	}
}

func TestSealedRejectsBadVersion(t *testing.T) {
	priv, pub := keypair(t)
	raw := make([]byte, SealedHeaderSize+AEADTagSize)
	copy(raw, Magic[:])
	raw[6] = 0xff
	if _, _, err := DecodeSealed(raw, priv, pub); err == nil {
		t.Fatal("expected unsupported-version error")
	}
}

func TestSealedRejectsTooShort(t *testing.T) {
	priv, pub := keypair(t)
	raw := []byte{0x57, 0x44, 0x49, 0x53, 0x43, 0x4f, SealedVersion}
	if _, _, err := DecodeSealed(raw, priv, pub); err == nil {
		t.Fatal("expected short-frame error")
	}
}

func TestParseSealedHeader(t *testing.T) {
	aPriv, aPub := keypair(t)
	_, bPub := keypair(t)
	raw, err := EncodeSealed(&Frame{Type: TypeProbe}, aPriv, aPub, bPub)
	if err != nil {
		t.Fatalf("EncodeSealed: %v", err)
	}
	h, err := ParseSealedHeader(raw)
	if err != nil {
		t.Fatalf("ParseSealedHeader: %v", err)
	}
	if h.Vers != SealedVersion {
		t.Fatalf("Vers: got %d want %d", h.Vers, SealedVersion)
	}
	if h.SrcNodeKey != aPub {
		t.Fatalf("SrcNodeKey mismatch")
	}
}

// TestDirectionalKeySeparation: A→B and B→A must derive different keys.
// A frame A→B MUST NOT open as a frame B→A even though both legs share
// the same ECDH shared secret.
func TestDirectionalKeySeparation(t *testing.T) {
	aPriv, aPub := keypair(t)
	bPriv, bPub := keypair(t)

	raw, err := EncodeSealed(&Frame{Type: TypeProbe}, aPriv, aPub, bPub)
	if err != nil {
		t.Fatalf("EncodeSealed: %v", err)
	}
	// Decoding as the wrong endpoint (A trying to read its own outgoing
	// frame as if B had sent it): wrong info, wrong key.
	if _, _, err := DecodeSealed(raw, aPriv, aPub); err == nil {
		t.Fatal("expected AEAD reject when decoder swaps direction")
	}
	if _, _, err := DecodeSealed(raw, bPriv, bPub); err != nil {
		t.Fatalf("legitimate B-side decode failed: %v", err)
	}
}

// TestSealedEncodeRejectsCandidateListOverflow keeps the existing guard
// in the sealed inner encoder.
func TestSealedEncodeRejectsCandidateListOverflow(t *testing.T) {
	aPriv, aPub := keypair(t)
	_, bPub := keypair(t)
	in := &Frame{Type: TypeCallMeMaybe}
	for i := 0; i < MaxCandidateListLen+1; i++ {
		in.CandidateList = append(in.CandidateList, netip.MustParseAddrPort("1.2.3.4:51820"))
	}
	if _, err := EncodeSealed(in, aPriv, aPub, bPub); err == nil {
		t.Fatal("expected MaxCandidateListLen overflow error")
	}
}

func TestIsSealedRejectsPlaintext(t *testing.T) {
	// Build a plaintext probe frame the legacy way and ensure IsSealed
	// returns false — the dispatcher must NOT misroute it.
	f := &Frame{Type: TypeProbe, Ed25519Sig: make([]byte, Ed25519SigSize)}
	raw, err := f.Encode()
	if err != nil {
		t.Fatalf("legacy Encode: %v", err)
	}
	if IsSealed(raw) {
		t.Fatal("IsSealed misidentified plaintext probe as sealed")
	}
}
