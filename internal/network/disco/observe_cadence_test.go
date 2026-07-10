package disco

import (
	"net/netip"
	"testing"
	"time"
)

// TestDefaults_STUNObserveLearning confirms New() fills the new
// STUNObserveLearning default (10s) when the caller leaves it zero.
// Keeps the agent's learning warm-up cadence pinned to the spec without
// requiring every Config struct in the codebase to set it explicitly.
func TestDefaults_STUNObserveLearning(t *testing.T) {
	bind := newFakeBind()
	priv, pub := newNodeKey(t)
	cfg := Config{
		SelfDeviceID:    "dev_test",
		SelfNodeKeyPriv: priv,
		SelfNodeKeyPub:  pub,
		Bind:            bind,
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.cfg.STUNObserveLearning != defaultSTUNObserveLearning {
		t.Fatalf("STUNObserveLearning default = %v, want %v",
			s.cfg.STUNObserveLearning, defaultSTUNObserveLearning)
	}
	if s.cfg.STUNObserveActive != defaultSTUNObserveActive {
		t.Fatalf("STUNObserveActive default = %v, want %v",
			s.cfg.STUNObserveActive, defaultSTUNObserveActive)
	}
}

// TestDefaults_LearningClampedToActive guards against accidental
// misconfiguration that would slow the warm-up below the active cadence
// (which would defeat the point of the learning phase). New() clamps
// STUNObserveLearning to STUNObserveActive when the caller sets the
// learning interval higher.
func TestDefaults_LearningClampedToActive(t *testing.T) {
	bind := newFakeBind()
	priv, pub := newNodeKey(t)
	cfg := Config{
		SelfDeviceID:    "dev_test",
		SelfNodeKeyPriv: priv,
		SelfNodeKeyPub:  pub,
		Bind:            bind,
		// Misconfiguration: learning > active.
		STUNObserveLearning: 5 * time.Minute,
		STUNObserveActive:   30 * time.Second,
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.cfg.STUNObserveLearning != 30*time.Second {
		t.Fatalf("STUNObserveLearning = %v, want %v (clamped to active)",
			s.cfg.STUNObserveLearning, 30*time.Second)
	}
}

// TestNextObserveInterval_NoV6 asserts that the loop sleeps for
// STUNObserveLearning while v6 has never been observed
// (firstObservedV6At == zero). This is the gate that drives the
// testnet-side v6 first-observation tail: relay listen → agent v4
// observed in ~30s, but v6 trails 10+ sweeps × 60s under the active
// cadence; the learning cadence keeps sweeps at 10s through that
// window.
func TestNextObserveInterval_NoV6(t *testing.T) {
	bind := newFakeBind()
	priv, pub := newNodeKey(t)
	cfg := Config{
		SelfDeviceID:        "dev_test",
		SelfNodeKeyPriv:     priv,
		SelfNodeKeyPub:      pub,
		Bind:                bind,
		STUNObserveLearning: 7 * time.Second,
		STUNObserveActive:   63 * time.Second,
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Service starts with s.firstObservedV6At = zero time.Time.
	if got := s.nextObserveInterval(); got != 7*time.Second {
		t.Fatalf("nextObserveInterval (no v6) = %v, want 7s", got)
	}
}

// TestNextObserveInterval_V4ObservedButNoV6 is the regression test for
// PR #97's failure mode. Earlier the gate was s.observed.IsValid(),
// which flipped true as soon as any STUN response landed — typically
// v4, within seconds of relay listen. The loop then fell back to
// active (60s) cadence while still waiting for v6, paying 12 sweeps ×
// 60s = 12 min for the v6 stamp. The fixed gate keys on
// firstObservedV6At specifically: with v4 observed but no v6 yet, the
// loop stays at the learning cadence.
func TestNextObserveInterval_V4ObservedButNoV6(t *testing.T) {
	bind := newFakeBind()
	priv, pub := newNodeKey(t)
	cfg := Config{
		SelfDeviceID:        "dev_test",
		SelfNodeKeyPriv:     priv,
		SelfNodeKeyPub:      pub,
		Bind:                bind,
		STUNObserveLearning: 7 * time.Second,
		STUNObserveActive:   63 * time.Second,
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Simulate observeOnce having committed a v4 observed addr (but
	// no v6 sample has landed yet → firstObservedV6At stays zero).
	s.mu.Lock()
	s.observed = netip.MustParseAddrPort("35.243.84.118:1120")
	s.mu.Unlock()

	if got := s.nextObserveInterval(); got != 7*time.Second {
		t.Fatalf("nextObserveInterval (v4 obs, no v6) = %v, want 7s (v6-learning)", got)
	}
}

// TestNextObserveInterval_V6Observed asserts that once v6 has been
// stamped (firstObservedV6At != zero), the loop switches to the active
// cadence so we don't keep banging the relay at the warm-up rate after
// the warm-up has served its purpose.
func TestNextObserveInterval_V6Observed(t *testing.T) {
	bind := newFakeBind()
	priv, pub := newNodeKey(t)
	cfg := Config{
		SelfDeviceID:        "dev_test",
		SelfNodeKeyPriv:     priv,
		SelfNodeKeyPub:      pub,
		Bind:                bind,
		STUNObserveLearning: 7 * time.Second,
		STUNObserveActive:   63 * time.Second,
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Simulate observeOnce having stamped firstObservedV6At after a
	// v6 sample landed. The v6 sample itself goes into lastObservedV6
	// (not exercised by this test).
	s.mu.Lock()
	s.firstObservedV6At = time.Now()
	s.mu.Unlock()

	if got := s.nextObserveInterval(); got != 63*time.Second {
		t.Fatalf("nextObserveInterval (v6 obs) = %v, want 63s", got)
	}
}

// TestUpdateRelays_WakesObserveLoopEagerly verifies the eager first-probe:
// when UpdateRelays learns a new relay set, runObserveLoop sweeps immediately
// rather than sleeping out the observe interval. The cadence here is 30s in
// both phases, so the only way a STUN probe can fire within the test window
// is the wake. An UpdateRelays with the *same* set must not re-trigger; a
// changed set must. This guards the testnet "Gate-1" latency removal without
// changing what is probed (same relays, same STUN round-trip).
func TestUpdateRelays_WakesObserveLoopEagerly(t *testing.T) {
	bind := newFakeBind()
	priv, pub := newNodeKey(t)
	s, err := New(Config{
		SelfDeviceID:        "dev_test",
		SelfNodeKeyPriv:     priv,
		SelfNodeKeyPub:      pub,
		RelaySharedSecret:   []byte("relay-shared-secret"),
		Bind:                bind,
		STUNObserveLearning: 30 * time.Second,
		STUNObserveActive:   30 * time.Second,
		STUNTimeout:         200 * time.Millisecond,
		// One disco port → exactly one probe per sweep, so draining
		// between sub-assertions is deterministic (no second-port tail).
		DiscoUDPPorts: []int{3478},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	go s.Run(t.Context())

	drain := func() {
		for {
			select {
			case <-bind.sent:
			default:
				return
			}
		}
	}

	// Pass the 500ms warm-up + first (empty-relay) sweep; the loop is now
	// asleep on the 30s wait with no relays known, so nothing was probed.
	time.Sleep(1 * time.Second)
	select {
	case p := <-bind.sent:
		t.Fatalf("unexpected STUN probe before any relay learned: %q", p.Dst)
	default:
	}

	// Learn a relay → eager wake → a probe within seconds (not 30s).
	s.UpdateRelays([]string{"wss://relay.example.com:443/relay/v1/connect"})
	select {
	case <-bind.sent:
	case <-time.After(3 * time.Second):
		t.Fatal("UpdateRelays did not wake the observe loop within 3s (cadence 30s): eager first-probe missing")
	}

	// Same set again → no spurious wake (change-detected), so no new probe.
	drain()
	s.UpdateRelays([]string{"wss://relay.example.com:443/relay/v1/connect"})
	select {
	case <-bind.sent:
		t.Fatal("unchanged UpdateRelays should not re-trigger a sweep")
	case <-time.After(500 * time.Millisecond):
	}

	// Changed set → wake again.
	s.UpdateRelays([]string{"wss://relay2.example.com:443/relay/v1/connect"})
	select {
	case <-bind.sent:
	case <-time.After(3 * time.Second):
		t.Fatal("changed UpdateRelays did not wake the observe loop")
	}
}
