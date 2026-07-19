package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"net/netip"
	"sync"

	"github.com/waired-ai/waired-agent/internal/inference"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// peerDirectory maintains an overlay-IP-keyed index of peers in the
// current NetworkMap, tracking the bits the inference overlay
// listener needs for peer auth: DeviceID and MachinePublicKey.
//
// The inferencemesh.Aggregator already tracks DeviceID + OverlayIP
// for the diagnose / mesh-snapshot path, but it intentionally does
// NOT carry MachinePublicKey (peers consume the snapshot's
// InferenceState fields without needing to verify peer identity at
// that layer). For inbound peer-overlay HTTP, the listener does need
// the public key to verify the request signature, so we keep this
// secondary index.
//
// peerDirectory is goroutine-safe; the network-map subscriber writes
// it on every frame and the inference listener reads it on every
// inbound request.
type peerDirectory struct {
	mu   sync.RWMutex
	byIP map[netip.Addr]inference.PeerIdentity
}

// newPeerDirectory returns an empty directory.
func newPeerDirectory() *peerDirectory {
	return &peerDirectory{byIP: map[netip.Addr]inference.PeerIdentity{}}
}

// Update replaces the directory with the peers in nm. Wholesale
// replacement (rather than diff-merge) ensures revoked / removed
// peers drop out the moment a new frame lands; staleness is the same
// model the inferencemesh.Aggregator uses.
func (d *peerDirectory) Update(nm *signer.NetworkMap) {
	if nm == nil {
		return
	}
	next := make(map[netip.Addr]inference.PeerIdentity, len(nm.Peers))
	for _, p := range nm.Peers {
		ip, err := netip.ParseAddr(p.OverlayIP)
		if err != nil {
			continue
		}
		if p.MachinePublicKey == "" {
			continue
		}
		// Network map encodes MachinePublicKey as base64; decode for
		// ed25519.Verify which expects raw bytes.
		raw, err := base64.StdEncoding.DecodeString(p.MachinePublicKey)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			continue
		}
		id := inference.PeerIdentity{
			DeviceID:   p.DeviceID,
			MachineKey: ed25519.PublicKey(raw),
		}
		if p.Grant != nil {
			id.Pseudonym = p.Grant.Pseudonym
			// Carry the whole grant annotation: the serving-side gate
			// chain classifies public consumers on Kind/Role (§8.1).
			g := *p.Grant
			id.Grant = &g
		}
		next[ip] = id
	}
	d.mu.Lock()
	d.byIP = next
	d.mu.Unlock()
}

// LookupByOverlayIP implements inference.PeerLookup.
func (d *peerDirectory) LookupByOverlayIP(ip netip.Addr) (inference.PeerIdentity, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	p, ok := d.byIP[ip]
	return p, ok
}
