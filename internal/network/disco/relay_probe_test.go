package disco

import (
	"context"
	"net/netip"
	"testing"
	"time"

	wireframe "github.com/waired-ai/waired-agent/proto/disco"
)

// TestProbeAllPeers_SendsRelayProbeWhenRelayURLSet asserts that a peer
// configured with a HomeRelay URL gets one direct UDP probe per
// candidate AND one extra probe via the relay session per probe round.
func TestProbeAllPeers_SendsRelayProbeWhenRelayURLSet(t *testing.T) {
	bind := newFakeBind()
	s, _, _ := newService(t, []byte("rs"), bind)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	_, pubB := newNodeKey(t)
	s.UpdatePeers(map[string]PeerSnapshot{
		"node_pub_b": {
			DeviceID:   "dev_b",
			NodePub:    pubB,
			Candidates: []string{"udp4:198.51.100.10:51820", "udp6:[2001:db8::1]:51820"},
			RelayURL:   "wss://relay.example.com:443/relay/v1/connect",
		},
	})

	// Two direct candidates → expect 2 udp sends.
	deadline := time.After(2 * time.Second)
	directSeen := map[string]bool{}
	for len(directSeen) < 2 {
		select {
		case sent := <-bind.sent:
			directSeen[sent.Dst] = true
		case <-deadline:
			t.Fatalf("expected 2 direct probes, got %d", len(directSeen))
		}
	}
	if !directSeen["udp4:198.51.100.10:51820"] {
		t.Errorf("missing IPv4 probe; got %v", directSeen)
	}
	if !directSeen["udp6:[2001:db8::1]:51820"] {
		t.Errorf("missing IPv6 probe; got %v", directSeen)
	}

	// Plus one relay probe.
	select {
	case rp := <-bind.relaySent:
		if rp.RelayURL != "wss://relay.example.com:443/relay/v1/connect" {
			t.Errorf("relay url = %q, want wss://relay.example.com:443/relay/v1/connect", rp.RelayURL)
		}
		if rp.DstDeviceID != "dev_b" {
			t.Errorf("dst device id = %q, want dev_b", rp.DstDeviceID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected relay probe, none observed")
	}
}

// TestProbeAllPeers_NoRelayProbeWhenRelayURLEmpty asserts that peers
// without a HomeRelay don't generate relay probes.
func TestProbeAllPeers_NoRelayProbeWhenRelayURLEmpty(t *testing.T) {
	bind := newFakeBind()
	s, _, _ := newService(t, []byte("rs"), bind)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	_, pubB := newNodeKey(t)
	s.UpdatePeers(map[string]PeerSnapshot{
		"node_pub_b": {
			DeviceID:   "dev_b",
			NodePub:    pubB,
			Candidates: []string{"udp4:198.51.100.10:51820"},
			// RelayURL intentionally empty
		},
	})

	// Wait for at least one direct probe so we know a probe round ran.
	select {
	case <-bind.sent:
	case <-time.After(2 * time.Second):
		t.Fatal("expected direct probe")
	}

	// Then assert no relay probe within a short window.
	select {
	case rp := <-bind.relaySent:
		t.Fatalf("unexpected relay probe: %+v", rp)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestPongOverDirect_EmitsRTTSampledAndPongFromPeer asserts that a
// pong arriving over direct UDP yields BOTH an EventProbeRTTSampled
// (with Path=direct) and an EventPongFromPeer (so the reconciler can
// adopt observed_addr).
func TestPongOverDirect_EmitsRTTSampledAndPongFromPeer(t *testing.T) {
	bind := newFakeBind()
	s, selfPriv, selfPub := newService(t, []byte("rs"), bind)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	privB, pubB := newNodeKey(t)
	s.UpdatePeers(map[string]PeerSnapshot{
		"node_pub_b": {
			DeviceID:   "dev_b",
			NodePub:    pubB,
			Candidates: []string{"udp4:198.51.100.10:51820"},
			RelayURL:   "wss://relay.example.com:443/relay/v1/connect",
		},
	})

	// Wait for the direct probe.
	var probe sentDiscoPacket
	select {
	case probe = <-bind.sent:
	case <-time.After(2 * time.Second):
		t.Fatal("no direct probe")
	}
	probeFrame, _ := mustDecodeSealed(t, probe.Payload, privB, pubB)
	_ = selfPriv

	// Build a matching pong (signed by B) and inject it as direct UDP.
	pong := &wireframe.Frame{
		Type:         wireframe.TypePong,
		SrcDeviceID:  "dev_b",
		DstDeviceID:  "dev_self",
		HasNonce:     true,
		Nonce:        probeFrame.Nonce,
		HasTimestamp: true,
		Timestamp:    uint64(time.Now().UnixMilli()),
	}
	pongBytes := mustEncodeSealed(t, pong, privB, pubB, selfPub)
	bind.inbound <- wireframe.Inbound{
		Payload: pongBytes,
		Src:     netip.MustParseAddrPort("198.51.100.10:51820"),
		// Path empty = direct
	}

	// Drain events until we see both expected types.
	gotRTT, gotPong := false, false
	deadline := time.After(2 * time.Second)
	for !gotRTT || !gotPong {
		select {
		case ev := <-s.Events():
			switch e := ev.(type) {
			case EventProbeRTTSampled:
				if e.Path != wireframe.PathDirect {
					t.Errorf("RTT sample path = %q, want direct", e.Path)
				}
				if e.PeerNodePub != "node_pub_b" {
					t.Errorf("RTT sample peer = %q, want node_pub_b", e.PeerNodePub)
				}
				if e.RTT < 0 {
					t.Errorf("RTT < 0: %v", e.RTT)
				}
				gotRTT = true
			case EventPongFromPeer:
				if e.PeerNodePub != "node_pub_b" {
					t.Errorf("pong peer = %q, want node_pub_b", e.PeerNodePub)
				}
				gotPong = true
			}
		case <-deadline:
			t.Fatalf("timeout: gotRTT=%v gotPong=%v", gotRTT, gotPong)
		}
	}
}

// TestPongOverRelay_EmitsRTTSampledOnly asserts that a pong arriving
// via the relay path yields only EventProbeRTTSampled (Path=relay) and
// NOT EventPongFromPeer (relay-arrived pong doesn't carry meaningful
// direct src info).
func TestPongOverRelay_EmitsRTTSampledOnly(t *testing.T) {
	bind := newFakeBind()
	s, selfPriv, selfPub := newService(t, []byte("rs"), bind)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	privB, pubB := newNodeKey(t)
	relayURL := "wss://relay.example.com:443/relay/v1/connect"
	s.UpdatePeers(map[string]PeerSnapshot{
		"node_pub_b": {
			DeviceID:   "dev_b",
			NodePub:    pubB,
			Candidates: nil, // no direct candidates → only relay probe sent
			RelayURL:   relayURL,
		},
	})

	// Wait for the relay probe.
	var rp sentRelayDiscoPacket
	select {
	case rp = <-bind.relaySent:
	case <-time.After(2 * time.Second):
		t.Fatal("no relay probe")
	}
	probeFrame, _ := mustDecodeSealed(t, rp.Payload, privB, pubB)
	_ = selfPriv

	// Build matching pong and inject it as relay-tunnelled inbound.
	pong := &wireframe.Frame{
		Type:         wireframe.TypePong,
		SrcDeviceID:  "dev_b",
		DstDeviceID:  "dev_self",
		HasNonce:     true,
		Nonce:        probeFrame.Nonce,
		HasTimestamp: true,
		Timestamp:    uint64(time.Now().UnixMilli()),
	}
	pongBytes := mustEncodeSealed(t, pong, privB, pubB, selfPub)
	bind.inbound <- wireframe.Inbound{
		Payload:          pongBytes,
		Path:             wireframe.PathRelay,
		RelayURL:         relayURL,
		RelaySrcDeviceID: "dev_b",
	}

	// Wait for the RTT sample.
	gotRTT := false
	deadline := time.After(2 * time.Second)
	for !gotRTT {
		select {
		case ev := <-s.Events():
			switch e := ev.(type) {
			case EventProbeRTTSampled:
				if e.Path != wireframe.PathRelay {
					t.Errorf("RTT sample path = %q, want relay", e.Path)
				}
				gotRTT = true
			case EventPongFromPeer:
				t.Errorf("unexpected EventPongFromPeer for relay-arrived pong: %+v", e)
			}
		case <-deadline:
			t.Fatal("EventProbeRTTSampled (relay) not received")
		}
	}

	// Then ensure no EventPongFromPeer comes within a short window.
	select {
	case ev := <-s.Events():
		if _, ok := ev.(EventPongFromPeer); ok {
			t.Errorf("unexpected late EventPongFromPeer: %+v", ev)
		}
	case <-time.After(200 * time.Millisecond):
	}
}

// TestProbeViaRelay_PongsBackOverRelay asserts that when a probe is
// received via the relay path, the responder pongs back via the SAME
// relay session (not direct UDP). The relay fan-in path expects this
// for the prober's RTT correlation to work.
func TestProbeViaRelay_PongsBackOverRelay(t *testing.T) {
	bind := newFakeBind()
	s, _, selfPub := newService(t, []byte("rs"), bind)
	// Suppress this side's own probe loop so we can isolate the
	// pong-back behavior under test.
	s.cfg.ProbeReprobeActive = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	privA, pubA := newNodeKey(t)
	relayURL := "wss://relay.example.com:443/relay/v1/connect"
	s.UpdatePeers(map[string]PeerSnapshot{
		"node_pub_a": {
			DeviceID: "dev_a",
			NodePub:  pubA,
			RelayURL: relayURL,
		},
	})

	// Build a probe from A and inject it as a relay-tunnelled inbound.
	probe := &wireframe.Frame{
		Type:         wireframe.TypeProbe,
		SrcDeviceID:  "dev_a",
		DstDeviceID:  "dev_self",
		HasNonce:     true,
		Nonce:        [wireframe.NonceSize]byte{0x42},
		HasTimestamp: true,
		Timestamp:    uint64(time.Now().UnixMilli()),
	}
	probeBytes := mustEncodeSealed(t, probe, privA, pubA, selfPub)
	bind.inbound <- wireframe.Inbound{
		Payload:          probeBytes,
		Path:             wireframe.PathRelay,
		RelayURL:         relayURL,
		RelaySrcDeviceID: "dev_a",
	}

	// Expect a relay pong, not a direct UDP one.
	select {
	case rp := <-bind.relaySent:
		if rp.RelayURL != relayURL {
			t.Errorf("pong relay URL = %q, want %q", rp.RelayURL, relayURL)
		}
		if rp.DstDeviceID != "dev_a" {
			t.Errorf("pong dst device = %q, want dev_a", rp.DstDeviceID)
		}
		f, _ := mustDecodeSealed(t, rp.Payload, privA, pubA)
		if f.Type != wireframe.TypePong {
			t.Errorf("frame type = %s, want pong", f.Type)
		}
		if f.HasObserved {
			t.Errorf("relay pong should not include observed_addr (got %v)", f.ObservedAddr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no relay pong")
	}

	select {
	case sp := <-bind.sent:
		t.Errorf("unexpected direct UDP pong: %+v", sp)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestProbeMissed_EmittedAfterReaperWindow asserts that a probe whose
// pong never arrives surfaces as EventProbeMissed once the gc loop
// sweeps its pending entry.
func TestProbeMissed_EmittedAfterReaperWindow(t *testing.T) {
	bind := newFakeBind()
	s, _, _ := newService(t, []byte("rs"), bind)
	// ProbeReprobeActive = 100ms (set by newService via service_test
	// helper). Reaper threshold = 2× = 200ms; gc cadence = 1×.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	_, pubB := newNodeKey(t)
	s.UpdatePeers(map[string]PeerSnapshot{
		"node_pub_b": {
			DeviceID:   "dev_b",
			NodePub:    pubB,
			Candidates: []string{"udp4:198.51.100.10:51820"},
			RelayURL:   "wss://relay.example.com:443/relay/v1/connect",
		},
	})

	// Drain (and discard) initial probes — both direct and relay.
	gotDirect, gotRelay := false, false
	for !gotDirect || !gotRelay {
		select {
		case <-bind.sent:
			gotDirect = true
		case <-bind.relaySent:
			gotRelay = true
		case <-time.After(2 * time.Second):
			t.Fatalf("did not see initial probe round; gotDirect=%v gotRelay=%v", gotDirect, gotRelay)
		}
	}

	// Wait for at least one EventProbeMissed for the direct path
	// AND one for the relay path (no pong was ever delivered).
	missedDirect, missedRelay := false, false
	deadline := time.After(2 * time.Second)
	for !missedDirect || !missedRelay {
		select {
		case ev := <-s.Events():
			if e, ok := ev.(EventProbeMissed); ok {
				if e.PeerNodePub != "node_pub_b" {
					t.Errorf("missed peer = %q, want node_pub_b", e.PeerNodePub)
				}
				switch e.Path {
				case wireframe.PathDirect:
					missedDirect = true
				case wireframe.PathRelay:
					missedRelay = true
				}
			}
		case <-deadline:
			t.Fatalf("timeout waiting for missed events: direct=%v relay=%v", missedDirect, missedRelay)
		}
	}
}
