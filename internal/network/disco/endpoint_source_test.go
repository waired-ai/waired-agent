package disco

import (
	"context"
	"net/netip"
	"sort"
	"sync"
	"testing"
	"time"

	wireframe "github.com/waired-ai/waired-agent/proto/disco"
)

// TestKnownAndHintedFor_UnionAndDedup checks that the testharness-
// oriented accessor returns the union of NetworkMap-published
// candidates and live CMM hints, deduped, in AddrPort form.
func TestKnownAndHintedFor_UnionAndDedup(t *testing.T) {
	s, _, _, _ := newCMMService(t, time.Hour)
	_, pubB := newNodeKey(t)

	// Seed candidates via UpdatePeers; then manually inject a CMM hint
	// that overlaps with one candidate so dedup is observable.
	s.UpdatePeers(map[string]PeerSnapshot{
		"node_pub_b": {
			DeviceID: "dev_b",
			NodePub:  pubB,
			Candidates: []string{
				"udp4:198.51.100.10:51820",
				"udp6:[2001:db8::1]:51820",
				"relay:wss://r/",           // unparseable → must be skipped
				"udp4:198.51.100.10:51820", // duplicate of first candidate
			},
		},
	})

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	// Inject hints directly to bypass the full CMM verification path.
	s.mu.Lock()
	p := s.peers["node_pub_b"]
	p.cmmHints = []cmmHint{
		{addr: "udp4:198.51.100.10:51820", expiresAt: now.Add(time.Minute)},  // dup of candidate → dedup
		{addr: "udp4:198.51.100.99:51820", expiresAt: now.Add(time.Minute)},  // new
		{addr: "udp4:198.51.100.77:51820", expiresAt: now.Add(-time.Second)}, // expired → pruned
	}
	s.peers["node_pub_b"] = p
	s.mu.Unlock()

	got := s.KnownAndHintedFor("node_pub_b")
	gotStrs := make([]string, 0, len(got))
	for _, ap := range got {
		gotStrs = append(gotStrs, ap.String())
	}
	sort.Strings(gotStrs)

	want := []string{
		"198.51.100.10:51820",
		"198.51.100.99:51820",
		"[2001:db8::1]:51820",
	}
	sort.Strings(want)

	if len(gotStrs) != len(want) {
		t.Fatalf("got %d endpoints, want %d: got=%v want=%v", len(gotStrs), len(want), gotStrs, want)
	}
	for i := range want {
		if gotStrs[i] != want[i] {
			t.Errorf("endpoint[%d] = %q, want %q (full got=%v)", i, gotStrs[i], want[i], gotStrs)
		}
	}

	// Verify the expired hint was pruned from peerState in place.
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range s.peers["node_pub_b"].cmmHints {
		if h.addr == "udp4:198.51.100.77:51820" {
			t.Errorf("expired hint not pruned: %v", h)
		}
	}
}

// TestKnownAndHintedFor_UnknownPeer returns nil for a peer not in the
// peers map. The dispatcher treats nil as "skip the union" and falls
// back to the NetworkMap snapshot.
func TestKnownAndHintedFor_UnknownPeer(t *testing.T) {
	s, _, _, _ := newCMMService(t, time.Hour)
	if got := s.KnownAndHintedFor("never_seen"); got != nil {
		t.Errorf("unknown peer = %v, want nil", got)
	}
}

// TestClearHintsFor_DropsHints exercises ClearHintsFor + idempotency on
// missing peers. NetworkMap-published candidates are left alone.
func TestClearHintsFor_DropsHints(t *testing.T) {
	s, _, _, _ := newCMMService(t, time.Hour)
	_, pubB := newNodeKey(t)
	s.UpdatePeers(map[string]PeerSnapshot{
		"node_pub_b": {
			DeviceID:   "dev_b",
			NodePub:    pubB,
			Candidates: []string{"udp4:198.51.100.10:51820"},
		},
	})
	now := time.Now()
	s.mu.Lock()
	p := s.peers["node_pub_b"]
	p.cmmHints = []cmmHint{{addr: "udp4:198.51.100.99:51820", expiresAt: now.Add(time.Minute)}}
	s.peers["node_pub_b"] = p
	s.mu.Unlock()

	s.ClearHintsFor("node_pub_b")

	s.mu.Lock()
	gotHints := append([]cmmHint(nil), s.peers["node_pub_b"].cmmHints...)
	gotCands := append([]string(nil), s.peers["node_pub_b"].candidates...)
	s.mu.Unlock()
	if len(gotHints) != 0 {
		t.Errorf("cmmHints after Clear = %v, want empty", gotHints)
	}
	if len(gotCands) != 1 || gotCands[0] != "udp4:198.51.100.10:51820" {
		t.Errorf("candidates mutated by Clear: %v", gotCands)
	}

	// Idempotent on unknown peer.
	s.ClearHintsFor("never_seen")
}

// TestOnCallMeMaybe_Fires registers a callback and verifies it's invoked
// after a verified call_me_maybe frame, with the correct peer keys.
func TestOnCallMeMaybe_Fires(t *testing.T) {
	s, bind, _, selfPub := newCMMService(t, 30*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	privB, pubB := newNodeKey(t)
	s.UpdatePeers(map[string]PeerSnapshot{
		"node_pub_b": {DeviceID: "dev_b", NodePub: pubB, RelayURL: "wss://r/"},
	})

	var (
		mu     sync.Mutex
		gotPub string
		gotDev string
		fired  = make(chan struct{}, 1)
	)
	s.OnCallMeMaybe(func(peerNodePub, peerDeviceID string) {
		mu.Lock()
		gotPub = peerNodePub
		gotDev = peerDeviceID
		mu.Unlock()
		select {
		case fired <- struct{}{}:
		default:
		}
	})
	// nil registration is a silent no-op (does not panic).
	s.OnCallMeMaybe(nil)

	cmm := &wireframe.Frame{
		Type:          wireframe.TypeCallMeMaybe,
		SrcDeviceID:   "dev_b",
		DstDeviceID:   "dev_self",
		HasNonce:      true,
		Nonce:         [wireframe.NonceSize]byte{0x42},
		HasTimestamp:  true,
		Timestamp:     uint64(time.Now().UnixMilli()),
		CandidateList: []netip.AddrPort{netip.MustParseAddrPort("198.51.100.10:51820")},
	}
	payload := mustEncodeSealed(t, cmm, privB, pubB, selfPub)
	bind.inbound <- wireframe.Inbound{Payload: payload, Path: wireframe.PathRelay, RelayURL: "wss://r/", RelaySrcDeviceID: "dev_b"}

	// Drain the fire-and-forget probe so it doesn't block the
	// bind.sent channel and unrelated to this assertion.
	go func() {
		for range bind.sent {
		}
	}()

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("OnCallMeMaybe callback not invoked within 2s")
	}
	mu.Lock()
	defer mu.Unlock()
	if gotPub != "node_pub_b" || gotDev != "dev_b" {
		t.Errorf("callback args = (%q, %q), want (node_pub_b, dev_b)", gotPub, gotDev)
	}
}

// TestParseUDPEndpoint covers the ipv4 / ipv6 / unparseable cases.
func TestParseUDPEndpoint(t *testing.T) {
	cases := []struct {
		in     string
		wantOK bool
		want   string
	}{
		{"udp4:1.2.3.4:51820", true, "1.2.3.4:51820"},
		{"udp6:[2001:db8::1]:51820", true, "[2001:db8::1]:51820"},
		{"relay:wss://r/", false, ""},
		{"", false, ""},
		{"udp4:not-an-ip", false, ""},
		{"udp4:1.2.3.4", false, ""}, // missing port
	}
	for _, tc := range cases {
		got, ok := parseUDPEndpoint(tc.in)
		if ok != tc.wantOK {
			t.Errorf("parseUDPEndpoint(%q) ok=%v want %v", tc.in, ok, tc.wantOK)
			continue
		}
		if ok && got.String() != tc.want {
			t.Errorf("parseUDPEndpoint(%q) = %q, want %q", tc.in, got.String(), tc.want)
		}
	}
}
