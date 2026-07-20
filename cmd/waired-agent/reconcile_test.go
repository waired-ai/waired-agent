package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"io"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/curve25519"

	disco "github.com/waired-ai/waired-agent/internal/network/disco"
	"github.com/waired-ai/waired-agent/internal/network/wgnet"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// fakeEngine implements peerEngine for tests. UpdatePeers records the
// last set; PeerHandshakeTimes returns whatever the test sets. The
// "current handshake time per peer" can be advanced to simulate either
// direct-path success or persistent failure.
type fakeEngine struct {
	mu             sync.Mutex
	lastPeers      []wgnet.Peer
	updateCalls    int
	handshakeByPub map[string]time.Time
	peerNets       map[string]string
}

func (f *fakeEngine) SetPeerNetworks(nets map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.peerNets = nets
}

func (f *fakeEngine) PeerNetworks() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.peerNets
}

func (f *fakeEngine) UpdatePeers(peers []wgnet.Peer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastPeers = append([]wgnet.Peer(nil), peers...)
	f.updateCalls++
	return nil
}

func (f *fakeEngine) PeerHandshakeTimes() (map[string]time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]time.Time, len(f.handshakeByPub))
	for k, v := range f.handshakeByPub {
		out[k] = v
	}
	return out, nil
}

func (f *fakeEngine) setHandshake(pubB64 string, t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.handshakeByPub == nil {
		f.handshakeByPub = map[string]time.Time{}
	}
	f.handshakeByPub[pubB64] = t
}

func (f *fakeEngine) lastEndpointFor(pubB64 string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	rawWant, _ := base64.StdEncoding.DecodeString(pubB64)
	for _, p := range f.lastPeers {
		if string(p.WireGuardPublicKey) == string(rawWant) {
			return p.Endpoint
		}
	}
	return ""
}

func mkPeerKey(t *testing.T) string {
	t.Helper()
	priv := make([]byte, 32)
	if _, err := rand.Read(priv); err != nil {
		t.Fatalf("rand: %v", err)
	}
	priv[0] &= 248
	priv[31] &= 127
	priv[31] |= 64
	pub, err := curve25519.X25519(priv, curve25519.Basepoint)
	if err != nil {
		t.Fatalf("x25519: %v", err)
	}
	return base64.StdEncoding.EncodeToString(pub)
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// nm1Peer constructs a single-peer NetworkMap with the given peer key,
// home relay, and one direct UDP candidate. Used as the common
// fixture across most tests.
func nm1Peer(pubA, candAddr string) *signer.NetworkMap {
	return &signer.NetworkMap{
		Relays: []signer.NetworkMapRelay{
			{RelayID: "relay_a", URL: "wss://relay-a.example.com/relay/v1/connect"},
		},
		Peers: []signer.NetworkMapPeer{
			{
				DeviceID:      "dev_peer_a",
				DeviceName:    "peer-a",
				OverlayIP:     "100.64.0.2",
				NodePublicKey: pubA,
				Endpoints:     []signer.EndpointCandidate{{Addr: candAddr, Kind: signer.KindLocal}},
				HomeRelay:     "relay_a",
			},
		},
	}
}

// fastTestConfig returns a reconcilerConfig with thresholds tuned to
// keep tests under ~200ms while exercising the same code paths the
// production defaults do. MinDwellTime is set to 1ms (effectively off
// for most tests); the dwell-suppression test overrides explicitly to
// a measurable value.
//
// Note that MinDwellTime cannot be 0 here because withDefaults treats
// 0 as "use production default" (30s). Tests that need to bypass
// dwell entirely either pass distinct timestamps via e.At or sleep
// past the 1ms window between trip and reverse-trip.
func fastTestConfig() reconcilerConfig {
	return reconcilerConfig{
		FallbackAfter:     50 * time.Millisecond,
		DowngradeRTTRatio: 2.0,
		// Deliberately stricter than the production default (1.5, see
		// defaultUpgradeRTTRatio): a sub-1.0 ratio lets tests assert
		// the gate actually compares RTTs, not just the pong streak.
		UpgradeRTTRatio:   0.8,
		DowngradeMisses:   3,
		UpgradePongStreak: 3,
		EWMAAlpha:         0.5,
		MinRTTSamples:     2,
		MinDwellTime:      time.Millisecond,
	}
}

// --- LEGACY behaviour, repurposed for the new model ---

// TestReconciler_SafetyNetFiresWhenProbesSilent: when no disco events
// arrive AND WireGuard hasn't completed a handshake within
// FallbackAfter, the safety-net Tick flips the peer to relay. This is
// the new shape of the old "FallsBackToRelayAfterFallbackAfter" test:
// the probe-driven layer is asleep (e.g. disco subsystem disabled), so
// the safety net is the sole fallback path.
func TestReconciler_SafetyNetFiresWhenProbesSilent(t *testing.T) {
	pubA := mkPeerKey(t)
	nm := nm1Peer(pubA, "udp4:198.51.100.10:51820")

	eng := &fakeEngine{}
	prov := &agentProvider{}
	rec := newReconciler(eng, prov, quietLogger(), nil, fastTestConfig())

	if err := rec.Apply(nm); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := eng.lastEndpointFor(pubA); !strings.HasPrefix(got, "udp4:") {
		t.Fatalf("expected direct after Apply, got %q", got)
	}

	// Tick immediately — too early.
	rec.Tick(context.Background())
	if got := eng.lastEndpointFor(pubA); !strings.HasPrefix(got, "udp4:") {
		t.Fatalf("expected direct still after early Tick, got %q", got)
	}

	// Wait past fallback-after AND past last-direct-evidence (= Apply
	// time). No disco events ever arrived (probes silent), no WG
	// handshake completed → safety net fires.
	time.Sleep(rec.cfg.FallbackAfter + 25*time.Millisecond)
	rec.Tick(context.Background())
	if got := eng.lastEndpointFor(pubA); !strings.HasPrefix(got, "relay:") {
		t.Fatalf("expected relay endpoint after safety-net fallback, got %q", got)
	}
	if !strings.Contains(eng.lastEndpointFor(pubA), "wss://relay-a.example.com") {
		t.Fatalf("relay endpoint should reference relay_a URL, got %q", eng.lastEndpointFor(pubA))
	}
}

// TestReconciler_StaysDirectIfHandshakeSucceeds: a successful
// handshake observed via PeerHandshakeTimes blocks the safety-net.
func TestReconciler_StaysDirectIfHandshakeSucceeds(t *testing.T) {
	pubA := mkPeerKey(t)
	nm := nm1Peer(pubA, "udp4:198.51.100.10:51820")

	eng := &fakeEngine{}
	prov := &agentProvider{}
	rec := newReconciler(eng, prov, quietLogger(), nil, fastTestConfig())

	if err := rec.Apply(nm); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Simulate a successful handshake observed mid-window.
	time.Sleep(rec.cfg.FallbackAfter / 2)
	eng.setHandshake(pubA, time.Now())

	// Wait past fallback-after and Tick; should stay direct.
	time.Sleep(rec.cfg.FallbackAfter)
	rec.Tick(context.Background())
	if got := eng.lastEndpointFor(pubA); !strings.HasPrefix(got, "udp4:") {
		t.Fatalf("expected direct UDP after successful handshake, got %q", got)
	}
}

// TestReconciler_NewMapPreservesPathAndEWMA: a peer that has flipped
// to relay via probe-driven downgrade MUST stay on relay across new
// NetworkMap publications. CP republishes happen for any peer change
// in the network (often every few seconds during enrollment); the
// reconciler must be idempotent w.r.t. path selection so an unrelated
// republish doesn't undo a downgrade. EWMAs / miss streak / etc. are
// also preserved across Apply.
//
// (This supersedes the legacy "fresh chance for direct on new map"
// behaviour. The probe-driven upgrade trigger now handles
// genuine reachability recoveries via asymmetric ratio + pong streak.)
func TestReconciler_NewMapPreservesPathAndEWMA(t *testing.T) {
	pubA := mkPeerKey(t)

	eng := &fakeEngine{}
	prov := &agentProvider{}
	rec := newReconciler(eng, prov, quietLogger(), nil, fastTestConfig())

	if err := rec.Apply(nm1Peer(pubA, "udp4:198.51.100.10:51820")); err != nil {
		t.Fatalf("Apply 1: %v", err)
	}

	// Inject a couple of RTT samples on each path so EWMAs are non-zero.
	now := time.Now()
	for i := 0; i < 3; i++ {
		rec.OnDiscoEvent(disco.EventProbeRTTSampled{
			PeerNodePub: pubA, PeerDeviceID: "dev_peer_a",
			Path: pathDirect, RTT: 100 * time.Millisecond, At: now,
		})
		rec.OnDiscoEvent(disco.EventProbeRTTSampled{
			PeerNodePub: pubA, PeerDeviceID: "dev_peer_a",
			Path: pathRelay, RTT: 50 * time.Millisecond, At: now,
		})
	}
	// Trip a downgrade via miss streak. Each round-finalized event with
	// AnySuccess=false bumps directMissStreak once — the round-aware
	// path replaces the per-probe EventProbeMissed handler.
	for i := 0; i < rec.cfg.DowngradeMisses; i++ {
		rec.OnDiscoEvent(disco.EventProbeRoundFinalized{
			PeerNodePub: pubA, PeerDeviceID: "dev_peer_a",
			Path: pathDirect, RoundID: uint64(i + 1), AnySuccess: false, At: now,
		})
	}
	if got := eng.lastEndpointFor(pubA); !strings.HasPrefix(got, "relay:") {
		t.Fatalf("expected relay after miss-streak downgrade, got %q", got)
	}
	beforeSnap := rec.Snapshot()[pubA]
	if beforeSnap.DirectRTTMS == 0 || beforeSnap.RelayRTTMS == 0 {
		t.Fatalf("EWMAs should be non-zero before Apply: %+v", beforeSnap)
	}
	wantMissStreak := beforeSnap.DirectMissStreak

	// New map → must preserve currentPath=relay (no oscillation).
	if err := rec.Apply(nm1Peer(pubA, "udp4:198.51.100.10:51820")); err != nil {
		t.Fatalf("Apply 2: %v", err)
	}
	if got := eng.lastEndpointFor(pubA); !strings.HasPrefix(got, "relay:") {
		t.Fatalf("expected relay PRESERVED across Apply, got %q", got)
	}
	afterSnap := rec.Snapshot()[pubA]
	if afterSnap.CurrentPath != pathRelay {
		t.Errorf("currentPath after Apply = %q, want relay (preserved)", afterSnap.CurrentPath)
	}
	if afterSnap.DirectMissStreak != wantMissStreak {
		t.Errorf("directMissStreak after Apply = %d, want %d (preserved)", afterSnap.DirectMissStreak, wantMissStreak)
	}
	if afterSnap.DirectRTTMS != beforeSnap.DirectRTTMS {
		t.Errorf("DirectRTTMS lost across Apply: before=%v after=%v", beforeSnap.DirectRTTMS, afterSnap.DirectRTTMS)
	}
	if afterSnap.RelayRTTMS != beforeSnap.RelayRTTMS {
		t.Errorf("RelayRTTMS lost across Apply: before=%v after=%v", beforeSnap.RelayRTTMS, afterSnap.RelayRTTMS)
	}
}

// TestReconciler_NewPeerInMapStartsOnDirect: a peer that appears for
// the first time in a NetworkMap initializes at currentPath=direct so
// the agent always tries direct before the safety net or probe-driven
// downgrade kicks in. Existing peers are unaffected.
func TestReconciler_NewPeerInMapStartsOnDirect(t *testing.T) {
	pubA := mkPeerKey(t)

	eng := &fakeEngine{}
	prov := &agentProvider{}
	rec := newReconciler(eng, prov, quietLogger(), nil, fastTestConfig())
	if err := rec.Apply(nm1Peer(pubA, "udp4:198.51.100.10:51820")); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	snap := rec.Snapshot()[pubA]
	if snap.CurrentPath != pathDirect {
		t.Errorf("new peer currentPath = %q, want direct", snap.CurrentPath)
	}
}

// TestReconciler_DiscoPongAdoptsObservedAddr: EventPongFromPeer no
// longer flips the path on its own (that's evaluateSwitch's job), but
// it does adopt the disco-observed addr so subsequent direct routing
// uses it instead of the published Endpoints[0].
func TestReconciler_DiscoPongAdoptsObservedAddr(t *testing.T) {
	pubA := mkPeerKey(t)
	nm := &signer.NetworkMap{
		Relays: []signer.NetworkMapRelay{
			{RelayID: "relay_a", URL: "wss://relay-a.example.com/relay/v1/connect"},
		},
		Peers: []signer.NetworkMapPeer{
			{
				DeviceID:      "dev_peer_a",
				DeviceName:    "peer-a",
				OverlayIP:     "100.64.0.2",
				NodePublicKey: pubA,
				// RFC1918 — won't be reachable cross-NAT, so the disco
				// addr should win once it arrives.
				Endpoints: []signer.EndpointCandidate{{Addr: "udp4:10.20.30.40:51820", Kind: signer.KindLocal}},
				HomeRelay: "relay_a",
			},
		},
	}

	eng := &fakeEngine{}
	prov := &agentProvider{}
	rec := newReconciler(eng, prov, quietLogger(), nil, fastTestConfig())
	if err := rec.Apply(nm); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Disco pong arrives from the peer at a public addr.
	rec.OnDiscoEvent(disco.EventPongFromPeer{
		PeerNodePub:  pubA,
		PeerDeviceID: "dev_peer_a",
		DirectSrc:    netip.MustParseAddrPort("203.0.113.7:54321"),
		ReceivedAt:   time.Now(),
	})

	// Trigger a recompute (any disco event the reconciler observes
	// works; here we use a benign relay-path RTT sample which doesn't
	// drive a switch by itself).
	rec.OnDiscoEvent(disco.EventProbeRTTSampled{
		PeerNodePub: pubA, PeerDeviceID: "dev_peer_a",
		Path: pathRelay, RTT: 30 * time.Millisecond, At: time.Now(),
	})
	if got := eng.lastEndpointFor(pubA); got != "udp4:203.0.113.7:54321" {
		t.Fatalf("expected direct addr from disco hint udp4:203.0.113.7:54321, got %q", got)
	}
}

// TestReconciler_ForceRelayShortCircuits: --force-relay routes via
// relay from the start; no probe-driven switch can override.
func TestReconciler_ForceRelayShortCircuits(t *testing.T) {
	pubA := mkPeerKey(t)
	nm := nm1Peer(pubA, "udp4:198.51.100.10:51820")

	eng := &fakeEngine{}
	prov := &agentProvider{}
	cfg := fastTestConfig()
	cfg.ForceRelay = true
	rec := newReconciler(eng, prov, quietLogger(), nil, cfg)

	if err := rec.Apply(nm); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := eng.lastEndpointFor(pubA); !strings.HasPrefix(got, "relay:") {
		t.Fatalf("expected relay endpoint with force-relay, got %q", got)
	}

	// Even a direct RTT sample with a great ratio doesn't override.
	for i := 0; i < 5; i++ {
		rec.OnDiscoEvent(disco.EventProbeRTTSampled{
			PeerNodePub: pubA, PeerDeviceID: "dev_peer_a",
			Path: pathDirect, RTT: 5 * time.Millisecond, At: time.Now(),
		})
		rec.OnDiscoEvent(disco.EventProbeRTTSampled{
			PeerNodePub: pubA, PeerDeviceID: "dev_peer_a",
			Path: pathRelay, RTT: 200 * time.Millisecond, At: time.Now(),
		})
	}
	if got := eng.lastEndpointFor(pubA); !strings.HasPrefix(got, "relay:") {
		t.Fatalf("force-relay must keep relay endpoint, got %q", got)
	}
}

// --- NEW probe-driven behaviour ---

// TestReconciler_DowngradeOnRTTRatio: enough samples on both paths,
// direct EWMA > N × relay EWMA → flip to relay.
func TestReconciler_DowngradeOnRTTRatio(t *testing.T) {
	pubA := mkPeerKey(t)
	nm := nm1Peer(pubA, "udp4:198.51.100.10:51820")

	eng := &fakeEngine{}
	prov := &agentProvider{}
	rec := newReconciler(eng, prov, quietLogger(), nil, fastTestConfig())
	if err := rec.Apply(nm); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Feed relay RTT first (low) so the comparison has both sides.
	now := time.Now()
	for i := 0; i < rec.cfg.MinRTTSamples; i++ {
		rec.OnDiscoEvent(disco.EventProbeRTTSampled{
			PeerNodePub: pubA, PeerDeviceID: "dev_peer_a",
			Path: pathRelay, RTT: 30 * time.Millisecond, At: now,
		})
	}
	// Direct RTT: well over 2× relay → should trigger downgrade on the
	// MinRTTSamples-th direct sample.
	for i := 0; i < rec.cfg.MinRTTSamples; i++ {
		rec.OnDiscoEvent(disco.EventProbeRTTSampled{
			PeerNodePub: pubA, PeerDeviceID: "dev_peer_a",
			Path: pathDirect, RTT: 200 * time.Millisecond, At: now,
		})
	}
	if got := eng.lastEndpointFor(pubA); !strings.HasPrefix(got, "relay:") {
		t.Fatalf("expected relay after RTT-ratio downgrade, got %q", got)
	}
	snap := rec.Snapshot()[pubA]
	if snap.LastSwitchReason != "rtt_ratio" {
		t.Errorf("LastSwitchReason = %q, want rtt_ratio", snap.LastSwitchReason)
	}
}

// TestReconciler_DowngradeOnMissStreak: M consecutive missed direct
// probes → flip to relay regardless of RTT data.
func TestReconciler_DowngradeOnMissStreak(t *testing.T) {
	pubA := mkPeerKey(t)
	nm := nm1Peer(pubA, "udp4:198.51.100.10:51820")

	eng := &fakeEngine{}
	prov := &agentProvider{}
	rec := newReconciler(eng, prov, quietLogger(), nil, fastTestConfig())
	if err := rec.Apply(nm); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	now := time.Now()
	// One fewer than the threshold — should NOT switch.
	for i := 0; i < rec.cfg.DowngradeMisses-1; i++ {
		rec.OnDiscoEvent(disco.EventProbeRoundFinalized{
			PeerNodePub: pubA, PeerDeviceID: "dev_peer_a",
			Path: pathDirect, RoundID: uint64(i + 1), AnySuccess: false, At: now,
		})
	}
	if got := eng.lastEndpointFor(pubA); !strings.HasPrefix(got, "udp4:") {
		t.Fatalf("expected direct still under threshold, got %q", got)
	}

	// One more → trigger.
	rec.OnDiscoEvent(disco.EventProbeRoundFinalized{
		PeerNodePub: pubA, PeerDeviceID: "dev_peer_a",
		Path: pathDirect, RoundID: uint64(rec.cfg.DowngradeMisses), AnySuccess: false, At: now,
	})
	if got := eng.lastEndpointFor(pubA); !strings.HasPrefix(got, "relay:") {
		t.Fatalf("expected relay after miss-streak downgrade, got %q", got)
	}
	snap := rec.Snapshot()[pubA]
	if snap.LastSwitchReason != "miss_streak" {
		t.Errorf("LastSwitchReason = %q, want miss_streak", snap.LastSwitchReason)
	}
}

// TestReconciler_UpgradeOnAsymmetricRatio: while on relay, a string
// of direct pongs with much-lower-than-relay RTT triggers upgrade.
func TestReconciler_UpgradeOnAsymmetricRatio(t *testing.T) {
	pubA := mkPeerKey(t)
	nm := nm1Peer(pubA, "udp4:198.51.100.10:51820")

	eng := &fakeEngine{}
	prov := &agentProvider{}
	rec := newReconciler(eng, prov, quietLogger(), nil, fastTestConfig())
	if err := rec.Apply(nm); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Force the peer onto relay first (miss streak).
	t0 := time.Now()
	for i := 0; i < rec.cfg.DowngradeMisses; i++ {
		rec.OnDiscoEvent(disco.EventProbeRoundFinalized{
			PeerNodePub: pubA, PeerDeviceID: "dev_peer_a",
			Path: pathDirect, RoundID: uint64(i + 1), AnySuccess: false, At: t0,
		})
	}
	if got := eng.lastEndpointFor(pubA); !strings.HasPrefix(got, "relay:") {
		t.Fatalf("setup: expected relay after miss-streak, got %q", got)
	}

	// Feed lots of RTT samples on both sides so the ratio criterion
	// fires; direct must be well below 0.8× relay AND the last K
	// direct probe rounds must all have pongged. Use a timestamp past
	// MinDwellTime so the dwell-suppression doesn't block the upgrade.
	tUp := t0.Add(2 * rec.cfg.MinDwellTime)
	for i := 0; i < rec.cfg.MinRTTSamples; i++ {
		rec.OnDiscoEvent(disco.EventProbeRTTSampled{
			PeerNodePub: pubA, PeerDeviceID: "dev_peer_a",
			Path: pathRelay, RTT: 100 * time.Millisecond, At: tUp,
		})
	}
	// RTT samples drive direct EWMA + sample count; RoundFinalized
	// drives the pong ring.
	for i := 0; i < rec.cfg.MinRTTSamples; i++ {
		rec.OnDiscoEvent(disco.EventProbeRTTSampled{
			PeerNodePub: pubA, PeerDeviceID: "dev_peer_a",
			Path: pathDirect, RTT: 20 * time.Millisecond, At: tUp,
		})
	}
	for i := 0; i < rec.cfg.UpgradePongStreak; i++ {
		rec.OnDiscoEvent(disco.EventProbeRoundFinalized{
			PeerNodePub: pubA, PeerDeviceID: "dev_peer_a",
			Path: pathDirect, RoundID: uint64(100 + i), AnySuccess: true, At: tUp,
		})
	}
	if got := eng.lastEndpointFor(pubA); !strings.HasPrefix(got, "udp4:") {
		t.Fatalf("expected direct after RTT-ratio upgrade, got %q", got)
	}
	snap := rec.Snapshot()[pubA]
	if snap.LastSwitchReason != "rtt_ratio_upgrade" {
		t.Errorf("LastSwitchReason = %q, want rtt_ratio_upgrade", snap.LastSwitchReason)
	}
	if snap.LastUpgradeRejectReason != "" {
		t.Errorf("LastUpgradeRejectReason = %q after successful upgrade, want \"\"", snap.LastUpgradeRejectReason)
	}
}

// TestReconciler_UpgradeAtRTTParityWithProductionDefault: a working
// direct path (full pong streak) whose RTT is at parity with — or even
// slightly worse than — relay must still upgrade under the production
// UpgradeRTTRatio default. Regression test for the recurring testnet
// fallback-revert timeout (issue #349): with the old default of 0.8,
// direct had to be ≥20% faster than relay, and on loaded shared-core
// VMs where scheduler latency dominates both paths (direct ≈ relay)
// the upgrade starved for minutes waiting for EWMA jitter.
func TestReconciler_UpgradeAtRTTParityWithProductionDefault(t *testing.T) {
	pubA := mkPeerKey(t)
	nm := nm1Peer(pubA, "udp4:198.51.100.10:51820")

	cfg := fastTestConfig()
	cfg.UpgradeRTTRatio = defaultUpgradeRTTRatio // pin the production default
	eng := &fakeEngine{}
	prov := &agentProvider{}
	rec := newReconciler(eng, prov, quietLogger(), nil, cfg)
	if err := rec.Apply(nm); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Force the peer onto relay first (miss streak).
	t0 := time.Now()
	for i := 0; i < rec.cfg.DowngradeMisses; i++ {
		rec.OnDiscoEvent(disco.EventProbeRoundFinalized{
			PeerNodePub: pubA, PeerDeviceID: "dev_peer_a",
			Path: pathDirect, RoundID: uint64(i + 1), AnySuccess: false, At: t0,
		})
	}
	if got := eng.lastEndpointFor(pubA); !strings.HasPrefix(got, "relay:") {
		t.Fatalf("setup: expected relay after miss-streak, got %q", got)
	}

	// Direct recovers, but at RTT parity: 110ms direct vs 100ms relay
	// (ratio 1.1 — within the prefer-direct band, far above the old
	// 0.8 gate). Pong streak fills; the upgrade must fire.
	tUp := t0.Add(2 * rec.cfg.MinDwellTime)
	for i := 0; i < rec.cfg.MinRTTSamples; i++ {
		rec.OnDiscoEvent(disco.EventProbeRTTSampled{
			PeerNodePub: pubA, PeerDeviceID: "dev_peer_a",
			Path: pathRelay, RTT: 100 * time.Millisecond, At: tUp,
		})
	}
	for i := 0; i < rec.cfg.MinRTTSamples; i++ {
		rec.OnDiscoEvent(disco.EventProbeRTTSampled{
			PeerNodePub: pubA, PeerDeviceID: "dev_peer_a",
			Path: pathDirect, RTT: 110 * time.Millisecond, At: tUp,
		})
	}
	for i := 0; i < rec.cfg.UpgradePongStreak; i++ {
		rec.OnDiscoEvent(disco.EventProbeRoundFinalized{
			PeerNodePub: pubA, PeerDeviceID: "dev_peer_a",
			Path: pathDirect, RoundID: uint64(100 + i), AnySuccess: true, At: tUp,
		})
	}
	if got := eng.lastEndpointFor(pubA); !strings.HasPrefix(got, "udp4:") {
		t.Fatalf("expected direct after parity upgrade (default ratio %v), got %q",
			defaultUpgradeRTTRatio, got)
	}
	snap := rec.Snapshot()[pubA]
	if snap.LastSwitchReason != "rtt_ratio_upgrade" {
		t.Errorf("LastSwitchReason = %q, want rtt_ratio_upgrade", snap.LastSwitchReason)
	}
}

// TestReconciler_UpgradeRejectReasonTraversesGates: starting from
// path=relay (forced via miss streak), each gate in evaluateSwitchLocked
// stamps its identifier on LastUpgradeRejectReason. Verifies the
// observability surface the testnet fallback-runner relies on for
// attribution of a stuck-on-relay state.
func TestReconciler_UpgradeRejectReasonTraversesGates(t *testing.T) {
	pubA := mkPeerKey(t)
	nm := nm1Peer(pubA, "udp4:198.51.100.10:51820")
	cfg := fastTestConfig()
	cfg.MinDwellTime = 1 * time.Microsecond
	rec := newReconciler(&fakeEngine{}, &agentProvider{}, quietLogger(), nil, cfg)
	if err := rec.Apply(nm); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	t0 := time.Now()
	for i := 0; i < rec.cfg.DowngradeMisses; i++ {
		rec.OnDiscoEvent(disco.EventProbeRoundFinalized{
			PeerNodePub: pubA, PeerDeviceID: "dev_peer_a",
			Path: pathDirect, RoundID: uint64(i + 1), AnySuccess: false, At: t0,
		})
	}
	if got := rec.Snapshot()[pubA].CurrentPath; got != pathRelay {
		t.Fatalf("setup: expected current_path=relay, got %q", got)
	}

	tUp := t0.Add(2 * rec.cfg.MinDwellTime)

	rec.OnDiscoEvent(disco.EventProbeRTTSampled{
		PeerNodePub: pubA, PeerDeviceID: "dev_peer_a",
		Path: pathRelay, RTT: 100 * time.Millisecond, At: tUp,
	})
	if got := rec.Snapshot()[pubA].LastUpgradeRejectReason; got != "samples" {
		t.Errorf("after 1 relay sample, LastUpgradeRejectReason = %q, want \"samples\"", got)
	}

	rec.OnDiscoEvent(disco.EventProbeRTTSampled{
		PeerNodePub: pubA, PeerDeviceID: "dev_peer_a",
		Path: pathRelay, RTT: 100 * time.Millisecond, At: tUp,
	})
	for i := 0; i < rec.cfg.MinRTTSamples; i++ {
		rec.OnDiscoEvent(disco.EventProbeRTTSampled{
			PeerNodePub: pubA, PeerDeviceID: "dev_peer_a",
			Path: pathDirect, RTT: 20 * time.Millisecond, At: tUp,
		})
	}
	if got := rec.Snapshot()[pubA].LastUpgradeRejectReason; got != "ring_not_full" {
		t.Errorf("with samples met but pong ring partial, LastUpgradeRejectReason = %q, want \"ring_not_full\"", got)
	}

	for i := 0; i < rec.cfg.UpgradePongStreak; i++ {
		rec.OnDiscoEvent(disco.EventProbeRoundFinalized{
			PeerNodePub: pubA, PeerDeviceID: "dev_peer_a",
			Path: pathDirect, RoundID: uint64(100 + i), AnySuccess: true, At: tUp,
		})
	}
	snap := rec.Snapshot()[pubA]
	if snap.CurrentPath != pathDirect {
		t.Fatalf("expected current_path=direct after fill, got %q (reject=%q)", snap.CurrentPath, snap.LastUpgradeRejectReason)
	}
	if snap.LastUpgradeRejectReason != "" {
		t.Errorf("after successful upgrade, LastUpgradeRejectReason = %q, want \"\"", snap.LastUpgradeRejectReason)
	}
	if got := len(snap.RecentDirectPongs); got == 0 {
		t.Errorf("RecentDirectPongs unexpectedly empty after pong stream")
	}
}

// TestReconciler_NoFlapWithinDwellTime: after a switch, the reverse
// switch is suppressed until MinDwellTime elapses.
func TestReconciler_NoFlapWithinDwellTime(t *testing.T) {
	pubA := mkPeerKey(t)
	nm := nm1Peer(pubA, "udp4:198.51.100.10:51820")

	eng := &fakeEngine{}
	prov := &agentProvider{}
	cfg := fastTestConfig()
	cfg.MinDwellTime = 200 * time.Millisecond
	rec := newReconciler(eng, prov, quietLogger(), nil, cfg)
	if err := rec.Apply(nm); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Trip downgrade by miss streak.
	now := time.Now()
	for i := 0; i < cfg.DowngradeMisses; i++ {
		rec.OnDiscoEvent(disco.EventProbeRoundFinalized{
			PeerNodePub: pubA, PeerDeviceID: "dev_peer_a",
			Path: pathDirect, RoundID: uint64(i + 1), AnySuccess: false, At: now,
		})
	}
	if got := eng.lastEndpointFor(pubA); !strings.HasPrefix(got, "relay:") {
		t.Fatalf("setup: expected relay, got %q", got)
	}

	// Immediately try to upgrade with a stack of pongs and great RTT.
	// Should NOT flip back because MinDwellTime hasn't elapsed.
	for i := 0; i < cfg.MinRTTSamples; i++ {
		rec.OnDiscoEvent(disco.EventProbeRTTSampled{
			PeerNodePub: pubA, PeerDeviceID: "dev_peer_a",
			Path: pathRelay, RTT: 100 * time.Millisecond, At: now,
		})
	}
	for i := 0; i < cfg.MinRTTSamples; i++ {
		rec.OnDiscoEvent(disco.EventProbeRTTSampled{
			PeerNodePub: pubA, PeerDeviceID: "dev_peer_a",
			Path: pathDirect, RTT: 5 * time.Millisecond, At: now,
		})
	}
	for i := 0; i < cfg.UpgradePongStreak; i++ {
		rec.OnDiscoEvent(disco.EventProbeRoundFinalized{
			PeerNodePub: pubA, PeerDeviceID: "dev_peer_a",
			Path: pathDirect, RoundID: uint64(100 + i), AnySuccess: true, At: now,
		})
	}
	if got := eng.lastEndpointFor(pubA); !strings.HasPrefix(got, "relay:") {
		t.Fatalf("dwell suppression broken: expected relay still, got %q", got)
	}

	// Wait past dwell, then a fresh sample should let the upgrade fire.
	time.Sleep(cfg.MinDwellTime + 25*time.Millisecond)
	rec.OnDiscoEvent(disco.EventProbeRTTSampled{
		PeerNodePub: pubA, PeerDeviceID: "dev_peer_a",
		Path: pathDirect, RTT: 5 * time.Millisecond, At: time.Now(),
	})
	if got := eng.lastEndpointFor(pubA); !strings.HasPrefix(got, "udp4:") {
		t.Fatalf("expected upgrade after dwell elapsed, got %q", got)
	}
}

// TestReconciler_RingDrivenOnlyByRoundFinalized verifies the
// round-aware pongRing fix: per-probe EventProbeRTTSampled /
// EventProbeMissed events no longer touch recentDirectPongs or
// directMissStreak. Multi-candidate fan-out (the disco prober emits N
// per-probe events per round when N candidates are tried) used to
// permanently lock the upgrade gate with mixed [false, false, true]
// rings; this test pins the new contract so a regression would surface
// loudly here, not as a 121s testnet-fallback-runner timeout.
func TestReconciler_RingDrivenOnlyByRoundFinalized(t *testing.T) {
	pubA := mkPeerKey(t)
	nm := nm1Peer(pubA, "udp4:198.51.100.10:51820")
	cfg := fastTestConfig()
	cfg.UpgradePongStreak = 3
	rec := newReconciler(&fakeEngine{}, &agentProvider{}, quietLogger(), nil, cfg)
	if err := rec.Apply(nm); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	now := time.Now()

	// Send many per-probe events — both successes (RTTSampled) and
	// misses (ProbeMissed). NONE of these should touch the ring or the
	// miss streak.
	for i := 0; i < 10; i++ {
		rec.OnDiscoEvent(disco.EventProbeRTTSampled{
			PeerNodePub: pubA, PeerDeviceID: "dev_peer_a",
			Path: pathDirect, RTT: 10 * time.Millisecond, At: now,
		})
		rec.OnDiscoEvent(disco.EventProbeMissed{
			PeerNodePub: pubA, PeerDeviceID: "dev_peer_a",
			Path: pathDirect, At: now,
		})
	}
	snap := rec.Snapshot()[pubA]
	if got := len(snap.RecentDirectPongs); got != 0 {
		t.Errorf("ring populated by per-probe events: len=%d entries=%v, want 0", got, snap.RecentDirectPongs)
	}
	if got := snap.DirectMissStreak; got != 0 {
		t.Errorf("miss streak bumped by per-probe events: %d, want 0", got)
	}

	// Now feed one round-finalized event per round. After 2 rounds the
	// ring should contain exactly 2 entries (1 per round, regardless of
	// how many candidates were in each).
	rec.OnDiscoEvent(disco.EventProbeRoundFinalized{
		PeerNodePub: pubA, PeerDeviceID: "dev_peer_a",
		Path: pathDirect, RoundID: 1, AnySuccess: true, At: now,
	})
	rec.OnDiscoEvent(disco.EventProbeRoundFinalized{
		PeerNodePub: pubA, PeerDeviceID: "dev_peer_a",
		Path: pathDirect, RoundID: 2, AnySuccess: false, At: now,
	})
	snap = rec.Snapshot()[pubA]
	if got, want := len(snap.RecentDirectPongs), 2; got != want {
		t.Fatalf("after 2 rounds: ring len=%d want=%d (entries=%v)", got, want, snap.RecentDirectPongs)
	}
	if snap.RecentDirectPongs[0] != true || snap.RecentDirectPongs[1] != false {
		t.Errorf("ring contents = %v, want [true false]", snap.RecentDirectPongs)
	}
	if got := snap.DirectMissStreak; got != 1 {
		t.Errorf("DirectMissStreak after miss round = %d, want 1", got)
	}

	// AnySuccess=true round resets the streak.
	rec.OnDiscoEvent(disco.EventProbeRoundFinalized{
		PeerNodePub: pubA, PeerDeviceID: "dev_peer_a",
		Path: pathDirect, RoundID: 3, AnySuccess: true, At: now,
	})
	if got := rec.Snapshot()[pubA].DirectMissStreak; got != 0 {
		t.Errorf("DirectMissStreak after success round = %d, want 0", got)
	}
}

// TestReconciler_SafetyNetSilencedByRecentDirectEvidence: when probe-
// driven IS active (RTT samples or misses arrive within FallbackAfter),
// the safety net stays out of the way even if WG hasn't handshaken.
// This prevents two-layer fighting.
func TestReconciler_SafetyNetSilencedByRecentDirectEvidence(t *testing.T) {
	pubA := mkPeerKey(t)
	nm := nm1Peer(pubA, "udp4:198.51.100.10:51820")

	eng := &fakeEngine{}
	prov := &agentProvider{}
	rec := newReconciler(eng, prov, quietLogger(), nil, fastTestConfig())
	if err := rec.Apply(nm); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Wait past FallbackAfter, but inject an RTT sample mid-way so
	// lastDirectEvidenceAt is recent. Safety net must NOT fire.
	time.Sleep(rec.cfg.FallbackAfter / 2)
	rec.OnDiscoEvent(disco.EventProbeRTTSampled{
		PeerNodePub: pubA, PeerDeviceID: "dev_peer_a",
		Path: pathDirect, RTT: 80 * time.Millisecond, At: time.Now(),
	})
	time.Sleep(rec.cfg.FallbackAfter/2 + 10*time.Millisecond)

	rec.Tick(context.Background())
	if got := eng.lastEndpointFor(pubA); !strings.HasPrefix(got, "udp4:") {
		t.Fatalf("safety net fired despite recent disco evidence: got %q", got)
	}
}

// TestReconciler_ColdStartUsesSafetyNet: at startup, before any disco
// events, the only fallback is the safety net. Verify it still fires
// after FallbackAfter when nothing else is happening.
func TestReconciler_ColdStartUsesSafetyNet(t *testing.T) {
	pubA := mkPeerKey(t)
	nm := nm1Peer(pubA, "udp4:198.51.100.10:51820")

	eng := &fakeEngine{}
	prov := &agentProvider{}
	rec := newReconciler(eng, prov, quietLogger(), nil, fastTestConfig())
	if err := rec.Apply(nm); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	time.Sleep(rec.cfg.FallbackAfter + 25*time.Millisecond)
	rec.Tick(context.Background())
	if got := eng.lastEndpointFor(pubA); !strings.HasPrefix(got, "relay:") {
		t.Fatalf("expected relay after cold-start safety net, got %q", got)
	}
	snap := rec.Snapshot()[pubA]
	if snap.LastSwitchReason != "safety_net" {
		t.Errorf("LastSwitchReason = %q, want safety_net", snap.LastSwitchReason)
	}
}

// TestPickEndpointWithHint_PrefersObservedV6 covers the v6 branch of
// the directHinted shortcut at reconcile.go:1004-1009. When disco has
// observed a v6 GUA for a peer and the reconciler is upgrading to
// direct, the engine endpoint must be "udp6:[host]:port", not v4 or
// relay.
func TestPickEndpointWithHint_PrefersObservedV6(t *testing.T) {
	peer := signer.NetworkMapPeer{
		DeviceID:      "dev_peer_b",
		HomeRelay:     "relay_1",
		NodePublicKey: "node_pub_b",
		Endpoints: []signer.EndpointCandidate{
			{Addr: "udp4:198.51.100.10:51820", Kind: signer.KindObserved},
		},
	}
	relays := []signer.NetworkMapRelay{
		{RelayID: "relay_1", URL: "wss://r.example.com/relay/v1/connect"},
	}
	observed := netip.MustParseAddrPort("[2001:db8::5]:51820")
	got := pickEndpointWithHint(peer, relays, false /*useRelay*/, true /*directHinted*/, observed)
	want := "udp6:[2001:db8::5]:51820"
	if got != want {
		t.Fatalf("pickEndpointWithHint = %q, want %q", got, want)
	}
}

// TestPickEndpointWithHint_FallsBackToV6Candidate covers the
// non-hinted, non-relay path: when observed is invalid and the peer's
// only published endpoint is "udp6:...", the reconciler must return it
// verbatim rather than skipping to a relay-shaped fallback. Regression
// guard for the iteration order at reconcile.go:1010-1023.
func TestPickEndpointWithHint_FallsBackToV6Candidate(t *testing.T) {
	peer := signer.NetworkMapPeer{
		DeviceID:      "dev_peer_c",
		HomeRelay:     "relay_1",
		NodePublicKey: "node_pub_c",
		Endpoints: []signer.EndpointCandidate{
			{Addr: "udp6:[2001:db8::5]:51820", Kind: signer.KindIPv6},
		},
	}
	relays := []signer.NetworkMapRelay{
		{RelayID: "relay_1", URL: "wss://r.example.com/relay/v1/connect"},
	}
	got := pickEndpointWithHint(peer, relays, false /*useRelay*/, false /*directHinted*/, netip.AddrPort{})
	want := "udp6:[2001:db8::5]:51820"
	if got != want {
		t.Fatalf("pickEndpointWithHint = %q, want %q", got, want)
	}
}

// TestPickEndpointWithHint_FollowsHomeRelayFlip locks in the relay
// failover contract: when a new map epoch changes peer.HomeRelay (the
// CP flipped relays[0] after the primary deregistered or went stale),
// the relay-path endpoint must move to the NEW relay's URL — that, plus
// the bind evicting the dead session (#163), is what lets agents follow
// a primary→secondary relay failover without a restart.
func TestPickEndpointWithHint_FollowsHomeRelayFlip(t *testing.T) {
	relays := []signer.NetworkMapRelay{
		{RelayID: "relay_prod_1", URL: "wss://r1.example.com/relay/v1/connect"},
		{RelayID: "relay_prod_2", URL: "wss://r2.example.com/relay/v1/connect"},
	}
	peer := signer.NetworkMapPeer{
		DeviceID:      "dev_peer_d",
		HomeRelay:     "relay_prod_1",
		NodePublicKey: "node_pub_d",
	}
	got := pickEndpointWithHint(peer, relays, true /*useRelay*/, false, netip.AddrPort{})
	if want := "relay:wss://r1.example.com/relay/v1/connect#dst=dev_peer_d&nk=node_pub_d"; got != want {
		t.Fatalf("before flip: %q, want %q", got, want)
	}
	peer.HomeRelay = "relay_prod_2"
	got = pickEndpointWithHint(peer, relays, true /*useRelay*/, false, netip.AddrPort{})
	if want := "relay:wss://r2.example.com/relay/v1/connect#dst=dev_peer_d&nk=node_pub_d"; got != want {
		t.Fatalf("after flip: %q, want %q", got, want)
	}
}

// TestOrderHostsV6First asserts the helper that places v6 literals
// (containing a colon) ahead of v4 literals while preserving relative
// order within each family. The observer's bestObs picks the first
// relay with ≥2 samples, so this ordering controls which family wins
// the per-agent observed_addr.
func TestOrderHostsV6First(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"empty", nil, nil},
		{"single v4", []string{"203.0.113.1"}, []string{"203.0.113.1"}},
		{"single v6", []string{"2001:db8::1"}, []string{"2001:db8::1"}},
		{"v4 first input -> v6 first output",
			[]string{"203.0.113.1", "2001:db8::1"},
			[]string{"2001:db8::1", "203.0.113.1"}},
		{"already v6 first stays",
			[]string{"2001:db8::1", "203.0.113.1"},
			[]string{"2001:db8::1", "203.0.113.1"}},
		{"multi families preserve intra order",
			[]string{"203.0.113.1", "2001:db8::1", "203.0.113.2", "2001:db8::2"},
			[]string{"2001:db8::1", "2001:db8::2", "203.0.113.1", "203.0.113.2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := orderHostsV6First(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("len mismatch: got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("idx %d: got %q, want %q (full got=%v)", i, got[i], tc.want[i], got)
				}
			}
		})
	}
}

// TestDiscoProbeURL asserts the synthetic wss URL wrapper used by the
// agent reconciler to feed raw DiscoHosts entries through the existing
// observer (which extracts a hostname from a URL string). v6 literals
// must be bracket-wrapped so url.Parse treats them as hostnames rather
// than authority parse errors.
func TestDiscoProbeURL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"203.0.113.1", "wss://203.0.113.1/relay/v1/disco"},
		{"2001:db8::1", "wss://[2001:db8::1]/relay/v1/disco"},
	}
	for _, tc := range cases {
		got := discoProbeURL(tc.in)
		if got != tc.want {
			t.Fatalf("discoProbeURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// recordingDisco captures the urls passed to UpdateRelays so tests can
// assert pushDiscoSnapshot's expansion of NetworkMapRelay.DiscoHosts.
// Other discoSubsystem methods are no-ops because pushDiscoSnapshot
// only writes through UpdateRelays + UpdatePeers and the latter's
// payload is checked elsewhere.
type recordingDisco struct{ urls []string }

func (r *recordingDisco) UpdateRelays(u []string) {
	r.urls = append([]string(nil), u...)
}
func (r *recordingDisco) UpdatePeers(map[string]disco.PeerSnapshot) {}
func (r *recordingDisco) SendCallMeMaybe(string, string, string, string, []netip.AddrPort) error {
	return nil
}
func (r *recordingDisco) ObservedAddr() netip.AddrPort { return netip.AddrPort{} }
func (r *recordingDisco) NATType() disco.NATType       { return disco.NATTypeUnknown }
func (r *recordingDisco) ClearHintsFor(string)         {}

// TestPushDiscoSnapshot_ExpandsDiscoHostsV6First asserts that when a
// NetworkMapRelay declares DiscoHosts containing both v4 and v6
// literals, the reconciler emits the relay URL + a synthetic v6 wss URL
// (first) + a synthetic v4 wss URL into the disco observer. The order
// matters: the observer's bestObs picks the first relay with ≥2
// samples, so putting the v6 host before the v4 host lets a dual-stack
// agent report a v6 observed_addr, which the CI verifier asserts.
func TestPushDiscoSnapshot_ExpandsDiscoHostsV6First(t *testing.T) {
	rec := &reconciler{}
	d := &recordingDisco{}
	nm := &signer.NetworkMap{
		Relays: []signer.NetworkMapRelay{
			{
				RelayID:    "relay_apne1",
				URL:        "wss://203.0.113.1:443/relay/v1/connect",
				DiscoHosts: []string{"203.0.113.1", "2001:db8::1"},
			},
		},
	}
	rec.pushDiscoSnapshot(d, nm)

	// DiscoHosts is authoritative: drop URL host to avoid the observer
	// landing on v4 first (URL host is v4 in production). v6 leads so
	// bestObs reports a v6 literal.
	want := []string{
		"wss://[2001:db8::1]/relay/v1/disco",
		"wss://203.0.113.1/relay/v1/disco",
	}
	if len(d.urls) != len(want) {
		t.Fatalf("urls len = %d, want %d: got %v", len(d.urls), len(want), d.urls)
	}
	for i, u := range want {
		if d.urls[i] != u {
			t.Fatalf("urls[%d] = %q, want %q (full: %v)", i, d.urls[i], u, d.urls)
		}
	}
}

// TestPushDiscoSnapshot_EmptyDiscoHostsBackwardCompat asserts the
// v4-only path stays unchanged: a relay without DiscoHosts produces the
// same single-URL list the observer has consumed since before the
// disco_hosts field existed.
func TestPushDiscoSnapshot_EmptyDiscoHostsBackwardCompat(t *testing.T) {
	rec := &reconciler{}
	d := &recordingDisco{}
	nm := &signer.NetworkMap{
		Relays: []signer.NetworkMapRelay{
			{RelayID: "relay_apne1", URL: "wss://203.0.113.1:443/relay/v1/connect"},
		},
	}
	rec.pushDiscoSnapshot(d, nm)
	if len(d.urls) != 1 || d.urls[0] != "wss://203.0.113.1:443/relay/v1/connect" {
		t.Fatalf("urls = %v, want single original URL", d.urls)
	}
}
