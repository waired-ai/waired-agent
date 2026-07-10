//go:build testharness

package testharness

import (
	"context"
	"log/slog"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// fakeDiscoSource implements DiscoEndpointSource with a recorded
// callback hook so tests can fire synthetic CMM events. All mutations
// happen under mu; KnownAndHintedFor returns a defensive copy.
type fakeDiscoSource struct {
	mu      sync.Mutex
	hinted  map[string][]netip.AddrPort // peerNodePub → addrs
	cleared []string
	cbs     []func(peerNodePub, peerDeviceID string)
}

func newFakeDisco() *fakeDiscoSource {
	return &fakeDiscoSource{hinted: map[string][]netip.AddrPort{}}
}

func (f *fakeDiscoSource) KnownAndHintedFor(peerNodePub string) []netip.AddrPort {
	f.mu.Lock()
	defer f.mu.Unlock()
	src := f.hinted[peerNodePub]
	if len(src) == 0 {
		return nil
	}
	out := make([]netip.AddrPort, len(src))
	copy(out, src)
	return out
}

func (f *fakeDiscoSource) ClearHintsFor(peerNodePub string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleared = append(f.cleared, peerNodePub)
	// Mirror the production behaviour: clearing drops only hints, but
	// the test fake doesn't distinguish hints from candidates — just
	// drop the bucket so subsequent KnownAndHintedFor reflects the
	// clear. Tests that need post-clear endpoints re-prime via
	// setHinted.
	delete(f.hinted, peerNodePub)
}

func (f *fakeDiscoSource) OnCallMeMaybe(fn func(peerNodePub, peerDeviceID string)) {
	if fn == nil {
		return
	}
	f.mu.Lock()
	f.cbs = append(f.cbs, fn)
	f.mu.Unlock()
}

func (f *fakeDiscoSource) setHinted(peerNodePub string, addrs []netip.AddrPort) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hinted[peerNodePub] = append([]netip.AddrPort(nil), addrs...)
}

func (f *fakeDiscoSource) clearedSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cleared...)
}

func (f *fakeDiscoSource) fireCMM(peerNodePub, peerDeviceID string) {
	f.mu.Lock()
	cbs := make([]func(string, string), len(f.cbs))
	copy(cbs, f.cbs)
	f.mu.Unlock()
	for _, cb := range cbs {
		cb(peerNodePub, peerDeviceID)
	}
}

// makeNMWithNodePub extends makeNM (in dispatcher_testharness_test.go)
// with an explicit NodePublicKey on the peer so disco lookup keys can
// match. peerEndpoints uses the same "udp4:host:port" format.
func makeNMWithNodePub(scenarioID, peerID, nodePub, direction string, nonce int64, peerEndpoints ...string) *signer.NetworkMap {
	nm := makeNM(scenarioID, peerID, direction, nonce, peerEndpoints...)
	if len(nm.Peers) > 0 {
		nm.Peers[0].NodePublicKey = nodePub
	}
	return nm
}

// waitFor polls fn every ms until it returns true or timeout elapses.
// Returns true on success, false on timeout. Helper for tests that
// observe the asynchronous worker (map-apply + CMM re-Apply).
func waitFor(t *testing.T, timeout time.Duration, fn func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// currentAppliedIPs returns a copy of the dispatcher's currently-applied
// IP set under a.mu, or nil when no scenario is active. Acquiring a.mu
// also synchronises against an in-flight worker applyMap (which holds
// a.mu for the whole op), so a non-nil result means applyLocked fully
// completed — including ClearHintsFor and the StateApplied report.
func currentAppliedIPs(d *activeDispatcher) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.current == nil {
		return nil
	}
	return append([]string(nil), d.current.appliedIPs...)
}

// TestActiveDispatcher_NilDiscoPreservesNMOnlyBehavior verifies that
// passing nil disco is identical to the pre-CMM behavior (existing
// dispatcher tests already cover this; here we just exercise the
// new signature with explicit nil).
func TestActiveDispatcher_NilDiscoPreservesNMOnlyBehavior(t *testing.T) {
	sc := &scenarioStub{id: signer.ScenarioIDFallbackBasic}
	d, _ := newDispatcherForTest(t, map[string]Scenario{
		signer.ScenarioIDFallbackBasic: sc,
	})
	nm := makeNM(signer.ScenarioIDFallbackBasic, "dev_b", signer.ScenarioDirectionBoth, 1, "udp4:1.2.3.4:51820")
	_ = d.Apply(context.Background(), nm)
	if !waitFor(t, waitTimeout, func() bool {
		return ipSetEqual(currentAppliedIPs(d), []string{"1.2.3.4"})
	}) {
		t.Errorf("appliedIPs: %v want [1.2.3.4]", currentAppliedIPs(d))
	}
}

// TestActiveDispatcher_DiscoMergesAtApplyTime verifies that the
// dispatcher's resolveWithDisco includes the peer's disco-known
// endpoints in the initial Apply call, so the scenario's iptables
// block covers them from t=0 — not only the NetworkMap snapshot.
func TestActiveDispatcher_DiscoMergesAtApplyTime(t *testing.T) {
	sc := &scenarioStub{id: signer.ScenarioIDFallbackBasic}
	fakeDisco := newFakeDisco()
	fakeDisco.setHinted("nodepub_b", []netip.AddrPort{
		netip.MustParseAddrPort("9.9.9.9:51820"),
	})

	d := newDispatcherWithDiscoForTest(t, map[string]Scenario{
		signer.ScenarioIDFallbackBasic: sc,
	}, fakeDisco)
	defer d.Stop(context.Background())

	nm := makeNMWithNodePub(signer.ScenarioIDFallbackBasic, "dev_b", "nodepub_b",
		signer.ScenarioDirectionBoth, 1, "udp4:1.2.3.4:51820")
	_ = d.Apply(context.Background(), nm)
	want := []string{"1.2.3.4", "9.9.9.9"}
	if !waitFor(t, waitTimeout, func() bool {
		return ipSetEqual(currentAppliedIPs(d), want)
	}) {
		t.Fatalf("appliedIPs after Apply: %v want %v", currentAppliedIPs(d), want)
	}
	// And the scenario received the union, not just the NM snapshot.
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if len(sc.applies) != 1 {
		t.Fatalf("scenario applies: %d want 1", len(sc.applies))
	}
	if !ipSetEqual(sc.applies[0].PeerEndpoints, want) {
		t.Errorf("scenario.PeerEndpoints: %v want %v", sc.applies[0].PeerEndpoints, want)
	}
	// Apply clears the peer's CMM hints so the prober doesn't keep
	// probing addrs the block just caught.
	if cleared := fakeDisco.clearedSnapshot(); len(cleared) != 1 || cleared[0] != "nodepub_b" {
		t.Errorf("cleared after Apply: %v want [nodepub_b]", cleared)
	}
}

// TestActiveDispatcher_CMMFiresIncrementalReapply verifies that after
// Apply, a synthetic CMM event for the active peer triggers a delta
// re-Apply that grows appliedIPs without re-Reverting.
func TestActiveDispatcher_CMMFiresIncrementalReapply(t *testing.T) {
	sc := &scenarioStub{id: signer.ScenarioIDFallbackBasic}
	fakeDisco := newFakeDisco()

	d := newDispatcherWithDiscoForTest(t, map[string]Scenario{
		signer.ScenarioIDFallbackBasic: sc,
	}, fakeDisco)
	defer d.Stop(context.Background())

	nm := makeNMWithNodePub(signer.ScenarioIDFallbackBasic, "dev_b", "nodepub_b",
		signer.ScenarioDirectionBoth, 1, "udp4:1.2.3.4:51820")
	_ = d.Apply(context.Background(), nm)
	if !waitFor(t, waitTimeout, func() bool {
		return ipSetEqual(currentAppliedIPs(d), []string{"1.2.3.4"})
	}) {
		t.Fatalf("appliedIPs after initial Apply: %v want [1.2.3.4]", currentAppliedIPs(d))
	}

	// Simulate disco learning a new endpoint, then a CMM frame.
	fakeDisco.setHinted("nodepub_b", []netip.AddrPort{
		netip.MustParseAddrPort("7.7.7.7:51820"),
	})
	fakeDisco.fireCMM("nodepub_b", "dev_b")

	// CMM re-Apply runs asynchronously on the worker; wait for it.
	ok := waitFor(t, waitTimeout, func() bool {
		return len(currentAppliedIPs(d)) == 2
	})
	if !ok {
		t.Fatalf("CMM re-Apply did not grow appliedIPs within %s: got %v", waitTimeout, currentAppliedIPs(d))
	}
	want := []string{"1.2.3.4", "7.7.7.7"}
	if got := currentAppliedIPs(d); !ipSetEqual(got, want) {
		t.Errorf("appliedIPs after CMM: %v want %v", got, want)
	}

	// scenario.Apply was called twice (initial + CMM re-Apply); no
	// Revert in between (delta re-Apply, not full Revert+Apply).
	if sc.applyCount() != 2 {
		t.Errorf("scenario apply count after CMM: %d want 2", sc.applyCount())
	}
	if sc.revertCount() != 0 {
		t.Errorf("scenario revert count after CMM: %d want 0 (delta re-Apply)", sc.revertCount())
	}
}

// TestActiveDispatcher_CMMNoNewIPsIsNoOp verifies that a CMM frame for
// the active peer that surfaces only already-applied endpoints does
// not re-call scenario.Apply.
func TestActiveDispatcher_CMMNoNewIPsIsNoOp(t *testing.T) {
	sc := &scenarioStub{id: signer.ScenarioIDFallbackBasic}
	fakeDisco := newFakeDisco()

	d := newDispatcherWithDiscoForTest(t, map[string]Scenario{
		signer.ScenarioIDFallbackBasic: sc,
	}, fakeDisco)
	defer d.Stop(context.Background())

	nm := makeNMWithNodePub(signer.ScenarioIDFallbackBasic, "dev_b", "nodepub_b",
		signer.ScenarioDirectionBoth, 1, "udp4:1.2.3.4:51820")
	_ = d.Apply(context.Background(), nm)
	if !waitFor(t, waitTimeout, func() bool { return currentAppliedIPs(d) != nil }) {
		t.Fatalf("initial Apply did not land within %s", waitTimeout)
	}
	initialApplies := sc.applyCount()

	// Hint contains only an addr already in appliedIPs.
	fakeDisco.setHinted("nodepub_b", []netip.AddrPort{
		netip.MustParseAddrPort("1.2.3.4:51820"),
	})
	fakeDisco.fireCMM("nodepub_b", "dev_b")

	// Give the drain a chance to run.
	time.Sleep(50 * time.Millisecond)

	if sc.applyCount() != initialApplies {
		t.Errorf("scenario.Apply called for no-op CMM: applies = %d want %d", sc.applyCount(), initialApplies)
	}
}

// TestActiveDispatcher_CMMForDifferentPeerIgnored verifies the
// callback short-circuit when the CMM frame targets a peer other than
// the currently active scenario's peer.
func TestActiveDispatcher_CMMForDifferentPeerIgnored(t *testing.T) {
	sc := &scenarioStub{id: signer.ScenarioIDFallbackBasic}
	fakeDisco := newFakeDisco()

	d := newDispatcherWithDiscoForTest(t, map[string]Scenario{
		signer.ScenarioIDFallbackBasic: sc,
	}, fakeDisco)
	defer d.Stop(context.Background())

	nm := makeNMWithNodePub(signer.ScenarioIDFallbackBasic, "dev_b", "nodepub_b",
		signer.ScenarioDirectionBoth, 1, "udp4:1.2.3.4:51820")
	_ = d.Apply(context.Background(), nm)
	if !waitFor(t, waitTimeout, func() bool { return currentAppliedIPs(d) != nil }) {
		t.Fatalf("initial Apply did not land within %s", waitTimeout)
	}
	initialApplies := sc.applyCount()

	// CMM for a completely different peer must not trigger re-Apply
	// even if that other peer has hinted endpoints.
	fakeDisco.setHinted("nodepub_other", []netip.AddrPort{
		netip.MustParseAddrPort("8.8.8.8:51820"),
	})
	fakeDisco.fireCMM("nodepub_other", "dev_other")
	time.Sleep(50 * time.Millisecond)
	if sc.applyCount() != initialApplies {
		t.Errorf("scenario.Apply called for wrong-peer CMM: applies = %d want %d", sc.applyCount(), initialApplies)
	}
}

// TestActiveDispatcher_StopHaltsWorker verifies that Stop() retires the
// worker goroutine and reverts the active scenario — a CMM event after
// Stop must not cause any further scenario invocations.
func TestActiveDispatcher_StopHaltsWorker(t *testing.T) {
	sc := &scenarioStub{id: signer.ScenarioIDFallbackBasic}
	fakeDisco := newFakeDisco()

	d := newDispatcherWithDiscoForTest(t, map[string]Scenario{
		signer.ScenarioIDFallbackBasic: sc,
	}, fakeDisco)

	nm := makeNMWithNodePub(signer.ScenarioIDFallbackBasic, "dev_b", "nodepub_b",
		signer.ScenarioDirectionBoth, 1, "udp4:1.2.3.4:51820")
	_ = d.Apply(context.Background(), nm)
	// Wait for the worker to apply before Stop, else Stop could retire the
	// worker before it processes the map and there'd be nothing to revert.
	if !waitFor(t, waitTimeout, func() bool { return currentAppliedIPs(d) != nil }) {
		t.Fatalf("initial Apply did not land within %s", waitTimeout)
	}

	if err := d.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if sc.revertCount() != 1 {
		t.Errorf("revert after Stop: %d want 1", sc.revertCount())
	}
	postStopApplies := sc.applyCount()

	// Late CMM frame after Stop: callback filter returns early
	// (a.current == nil), and the worker has already exited, so
	// scenario.Apply must not run again.
	fakeDisco.setHinted("nodepub_b", []netip.AddrPort{
		netip.MustParseAddrPort("9.9.9.9:51820"),
	})
	fakeDisco.fireCMM("nodepub_b", "dev_b")
	time.Sleep(50 * time.Millisecond)
	if sc.applyCount() != postStopApplies {
		t.Errorf("scenario.Apply called after Stop: applies = %d want %d", sc.applyCount(), postStopApplies)
	}

	// Second Stop is safe (stopOnce-guarded close + closed-channel reads).
	if err := d.Stop(context.Background()); err != nil {
		t.Errorf("second Stop: %v", err)
	}
}

// newDispatcherWithDiscoForTest mirrors newDispatcherForTest but wires
// a DiscoEndpointSource so the CMM hook is exercised.
func newDispatcherWithDiscoForTest(t *testing.T, reg map[string]Scenario, src DiscoEndpointSource) *activeDispatcher {
	t.Helper()
	rep := &captureReporter{}
	log := slog.New(slog.NewTextHandler(testWriter{t: t}, nil))
	d := NewActive(log, rep, "dev_self", reg, src).(*activeDispatcher)
	t.Cleanup(func() { _ = d.Stop(context.Background()) })
	return d
}
