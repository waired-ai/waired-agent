// Package disco implements the agent-side NAT-traversal subsystem:
// STUN-like endpoint observation against relays, peer↔peer probe/pong,
// and NAT-type detection. It produces a stream of Events that the
// reconciler consumes to flip peer endpoints between relay and direct
// once a path is verified.
//
// Wire format and security live in internal/disco. The relay-side STUN
// echo is internal/relay/disco. The shared UDP socket plumbing
// (magic-prefix classifier, SendDisco) lives in internal/network/wgnet.
//
// Two-side initiator pattern (Tailscale-style): both peers probe the
// other's candidates as soon as they appear in the NetworkMap. The
// reconciler additionally drives call_me_maybe (relay-tunnelled
// candidate exchange) when direct probing fails to bootstrap or when
// it has been forced to relay — both peers then refresh their NAT
// mappings simultaneously, opening a hole even on EIM-style NATs that
// the bare two-side-initiator race could not punch.
package disco

import (
	"net/netip"
	"time"
)

// NATType is the result of comparing observed src_port across two
// disco_stun_request responses sent to different relay UDP ports. EIM
// nets ("full-cone", "address-restricted", "port-restricted") punch.
// Symmetric (endpoint-dependent mapping) does not — the agent stays
// on relay for cross-NAT peers.
type NATType int

const (
	NATTypeUnknown   NATType = iota
	NATTypeEIM               // endpoint-independent mapping
	NATTypeSymmetric         // endpoint-dependent mapping
)

func (n NATType) String() string {
	switch n {
	case NATTypeEIM:
		return "eim"
	case NATTypeSymmetric:
		return "symmetric"
	default:
		return "unknown"
	}
}

// Event is the union type emitted on Service.Events(). Consumers (the
// reconciler) type-switch on Event to update per-peer state.
type Event interface {
	isEvent()
}

// EventObservedAddr fires when the relay STUN echo returns a fresh
// observed public addr for self. The reconciler uses this to advertise
// the addr to CP via POST /v1/devices/self/endpoints.
type EventObservedAddr struct {
	Addr       netip.AddrPort // self's NAT-mapped public addr
	NATType    NATType        // detection state at the moment of observation
	ObservedAt time.Time
}

func (EventObservedAddr) isEvent() {}

// EventNATTypeDetected fires when the NAT-type detector flips between
// states (Unknown→EIM, Unknown→Symmetric, EIM→Symmetric, etc.).
type EventNATTypeDetected struct {
	Kind NATType
}

func (EventNATTypeDetected) isEvent() {}

// EventPongFromPeer fires when a disco_pong arrives from a peer over
// direct UDP and its Ed25519 signature verifies against the peer's
// MachinePublicKey from the NetworkMap. The reconciler interprets this
// as "direct path is alive" for the peer and flips the engine's peer
// endpoint to udp4:DirectSrc:port (then waits for a WG handshake to
// confirm).
type EventPongFromPeer struct {
	PeerNodePub      string         // peer's NodePublicKey (std-base64), the reconciler's key
	PeerDeviceID     string         // peer device_id from the pong's TLV
	DirectSrc        netip.AddrPort // src AddrPort of the pong (= peer's outer endpoint as we saw it)
	ObservedSelfAddr netip.AddrPort // peer's view of OUR outer src on the matching probe (optional)
	ReceivedAt       time.Time
}

func (EventPongFromPeer) isEvent() {}

// EventProbeRTTSampled fires when a pong arrives that matches an
// outstanding probe nonce. RTT is now-sentAt for that nonce. Path
// identifies which transport the probe was sent on and the pong came
// back on (always equal in v0; relay-arrived pongs have Path="relay").
//
// The reconciler maintains an EWMA of RTT per (peer, path) and uses
// the asymmetric ratio between direct and relay EWMAs to decide
// fallback / upgrade.
type EventProbeRTTSampled struct {
	PeerNodePub  string
	PeerDeviceID string
	Path         string // "direct" | "relay"
	RTT          time.Duration
	At           time.Time
}

func (EventProbeRTTSampled) isEvent() {}

// EventProbeMissed fires when a probe nonce ages out of pendingProbes
// without ever being matched by a pong (i.e., the receiver never
// responded). Path identifies which transport the probe was sent on.
//
// The reconciler increments per-(peer,path) miss counters; a streak
// of N consecutive misses on the direct path is one of the downgrade
// triggers (the other being RTT ratio).
type EventProbeMissed struct {
	PeerNodePub  string
	PeerDeviceID string
	Path         string // "direct" | "relay"
	At           time.Time
}

func (EventProbeMissed) isEvent() {}

// EventProbeRoundFinalized fires once per direct-path probe round when
// all candidate probes of that round have been accounted for (either
// pong received or aged out into a miss). AnySuccess is true if at
// least one candidate in the round produced a pong.
//
// This is the round-aware signal the reconciler uses to drive its
// pong-ring and direct-path miss-streak. probeAllPeers() may send N
// probes per round (one per candidate endpoint plus CMM hints), and
// per-probe EventProbeRTTSampled / EventProbeMissed events still fire
// for EWMA / lastDirectEvidenceAt updates — but only this event drives
// the upgrade-gate hysteresis, so multi-candidate fan-out can't
// permanently lock the ring with mixed [success, miss, miss] outcomes.
//
// Relay-path probes are 1 probe per round (single HomeRelay URL) so
// don't need round bookkeeping; Path here is always "direct".
type EventProbeRoundFinalized struct {
	PeerNodePub  string
	PeerDeviceID string
	Path         string // always "direct" in v1
	RoundID      uint64
	AnySuccess   bool
	At           time.Time
}

func (EventProbeRoundFinalized) isEvent() {}

// EventCallMeMaybeReceived fires when a call_me_maybe frame from a
// known peer verifies and is processed. The disco service merges the
// candidates into its probe target set as cmmHints internally, so the
// reconciler does not need to act on this directly — but main.go uses
// it for structured logging and tests assert on receipt.
type EventCallMeMaybeReceived struct {
	PeerNodePub  string
	PeerDeviceID string
	Candidates   []netip.AddrPort
	At           time.Time
}

func (EventCallMeMaybeReceived) isEvent() {}
