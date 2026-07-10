package disco

// AEAD-protected wire format for peer↔peer disco frames (probe / pong /
// call_me_maybe). Issue #122 privacy hardening. The plaintext STUN
// frames (TypeSTUNRequest / TypeSTUNResponse) keep the existing layout
// in frame.go because they need a relay-distributed X25519 pub key the
// CP does not yet surface to agents; STUN AEAD is a follow-up.
//
// Wire layout (sealed):
//
//	+----------+------+---------------+----------+--------------------+
//	| Magic    | Vers | SrcNodeKey    | Nonce    | AEAD-sealed body   |
//	| 6 B      | 1 B  | 32 B          | 12 B     | inner + 16 B tag   |
//	| "WDISCO" | 0x10 | X25519 pub    | random   | ChaCha20-Poly1305  |
//	+----------+------+---------------+----------+--------------------+
//	    51 B fixed sealed header                       variable body
//
// Magic is unchanged so wgnet's MultiplexBind classifier still
// demultiplexes disco from WireGuard on the same UDP socket. The Vers
// byte (0x10) is chosen above the plaintext Type byte range (0x01..0x05)
// so receivers can dispatch on raw[6]:
//
//	0x01, 0x02 → legacy plaintext STUN (frame.go)
//	0x10       → AEAD peer↔peer        (this file)
//	other      → drop
//
// AEAD construction:
//
//	shared = curve25519.X25519(sender_priv, receiver_pub)
//	key    = HKDF-SHA256(shared, salt=nil,
//	                     info = "waired-disco-v1" || sender_pub || receiver_pub)
//	ct+tag = ChaCha20-Poly1305.Seal(key, nonce, plaintext, ad)
//	   ad = Magic || Vers || SrcNodeKey || Nonce  (= the 51 B sealed header)
//
// Including sender_pub and receiver_pub (in that order, not lex-sorted)
// in HKDF info gives directional key separation: A→B and B→A derive
// distinct AEAD keys, so a relayed reflection cannot be opened as a
// frame in the opposite direction.
//
// Inner (sealed) body layout:
//
//	Type(1B) || zero-or-more TLVs
//
// Each TLV is `<tag:1><len:2 BE><value:len>` using the same tag and
// helper functions as the plaintext path. Unknown tags are skipped for
// forward-compat. Inner replay defence: TagTimestamp ±60 s window +
// (SrcNodeKey, AEAD-Nonce) cache at the receiver.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
)

// SealedVersion is the wire byte that flags an AEAD-protected peer↔peer
// disco frame at offset 6 (= byte after Magic). Chosen above the
// plaintext Type range (0x01..0x07) so dispatch is unambiguous.
const SealedVersion = 0x10

// NodeKeySize is the byte length of a curve25519 public/private scalar.
const NodeKeySize = 32

// AEADNonceSize is the ChaCha20-Poly1305 nonce length on the wire.
const AEADNonceSize = chacha20poly1305.NonceSize

// AEADTagSize is the Poly1305 authentication tag length appended by
// ChaCha20-Poly1305.Seal.
const AEADTagSize = chacha20poly1305.Overhead

// SealedHeaderSize is the fixed plaintext prefix preceding the AEAD
// body: 6 (magic) + 1 (vers) + 32 (srcNodeKey) + 12 (nonce) = 51.
const SealedHeaderSize = 6 + 1 + NodeKeySize + AEADNonceSize

// hkdfInfoPrefix is prepended to the HKDF info field. Versioned so a
// future key-derivation change can be made cleanly without parsing
// gymnastics — the wire SealedVersion byte changes in lockstep.
const hkdfInfoPrefix = "waired-disco-v1"

// SealedHeader is the parsed plaintext prefix of a sealed disco frame.
// Returned by ParseSealedHeader so callers can recover the sender
// NodeKey (for identity lookup) without performing AEAD opening.
type SealedHeader struct {
	Vers       uint8
	SrcNodeKey [NodeKeySize]byte
	Nonce      [AEADNonceSize]byte
}

// IsSealed reports whether raw has the sealed-frame wire signature
// (Magic + SealedVersion at offset 6). Used by the agent inbound
// dispatcher to branch between sealed and plaintext code paths cheaply
// before the more expensive Decode/DecodeSealed runs.
func IsSealed(raw []byte) bool {
	if len(raw) < HeaderSize {
		return false
	}
	if [6]byte(raw[:6]) != Magic {
		return false
	}
	return raw[6] == SealedVersion
}

// EncodeSealed serialises f into wire bytes:
//
//	Magic || SealedVersion || senderPub || nonce || AEAD-Seal(key, nonce, inner, ad)
//
// where inner = `Type(1B) || TLVs` and the AEAD additional-data is the
// 51 B sealed header. f.HMACTag / f.Ed25519Sig are ignored (the AEAD
// itself provides confidentiality + authentication, and the legacy
// plaintext fields would defeat the privacy goal if they leaked here).
//
// senderPub MUST equal curve25519.X25519(senderPriv, Basepoint); callers
// cache it to avoid scalar-mult per encode.
func EncodeSealed(f *Frame, senderPriv, senderPub, receiverPub [NodeKeySize]byte) ([]byte, error) {
	inner, err := encodeSealedInner(f)
	if err != nil {
		return nil, err
	}

	key, err := encoderKey(senderPriv, senderPub, receiverPub)
	if err != nil {
		return nil, err
	}

	aead, err := chacha20poly1305.New(key[:])
	if err != nil {
		return nil, fmt.Errorf("disco: chacha20poly1305: %w", err)
	}

	var nonce [AEADNonceSize]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, fmt.Errorf("disco: nonce rand: %w", err)
	}

	// out doubles as both the wire prefix and the AEAD additional data;
	// AEAD.Seal appends ciphertext+tag to the existing prefix returning
	// a single contiguous wire-format slice.
	out := make([]byte, 0, SealedHeaderSize+len(inner)+AEADTagSize)
	out = append(out, Magic[:]...)
	out = append(out, SealedVersion)
	out = append(out, senderPub[:]...)
	out = append(out, nonce[:]...)
	ad := out[:SealedHeaderSize]
	sealed := aead.Seal(out, nonce[:], inner, ad)

	if len(sealed) > MaxFrameSize {
		return nil, fmt.Errorf("disco: encoded len %d > max %d", len(sealed), MaxFrameSize)
	}
	return sealed, nil
}

// DecodeSealed parses wire bytes, derives the AEAD key from ECDH with
// the wire-provided SrcNodeKey, opens the body, and parses the inner
// TLVs into *Frame. The returned Frame never has HMACTag / Ed25519Sig
// set — sealed frames don't carry those.
//
// Returns the sender's NodeKey public bytes alongside the Frame so the
// caller can look up the peer in its NodeKey-keyed peer table without
// re-parsing the header.
//
// selfPub MUST equal curve25519.X25519(selfPriv, Basepoint); callers
// cache it.
func DecodeSealed(raw []byte, selfPriv, selfPub [NodeKeySize]byte) (*Frame, [NodeKeySize]byte, error) {
	var zero [NodeKeySize]byte

	hdr, err := ParseSealedHeader(raw)
	if err != nil {
		return nil, zero, err
	}

	key, err := decoderKey(selfPriv, hdr.SrcNodeKey, selfPub)
	if err != nil {
		return nil, zero, err
	}

	aead, err := chacha20poly1305.New(key[:])
	if err != nil {
		return nil, zero, fmt.Errorf("disco: chacha20poly1305: %w", err)
	}

	ad := raw[:SealedHeaderSize]
	ct := raw[SealedHeaderSize:]
	plain, err := aead.Open(nil, hdr.Nonce[:], ct, ad)
	if err != nil {
		return nil, zero, fmt.Errorf("disco: aead open: %w", err)
	}

	f, err := decodeSealedInner(plain)
	if err != nil {
		return nil, zero, err
	}
	return f, hdr.SrcNodeKey, nil
}

// ParseSealedHeader extracts the 51-byte plaintext prefix without
// attempting AEAD opening. Used by callers that need the sender
// identity (SrcNodeKey) before the AEAD key can be derived.
func ParseSealedHeader(raw []byte) (SealedHeader, error) {
	var h SealedHeader
	if len(raw) < SealedHeaderSize+AEADTagSize {
		return h, errors.New("disco: frame shorter than sealed header + tag")
	}
	if len(raw) > MaxFrameSize {
		return h, fmt.Errorf("disco: frame len %d > max %d", len(raw), MaxFrameSize)
	}
	if [6]byte(raw[:6]) != Magic {
		return h, errors.New("disco: bad magic")
	}
	h.Vers = raw[6]
	if h.Vers != SealedVersion {
		return h, fmt.Errorf("disco: not a sealed frame (vers=%#x)", h.Vers)
	}
	copy(h.SrcNodeKey[:], raw[7:7+NodeKeySize])
	copy(h.Nonce[:], raw[7+NodeKeySize:SealedHeaderSize])
	return h, nil
}

// encoderKey derives the AEAD key from the encoder's POV:
//
//	shared = X25519(senderPriv, receiverPub)
//	info   = "waired-disco-v1" || senderPub || receiverPub
//	key    = HKDF-SHA256(shared, salt=nil, info)[:32]
func encoderKey(senderPriv, senderPub, receiverPub [NodeKeySize]byte) ([32]byte, error) {
	if senderPub == receiverPub {
		return [32]byte{}, errors.New("disco: sender and receiver pub are equal")
	}
	return hkdfKey(senderPriv, receiverPub, senderPub, receiverPub)
}

// decoderKey derives the same AEAD key from the decoder's POV.
// srcNodeKey is the sender's pub from the wire header; selfPub is the
// receiver's own cached pub.
//
//	shared = X25519(selfPriv, srcNodeKey)
//	info   = "waired-disco-v1" || srcNodeKey || selfPub
func decoderKey(selfPriv, srcNodeKey, selfPub [NodeKeySize]byte) ([32]byte, error) {
	if srcNodeKey == selfPub {
		return [32]byte{}, errors.New("disco: srcNodeKey equals selfPub")
	}
	return hkdfKey(selfPriv, srcNodeKey, srcNodeKey, selfPub)
}

// hkdfKey is the shared kernel of encoderKey / decoderKey. ownPriv +
// peerPub define the ECDH input; senderPub + receiverPub define the
// HKDF info field (directional separator).
func hkdfKey(ownPriv, peerPub, senderPub, receiverPub [NodeKeySize]byte) ([32]byte, error) {
	var key [32]byte
	shared, err := curve25519.X25519(ownPriv[:], peerPub[:])
	if err != nil {
		return key, fmt.Errorf("disco: x25519: %w", err)
	}
	info := make([]byte, 0, len(hkdfInfoPrefix)+NodeKeySize*2)
	info = append(info, hkdfInfoPrefix...)
	info = append(info, senderPub[:]...)
	info = append(info, receiverPub[:]...)
	r := hkdf.New(sha256.New, shared, nil, info)
	if _, err := io.ReadFull(r, key[:]); err != nil {
		return key, fmt.Errorf("disco: hkdf: %w", err)
	}
	return key, nil
}

// DeriveNodePub returns curve25519.X25519(priv, Basepoint) — the public
// half of a NodeKey, as a [NodeKeySize]byte. Exported so wiring code
// (agent startup) can derive once and cache.
func DeriveNodePub(priv [NodeKeySize]byte) ([NodeKeySize]byte, error) {
	var pub [NodeKeySize]byte
	out, err := curve25519.X25519(priv[:], curve25519.Basepoint)
	if err != nil {
		return pub, fmt.Errorf("disco: x25519 derive pub: %w", err)
	}
	copy(pub[:], out)
	return pub, nil
}

// --- Inner (sealed) body encoder / decoder ---
//
// Reuses the TLV helpers from frame.go (appendStringTLV, appendAddrTLV,
// appendAddrListTLV, decodeAddr, decodeAddrList). Inner format differs
// from the plaintext frame body in that:
//   - Type is the first byte (rather than living in the plaintext header)
//   - No HMACTag / Ed25519Sig TLV (AEAD authenticates the whole frame)
//   - No Flags byte (the plaintext flags byte is unused today)

func encodeSealedInner(f *Frame) ([]byte, error) {
	buf := make([]byte, 0, 128)
	buf = append(buf, byte(f.Type))

	if f.SrcDeviceID != "" {
		var err error
		buf, err = appendStringTLV(buf, TagSrcDeviceID, f.SrcDeviceID)
		if err != nil {
			return nil, err
		}
	}
	if f.DstDeviceID != "" {
		var err error
		buf, err = appendStringTLV(buf, TagDstDeviceID, f.DstDeviceID)
		if err != nil {
			return nil, err
		}
	}
	if f.HasNonce {
		buf = appendTLVHeader(buf, TagNonce, NonceSize)
		buf = append(buf, f.Nonce[:]...)
	}
	if f.HasTimestamp {
		buf = appendTLVHeader(buf, TagTimestamp, 8)
		buf = binary.BigEndian.AppendUint64(buf, f.Timestamp)
	}
	if f.CandidateID != "" {
		var err error
		buf, err = appendStringTLV(buf, TagCandidateID, f.CandidateID)
		if err != nil {
			return nil, err
		}
	}
	if f.HasObserved {
		var err error
		buf, err = appendAddrTLV(buf, TagObservedAddr, f.ObservedAddr)
		if err != nil {
			return nil, err
		}
	}
	if len(f.CandidateList) > 0 {
		if len(f.CandidateList) > MaxCandidateListLen {
			return nil, fmt.Errorf("disco: candidate_list len %d > max %d",
				len(f.CandidateList), MaxCandidateListLen)
		}
		var err error
		buf, err = appendAddrListTLV(buf, TagCandidateList, f.CandidateList)
		if err != nil {
			return nil, err
		}
	}
	return buf, nil
}

func decodeSealedInner(plain []byte) (*Frame, error) {
	if len(plain) < 1 {
		return nil, errors.New("disco: inner body empty (missing type byte)")
	}
	f := &Frame{Type: Type(plain[0])}
	body := plain[1:]
	for len(body) > 0 {
		if len(body) < 3 {
			return nil, errors.New("disco: truncated TLV header")
		}
		tag := Tag(body[0])
		l := int(binary.BigEndian.Uint16(body[1:3]))
		if 3+l > len(body) {
			return nil, fmt.Errorf("disco: TLV %d length %d exceeds remaining %d", tag, l, len(body)-3)
		}
		val := body[3 : 3+l]
		switch tag {
		case TagSrcDeviceID:
			f.SrcDeviceID = string(val)
		case TagDstDeviceID:
			f.DstDeviceID = string(val)
		case TagNonce:
			if l != NonceSize {
				return nil, fmt.Errorf("disco: nonce len %d != %d", l, NonceSize)
			}
			copy(f.Nonce[:], val)
			f.HasNonce = true
		case TagTimestamp:
			if l != 8 {
				return nil, fmt.Errorf("disco: timestamp len %d != 8", l)
			}
			f.Timestamp = binary.BigEndian.Uint64(val)
			f.HasTimestamp = true
		case TagCandidateID:
			f.CandidateID = string(val)
		case TagObservedAddr:
			ap, err := decodeAddr(val)
			if err != nil {
				return nil, fmt.Errorf("disco: observed_addr: %w", err)
			}
			f.ObservedAddr = ap
			f.HasObserved = true
		case TagCandidateList:
			list, err := decodeAddrList(val)
			if err != nil {
				return nil, fmt.Errorf("disco: candidate_list: %w", err)
			}
			f.CandidateList = list
		default:
			// Unknown TLVs are skipped for forward-compat within this
			// SealedVersion. Strict validation happens at the type-handler
			// layer (e.g., probe handler requires Nonce + Timestamp +
			// CandidateID).
		}
		body = body[3+l:]
	}
	return f, nil
}
