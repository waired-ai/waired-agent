package disco

import (
	"testing"
	"time"
)

// TestReachableSnapshot_EmptyWhenNoSamples confirms that a Service
// which has never received a pong returns nil. Phase 8 wires this
// into the Selector's LocalReachable input where nil/empty means
// "no exclusions" — a freshly started agent must not accidentally
// exclude every mesh peer.
func TestReachableSnapshot_EmptyWhenNoSamples(t *testing.T) {
	s := &Service{lastPongAt: map[string]time.Time{}}
	if got := s.ReachableSnapshot(time.Now(), 5*time.Second); got != nil {
		t.Errorf("ReachableSnapshot() on fresh Service = %+v, want nil", got)
	}
}

// TestReachableSnapshot_FreshIsTrue confirms peers stamped within the
// freshness window appear as true. That's the positive Selector
// signal — "we've validated bidirectional reachability recently".
func TestReachableSnapshot_FreshIsTrue(t *testing.T) {
	now := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	s := &Service{lastPongAt: map[string]time.Time{
		"peer-a": now.Add(-1 * time.Second),
		"peer-b": now.Add(-4 * time.Second),
	}}
	got := s.ReachableSnapshot(now, 5*time.Second)
	if got["peer-a"] != true {
		t.Errorf("peer-a (1 s old) = %v, want true", got["peer-a"])
	}
	if got["peer-b"] != true {
		t.Errorf("peer-b (4 s old, just inside window) = %v, want true", got["peer-b"])
	}
}

// TestReachableSnapshot_StaleIsFalse confirms peers whose last pong is
// older than the freshness window appear as false. This is the hard-
// exclusion signal the Phase 8 Selector uses to drop NAT-asymmetry /
// WG-keepalive-failed peers before they consume a probe slot.
func TestReachableSnapshot_StaleIsFalse(t *testing.T) {
	now := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	s := &Service{lastPongAt: map[string]time.Time{
		"peer-stale": now.Add(-10 * time.Second),
	}}
	got := s.ReachableSnapshot(now, 5*time.Second)
	if _, present := got["peer-stale"]; !present {
		t.Fatal("peer-stale missing from snapshot; once-observed peers must surface as explicit false, not absent")
	}
	if got["peer-stale"] != false {
		t.Errorf("peer-stale (10 s old, freshness=5s) = %v, want false", got["peer-stale"])
	}
}

// TestReachableSnapshot_MixedFreshAndStale exercises the common case
// of a Selector reading a snapshot during steady-state: some peers
// just pong'd, others haven't in a while. Both must be representable
// in one snapshot — the Selector's hard-exclusion check distinguishes
// them by the bool value.
func TestReachableSnapshot_MixedFreshAndStale(t *testing.T) {
	now := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	s := &Service{lastPongAt: map[string]time.Time{
		"peer-fresh-1": now.Add(-500 * time.Millisecond),
		"peer-fresh-2": now.Add(-2 * time.Second),
		"peer-stale-1": now.Add(-10 * time.Second),
		"peer-stale-2": now.Add(-1 * time.Minute),
	}}
	got := s.ReachableSnapshot(now, 5*time.Second)
	want := map[string]bool{
		"peer-fresh-1": true,
		"peer-fresh-2": true,
		"peer-stale-1": false,
		"peer-stale-2": false,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("ReachableSnapshot()[%q] = %v, want %v", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("ReachableSnapshot() len = %d, want %d (got=%+v)", len(got), len(want), got)
	}
}

// TestReachableSnapshot_NeverObservedPeerIsAbsent confirms that a peer
// the Service has never received a pong from does NOT appear in the
// snapshot. That matters because the Phase 8 Selector treats absence
// as "no signal, default trust" — excluding fresh-enrolled peers
// before the first probe round completes would break the system.
func TestReachableSnapshot_NeverObservedPeerIsAbsent(t *testing.T) {
	now := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	s := &Service{lastPongAt: map[string]time.Time{
		"peer-known": now.Add(-1 * time.Second),
	}}
	got := s.ReachableSnapshot(now, 5*time.Second)
	if _, present := got["peer-unknown"]; present {
		t.Errorf("never-observed peer surfaced in snapshot: %+v", got)
	}
	if got["peer-known"] != true {
		t.Errorf("peer-known sanity check failed: got %v", got["peer-known"])
	}
}

// TestReachableSnapshot_ExactBoundaryIsFresh confirms the freshness
// threshold is inclusive on the fresh side (last_pong == now-freshness
// → true). This nail-on-the-boundary case decides whether a 5 s probe
// cadence with 5 s freshness flickers every cycle.
func TestReachableSnapshot_ExactBoundaryIsFresh(t *testing.T) {
	now := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	s := &Service{lastPongAt: map[string]time.Time{
		"peer-boundary": now.Add(-5 * time.Second),
	}}
	got := s.ReachableSnapshot(now, 5*time.Second)
	if got["peer-boundary"] != true {
		t.Errorf("peer-boundary at exact freshness boundary = %v, want true (inclusive)", got["peer-boundary"])
	}
}

// TestReachableSnapshot_ZeroFreshnessRejectsEverything is a degenerate
// case: passing freshness=0 means "no peer is fresh, ever". Confirms
// the method doesn't accidentally treat zero as "no filter".
func TestReachableSnapshot_ZeroFreshnessRejectsEverything(t *testing.T) {
	now := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	s := &Service{lastPongAt: map[string]time.Time{
		"peer-a": now,
		"peer-b": now.Add(-1 * time.Nanosecond),
	}}
	got := s.ReachableSnapshot(now, 0)
	if got["peer-a"] != true {
		t.Errorf("peer-a at exact now should still satisfy freshness=0 boundary: got %v", got["peer-a"])
	}
	if got["peer-b"] != false {
		t.Errorf("peer-b 1 ns before now with freshness=0 = %v, want false", got["peer-b"])
	}
}

// TestRecordPongReceived_StampsLastPongAt confirms the helper writes
// the timestamp under the same mu the rttEMA update uses. Exercised
// directly since handlePong's full path needs frame/signature
// machinery that's covered in service_test.go.
func TestRecordPongReceived_StampsLastPongAt(t *testing.T) {
	now := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	s := &Service{lastPongAt: map[string]time.Time{}, now: func() time.Time { return now }}
	s.recordPongReceived("peer-a")
	if got := s.lastPongAt["peer-a"]; !got.Equal(now) {
		t.Errorf("recordPongReceived didn't stamp lastPongAt: peer-a = %v, want %v", got, now)
	}
}

// TestRecordPongReceived_EmptyDeviceIDNoOp guards against signed pongs
// that decode with an empty SrcDeviceID — the existing handlePong
// short-circuits earlier in that case, but the helper should be
// defensive in case future code paths change.
func TestRecordPongReceived_EmptyDeviceIDNoOp(t *testing.T) {
	s := &Service{lastPongAt: map[string]time.Time{}, now: time.Now}
	s.recordPongReceived("")
	if _, ok := s.lastPongAt[""]; ok {
		t.Error("empty deviceID created a lastPongAt entry; should be no-op")
	}
}
