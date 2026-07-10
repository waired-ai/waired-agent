//go:build testharness

package testharness

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// waitTimeout bounds the wait-based assertions. Apply is now async (the
// worker performs the actual state transition), so tests poll the
// reporter / current state rather than reading immediately after Apply.
// scenarioStub ops are instant, so worker turnaround is microseconds —
// this budget is ~400x the expected latency.
const waitTimeout = 2 * time.Second

// waitForRecords polls rep.snapshot() until pred is satisfied or the
// deadline elapses (t.Fatalf on timeout). Returns the satisfying
// snapshot. Replaces the pre-async pattern of reading snapshot()
// immediately after a synchronous Apply.
func waitForRecords(t *testing.T, rep *captureReporter, pred func([]reporterRecord) bool, timeout time.Duration) []reporterRecord {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		rs := rep.snapshot()
		if pred(rs) {
			return rs
		}
		if time.Now().After(deadline) {
			t.Fatalf("waitForRecords timed out after %s; last snapshot: %+v", timeout, rs)
			return rs
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// scenarioStub records each Apply / Revert call so tests can verify
// the dispatcher state machine without poking at activeDispatcher's
// unexported `current` field.
type scenarioStub struct {
	id        string
	mu        sync.Mutex
	applies   []ScenarioParams
	reverts   int
	applyErr  error
	revertErr error
}

func (s *scenarioStub) ID() string { return s.id }

func (s *scenarioStub) Apply(_ context.Context, p ScenarioParams) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applies = append(s.applies, p)
	return s.applyErr
}

func (s *scenarioStub) Revert(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reverts++
	return s.revertErr
}

func (s *scenarioStub) applyCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.applies)
}

func (s *scenarioStub) revertCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reverts
}

type captureReporter struct {
	mu      sync.Mutex
	records []reporterRecord
}

type reporterRecord struct {
	state, sid, peer, errMsg string
	nonce                    int64
}

func (c *captureReporter) ReportScenario(state, sid, peer string, nonce int64, errMsg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, reporterRecord{state, sid, peer, errMsg, nonce})
}

func (c *captureReporter) snapshot() []reporterRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]reporterRecord, len(c.records))
	copy(out, c.records)
	return out
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func newDispatcherForTest(t *testing.T, reg map[string]Scenario) (*activeDispatcher, *captureReporter) {
	t.Helper()
	rep := &captureReporter{}
	log := slog.New(slog.NewTextHandler(testWriter{t: t}, nil))
	d := NewActive(log, rep, "dev_self", reg, nil).(*activeDispatcher)
	t.Cleanup(func() { _ = d.Stop(context.Background()) })
	return d, rep
}

func makeNM(scenarioID, peerID, direction string, nonce int64, peerEndpoints ...string) *signer.NetworkMap {
	nm := &signer.NetworkMap{
		Self:  signer.NetworkMapPeer{DeviceID: "dev_self"},
		Peers: []signer.NetworkMapPeer{{DeviceID: peerID}},
	}
	for _, ep := range peerEndpoints {
		nm.Peers[0].Endpoints = append(nm.Peers[0].Endpoints, signer.EndpointCandidate{
			Addr: ep, Kind: signer.KindLocal, Priority: 0,
		})
	}
	if scenarioID != "" {
		nm.ActiveTestScenario = &signer.ActiveTestScenario{
			ScenarioID:    scenarioID,
			PeerDeviceID:  peerID,
			Direction:     direction,
			ExpectedNonce: nonce,
		}
	}
	return nm
}

func TestActiveDispatcher_NoScenarioNoOp(t *testing.T) {
	sc := &scenarioStub{id: signer.ScenarioIDFallbackBasic}
	d, rep := newDispatcherForTest(t, map[string]Scenario{
		signer.ScenarioIDFallbackBasic: sc,
	})
	nm := makeNM("", "", "", 0)
	_ = d.Apply(context.Background(), nm)
	// Stop waits for workerDone, so the coalesced nil map has been fully
	// processed by the time Stop returns — deterministic without a sleep.
	if err := d.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if sc.applyCount() != 0 {
		t.Errorf("apply count: got %d want 0", sc.applyCount())
	}
	if got := len(rep.snapshot()); got != 0 {
		t.Errorf("reporter records: got %d want 0", got)
	}
}

func TestActiveDispatcher_ApplyOnce(t *testing.T) {
	sc := &scenarioStub{id: signer.ScenarioIDFallbackBasic}
	d, rep := newDispatcherForTest(t, map[string]Scenario{
		signer.ScenarioIDFallbackBasic: sc,
	})
	nm := makeNM(signer.ScenarioIDFallbackBasic, "dev_b", signer.ScenarioDirectionBoth, 1, "udp4:1.2.3.4:51820")
	_ = d.Apply(context.Background(), nm)
	waitForRecords(t, rep, func(rs []reporterRecord) bool {
		return len(rs) == 1 && rs[0].state == StateApplied
	}, waitTimeout)
	if sc.applyCount() != 1 {
		t.Fatalf("apply count: got %d want 1", sc.applyCount())
	}
	// current is set under a.mu before the StateApplied report; acquiring
	// a.mu here also waits for applyMap to fully return.
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.current == nil || d.current.peer != "dev_b" || d.current.nonce != 1 {
		t.Errorf("current: %+v", d.current)
	}
	if !ipSetEqual(d.current.appliedIPs, []string{"1.2.3.4"}) {
		t.Errorf("appliedIPs: %v", d.current.appliedIPs)
	}
}

func TestActiveDispatcher_SameNonceAndIPSetIsNoOp(t *testing.T) {
	sc := &scenarioStub{id: signer.ScenarioIDFallbackBasic}
	d, rep := newDispatcherForTest(t, map[string]Scenario{
		signer.ScenarioIDFallbackBasic: sc,
	})
	nm := makeNM(signer.ScenarioIDFallbackBasic, "dev_b", signer.ScenarioDirectionBoth, 1, "udp4:1.2.3.4:51820")
	_ = d.Apply(context.Background(), nm)
	_ = d.Apply(context.Background(), nm)
	waitForRecords(t, rep, func(rs []reporterRecord) bool {
		return len(rs) == 1 && rs[0].state == StateApplied
	}, waitTimeout)
	// Whether the two Applies coalesce or run twice, the second is a same
	// nonce+IP-set no-op: applyCount can never exceed 1 and revertCount
	// stays 0. Settle to let any second applyMap run and confirm so.
	time.Sleep(50 * time.Millisecond)
	if sc.applyCount() != 1 {
		t.Errorf("apply count: got %d want 1 (idempotent)", sc.applyCount())
	}
	if sc.revertCount() != 0 {
		t.Errorf("revert count: got %d want 0 (idempotent)", sc.revertCount())
	}
}

func TestActiveDispatcher_NonceChangeTriggersRevertAndReapply(t *testing.T) {
	sc := &scenarioStub{id: signer.ScenarioIDFallbackBasic}
	d, rep := newDispatcherForTest(t, map[string]Scenario{
		signer.ScenarioIDFallbackBasic: sc,
	})
	nm1 := makeNM(signer.ScenarioIDFallbackBasic, "dev_b", signer.ScenarioDirectionBoth, 1, "udp4:1.2.3.4:51820")
	nm2 := makeNM(signer.ScenarioIDFallbackBasic, "dev_b", signer.ScenarioDirectionBoth, 2, "udp4:1.2.3.4:51820")
	_ = d.Apply(context.Background(), nm1)
	// Wait for nm1 to be applied before issuing nm2: rapid back-to-back
	// Applies coalesce (latest-wins), which would skip the intermediate
	// transition this test asserts.
	waitForRecords(t, rep, func(rs []reporterRecord) bool {
		return len(rs) == 1 && rs[0].state == StateApplied
	}, waitTimeout)
	_ = d.Apply(context.Background(), nm2)
	rs := waitForRecords(t, rep, func(rs []reporterRecord) bool {
		return len(rs) == 3
	}, waitTimeout)
	if sc.applyCount() != 2 || sc.revertCount() != 1 {
		t.Errorf("counts: apply=%d revert=%d want apply=2 revert=1", sc.applyCount(), sc.revertCount())
	}
	if rs[0].state != StateApplied || rs[1].state != StateReverted || rs[2].state != StateApplied {
		t.Errorf("state seq: %+v", rs)
	}
}

func TestActiveDispatcher_IPSetGrowsTriggersReapply(t *testing.T) {
	sc := &scenarioStub{id: signer.ScenarioIDFallbackBasic}
	d, rep := newDispatcherForTest(t, map[string]Scenario{
		signer.ScenarioIDFallbackBasic: sc,
	})
	nm1 := makeNM(signer.ScenarioIDFallbackBasic, "dev_b", signer.ScenarioDirectionBoth, 1, "udp4:1.2.3.4:51820")
	nm2 := makeNM(signer.ScenarioIDFallbackBasic, "dev_b", signer.ScenarioDirectionBoth, 1,
		"udp4:1.2.3.4:51820", "udp4:5.6.7.8:51820")
	_ = d.Apply(context.Background(), nm1)
	waitForRecords(t, rep, func(rs []reporterRecord) bool {
		return len(rs) == 1 && rs[0].state == StateApplied
	}, waitTimeout)
	_ = d.Apply(context.Background(), nm2)
	// Same nonce, grown IP set → revert + reapply (applied/reverted/applied).
	waitForRecords(t, rep, func(rs []reporterRecord) bool {
		return len(rs) == 3
	}, waitTimeout)
	if sc.applyCount() != 2 || sc.revertCount() != 1 {
		t.Errorf("counts: apply=%d revert=%d want apply=2 revert=1 (IP set grew at same nonce)",
			sc.applyCount(), sc.revertCount())
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !ipSetEqual(d.current.appliedIPs, []string{"1.2.3.4", "5.6.7.8"}) {
		t.Errorf("appliedIPs after grow: %v", d.current.appliedIPs)
	}
}

func TestActiveDispatcher_ScenarioGoesNilTriggersRevert(t *testing.T) {
	sc := &scenarioStub{id: signer.ScenarioIDFallbackBasic}
	d, rep := newDispatcherForTest(t, map[string]Scenario{
		signer.ScenarioIDFallbackBasic: sc,
	})
	nm1 := makeNM(signer.ScenarioIDFallbackBasic, "dev_b", signer.ScenarioDirectionBoth, 1, "udp4:1.2.3.4:51820")
	nm2 := makeNM("", "", "", 0)
	_ = d.Apply(context.Background(), nm1)
	waitForRecords(t, rep, func(rs []reporterRecord) bool {
		return len(rs) == 1 && rs[0].state == StateApplied
	}, waitTimeout)
	_ = d.Apply(context.Background(), nm2)
	waitForRecords(t, rep, func(rs []reporterRecord) bool {
		return len(rs) == 2 && rs[1].state == StateReverted
	}, waitTimeout)
	if sc.revertCount() != 1 {
		t.Errorf("revert count: got %d want 1", sc.revertCount())
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.current != nil {
		t.Errorf("current should be nil after revert: %+v", d.current)
	}
}

func TestActiveDispatcher_StopRevertsCurrent(t *testing.T) {
	sc := &scenarioStub{id: signer.ScenarioIDFallbackBasic}
	d, rep := newDispatcherForTest(t, map[string]Scenario{
		signer.ScenarioIDFallbackBasic: sc,
	})
	nm := makeNM(signer.ScenarioIDFallbackBasic, "dev_b", signer.ScenarioDirectionBoth, 1, "udp4:1.2.3.4:51820")
	_ = d.Apply(context.Background(), nm)
	// Ensure the scenario is applied (current set) before Stop, else Stop
	// could retire the worker before it processes the map → nothing to revert.
	waitForRecords(t, rep, func(rs []reporterRecord) bool {
		return len(rs) == 1 && rs[0].state == StateApplied
	}, waitTimeout)
	if err := d.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if sc.revertCount() != 1 {
		t.Errorf("revert count: got %d want 1", sc.revertCount())
	}
	// Stop again — should be a no-op now.
	if err := d.Stop(context.Background()); err != nil {
		t.Fatalf("Stop (no current): %v", err)
	}
	if sc.revertCount() != 1 {
		t.Errorf("revert count after second Stop: got %d want 1", sc.revertCount())
	}
}

func TestActiveDispatcher_UnknownScenarioReportsAndNoOp(t *testing.T) {
	d, rep := newDispatcherForTest(t, map[string]Scenario{})
	nm := makeNM("never-registered", "dev_b", signer.ScenarioDirectionBoth, 1, "udp4:1.2.3.4:51820")
	_ = d.Apply(context.Background(), nm)
	waitForRecords(t, rep, func(rs []reporterRecord) bool {
		return len(rs) == 1 && rs[0].state == StateUnknownScenario
	}, waitTimeout)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.current != nil {
		t.Errorf("current should remain nil for unknown scenario")
	}
}

func TestActiveDispatcher_ApplyErrorReportsAndDoesNotSetCurrent(t *testing.T) {
	sc := &scenarioStub{id: signer.ScenarioIDFallbackBasic, applyErr: errors.New("boom")}
	d, rep := newDispatcherForTest(t, map[string]Scenario{
		signer.ScenarioIDFallbackBasic: sc,
	})
	nm := makeNM(signer.ScenarioIDFallbackBasic, "dev_b", signer.ScenarioDirectionBoth, 1, "udp4:1.2.3.4:51820")
	// Apply is async and never returns the scenario error — the failure
	// surfaces as a StateApplyError reporter record (the runner.sh CI
	// contract), carrying the underlying error message.
	_ = d.Apply(context.Background(), nm)
	rs := waitForRecords(t, rep, func(rs []reporterRecord) bool {
		return len(rs) == 1 && rs[0].state == StateApplyError
	}, waitTimeout)
	if !strings.Contains(rs[0].errMsg, "boom") {
		t.Errorf("errMsg: %q want substring %q", rs[0].errMsg, "boom")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.current != nil {
		t.Errorf("current should be nil after apply error")
	}
}

func TestActiveDispatcher_RevertErrorOnSwitchKeepsApplyingNew(t *testing.T) {
	scA := &scenarioStub{id: signer.ScenarioIDFallbackBasic, revertErr: errors.New("revert-fail")}
	scB := &scenarioStub{id: signer.ScenarioIDAsymmetricDirect}
	d, rep := newDispatcherForTest(t, map[string]Scenario{
		signer.ScenarioIDFallbackBasic:    scA,
		signer.ScenarioIDAsymmetricDirect: scB,
	})
	nm1 := makeNM(signer.ScenarioIDFallbackBasic, "dev_b", signer.ScenarioDirectionBoth, 1, "udp4:1.2.3.4:51820")
	nm2 := makeNM(signer.ScenarioIDAsymmetricDirect, "dev_c", signer.ScenarioDirectionOutbound, 1, "udp4:5.6.7.8:51820")
	_ = d.Apply(context.Background(), nm1)
	waitForRecords(t, rep, func(rs []reporterRecord) bool {
		return len(rs) == 1 && rs[0].state == StateApplied
	}, waitTimeout)
	_ = d.Apply(context.Background(), nm2)
	rs := waitForRecords(t, rep, func(rs []reporterRecord) bool {
		return len(rs) == 3
	}, waitTimeout)
	// New scenario is still applied even though previous one's revert errored.
	if scA.revertCount() != 1 {
		t.Errorf("scA revert count: %d want 1", scA.revertCount())
	}
	if scB.applyCount() != 1 {
		t.Errorf("scB apply count: %d want 1", scB.applyCount())
	}
	d.mu.Lock()
	if d.current == nil || d.current.scenario.ID() != signer.ScenarioIDAsymmetricDirect {
		t.Errorf("current: %+v", d.current)
	}
	d.mu.Unlock()
	// Reporter sequence: applied(A), revert_error(A), applied(B).
	states := []string{}
	for _, r := range rs {
		states = append(states, r.state)
	}
	want := []string{StateApplied, StateRevertError, StateApplied}
	if !equalStrings(states, want) {
		t.Errorf("state seq: got %v want %v", states, want)
	}
}

// TestActiveDispatcher_CoalescesRapidAppliesToLatest documents the
// latest-wins behaviour: two Applies issued back-to-back (no wait
// between) converge on the most recent scenario. The first may or may
// not be applied depending on whether the worker drained the first
// mapTrigger token before the second store — so we assert only the
// convergent end-state plus the invariant that any apply of A was
// matched by a revert of A.
func TestActiveDispatcher_CoalescesRapidAppliesToLatest(t *testing.T) {
	scA := &scenarioStub{id: signer.ScenarioIDFallbackBasic}
	scB := &scenarioStub{id: signer.ScenarioIDAsymmetricDirect}
	d, _ := newDispatcherForTest(t, map[string]Scenario{
		signer.ScenarioIDFallbackBasic:    scA,
		signer.ScenarioIDAsymmetricDirect: scB,
	})
	nmA := makeNM(signer.ScenarioIDFallbackBasic, "dev_b", signer.ScenarioDirectionBoth, 1, "udp4:1.2.3.4:51820")
	nmB := makeNM(signer.ScenarioIDAsymmetricDirect, "dev_c", signer.ScenarioDirectionOutbound, 1, "udp4:5.6.7.8:51820")
	_ = d.Apply(context.Background(), nmA)
	_ = d.Apply(context.Background(), nmB)
	ok := waitFor(t, waitTimeout, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.current != nil && d.current.scenario.ID() == signer.ScenarioIDAsymmetricDirect
	})
	if !ok {
		t.Fatalf("did not converge on scenario B within %s", waitTimeout)
	}
	// Latest wins: B applied exactly once, never reverted.
	if scB.applyCount() != 1 {
		t.Errorf("scB applies=%d want 1", scB.applyCount())
	}
	if scB.revertCount() != 0 {
		t.Errorf("scB reverts=%d want 0", scB.revertCount())
	}
	// A was either coalesced away (0 applies) or applied-then-reverted.
	if scA.applyCount() > 0 && scA.revertCount() != scA.applyCount() {
		t.Errorf("scA applied %d but reverted %d (apply/revert must pair)", scA.applyCount(), scA.revertCount())
	}
}

func TestResolvePeer_BasicAndDedup(t *testing.T) {
	nm := makeNM(signer.ScenarioIDFallbackBasic, "dev_b", signer.ScenarioDirectionBoth, 1,
		"udp4:1.2.3.4:51820",
		"udp4:5.6.7.8:51820",
		"udp4:1.2.3.4:51821",
		"udp6:[2001:db8::1]:51820",
		"relay:https://x/relay#dst=foo",
	)
	p := resolvePeer(nm, nm.ActiveTestScenario)
	want := []string{"1.2.3.4", "2001:db8::1", "5.6.7.8"}
	if !ipSetEqual(p.PeerEndpoints, want) {
		t.Errorf("PeerEndpoints: got %v want %v", p.PeerEndpoints, want)
	}
	if p.Direction != signer.ScenarioDirectionBoth || p.Nonce != 1 {
		t.Errorf("Direction/Nonce: %+v", p)
	}
}

func TestResolvePeer_PeerNotFound_ReturnsBaseOnly(t *testing.T) {
	nm := &signer.NetworkMap{
		Self:  signer.NetworkMapPeer{DeviceID: "dev_self"},
		Peers: []signer.NetworkMapPeer{{DeviceID: "dev_c"}},
	}
	want := &signer.ActiveTestScenario{
		ScenarioID:    signer.ScenarioIDFallbackBasic,
		PeerDeviceID:  "dev_b",
		Direction:     signer.ScenarioDirectionBoth,
		ExpectedNonce: 7,
	}
	p := resolvePeer(nm, want)
	if p.PeerEndpoints != nil {
		t.Errorf("PeerEndpoints should be nil for unknown peer: %v", p.PeerEndpoints)
	}
	if p.PeerDeviceID != "dev_b" || p.Nonce != 7 {
		t.Errorf("PeerDeviceID/Nonce: %+v", p)
	}
}

func TestParseHostFromAddr(t *testing.T) {
	cases := []struct{ in, want string }{
		{"udp4:1.2.3.4:51820", "1.2.3.4"},
		{"udp6:[2001:db8::1]:51820", "2001:db8::1"},
		{"relay:https://x/relay#dst=foo", ""},
		{"", ""},
		{"udp4:bogus", ""},
	}
	for _, c := range cases {
		got := parseHostFromAddr(c.in)
		if got != c.want {
			t.Errorf("parseHostFromAddr(%q) = %q want %q", c.in, got, c.want)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
