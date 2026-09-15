package main

import (
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/network/disco"
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

// TestPeerLogName pins the log-identity rule. PRODUCT CONTRACT: public
// share spec §8.5 (logs name a public peer by its pseudonym, never its
// real device id) and team share spec §10.2 for a teammate's computer.
// A grant peer is never named by its DeviceID, including one whose grant
// carries no pseudonym — that case fell back to the DeviceID until
// waired-agent#1368.
func TestPeerLogName(t *testing.T) {
	cases := []struct {
		name string
		peer signer.NetworkMapPeer
		want string
	}{
		{"own", signer.NetworkMapPeer{DeviceID: "dev_own"}, "dev_own"},
		{"public", signer.NetworkMapPeer{
			DeviceID: "dev_foreign",
			Grant:    &signer.PeerGrant{ID: "grant_1", Kind: signer.GrantKindPublic, Pseudonym: "amber-fox-42"},
		}, "amber-fox-42"},
		{"public without a pseudonym", signer.NetworkMapPeer{
			DeviceID: "dev_foreign",
			Grant:    &signer.PeerGrant{ID: "grant_1", Kind: signer.GrantKindPublic},
		}, inferencemesh.PublicPeerLabelFor("grant_1")},
		{"team", signer.NetworkMapPeer{
			DeviceID:   "dev_teammate",
			DeviceName: "laptop",
			Grant:      &signer.PeerGrant{ID: "grant_2", Kind: signer.GrantKindTeam, DisplayName: "Alex Example"},
		}, "laptop (Alex Example)"},
		{"team with neither name", signer.NetworkMapPeer{
			DeviceID: "dev_teammate",
			Grant:    &signer.PeerGrant{ID: "grant_2", Kind: signer.GrantKindTeam},
		}, inferencemesh.TeamPeerFallbackLabel},
	}
	for _, c := range cases {
		if got := peerLogName(c.peer); got != c.want {
			t.Errorf("%s: peerLogName = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestReconciler_DiscoEventsForARemovedPeerDoNotRecreateItsState pins
// waired-agent#1372. Probes sent before a grant peer left the map time
// out afterwards, and the disco service reports those misses for the
// removed key. Recreating path state for it left a row Snapshot reported
// with an empty device id for the life of the process.
func TestReconciler_DiscoEventsForARemovedPeerDoNotRecreateItsState(t *testing.T) {
	pubOwn := mkPeerKey(t)
	pubForeign := mkPeerKey(t)

	eng := &fakeEngine{}
	rec := newReconciler(eng, &agentProvider{}, quietLogger(), nil, fastTestConfig())

	withGrant := nm1Peer(pubOwn, "udp4:198.51.100.10:51820")
	withGrant.Peers = append(withGrant.Peers, signer.NetworkMapPeer{
		DeviceID:      "dev_foreign",
		OverlayIP:     "100.99.0.3",
		NodePublicKey: pubForeign,
		HomeRelay:     "relay_a",
		Grant:         &signer.PeerGrant{ID: "grant_1", Kind: "public", Role: "provider", Pseudonym: "amber-fox-42"},
	})
	if err := rec.Apply(withGrant); err != nil {
		t.Fatalf("Apply with grant peer: %v", err)
	}
	if err := rec.Apply(nm1Peer(pubOwn, "udp4:198.51.100.10:51820")); err != nil {
		t.Fatalf("Apply without grant peer: %v", err)
	}

	at := time.Now()
	for _, ev := range []disco.Event{
		disco.EventProbeMissed{PeerNodePub: pubForeign, PeerDeviceID: "dev_foreign", Path: pathDirect, At: at},
		disco.EventProbeRoundFinalized{PeerNodePub: pubForeign, PeerDeviceID: "dev_foreign", Path: pathDirect, At: at},
		disco.EventProbeRTTSampled{PeerNodePub: pubForeign, PeerDeviceID: "dev_foreign", Path: pathRelay, RTT: time.Millisecond, At: at},
		disco.EventPongFromPeer{PeerNodePub: pubForeign, PeerDeviceID: "dev_foreign", ReceivedAt: at},
		disco.EventCallMeMaybeReceived{PeerNodePub: pubForeign, PeerDeviceID: "dev_foreign", At: at},
	} {
		rec.OnDiscoEvent(ev)
		if _, ok := rec.Snapshot()[pubForeign]; ok {
			t.Fatalf("%T for a peer that left the map recreated its path state", ev)
		}
	}
	rec.mu.Lock()
	_, kept := rec.state[pubForeign]
	rec.mu.Unlock()
	if kept {
		t.Fatal("path state for the removed peer exists behind Snapshot")
	}
	// The peer still in the map keeps receiving events.
	rec.OnDiscoEvent(disco.EventProbeMissed{PeerNodePub: pubOwn, PeerDeviceID: "dev_peer_a", Path: pathDirect, At: at})
	if ps, ok := rec.Snapshot()[pubOwn]; !ok || ps.DeviceID != "dev_peer_a" {
		t.Fatalf("own peer snapshot = %+v, %v", ps, ok)
	}
}

// TestReconciler_SnapshotSkipsStateForPeersOutsideTheMap is the second
// half of waired-agent#1372: state that exists for a key the map does not
// carry is not reported as a peer.
func TestReconciler_SnapshotSkipsStateForPeersOutsideTheMap(t *testing.T) {
	pubOwn := mkPeerKey(t)
	eng := &fakeEngine{}
	rec := newReconciler(eng, &agentProvider{}, quietLogger(), nil, fastTestConfig())
	if err := rec.Apply(nm1Peer(pubOwn, "udp4:198.51.100.10:51820")); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	rec.mu.Lock()
	rec.state["stray-node-key"] = &peerPathState{currentPath: pathRelay}
	rec.mu.Unlock()

	snap := rec.Snapshot()
	if _, ok := snap["stray-node-key"]; ok {
		t.Fatalf("Snapshot reported state for a key the map does not carry: %+v", snap)
	}
	if len(snap) != 1 {
		t.Fatalf("Snapshot = %+v, want the one map peer", snap)
	}
}

// TestReconciler_FeedsGrantPeerLogNamesToEngine: the relay bind names
// relay senders and endpoints in its own log lines and in wireguard-go's,
// and has no map of its own, so Apply hands it the grant peers' log
// names. Own-network peers are not in the table (they are logged by id).
func TestReconciler_FeedsGrantPeerLogNamesToEngine(t *testing.T) {
	pubOwn := mkPeerKey(t)
	pubForeign := mkPeerKey(t)

	eng := &fakeEngine{}
	rec := newReconciler(eng, &agentProvider{}, quietLogger(), nil, fastTestConfig())

	nm := nm1Peer(pubOwn, "udp4:198.51.100.10:51820")
	nm.Peers = append(nm.Peers, signer.NetworkMapPeer{
		DeviceID:      "dev_foreign",
		OverlayIP:     "100.99.0.3",
		NodePublicKey: pubForeign,
		NetworkID:     "net_foreign",
		Grant:         &signer.PeerGrant{ID: "grant_1", Kind: "public", Role: "provider", Pseudonym: "amber-fox-42"},
	})
	if err := rec.Apply(nm); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	eng.mu.Lock()
	names := eng.peerLogNames
	eng.mu.Unlock()
	if len(names) != 1 || names["dev_foreign"] != "amber-fox-42" {
		t.Fatalf("engine peer log names = %v, want only dev_foreign→amber-fox-42", names)
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
