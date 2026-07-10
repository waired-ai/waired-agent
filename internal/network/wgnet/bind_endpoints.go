package wgnet

import (
	"net/netip"
)

// relayEndpoint is the conn.Endpoint variant used when a peer is reached
// through a relay rather than via direct UDP. wireguard-go treats the
// returned value as opaque - it only uses DstToBytes for cookie caching
// and DstIP for sticky-source bookkeeping, both of which are harmless
// when set to a deterministic placeholder.
type relayEndpoint struct {
	url         string // ws://host:port/relay/v1/connect
	dstDeviceID string
	dstNodeKey  string // std-base64
}

func (e *relayEndpoint) ClearSrc() {}

func (e *relayEndpoint) SrcToString() string { return "" }

func (e *relayEndpoint) DstToString() string {
	return "relay:" + e.url + "#dst=" + e.dstDeviceID + "&nk=" + e.dstNodeKey
}

// DstToBytes is what wireguard-go feeds into mac2 cookie calculation. It
// must be deterministic for a given peer; using DstToString's bytes is
// adequate (the relay path doesn't compete with direct UDP for the same
// peer in step1).
func (e *relayEndpoint) DstToBytes() []byte { return []byte(e.DstToString()) }

// DstIP returns a placeholder; the peer's real path is the relay URL.
func (e *relayEndpoint) DstIP() netip.Addr { return netip.AddrFrom4([4]byte{127, 0, 0, 1}) }

// SrcIP is unused for relay endpoints.
func (e *relayEndpoint) SrcIP() netip.Addr { return netip.Addr{} }
