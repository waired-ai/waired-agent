package main

import (
	"context"
	"net/netip"
	"sync"
	"testing"
	"time"

	disco "github.com/waired-ai/waired-agent/internal/network/disco"
)

// fakeDisco implements discoSubsystem for tests. Records SendCallMeMaybe
// invocations on `cmmSends` for assertion. ObservedAddr / NATType are
// settable per-test to drive the trigger gates.
type fakeDisco struct {
	mu sync.Mutex

	natType  disco.NATType
	observed netip.AddrPort

	cmmSends []fakeCMMSend
}

type fakeCMMSend struct {
	PeerNodePub  string
	PeerDeviceID string
	PeerNodeKey  string
	RelayURL     string
	Candidates   []netip.AddrPort
}

func (f *fakeDisco) UpdateRelays([]string)                     {}
func (f *fakeDisco) UpdatePeers(map[string]disco.PeerSnapshot) {}

func (f *fakeDisco) ObservedAddr() netip.AddrPort {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.observed
}

func (f *fakeDisco) NATType() disco.NATType {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.natType
}

func (f *fakeDisco) SendCallMeMaybe(peerNodePub, peerDeviceID, peerNodeKey, relayURL string, candidates []netip.AddrPort) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmmSends = append(f.cmmSends, fakeCMMSend{
		PeerNodePub:  peerNodePub,
		PeerDeviceID: peerDeviceID,
		PeerNodeKey:  peerNodeKey,
		RelayURL:     relayURL,
		Candidates:   append([]netip.AddrPort(nil), candidates...),
	})
	return nil
}

func (f *fakeDisco) sendCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.cmmSends)
}

func (f *fakeDisco) setObserved(a netip.AddrPort) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.observed = a
}

func (f *fakeDisco) setNATType(n disco.NATType) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.natType = n
}

// cmmTestConfig is fastTestConfig with CMM cadence dialed to 5ms so
// tests can step past it deterministically. CMMBootstrapDelay defaults
// to 30s; tests that exercise the direct-stuck trigger override
// lastEvalAt directly.
func cmmTestConfig() reconcilerConfig {
	c := fastTestConfig()
	c.CallMeMaybeInterval = 5 * time.Millisecond
	c.CallMeMaybeBackoffMax = 100 * time.Millisecond
	c.CMMBootstrapDelay = 5 * time.Millisecond
	c.MinDwellTime = time.Millisecond
	c.FallbackAfter = time.Hour // disable the safety-net flip during these tests
	return c
}

// waitForCMMSendCount blocks until f.cmmSends reaches at least n, or
// fails the test on timeout. Tick spawns the actual SendCallMeMaybe in
// a goroutine, so the assertion needs a small wait.
func waitForCMMSendCount(t *testing.T, f *fakeDisco, n int, deadline time.Duration) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if f.sendCount() >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("expected ≥%d CMM sends within %v, got %d", n, deadline, f.sendCount())
}

// TestReconciler_EmitsCallMeMaybeWhileOnRelay verifies the relay-state
// rescue trigger: a peer whose currentPath is "relay", with valid
// observed_addr and NAT type != Symmetric, should receive a CMM frame
// on the next Tick.
func TestReconciler_EmitsCallMeMaybeWhileOnRelay(t *testing.T) {
	pubA := mkPeerKey(t)
	nm := nm1Peer(pubA, "udp4:198.51.100.10:51820")

	eng := &fakeEngine{}
	prov := &agentProvider{}
	rec := newReconciler(eng, prov, quietLogger(), nil, cmmTestConfig())

	fd := &fakeDisco{}
	fd.setObserved(netip.MustParseAddrPort("203.0.113.42:51820"))
	fd.setNATType(disco.NATTypeEIM)
	rec.AttachDisco(fd)

	if err := rec.Apply(nm); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Force the peer into relay state so the trigger evaluates.
	rec.mu.Lock()
	st := rec.state[pubA]
	st.currentPath = pathRelay
	rec.mu.Unlock()

	rec.Tick(context.Background())
	waitForCMMSendCount(t, fd, 1, 250*time.Millisecond)

	fd.mu.Lock()
	if fd.cmmSends[0].PeerDeviceID != "dev_peer_a" {
		t.Errorf("PeerDeviceID = %q, want dev_peer_a", fd.cmmSends[0].PeerDeviceID)
	}
	if len(fd.cmmSends[0].Candidates) != 1 || fd.cmmSends[0].Candidates[0].String() != "203.0.113.42:51820" {
		t.Errorf("Candidates = %v, want [203.0.113.42:51820]", fd.cmmSends[0].Candidates)
	}
	fd.mu.Unlock()
}

// TestReconciler_EmitsCallMeMaybeOnDirectStuck verifies the
// direct-stuck-bootstrap trigger: a peer with currentPath == direct,
// no direct samples, no disco hint, and lastEvalAt > CMMBootstrapDelay
// ago receives a CMM frame.
func TestReconciler_EmitsCallMeMaybeOnDirectStuck(t *testing.T) {
	pubA := mkPeerKey(t)
	nm := nm1Peer(pubA, "udp4:198.51.100.10:51820")

	eng := &fakeEngine{}
	prov := &agentProvider{}
	rec := newReconciler(eng, prov, quietLogger(), nil, cmmTestConfig())

	fd := &fakeDisco{}
	fd.setObserved(netip.MustParseAddrPort("203.0.113.42:51820"))
	fd.setNATType(disco.NATTypeEIM)
	rec.AttachDisco(fd)

	if err := rec.Apply(nm); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// Move lastEvalAt back so the bootstrap delay has elapsed.
	rec.mu.Lock()
	rec.state[pubA].lastEvalAt = time.Now().Add(-time.Second)
	rec.mu.Unlock()

	rec.Tick(context.Background())
	waitForCMMSendCount(t, fd, 1, 250*time.Millisecond)
}

// TestReconciler_DoesNotEmitOnDirectWithSamples ensures the direct-
// stuck trigger does NOT fire once we've successfully sampled direct
// RTT — at that point, regular probing is working.
func TestReconciler_DoesNotEmitOnDirectWithSamples(t *testing.T) {
	pubA := mkPeerKey(t)
	nm := nm1Peer(pubA, "udp4:198.51.100.10:51820")

	eng := &fakeEngine{}
	prov := &agentProvider{}
	rec := newReconciler(eng, prov, quietLogger(), nil, cmmTestConfig())

	fd := &fakeDisco{}
	fd.setObserved(netip.MustParseAddrPort("203.0.113.42:51820"))
	fd.setNATType(disco.NATTypeEIM)
	rec.AttachDisco(fd)

	if err := rec.Apply(nm); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	rec.mu.Lock()
	st := rec.state[pubA]
	st.directSampleCount = 5
	st.directRTTEWMA = 5 * time.Millisecond
	st.relaySampleCount = 5
	st.relayRTTEWMA = 50 * time.Millisecond
	st.lastEvalAt = time.Now().Add(-time.Second)
	rec.mu.Unlock()

	rec.Tick(context.Background())
	time.Sleep(50 * time.Millisecond)
	if fd.sendCount() != 0 {
		t.Fatalf("expected no CMM sends; got %d", fd.sendCount())
	}
}

// TestReconciler_RateLimitsCallMeMaybe ensures back-to-back Ticks
// within the cadence interval result in only one CMM emission.
func TestReconciler_RateLimitsCallMeMaybe(t *testing.T) {
	pubA := mkPeerKey(t)
	nm := nm1Peer(pubA, "udp4:198.51.100.10:51820")

	eng := &fakeEngine{}
	prov := &agentProvider{}
	cfg := cmmTestConfig()
	cfg.CallMeMaybeInterval = 200 * time.Millisecond // longer than the test span
	rec := newReconciler(eng, prov, quietLogger(), nil, cfg)

	fd := &fakeDisco{}
	fd.setObserved(netip.MustParseAddrPort("203.0.113.42:51820"))
	fd.setNATType(disco.NATTypeEIM)
	rec.AttachDisco(fd)

	if err := rec.Apply(nm); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	rec.mu.Lock()
	rec.state[pubA].currentPath = pathRelay
	rec.mu.Unlock()

	rec.Tick(context.Background())
	waitForCMMSendCount(t, fd, 1, 100*time.Millisecond)

	rec.Tick(context.Background())
	rec.Tick(context.Background())
	time.Sleep(50 * time.Millisecond)
	if got := fd.sendCount(); got != 1 {
		t.Fatalf("expected exactly 1 CMM send across 3 ticks within cadence; got %d", got)
	}
}

// TestEffectiveCMMCadence covers the linear-with-cap backoff helper
// directly — the integration of backoff with Tick scheduling is
// brittle to exercise via wall-clock, but the cadence calculation
// itself is straightforward arithmetic.
func TestEffectiveCMMCadence(t *testing.T) {
	base := 15 * time.Second
	max := 5 * time.Minute
	cases := []struct {
		streak int
		want   time.Duration
	}{
		{0, base},
		{1, base},
		{2, base},
		{3, 2 * base},
		{4, 3 * base},
		{5, 4 * base},
		{20, 19 * base},
		{30, max}, // 29 * 15s = 435s > 300s cap → max
	}
	for _, c := range cases {
		got := effectiveCMMCadence(base, max, c.streak)
		if got != c.want {
			t.Errorf("effectiveCMMCadence(streak=%d) = %v, want %v", c.streak, got, c.want)
		}
	}
}

// TestReconciler_BacksOffOnFailStreak ensures that when callMeMaybeFailStreak
// is already large, Tick respects the stretched cadence rather than
// emitting another CMM at base interval. We pre-seed state so the test
// doesn't depend on wall-clock-gated streak growth.
func TestReconciler_BacksOffOnFailStreak(t *testing.T) {
	pubA := mkPeerKey(t)
	nm := nm1Peer(pubA, "udp4:198.51.100.10:51820")

	eng := &fakeEngine{}
	prov := &agentProvider{}
	cfg := cmmTestConfig()
	cfg.CallMeMaybeInterval = 50 * time.Millisecond
	cfg.CallMeMaybeBackoffMax = time.Second
	rec := newReconciler(eng, prov, quietLogger(), nil, cfg)

	fd := &fakeDisco{}
	fd.setObserved(netip.MustParseAddrPort("203.0.113.42:51820"))
	fd.setNATType(disco.NATTypeEIM)
	rec.AttachDisco(fd)

	if err := rec.Apply(nm); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Pre-seed: peer is on relay, fail-streak is 5 (= 4×base = 200ms
	// effective cadence), last CMM was sent 100ms ago. Cadence not yet
	// elapsed → no emission.
	rec.mu.Lock()
	st := rec.state[pubA]
	st.currentPath = pathRelay
	st.callMeMaybeFailStreak = 5
	st.cmmFailureRecorded = true // already counted as failure, won't double
	st.lastCallMeMaybeAt = time.Now().Add(-100 * time.Millisecond)
	rec.mu.Unlock()

	rec.Tick(context.Background())
	time.Sleep(50 * time.Millisecond)
	if got := fd.sendCount(); got != 0 {
		t.Fatalf("backoff should suppress emission within stretched cadence; got %d sends", got)
	}

	// Move lastCallMeMaybeAt back so cadence (200ms) elapsed → emit.
	rec.mu.Lock()
	rec.state[pubA].lastCallMeMaybeAt = time.Now().Add(-300 * time.Millisecond)
	rec.mu.Unlock()
	rec.Tick(context.Background())
	waitForCMMSendCount(t, fd, 1, 200*time.Millisecond)
}

// TestReconciler_SkipsCallMeMaybeOnSymmetricNAT ensures that when the
// agent's own NAT is symmetric, CMM emission is skipped — the
// observedAddr from STUN is per-relay-flow, so advertising it to a
// peer is misleading.
func TestReconciler_SkipsCallMeMaybeOnSymmetricNAT(t *testing.T) {
	pubA := mkPeerKey(t)
	nm := nm1Peer(pubA, "udp4:198.51.100.10:51820")

	eng := &fakeEngine{}
	prov := &agentProvider{}
	rec := newReconciler(eng, prov, quietLogger(), nil, cmmTestConfig())

	fd := &fakeDisco{}
	fd.setObserved(netip.MustParseAddrPort("203.0.113.42:51820"))
	fd.setNATType(disco.NATTypeSymmetric)
	rec.AttachDisco(fd)

	if err := rec.Apply(nm); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	rec.mu.Lock()
	rec.state[pubA].currentPath = pathRelay
	rec.mu.Unlock()

	rec.Tick(context.Background())
	time.Sleep(50 * time.Millisecond)
	if got := fd.sendCount(); got != 0 {
		t.Fatalf("symmetric NAT should suppress CMM emission; got %d sends", got)
	}
}

// TestReconciler_SkipsCallMeMaybeWhenObservedAddrUnknown ensures CMM
// is skipped before STUN observation has produced a usable addr.
func TestReconciler_SkipsCallMeMaybeWhenObservedAddrUnknown(t *testing.T) {
	pubA := mkPeerKey(t)
	nm := nm1Peer(pubA, "udp4:198.51.100.10:51820")

	eng := &fakeEngine{}
	prov := &agentProvider{}
	rec := newReconciler(eng, prov, quietLogger(), nil, cmmTestConfig())

	fd := &fakeDisco{}
	// observed left as zero AddrPort
	fd.setNATType(disco.NATTypeEIM)
	rec.AttachDisco(fd)

	if err := rec.Apply(nm); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	rec.mu.Lock()
	rec.state[pubA].currentPath = pathRelay
	rec.mu.Unlock()

	rec.Tick(context.Background())
	time.Sleep(50 * time.Millisecond)
	if got := fd.sendCount(); got != 0 {
		t.Fatalf("missing observedAddr should suppress CMM emission; got %d sends", got)
	}
}

// TestReconciler_ResetsFailStreakOnDirectPong verifies the per-peer
// CMM fail-streak collapses to 0 when a direct pong arrives — the
// path is alive again, future cadences should use the base interval.
func TestReconciler_ResetsFailStreakOnDirectPong(t *testing.T) {
	pubA := mkPeerKey(t)
	nm := nm1Peer(pubA, "udp4:198.51.100.10:51820")

	eng := &fakeEngine{}
	prov := &agentProvider{}
	rec := newReconciler(eng, prov, quietLogger(), nil, cmmTestConfig())

	if err := rec.Apply(nm); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	rec.mu.Lock()
	st := rec.state[pubA]
	st.callMeMaybeFailStreak = 7
	st.cmmFailureRecorded = true
	rec.mu.Unlock()

	rec.OnDiscoEvent(disco.EventPongFromPeer{
		PeerNodePub:  pubA,
		PeerDeviceID: "dev_peer_a",
		DirectSrc:    netip.MustParseAddrPort("198.51.100.10:51820"),
		ReceivedAt:   time.Now(),
	})

	rec.mu.Lock()
	streak := rec.state[pubA].callMeMaybeFailStreak
	failed := rec.state[pubA].cmmFailureRecorded
	rec.mu.Unlock()
	if streak != 0 || failed {
		t.Fatalf("expected streak=0 failureRecorded=false after direct pong; got streak=%d failed=%v", streak, failed)
	}
}

// TestReconciler_OnEventCallMeMaybeReceivedRecordsTimestamp ensures the
// observability fields surface incoming CMM events for the management
// API.
func TestReconciler_OnEventCallMeMaybeReceivedRecordsTimestamp(t *testing.T) {
	pubA := mkPeerKey(t)
	nm := nm1Peer(pubA, "udp4:198.51.100.10:51820")

	eng := &fakeEngine{}
	prov := &agentProvider{}
	rec := newReconciler(eng, prov, quietLogger(), nil, cmmTestConfig())

	if err := rec.Apply(nm); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	at := time.Now()
	rec.OnDiscoEvent(disco.EventCallMeMaybeReceived{
		PeerNodePub:  pubA,
		PeerDeviceID: "dev_peer_a",
		Candidates:   []netip.AddrPort{netip.MustParseAddrPort("198.51.100.10:51820")},
		At:           at,
	})

	snap := rec.Snapshot()
	ps, ok := snap[pubA]
	if !ok {
		t.Fatalf("missing snapshot entry for %s", pubA)
	}
	if ps.CallMeMaybeRecvCount != 1 {
		t.Errorf("CallMeMaybeRecvCount = %d, want 1", ps.CallMeMaybeRecvCount)
	}
	if !ps.CallMeMaybeRecvAt.Equal(at) {
		t.Errorf("CallMeMaybeRecvAt = %v, want %v", ps.CallMeMaybeRecvAt, at)
	}
}
