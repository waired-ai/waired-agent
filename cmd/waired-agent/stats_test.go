package main

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
)

// captureHandler is a tiny slog.Handler that appends every record's
// (level, message, attrs-as-map) to a slice. Used by stats tests to
// assert what runStatsPublisher / emitStatsRecord actually emits
// without parsing JSON or shelling out to a real handler.
type captureHandler struct {
	mu      sync.Mutex
	records []captured
}

type captured struct {
	Level slog.Level
	Msg   string
	Attrs map[string]any
}

func (h *captureHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]any)
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, captured{Level: r.Level, Msg: r.Message, Attrs: attrs})
	h.mu.Unlock()
	return nil
}

func (h *captureHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(_ string) slog.Handler      { return h }

func (h *captureHandler) snapshot() []captured {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]captured, len(h.records))
	copy(out, h.records)
	return out
}

// withCaptureLogger swaps slog.Default for the duration of the test
// and returns the capture handle plus a restore func.
func withCaptureLogger(t *testing.T) (*captureHandler, func()) {
	t.Helper()
	prev := slog.Default()
	h := &captureHandler{}
	slog.SetDefault(slog.New(h))
	return h, func() { slog.SetDefault(prev) }
}

// stubProvider lets the test feed runStatsPublisher a deterministic
// Status snapshot. Counts how many times Status() was invoked so the
// test can assert the ticker fired.
type stubProvider struct {
	st    management.Status
	calls atomic.Int64
}

func (s *stubProvider) Status() management.Status {
	s.calls.Add(1)
	return s.st
}

func TestEmitStatsRecord(t *testing.T) {
	h, restore := withCaptureLogger(t)
	defer restore()

	st := management.Status{
		NetworkID:    "net_abc",
		DeviceID:     "dev_aaa",
		DeviceName:   "agent-a1-native",
		OverlayIP:    "100.96.0.10",
		ListenPort:   41010,
		NATType:      "endpoint-independent",
		ObservedAddr: "203.0.113.5:41010",
		DiscoEnabled: true,
		PeerCount:    5,
		Phase:        "active",
		DesiredPhase: "active",
		Peers: []management.PeerStatus{
			{DeviceID: "dev_aaa", CurrentPath: "direct"},
			{DeviceID: "dev_bbb", CurrentPath: "relay"},
		},
	}
	emitStatsRecord(st, nil) // cl=nil => slog-only path (developer machine)

	// One emit now produces the INFO summary and a DEBUG record carrying
	// the peer detail (#692), so select rather than assume a count. The
	// capture handler is Enabled at every level, which a real handler at
	// the default level is not.
	var r captured
	found := false
	for _, rec := range h.snapshot() {
		if rec.Level == slog.LevelInfo && rec.Msg == "waired_agent_stats" {
			r, found = rec, true
		}
	}
	if !found {
		t.Fatalf("no INFO waired_agent_stats record in %+v", h.snapshot())
	}
	for _, key := range []string{
		"network_id", "device_id",
		"device_name", "overlay_ip", "listen_port",
		"nat_type", "observed_addr", "disco_enabled",
		"peer_count", "phase", "desired_phase",
	} {
		if _, ok := r.Attrs[key]; !ok {
			t.Errorf("missing attr %q in record", key)
		}
	}
	// The peer list is deliberately NOT here — see
	// TestEmitStatsRecord_PeerListIsDebugOnly.
	if _, ok := r.Attrs["peers"]; ok {
		t.Errorf("INFO record still carries the full peer list (#692)")
	}
	if got := r.Attrs["device_name"]; got != "agent-a1-native" {
		t.Errorf("device_name = %v, want agent-a1-native", got)
	}
	if got := r.Attrs["network_id"]; got != "net_abc" {
		t.Errorf("network_id = %v, want net_abc", got)
	}
	if got := r.Attrs["device_id"]; got != "dev_aaa" {
		t.Errorf("device_id = %v, want dev_aaa", got)
	}
	if got := r.Attrs["peer_count"]; got != int(5) && got != int64(5) {
		t.Errorf("peer_count = %v (%T), want 5", got, got)
	}
}

// PRODUCT CONTRACT (#692). The periodic INFO record must not carry the
// per-peer detail, and the detail must still be reachable at DEBUG.
//
// The record fires every 5 s by default with ~25 fields per peer, which
// measured ~890 KB/h on a two-peer host before anything else logged
// anything. On macOS — 1 MB rotation, five copies — that is roughly six
// hours of history, most of it the same status repeated, and #636 gave
// the Windows agent log the same bound.
func TestEmitStatsRecord_PeerListIsDebugOnly(t *testing.T) {
	h, restore := withCaptureLogger(t)
	defer restore()

	st := management.Status{
		DeviceID:  "dev_aaa",
		PeerCount: 2,
		Peers: []management.PeerStatus{
			{DeviceID: "dev_aaa", CurrentPath: "direct"},
			{DeviceID: "dev_bbb", CurrentPath: "relay"},
		},
	}
	emitStatsRecord(st, nil)

	recs := h.snapshot()
	var info, debug *captured
	for i := range recs {
		switch {
		case recs[i].Level == slog.LevelInfo && recs[i].Msg == "waired_agent_stats":
			info = &recs[i]
		case recs[i].Level == slog.LevelDebug && recs[i].Msg == "waired_agent_stats_peers":
			debug = &recs[i]
		}
	}
	if info == nil {
		t.Fatal("no INFO waired_agent_stats record")
	}
	if _, ok := info.Attrs["peers"]; ok {
		t.Error("INFO record carries the peer list; that is the whole cost this removes")
	}
	// peer_count stays: it is one integer, and it is what a reader
	// scanning INFO actually wants from the peer set.
	if got := info.Attrs["peer_count"]; got != int(2) && got != int64(2) {
		t.Errorf("peer_count = %v (%T), want 2", got, got)
	}
	if debug == nil {
		t.Fatal("the peer detail is gone entirely, not moved to DEBUG")
	}
	peers, ok := debug.Attrs["peers"].([]management.PeerStatus)
	if !ok {
		t.Fatalf("DEBUG peers attr type = %T, want []management.PeerStatus", debug.Attrs["peers"])
	}
	if len(peers) != 2 {
		t.Errorf("DEBUG peers len = %d, want 2", len(peers))
	}
}

// The Cloud Logging payload is deliberately untouched by #692: the
// testnet harness reads THAT back with `gcloud logging read`
// (scripts/dev/testnet-punch-verify.sh), not the local log, so trimming
// the slog record cannot cost the harness anything. If this ever fails,
// the local-log saving was taken out of the harness's side by mistake.
func TestBuildPayloadStillCarriesThePeerList(t *testing.T) {
	st := management.Status{
		DeviceID: "dev_aaa",
		Peers: []management.PeerStatus{
			{DeviceID: "dev_bbb", CurrentPath: "relay"},
		},
	}
	got := buildPayload("waired_agent_stats", st)
	peers, ok := got["peers"].([]management.PeerStatus)
	if !ok {
		t.Fatalf("payload peers type = %T, want []management.PeerStatus", got["peers"])
	}
	if len(peers) != 1 {
		t.Errorf("payload peers len = %d, want 1", len(peers))
	}
}

// A peer appearing or disappearing is what INFO keeps instead of the
// repeated full list.
func TestLogPeerSetChange(t *testing.T) {
	h, restore := withCaptureLogger(t)
	defer restore()

	two := management.Status{Peers: []management.PeerStatus{
		{DeviceID: "dev_a"}, {DeviceID: "dev_b"},
	}}
	three := management.Status{Peers: []management.PeerStatus{
		{DeviceID: "dev_a"}, {DeviceID: "dev_b"}, {DeviceID: "dev_c"},
	}}

	// First sample says nothing: nothing has changed yet, and the
	// periodic record already carries peer_count.
	set := logPeerSetChange(nil, two)
	if n := len(h.snapshot()); n != 0 {
		t.Fatalf("first sample emitted %d records, want 0", n)
	}
	// An unchanged set stays quiet — this is the case that runs all day.
	set = logPeerSetChange(set, two)
	if n := len(h.snapshot()); n != 0 {
		t.Fatalf("unchanged peer set emitted %d records, want 0", n)
	}
	// A join is reported.
	set = logPeerSetChange(set, three)
	recs := h.snapshot()
	if len(recs) != 1 || recs[0].Msg != "waired_agent_peers_changed" {
		t.Fatalf("records = %+v, want one waired_agent_peers_changed", recs)
	}
	if joined, _ := recs[0].Attrs["joined"].([]string); len(joined) != 1 || joined[0] != "dev_c" {
		t.Errorf("joined = %v, want [dev_c]", recs[0].Attrs["joined"])
	}
	// ...and so is a departure.
	logPeerSetChange(set, two)
	recs = h.snapshot()
	if len(recs) != 2 {
		t.Fatalf("want 2 records after a departure, got %d", len(recs))
	}
	if left, _ := recs[1].Attrs["left"].([]string); len(left) != 1 || left[0] != "dev_c" {
		t.Errorf("left = %v, want [dev_c]", recs[1].Attrs["left"])
	}
}

func TestBuildPayloadIncludesIdentityKeys(t *testing.T) {
	// Pin the Cloud Logging payload schema the testnet fallback
	// runner relies on (scripts/dev/testnet-fallback-runner.sh polls
	// jsonPayload.network_id and jsonPayload.device_id via
	// `gcloud logging read` to discover the 6 testnet agents'
	// identities without SSH). If this assertion ever flips, the
	// runner.sh contract needs to update in lockstep.
	st := management.Status{
		NetworkID:  "net_abc",
		DeviceID:   "dev_xyz",
		DeviceName: "agent-a1",
	}
	got := buildPayload("waired_agent_stats", st)
	if got["network_id"] != "net_abc" {
		t.Errorf("network_id = %v, want net_abc", got["network_id"])
	}
	if got["device_id"] != "dev_xyz" {
		t.Errorf("device_id = %v, want dev_xyz", got["device_id"])
	}
	if got["msg"] != "waired_agent_stats" {
		t.Errorf("msg = %v, want waired_agent_stats", got["msg"])
	}
}

func TestScenarioPayloadSchema(t *testing.T) {
	// Pin the waired_test_scenario payload the fallback runner polls
	// (scripts/dev/testnet-fallback-runner.sh poll_scenario_state reads
	// jsonPayload.state + jsonPayload.nonce). If this flips, runner.sh
	// must update in lockstep.
	got := scenarioPayload(scenarioRecord{
		state:        "applied",
		scenarioID:   "fallback-basic",
		peerDeviceID: "dev_bbb",
		nonce:        7,
	})
	if got["msg"] != "waired_test_scenario" {
		t.Errorf("msg = %v, want waired_test_scenario", got["msg"])
	}
	if got["state"] != "applied" {
		t.Errorf("state = %v, want applied", got["state"])
	}
	if got["scenario_id"] != "fallback-basic" {
		t.Errorf("scenario_id = %v, want fallback-basic", got["scenario_id"])
	}
	if got["peer_device_id"] != "dev_bbb" {
		t.Errorf("peer_device_id = %v, want dev_bbb", got["peer_device_id"])
	}
	if got["nonce"] != int64(7) {
		t.Errorf("nonce = %v (%T), want int64(7)", got["nonce"], got["nonce"])
	}
	if _, ok := got["error"]; ok {
		t.Errorf("error key present with empty errMsg; want omitted")
	}

	withErr := scenarioPayload(scenarioRecord{state: "apply_error", errMsg: "boom"})
	if withErr["error"] != "boom" {
		t.Errorf("error = %v, want boom", withErr["error"])
	}
}

func TestPublishScenarioCachesLatest(t *testing.T) {
	// publishScenario must cache the record so runStatsPublisher's tick
	// can re-emit it (#592). logger is nil here (no GCE client) so the
	// actual Cloud Logging emit is a no-op; we assert the cache only.
	cl := &cloudLogger{}

	if cl.lastScenario.Load() != nil {
		t.Fatalf("cache non-nil before any publishScenario")
	}

	cl.publishScenario("applied", "fallback-basic", "dev_bbb", 1, "")
	rec := cl.lastScenario.Load()
	if rec == nil {
		t.Fatalf("cache nil after publishScenario")
	}
	if rec.state != "applied" || rec.scenarioID != "fallback-basic" ||
		rec.peerDeviceID != "dev_bbb" || rec.nonce != 1 {
		t.Fatalf("cached record = %+v, want {applied fallback-basic dev_bbb 1}", *rec)
	}

	// Latest transition wins — a later revert must replace the cache so
	// re-emits reflect the current state, not the first one.
	cl.publishScenario("reverted", "fallback-basic", "dev_bbb", 1, "")
	if got := cl.lastScenario.Load(); got == nil || got.state != "reverted" {
		t.Fatalf("cache after revert = %+v, want state=reverted", got)
	}
}

func TestRepublishLastScenarioNilSafe(t *testing.T) {
	// republishLastScenario must be safe when there's nothing to re-emit:
	// on a nil *cloudLogger (developer machine, cl==nil), and on a live
	// cloudLogger that has not seen a scenario yet (cache nil). Neither
	// may panic; both are no-ops.
	var nilCL *cloudLogger
	nilCL.republishLastScenario() // must not panic

	cl := &cloudLogger{}
	cl.republishLastScenario() // cache nil → no-op, must not panic

	// After a publish, re-emit runs the logScenario path (logger nil →
	// still a no-op) without panicking.
	cl.publishScenario("applied", "upgrade-basic", "dev_ccc", 2, "")
	cl.republishLastScenario()
}

func TestRunStatsPublisherTicks(t *testing.T) {
	h, restore := withCaptureLogger(t)
	defer restore()

	p := &stubProvider{st: management.Status{DeviceName: "tick-agent"}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runStatsPublisher(ctx, p, 20*time.Millisecond, nil)
		close(done)
	}()

	// Wait until at least 3 ticks have fired, with a generous overall
	// timeout so a slow CI machine doesn't flake the test.
	deadline := time.Now().Add(waitBackstop)
	for p.calls.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("only %d ticks fired before deadline; want ≥ 3", p.calls.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	if got := len(h.snapshot()); got < 3 {
		t.Fatalf("emitted %d records, want ≥ 3", got)
	}
}

func TestRunStatsPublisherStopsOnCtxCancel(t *testing.T) {
	_, restore := withCaptureLogger(t)
	defer restore()

	p := &stubProvider{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runStatsPublisher(ctx, p, 10*time.Millisecond, nil)
		close(done)
	}()
	// Cancel before the first tick (interval is 10 ms; we cancel right away).
	cancel()
	select {
	case <-done:
	case <-time.After(waitBackstop):
		t.Fatalf("publisher goroutine did not exit within 2 s of ctx cancel")
	}
}

func TestRunStatsPublisherDefaultInterval(t *testing.T) {
	// Just verify the function returns promptly when ctx is already
	// cancelled — the interval normalisation ( <= 0 → 10 s ) shouldn't
	// trip the test deadline because cancellation wins over the ticker.
	_, restore := withCaptureLogger(t)
	defer restore()

	p := &stubProvider{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		runStatsPublisher(ctx, p, 0, nil) // 0 → interpreted as default
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(waitBackstop):
		t.Fatalf("publisher did not exit on already-cancelled ctx")
	}
}
