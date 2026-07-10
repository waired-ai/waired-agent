// Package disco defines the wire format for waired's NAT-traversal
// discovery messages: STUN-like observation (agent ↔ relay UDP), and
// peer↔peer probe / pong / call_me_maybe (agent ↔ agent, both direct
// UDP and relay-tunnelled).
//
// See docs/specs/waired_client_network_spec.md §9 (NAT traversal) and
// §10.4 (relay frame types). The wire format is shared between the
// relay's UDP STUN-echo server (internal/relay/disco) and the agent's
// disco subsystem (internal/network/disco) so both ends agree on bytes.
//
// Layout:
//
//	+--------+------+-------+--------------------+
//	| magic  | type | flags | TLV body           |
//	| 6 B    | 1 B  | 1 B   | variable           |
//	+--------+------+-------+--------------------+
//
// Magic = "WDISCO" (0x57 0x44 0x49 0x53 0x43 0x4f). The first byte 0x57
// is chosen so it cannot collide with any WireGuard packet type (which
// occupy first byte 0x01..0x04). The agent's MultiplexBind uses this
// prefix to demultiplex disco frames from WG datagrams on the same UDP
// socket.
//
// Each TLV is `<tag:1><len:2 BE><value:len>`. Authentication tags
// (HMAC / Ed25519) MUST be the last TLV in the body so signers and
// verifiers can sign/verify the byte-prefix up to that TLV's start.
package disco

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
)

// Inbound is one disco frame demultiplexed off a transport. The
// wgnet.MultiplexBind classifier produces these; both relay-side and
// agent-side consumers read the same shape.
//
// Payload is a defensive copy (the socket buffer is reused). Src is
// the kernel-reported peer addr for direct-UDP datagrams; the agent's
// disco service uses it as observed_outer when responding with a pong.
//
// For frames demultiplexed off a relay session (probes/pongs that came
// in over WebSocket-tunnelled disco), Path == "relay", Src is the zero
// AddrPort, and RelayURL+RelaySrcDeviceID identify the path so the
// receiver can pong back via the same relay. For direct UDP, Path is
// either "direct" or empty (default = direct, kept zero to avoid
// touching the relay-UDP STUN-echo code path which only sees direct).
type Inbound struct {
	Payload          []byte
	Src              netip.AddrPort
	Path             string // "" or "direct" for UDP; "relay" for relay-tunnelled
	RelayURL         string // set when Path == "relay"
	RelaySrcDeviceID string // set when Path == "relay"
}

// PathDirect / PathRelay are the canonical Path values for Inbound and
// for the disco service's per-probe path tagging.
const (
	PathDirect = "direct"
	PathRelay  = "relay"
)

// Magic is the 6-byte prefix common to every disco frame.
var Magic = [6]byte{'W', 'D', 'I', 'S', 'C', 'O'}

// Type discriminates between disco message kinds. It occupies the byte
// immediately after Magic.
type Type uint8

const (
	// TypeSTUNRequest: agent → relay (UDP). Carries SrcDeviceID, Nonce,
	// Timestamp, HMACTag. Relay echoes back TypeSTUNResponse.
	TypeSTUNRequest Type = 0x01
	// TypeSTUNResponse: relay → agent (UDP). Echoes Nonce, includes
	// ObservedAddr (the agent's public IP:port as the relay saw it),
	// Timestamp, HMACTag.
	TypeSTUNResponse Type = 0x02
	// TypeProbe: agent → agent on direct UDP (NAT punching) or via
	// relay. Carries SrcDeviceID, DstDeviceID, Nonce, Timestamp,
	// CandidateID, Ed25519Sig.
	TypeProbe Type = 0x03
	// TypePong: agent → agent. Same shape as Probe, plus ObservedAddr
	// (the responder's view of the prober's outer src addr — the prober's
	// true public endpoint when the probe arrived directly).
	TypePong Type = 0x04
	// TypeCallMeMaybe: agent → agent via relay only (never direct).
	// Initiator pushes its CandidateList so the responder starts probing
	// in parallel — both-side initiator pattern (Tailscale-style).
	TypeCallMeMaybe Type = 0x05
)

func (t Type) String() string {
	switch t {
	case TypeSTUNRequest:
		return "stun_request"
	case TypeSTUNResponse:
		return "stun_response"
	case TypeProbe:
		return "probe"
	case TypePong:
		return "pong"
	case TypeCallMeMaybe:
		return "call_me_maybe"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(t))
	}
}

// Flags is a bitmask reserved for future extensions. v0 leaves it at 0.
type Flags uint8

// Tag identifies a TLV entry in the body.
type Tag uint8

const (
	TagSrcDeviceID   Tag = 0x01
	TagDstDeviceID   Tag = 0x02
	TagNonce         Tag = 0x03
	TagTimestamp     Tag = 0x04
	TagCandidateID   Tag = 0x05
	TagObservedAddr  Tag = 0x06
	TagCandidateList Tag = 0x07
	TagHMACTag       Tag = 0x08
	TagEd25519Sig    Tag = 0x09
)

// HMACTagSize is the byte length of TagHMACTag values (HMAC-SHA256).
const HMACTagSize = 32

// Ed25519SigSize is the byte length of TagEd25519Sig values.
const Ed25519SigSize = 64

// NonceSize is the byte length of TagNonce values.
const NonceSize = 16

// HeaderSize is the fixed prefix size: 6 (magic) + 1 (type) + 1 (flags).
const HeaderSize = 8

// MaxFrameSize bounds incoming frames to ~1KB to keep STUN amplification
// out of reach. call_me_maybe with a long candidate list stays well
// within this.
const MaxFrameSize = 1500

// Frame is the parsed representation of a disco message. Producers fill
// fields, then call Encode/SignedPrefix/Append* helpers; consumers parse
// raw bytes via Decode and inspect fields.
type Frame struct {
	Type  Type
	Flags Flags

	// String fields are optional; empty means "not present".
	SrcDeviceID string
	DstDeviceID string
	CandidateID string

	// Set HasNonce when Nonce is meaningful.
	Nonce    [NonceSize]byte
	HasNonce bool

	// Set HasTimestamp when Timestamp (unix milliseconds) is meaningful.
	Timestamp    uint64
	HasTimestamp bool

	// ObservedAddr is the responder's view of the prober's outer addr
	// (for TypePong / TypeSTUNResponse). HasObserved gates emission.
	ObservedAddr netip.AddrPort
	HasObserved  bool

	// CandidateList is sent in TypeCallMeMaybe to share initiator's
	// candidate endpoints. Up to 16 entries.
	CandidateList []netip.AddrPort

	// HMACTag and Ed25519Sig are exclusive — at most one is set per
	// frame. Encode writes whichever the caller populated.
	HMACTag    []byte
	Ed25519Sig []byte
}

// MaxCandidateListLen caps the number of candidates a call_me_maybe may
// carry. Receivers MUST drop frames exceeding this.
const MaxCandidateListLen = 16

// Encode serialises the frame to wire bytes. The caller is expected to
// have populated either HMACTag or Ed25519Sig — Encode does not compute
// authentication; that's the signer's responsibility (see SignedPrefix).
//
// Returns ErrTooLarge if the encoded size exceeds MaxFrameSize.
func (f *Frame) Encode() ([]byte, error) {
	buf := make([]byte, 0, 256)
	buf = append(buf, Magic[:]...)
	buf = append(buf, byte(f.Type), byte(f.Flags))

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
	// Auth tag is written LAST so verifiers can hash the prefix
	// (= bytes before this TLV's tag byte) without ambiguity.
	switch {
	case len(f.HMACTag) > 0:
		if len(f.HMACTag) != HMACTagSize {
			return nil, fmt.Errorf("disco: hmac_tag len %d != %d", len(f.HMACTag), HMACTagSize)
		}
		buf = appendTLVHeader(buf, TagHMACTag, HMACTagSize)
		buf = append(buf, f.HMACTag...)
	case len(f.Ed25519Sig) > 0:
		if len(f.Ed25519Sig) != Ed25519SigSize {
			return nil, fmt.Errorf("disco: ed25519_sig len %d != %d", len(f.Ed25519Sig), Ed25519SigSize)
		}
		buf = appendTLVHeader(buf, TagEd25519Sig, Ed25519SigSize)
		buf = append(buf, f.Ed25519Sig...)
	}
	if len(buf) > MaxFrameSize {
		return nil, fmt.Errorf("disco: encoded len %d > max %d", len(buf), MaxFrameSize)
	}
	return buf, nil
}

// SignedPrefix returns the byte-prefix of a fully encoded frame that
// authentication covers — i.e., everything up to the start of the
// trailing HMAC / Ed25519 TLV. Used by both signers (over a frame
// encoded *without* the auth tag) and verifiers (over the received
// frame, locating the auth TLV and slicing).
//
// For signing: encode with HMACTag/Ed25519Sig empty, call SignedPrefix
// on the result, compute the auth, then re-encode with the tag set.
//
// For verifying: parse the frame, find the auth TLV's offset (returned
// by Decode in tagOffset), slice raw[:tagOffset], hash/verify.
func SignedPrefix(raw []byte) ([]byte, error) {
	off, _, err := findAuthTLV(raw)
	if err != nil {
		return nil, err
	}
	return raw[:off], nil
}

// Decode parses raw bytes into a Frame. It does NOT verify authentication;
// callers must check HMACTag against the relay-shared-secret (for STUN
// frames) or Ed25519Sig against peer.NodePublicKey (for peer↔peer frames).
func Decode(raw []byte) (*Frame, error) {
	if len(raw) < HeaderSize {
		return nil, errors.New("disco: frame shorter than header")
	}
	if [6]byte(raw[:6]) != Magic {
		return nil, errors.New("disco: bad magic")
	}
	if len(raw) > MaxFrameSize {
		return nil, fmt.Errorf("disco: frame len %d > max %d", len(raw), MaxFrameSize)
	}
	f := &Frame{
		Type:  Type(raw[6]),
		Flags: Flags(raw[7]),
	}
	body := raw[HeaderSize:]
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
		case TagHMACTag:
			if l != HMACTagSize {
				return nil, fmt.Errorf("disco: hmac_tag len %d != %d", l, HMACTagSize)
			}
			f.HMACTag = append([]byte(nil), val...)
		case TagEd25519Sig:
			if l != Ed25519SigSize {
				return nil, fmt.Errorf("disco: ed25519_sig len %d != %d", l, Ed25519SigSize)
			}
			f.Ed25519Sig = append([]byte(nil), val...)
		default:
			// Unknown TLVs are skipped for forward-compat. Strict
			// validation happens at the type-handler layer (e.g., the
			// STUN echo server requires Nonce + Timestamp + HMACTag).
		}
		body = body[3+l:]
	}
	return f, nil
}

// findAuthTLV locates the trailing HMAC / Ed25519 TLV in raw bytes and
// returns (offset_of_tag_byte, tag_kind, err). Used by SignedPrefix.
func findAuthTLV(raw []byte) (int, Tag, error) {
	if len(raw) < HeaderSize {
		return 0, 0, errors.New("disco: too short for header")
	}
	body := raw[HeaderSize:]
	off := HeaderSize
	for len(body) > 0 {
		if len(body) < 3 {
			return 0, 0, errors.New("disco: truncated TLV header")
		}
		tag := Tag(body[0])
		l := int(binary.BigEndian.Uint16(body[1:3]))
		if 3+l > len(body) {
			return 0, 0, errors.New("disco: TLV length exceeds remaining")
		}
		if tag == TagHMACTag || tag == TagEd25519Sig {
			// Auth TLV must be the LAST TLV.
			if 3+l != len(body) {
				return 0, 0, errors.New("disco: auth TLV is not last")
			}
			return off, tag, nil
		}
		off += 3 + l
		body = body[3+l:]
	}
	return 0, 0, errors.New("disco: no auth TLV present")
}

// --- TLV helpers ---

func appendTLVHeader(buf []byte, tag Tag, length int) []byte {
	buf = append(buf, byte(tag))
	return binary.BigEndian.AppendUint16(buf, uint16(length))
}

func appendStringTLV(buf []byte, tag Tag, s string) ([]byte, error) {
	if len(s) > 0xffff {
		return nil, fmt.Errorf("disco: string TLV %d too long: %d bytes", tag, len(s))
	}
	buf = appendTLVHeader(buf, tag, len(s))
	buf = append(buf, s...)
	return buf, nil
}

// appendAddrTLV encodes a netip.AddrPort as `family(1B) + ip + port(2B BE)`
// where family=4 → 4-byte IPv4, family=6 → 16-byte IPv6.
func appendAddrTLV(buf []byte, tag Tag, ap netip.AddrPort) ([]byte, error) {
	enc, err := encodeAddr(ap)
	if err != nil {
		return nil, err
	}
	buf = appendTLVHeader(buf, tag, len(enc))
	buf = append(buf, enc...)
	return buf, nil
}

func encodeAddr(ap netip.AddrPort) ([]byte, error) {
	if !ap.IsValid() {
		return nil, errors.New("disco: invalid AddrPort")
	}
	a := ap.Addr()
	if a.Is4() {
		out := make([]byte, 1+4+2)
		out[0] = 4
		v4 := a.As4()
		copy(out[1:5], v4[:])
		binary.BigEndian.PutUint16(out[5:7], ap.Port())
		return out, nil
	}
	if a.Is6() {
		out := make([]byte, 1+16+2)
		out[0] = 6
		v6 := a.As16()
		copy(out[1:17], v6[:])
		binary.BigEndian.PutUint16(out[17:19], ap.Port())
		return out, nil
	}
	return nil, errors.New("disco: address is neither v4 nor v6")
}

func decodeAddr(b []byte) (netip.AddrPort, error) {
	if len(b) < 1 {
		return netip.AddrPort{}, errors.New("empty addr value")
	}
	switch b[0] {
	case 4:
		if len(b) != 1+4+2 {
			return netip.AddrPort{}, fmt.Errorf("v4 addr len %d != 7", len(b))
		}
		var raw [4]byte
		copy(raw[:], b[1:5])
		port := binary.BigEndian.Uint16(b[5:7])
		return netip.AddrPortFrom(netip.AddrFrom4(raw), port), nil
	case 6:
		if len(b) != 1+16+2 {
			return netip.AddrPort{}, fmt.Errorf("v6 addr len %d != 19", len(b))
		}
		var raw [16]byte
		copy(raw[:], b[1:17])
		port := binary.BigEndian.Uint16(b[17:19])
		return netip.AddrPortFrom(netip.AddrFrom16(raw), port), nil
	default:
		return netip.AddrPort{}, fmt.Errorf("unknown addr family %d", b[0])
	}
}

func appendAddrListTLV(buf []byte, tag Tag, list []netip.AddrPort) ([]byte, error) {
	// body = count(1B) + concat(addr_records)
	body := []byte{byte(len(list))}
	for _, ap := range list {
		enc, err := encodeAddr(ap)
		if err != nil {
			return nil, err
		}
		body = append(body, enc...)
	}
	if len(body) > 0xffff {
		return nil, fmt.Errorf("disco: candidate_list bytes %d > 65535", len(body))
	}
	buf = appendTLVHeader(buf, tag, len(body))
	buf = append(buf, body...)
	return buf, nil
}

func decodeAddrList(b []byte) ([]netip.AddrPort, error) {
	if len(b) < 1 {
		return nil, errors.New("empty list value")
	}
	count := int(b[0])
	if count > MaxCandidateListLen {
		return nil, fmt.Errorf("count %d > max %d", count, MaxCandidateListLen)
	}
	cur := b[1:]
	out := make([]netip.AddrPort, 0, count)
	for i := 0; i < count; i++ {
		if len(cur) < 1 {
			return nil, errors.New("list truncated")
		}
		var n int
		switch cur[0] {
		case 4:
			n = 1 + 4 + 2
		case 6:
			n = 1 + 16 + 2
		default:
			return nil, fmt.Errorf("entry %d: unknown family %d", i, cur[0])
		}
		if len(cur) < n {
			return nil, fmt.Errorf("entry %d: truncated", i)
		}
		ap, err := decodeAddr(cur[:n])
		if err != nil {
			return nil, fmt.Errorf("entry %d: %w", i, err)
		}
		out = append(out, ap)
		cur = cur[n:]
	}
	return out, nil
}
