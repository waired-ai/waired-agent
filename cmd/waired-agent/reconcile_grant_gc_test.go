package main

import (
	"testing"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// TestReconciler_GrantPeerGCOnMapRemoval pins the §7.3 teardown path:
// a foreign grant peer is torn down by the EXISTING map-GC — when the
// grant expires the CP stops injecting the peer, the next map frame
// omits it, and Apply garbage-collects its state and shrinks the
// WireGuard peer set. No new data-plane teardown code exists or is
// needed.
func TestReconciler_GrantPeerGCOnMapRemoval(t *testing.T) {
	pubOwn := mkPeerKey(t)
	pubForeign := mkPeerKey(t)

	eng := &fakeEngine{}
	rec := newReconciler(eng, &agentProvider{}, quietLogger(), nil, fastTestConfig())

	withGrant := nm1Peer(pubOwn, "udp4:198.51.100.10:51820")
	withGrant.Peers = append(withGrant.Peers, signer.NetworkMapPeer{
		DeviceID:      "dev_foreign",
		DeviceName:    "foreign",
		OverlayIP:     "100.99.0.3",
		NodePublicKey: pubForeign,
		Endpoints:     []signer.EndpointCandidate{{Addr: "udp4:203.0.113.9:51820", Kind: signer.KindLocal}},
		HomeRelay:     "relay_a",
		Grant:         &signer.PeerGrant{ID: "grant_1", Kind: "public", Role: "provider", Pseudonym: "amber-fox-42"},
	})
	if err := rec.Apply(withGrant); err != nil {
		t.Fatalf("Apply with grant peer: %v", err)
	}
	if got := len(eng.lastPeers); got != 2 {
		t.Fatalf("WG peers after inject = %d, want 2", got)
	}
	if _, ok := rec.Snapshot()[pubForeign]; !ok {
		t.Fatalf("grant peer must have path state while in the map")
	}

	// Grant expired → CP omits the peer from the next frame.
	if err := rec.Apply(nm1Peer(pubOwn, "udp4:198.51.100.10:51820")); err != nil {
		t.Fatalf("Apply without grant peer: %v", err)
	}
	if got := len(eng.lastPeers); got != 1 {
		t.Fatalf("WG peers after grant expiry = %d, want 1 (foreign peer GC'd)", got)
	}
	if _, ok := rec.Snapshot()[pubForeign]; ok {
		t.Fatalf("grant peer path state must be GC'd when it leaves the map")
	}
}

// TestPeerLogName pins the log-identity rule (§8.5): pseudonym for
// grant peers, DeviceID otherwise.
func TestPeerLogName(t *testing.T) {
	own := signer.NetworkMapPeer{DeviceID: "dev_own"}
	if got := peerLogName(own); got != "dev_own" {
		t.Fatalf("own peer log name = %q", got)
	}
	foreign := signer.NetworkMapPeer{
		DeviceID: "dev_foreign",
		Grant:    &signer.PeerGrant{Pseudonym: "amber-fox-42"},
	}
	if got := peerLogName(foreign); got != "amber-fox-42" {
		t.Fatalf("grant peer log name = %q, want pseudonym", got)
	}
}

// TestReconciler_FeedsPeerNetworksToEngine pins the §10 stamping
// conduit: Apply extracts NetworkID from CP-injected cross-network
// peers into the engine's peer-network table (same-network peers
// excluded), and removal clears the entry on the next frame.
func TestReconciler_FeedsPeerNetworksToEngine(t *testing.T) {
	pubOwn := mkPeerKey(t)
	pubForeign := mkPeerKey(t)

	eng := &fakeEngine{}
	rec := newReconciler(eng, &agentProvider{}, quietLogger(), nil, fastTestConfig())

	nm := nm1Peer(pubOwn, "udp4:198.51.100.10:51820")
	nm.Peers = append(nm.Peers, signer.NetworkMapPeer{
		DeviceID:      "dev_foreign",
		DeviceName:    "amber-fox-42",
		OverlayIP:     "100.99.0.3",
		NodePublicKey: pubForeign,
		NetworkID:     "net_foreign",
		Grant:         &signer.PeerGrant{ID: "grant_1", Kind: "public", Role: "provider", Pseudonym: "amber-fox-42"},
	})
	if err := rec.Apply(nm); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	nets := eng.PeerNetworks()
	if nets["dev_foreign"] != "net_foreign" {
		t.Fatalf("engine peer networks = %v, want dev_foreign→net_foreign", nets)
	}
	if _, ok := nets[nm.Peers[0].DeviceID]; ok {
		t.Fatalf("same-network peer leaked into the table: %v", nets)
	}

	// Peer leaves the map → table entry cleared on the next Apply.
	if err := rec.Apply(nm1Peer(pubOwn, "udp4:198.51.100.10:51820")); err != nil {
		t.Fatalf("Apply without foreign peer: %v", err)
	}
	if nets := eng.PeerNetworks(); len(nets) != 0 {
		t.Fatalf("stale peer-network entries after removal: %v", nets)
	}
}
