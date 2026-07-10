package disco

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"

	wireframe "github.com/waired-ai/waired-agent/proto/disco"
)

// newCMMService spins a service preconfigured for CMM tests: short
// CMM hint TTL so expiration is observable inside test deadlines, and
// no STUN noise. Returns the self NodeKey (priv, pub) so callers can
// build sealed peer→self frames addressed at the SUT.
func newCMMService(t *testing.T, hintTTL time.Duration) (*Service, *fakeBind, [wireframe.NodeKeySize]byte, [wireframe.NodeKeySize]byte) {
	t.Helper()
	bind := newFakeBind()
	priv, pub := newNodeKey(t)
	cfg := Config{
		SelfDeviceID:       "dev_self",
		SelfNodeKeyPriv:    priv,
		SelfNodeKeyPub:     pub,
		Bind:               bind,
		STUNObserveActive:  time.Hour, // suppress STUN noise
		STUNObserveIdle:    time.Hour,
		ProbeReprobeActive: time.Hour, // disable the periodic probe loop
		ProbeWindow:        5 * time.Second,
		CMMHintTTL:         hintTTL,
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, bind, priv, pub
}

// TestHandleCallMeMaybe_TriggersDirectProbes verifies that a valid CMM
// frame from a known peer causes the receiver to immediately emit one
// direct probe per advertised candidate.
func TestHandleCallMeMaybe_TriggersDirectProbes(t *testing.T) {
	s, bind, _, selfPub := newCMMService(t, 30*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	privB, pubB := newNodeKey(t)
	s.UpdatePeers(map[string]PeerSnapshot{
		"node_pub_b": {DeviceID: "dev_b", NodePub: pubB, RelayURL: "wss://r/"},
	})

	cands := []netip.AddrPort{
		netip.MustParseAddrPort("198.51.100.10:51820"),
		netip.MustParseAddrPort("[2001:db8::1]:51820"),
	}
	cmm := &wireframe.Frame{
		Type:          wireframe.TypeCallMeMaybe,
		SrcDeviceID:   "dev_b",
		DstDeviceID:   "dev_self",
		HasNonce:      true,
		Nonce:         [wireframe.NonceSize]byte{0x11},
		HasTimestamp:  true,
		Timestamp:     uint64(time.Now().UnixMilli()),
		CandidateList: cands,
	}
	payload := mustEncodeSealed(t, cmm, privB, pubB, selfPub)
	bind.inbound <- wireframe.Inbound{Payload: payload, Path: wireframe.PathRelay, RelayURL: "wss://r/", RelaySrcDeviceID: "dev_b"}

	// Expect one probe per candidate. The probes are sent fire-and-forget
	// (no pendingProbes entry), so we observe them on bind.sent. The
	// agent sealed them with (selfPriv → pubB), so we open them from B's
	// POV using (privB, pubB).
	gotDsts := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(gotDsts) < 2 {
		select {
		case got := <-bind.sent:
			f, _ := mustDecodeSealedOrNil(got.Payload, privB, pubB)
			if f == nil || f.Type != wireframe.TypeProbe {
				continue
			}
			gotDsts[got.Dst] = true
		case <-deadline:
			t.Fatalf("expected 2 direct probes, got %d: %v", len(gotDsts), gotDsts)
		}
	}
	if !gotDsts["udp4:198.51.100.10:51820"] {
		t.Errorf("missing v4 probe; got %v", gotDsts)
	}
	if !gotDsts["udp6:[2001:db8::1]:51820"] {
		t.Errorf("missing v6 probe; got %v", gotDsts)
	}

	// And EventCallMeMaybeReceived is emitted.
	deadline = time.After(2 * time.Second)
	for {
		select {
		case ev := <-s.Events():
			if got, ok := ev.(EventCallMeMaybeReceived); ok {
				if got.PeerNodePub != "node_pub_b" || got.PeerDeviceID != "dev_b" {
					t.Errorf("event keys = (%q, %q), want (node_pub_b, dev_b)", got.PeerNodePub, got.PeerDeviceID)
				}
				if len(got.Candidates) != 2 {
					t.Errorf("event candidates len = %d, want 2", len(got.Candidates))
				}
				return
			}
		case <-deadline:
			t.Fatal("EventCallMeMaybeReceived not emitted within 2s")
		}
	}
}

// TestHandleCallMeMaybe_RejectsBadSignature ensures a CMM frame signed
// by an attacker key (not the peer's MachinePub) is silently dropped.
func TestHandleCallMeMaybe_RejectsBadSignature(t *testing.T) {
	s, bind, _, selfPub := newCMMService(t, 30*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	_, pubB := newNodeKey(t)
	privAttacker, pubAttacker := newNodeKey(t)
	s.UpdatePeers(map[string]PeerSnapshot{
		"node_pub_b": {DeviceID: "dev_b", NodePub: pubB, RelayURL: "wss://r/"},
	})

	cmm := &wireframe.Frame{
		Type:          wireframe.TypeCallMeMaybe,
		SrcDeviceID:   "dev_b",
		DstDeviceID:   "dev_self",
		HasNonce:      true,
		Nonce:         [wireframe.NonceSize]byte{0x22},
		HasTimestamp:  true,
		Timestamp:     uint64(time.Now().UnixMilli()),
		CandidateList: []netip.AddrPort{netip.MustParseAddrPort("198.51.100.10:51820")},
	}
	payload := mustEncodeSealed(t, cmm, privAttacker, pubAttacker, selfPub)
	bind.inbound <- wireframe.Inbound{Payload: payload, Path: wireframe.PathRelay, RelaySrcDeviceID: "dev_b"}

	// No outbound probes should fire.
	select {
	case got := <-bind.sent:
		t.Fatalf("forged CMM caused outbound probe to %s", got.Dst)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestHandleCallMeMaybe_RejectsReplay ensures the same nonce twice
// causes the second to be dropped (consumeNonce gate). Without replay
// protection, an attacker who captured one CMM frame could keep
// triggering probes from the receiver indefinitely.
func TestHandleCallMeMaybe_RejectsReplay(t *testing.T) {
	s, bind, _, selfPub := newCMMService(t, 30*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	privB, pubB := newNodeKey(t)
	s.UpdatePeers(map[string]PeerSnapshot{
		"node_pub_b": {DeviceID: "dev_b", NodePub: pubB, RelayURL: "wss://r/"},
	})

	cmm := &wireframe.Frame{
		Type:          wireframe.TypeCallMeMaybe,
		SrcDeviceID:   "dev_b",
		DstDeviceID:   "dev_self",
		HasNonce:      true,
		Nonce:         [wireframe.NonceSize]byte{0x33},
		HasTimestamp:  true,
		Timestamp:     uint64(time.Now().UnixMilli()),
		CandidateList: []netip.AddrPort{netip.MustParseAddrPort("198.51.100.10:51820")},
	}
	payload := mustEncodeSealed(t, cmm, privB, pubB, selfPub)

	// First send: probe expected.
	bind.inbound <- wireframe.Inbound{Payload: payload, Path: wireframe.PathRelay, RelaySrcDeviceID: "dev_b"}
	select {
	case got := <-bind.sent:
		f, _ := mustDecodeSealedOrNil(got.Payload, privB, pubB)
		if f.Type != wireframe.TypeProbe {
			t.Fatalf("first frame not a probe: %s", f.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first CMM did not produce a probe")
	}

	// Second send (same payload, same nonce): no probe expected.
	bind.inbound <- wireframe.Inbound{Payload: payload, Path: wireframe.PathRelay, RelaySrcDeviceID: "dev_b"}
	select {
	case got := <-bind.sent:
		t.Fatalf("replayed CMM caused additional probe to %s", got.Dst)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestHandleCallMeMaybe_StoresHintsWithTTL verifies the receiver-side
// hints are persisted on the peer's state with the configured TTL.
func TestHandleCallMeMaybe_StoresHintsWithTTL(t *testing.T) {
	hintTTL := 30 * time.Second
	s, bind, _, selfPub := newCMMService(t, hintTTL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	privB, pubB := newNodeKey(t)
	s.UpdatePeers(map[string]PeerSnapshot{
		"node_pub_b": {DeviceID: "dev_b", NodePub: pubB, RelayURL: "wss://r/"},
	})

	cand := netip.MustParseAddrPort("198.51.100.42:51820")
	cmm := &wireframe.Frame{
		Type:          wireframe.TypeCallMeMaybe,
		SrcDeviceID:   "dev_b",
		DstDeviceID:   "dev_self",
		HasNonce:      true,
		Nonce:         [wireframe.NonceSize]byte{0x44},
		HasTimestamp:  true,
		Timestamp:     uint64(time.Now().UnixMilli()),
		CandidateList: []netip.AddrPort{cand},
	}
	payload := mustEncodeSealed(t, cmm, privB, pubB, selfPub)
	before := time.Now()
	bind.inbound <- wireframe.Inbound{Payload: payload, Path: wireframe.PathRelay, RelaySrcDeviceID: "dev_b"}

	// Drain the induced probe so it doesn't block.
	select {
	case <-bind.sent:
	case <-time.After(2 * time.Second):
		t.Fatal("no induced probe")
	}

	// Wait briefly for the handler's lock-protected hint store to land.
	hintAddr := "udp4:198.51.100.42:51820"
	deadline := time.After(2 * time.Second)
	for {
		s.mu.Lock()
		p := s.peers["node_pub_b"]
		s.mu.Unlock()
		if len(p.cmmHints) == 1 && p.cmmHints[0].addr == hintAddr {
			minExpires := before.Add(hintTTL).Add(-100 * time.Millisecond)
			if p.cmmHints[0].expiresAt.Before(minExpires) {
				t.Fatalf("hint expiresAt %v < minExpected %v", p.cmmHints[0].expiresAt, minExpires)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("hint not stored; got %+v", p.cmmHints)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// TestHandleCallMeMaybe_RejectsUnknownPeer ensures CMM frames from
// peers we haven't been told about (no UpdatePeers entry) are ignored.
func TestHandleCallMeMaybe_RejectsUnknownPeer(t *testing.T) {
	s, bind, _, selfPub := newCMMService(t, 30*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	privUnknown, pubUnknown := newNodeKey(t)
	cmm := &wireframe.Frame{
		Type:          wireframe.TypeCallMeMaybe,
		SrcDeviceID:   "dev_unknown",
		DstDeviceID:   "dev_self",
		HasNonce:      true,
		Nonce:         [wireframe.NonceSize]byte{0x55},
		HasTimestamp:  true,
		Timestamp:     uint64(time.Now().UnixMilli()),
		CandidateList: []netip.AddrPort{netip.MustParseAddrPort("198.51.100.10:51820")},
	}
	payload := mustEncodeSealed(t, cmm, privUnknown, pubUnknown, selfPub)
	bind.inbound <- wireframe.Inbound{Payload: payload, Path: wireframe.PathRelay, RelaySrcDeviceID: "dev_unknown"}

	select {
	case got := <-bind.sent:
		t.Fatalf("CMM from unknown peer triggered probe to %s", got.Dst)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestProbeAllPeers_IncludesCmmHints exercises the prober loop's
// behaviour when receiver-side hints exist in addition to the
// CP-published candidates: probes go to BOTH sets, deduped if the
// hint and the published candidate happen to share an addr.
func TestProbeAllPeers_IncludesCmmHints(t *testing.T) {
	bind := newFakeBind()
	priv, pub := newNodeKey(t)
	cfg := Config{
		SelfDeviceID:       "dev_self",
		SelfNodeKeyPriv:    priv,
		SelfNodeKeyPub:     pub,
		Bind:               bind,
		STUNObserveActive:  time.Hour,
		STUNObserveIdle:    time.Hour,
		ProbeReprobeActive: time.Hour, // hand-driven via probeAllPeers
		ProbeWindow:        5 * time.Second,
		CMMHintTTL:         time.Hour,
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	privB, pubB := newNodeKey(t)
	s.UpdatePeers(map[string]PeerSnapshot{
		"node_pub_b": {
			DeviceID:   "dev_b",
			NodePub:    pubB,
			Candidates: []string{"udp4:198.51.100.10:51820"},
		},
	})

	// Inject a hint at a different addr.
	hintAddr := "udp4:198.51.100.99:51820"
	s.mu.Lock()
	p := s.peers["node_pub_b"]
	p.cmmHints = []cmmHint{{addr: hintAddr, expiresAt: time.Now().Add(time.Hour)}}
	s.peers["node_pub_b"] = p
	s.mu.Unlock()

	s.probeAllPeers(context.Background())

	// Drain bind.sent for ~250ms and collect direct probe destinations.
	dsts := map[string]bool{}
	deadline := time.After(500 * time.Millisecond)
DRAIN:
	for {
		select {
		case got := <-bind.sent:
			f, _ := mustDecodeSealedOrNil(got.Payload, privB, pubB)
			if f == nil || f.Type != wireframe.TypeProbe {
				continue
			}
			if !strings.HasPrefix(got.Dst, "udp4:") && !strings.HasPrefix(got.Dst, "udp6:") {
				continue
			}
			dsts[got.Dst] = true
		case <-deadline:
			break DRAIN
		}
	}

	if !dsts["udp4:198.51.100.10:51820"] {
		t.Errorf("expected probe to published candidate; got %v", dsts)
	}
	if !dsts[hintAddr] {
		t.Errorf("expected probe to CMM hint addr %q; got %v", hintAddr, dsts)
	}
}

// TestProbeAllPeers_ExpiresCmmHints ensures hints whose expiresAt is
// in the past are pruned and not probed.
func TestProbeAllPeers_ExpiresCmmHints(t *testing.T) {
	bind := newFakeBind()
	priv, pub := newNodeKey(t)
	cfg := Config{
		SelfDeviceID:       "dev_self",
		SelfNodeKeyPriv:    priv,
		SelfNodeKeyPub:     pub,
		Bind:               bind,
		STUNObserveActive:  time.Hour,
		STUNObserveIdle:    time.Hour,
		ProbeReprobeActive: time.Hour,
		ProbeWindow:        5 * time.Second,
		CMMHintTTL:         time.Hour,
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	privB, pubB := newNodeKey(t)
	s.UpdatePeers(map[string]PeerSnapshot{
		"node_pub_b": {
			DeviceID:   "dev_b",
			NodePub:    pubB,
			Candidates: []string{"udp4:198.51.100.10:51820"},
		},
	})

	expiredAddr := "udp4:198.51.100.77:51820"
	s.mu.Lock()
	p := s.peers["node_pub_b"]
	p.cmmHints = []cmmHint{{addr: expiredAddr, expiresAt: time.Now().Add(-time.Minute)}}
	s.peers["node_pub_b"] = p
	s.mu.Unlock()

	s.probeAllPeers(context.Background())

	// Drain briefly: only the published candidate should be probed.
	deadline := time.After(500 * time.Millisecond)
	dsts := map[string]bool{}
DRAIN:
	for {
		select {
		case got := <-bind.sent:
			f, _ := mustDecodeSealedOrNil(got.Payload, privB, pubB)
			if f == nil || f.Type != wireframe.TypeProbe {
				continue
			}
			dsts[got.Dst] = true
		case <-deadline:
			break DRAIN
		}
	}
	if dsts[expiredAddr] {
		t.Errorf("expired hint should not have been probed; got %v", dsts)
	}
	if !dsts["udp4:198.51.100.10:51820"] {
		t.Errorf("published candidate should have been probed; got %v", dsts)
	}

	// Hint should also have been pruned from peerState.
	s.mu.Lock()
	defer s.mu.Unlock()
	if hints := s.peers["node_pub_b"].cmmHints; len(hints) != 0 {
		t.Errorf("expected hints to be pruned; got %+v", hints)
	}
}

// TestSendCallMeMaybe_RoundTripsThroughBind checks that SendCallMeMaybe
// produces a wire-decodable AEAD-sealed frame on the bind's relay
// channel with the expected fields populated and a verifiable
// SrcNodeKey (the agent's own pub).
func TestSendCallMeMaybe_RoundTripsThroughBind(t *testing.T) {
	s, bind, _, selfPub := newCMMService(t, 30*time.Second)

	// Set up a peer "B" with a real keypair so SendCallMeMaybe can
	// resolve its NodePub via the s.peers lookup and seal to it. The
	// std-base64 form of pubB is what production code uses as the map
	// key; we mirror that here so SendCallMeMaybe's lookupNodePub
	// works the same way.
	privB, pubB := newNodeKey(t)
	peerKey := encodeNodePubB64(pubB)
	s.UpdatePeers(map[string]PeerSnapshot{
		peerKey: {DeviceID: "dev_b", NodePub: pubB, RelayURL: "wss://r/"},
	})

	cands := []netip.AddrPort{
		netip.MustParseAddrPort("198.51.100.10:51820"),
	}
	if err := s.SendCallMeMaybe(peerKey, "dev_b", peerKey, "wss://r/", cands); err != nil {
		t.Fatalf("SendCallMeMaybe: %v", err)
	}
	select {
	case got := <-bind.relaySent:
		if got.DstDeviceID != "dev_b" || got.RelayURL != "wss://r/" {
			t.Errorf("dst=(dev=%q url=%q), want (dev_b, wss://r/)", got.DstDeviceID, got.RelayURL)
		}
		// Open the sealed frame from the peer's POV. The SrcNodeKey
		// the AEAD returns is the agent's selfPub — i.e., authenticated
		// proof the frame came from the agent under test.
		f, src := mustDecodeSealed(t, got.Payload, privB, pubB)
		if f.Type != wireframe.TypeCallMeMaybe {
			t.Errorf("type = %s, want call_me_maybe", f.Type)
		}
		if len(f.CandidateList) != 1 || f.CandidateList[0] != cands[0] {
			t.Errorf("candidates = %v, want %v", f.CandidateList, cands)
		}
		if src != selfPub {
			t.Errorf("srcNodeKey != agent's selfPub")
		}
	case <-time.After(time.Second):
		t.Fatal("no relay frame observed")
	}
}

// TestSendCallMeMaybe_RejectsEmptyCandidateList ensures we don't ship
// a useless frame.
func TestSendCallMeMaybe_RejectsEmptyCandidateList(t *testing.T) {
	s, _, _, _ := newCMMService(t, 30*time.Second)
	if err := s.SendCallMeMaybe("node_pub_b", "dev_b", "node_pub_b", "wss://r/", nil); err == nil {
		t.Fatal("expected error for empty candidate list")
	}
}
