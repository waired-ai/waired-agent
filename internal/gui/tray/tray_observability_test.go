package tray

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/management/observabilityclient"
	"github.com/waired-ai/waired-agent/internal/observability"
)

func TestTrimRecentFallbacks_DropsStaleEntries(t *testing.T) {
	now := time.Date(2026, 5, 16, 10, 0, 0, 0, time.UTC)
	buf := []FallbackEntry{
		{TS: now.Add(-3 * time.Hour), From: "old"}, // way past 2× window
		{TS: now.Add(-15 * time.Minute), From: "mid"},
		{TS: now.Add(-1 * time.Minute), From: "fresh"},
	}
	got := trimRecentFallbacks(buf, now)
	if len(got) != 2 {
		t.Fatalf("trim len=%d, want 2: %+v", len(got), got)
	}
	if got[0].From != "mid" || got[1].From != "fresh" {
		t.Errorf("trim preserved wrong entries: %+v", got)
	}
}

func TestTrimRecentFallbacks_CapsAt64(t *testing.T) {
	now := time.Date(2026, 5, 16, 10, 0, 0, 0, time.UTC)
	buf := make([]FallbackEntry, 100)
	for i := range buf {
		buf[i] = FallbackEntry{
			TS:   now.Add(-time.Duration(i) * time.Second),
			From: strconv.Itoa(i),
		}
	}
	got := trimRecentFallbacks(buf, now)
	if len(got) != 64 {
		t.Errorf("cap=%d, want 64", len(got))
	}
}

func TestReverseFallbacks(t *testing.T) {
	buf := []FallbackEntry{{From: "a"}, {From: "b"}, {From: "c"}}
	got := reverseFallbacks(buf)
	if len(got) != 3 || got[0].From != "c" || got[2].From != "a" {
		t.Errorf("reverse wrong: %+v", got)
	}
	// Source must not have mutated.
	if buf[0].From != "a" || buf[2].From != "c" {
		t.Errorf("reverse mutated source: %+v", buf)
	}
}

func TestReverseFallbacks_Empty(t *testing.T) {
	if got := reverseFallbacks(nil); got != nil {
		t.Errorf("empty input should yield nil, got %+v", got)
	}
}

// TestPollObservability_SuccessfulPoll asserts that one round trip
// fills snap.Observability and snap.RecentFallbacks, and advances the
// cursor to the server's next_since.
func TestPollObservability_SuccessfulPoll(t *testing.T) {
	var sinceSeen atomic.Uint64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/waired/v1/observability/state":
			_ = json.NewEncoder(w).Encode(management.ObservabilityState{
				Agent: management.AgentState{DeviceID: "dev_a", EngineReady: true},
			})
		case "/waired/v1/observability/events":
			if v := r.URL.Query().Get("since"); v != "" {
				n, _ := strconv.ParseUint(v, 10, 64)
				sinceSeen.Store(n)
			}
			_ = json.NewEncoder(w).Encode(observabilityclient.EventsResponse{
				Events: []observability.Event{
					{
						Seq:  1,
						TS:   time.Now().Add(-time.Minute),
						Kind: observability.KindFallback,
						Fallback: &observability.FallbackEvent{
							From: "peer_a", To: "peer_b",
							Reason: "engine_not_ready", Model: "qwen3:8b",
						},
					},
				},
				NextSince: 1,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	tr := &tray{cli: newTestClient(srv.URL), obsSupported: true}
	snap := Snapshot{}
	tr.pollObservability(context.Background(), &snap)

	if snap.Observability == nil || snap.Observability.Agent.DeviceID != "dev_a" {
		t.Errorf("snap.Observability not populated: %+v", snap.Observability)
	}
	if len(snap.RecentFallbacks) != 1 || snap.RecentFallbacks[0].From != "peer_a" {
		t.Errorf("snap.RecentFallbacks not populated: %+v", snap.RecentFallbacks)
	}
	tr.mu.Lock()
	got := tr.obsCursor
	tr.mu.Unlock()
	if got != 1 {
		t.Errorf("cursor=%d, want 1 (after server reported NextSince=1)", got)
	}
	if sinceSeen.Load() != 0 {
		t.Errorf("first poll should send since=0 (or omit), got since=%d", sinceSeen.Load())
	}
}

// TestPollObservability_StickyCursorAcrossPolls asserts that the
// cursor returned by one poll is sent back to the server on the next.
func TestPollObservability_StickyCursorAcrossPolls(t *testing.T) {
	var (
		callIdx   atomic.Int32
		sinceSeen atomic.Uint64
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/waired/v1/observability/state":
			_ = json.NewEncoder(w).Encode(management.ObservabilityState{})
		case "/waired/v1/observability/events":
			n := callIdx.Add(1)
			if v := r.URL.Query().Get("since"); v != "" {
				p, _ := strconv.ParseUint(v, 10, 64)
				sinceSeen.Store(p)
			}
			_ = json.NewEncoder(w).Encode(observabilityclient.EventsResponse{
				NextSince: uint64(n * 10),
			})
		}
	}))
	t.Cleanup(srv.Close)

	tr := &tray{cli: newTestClient(srv.URL), obsSupported: true}

	// First poll: server returns NextSince=10.
	tr.pollObservability(context.Background(), &Snapshot{})
	tr.mu.Lock()
	if tr.obsCursor != 10 {
		t.Errorf("after poll 1, cursor=%d, want 10", tr.obsCursor)
	}
	tr.mu.Unlock()

	// Second poll: client must send since=10.
	tr.pollObservability(context.Background(), &Snapshot{})
	if got := sinceSeen.Load(); got != 10 {
		t.Errorf("second poll sent since=%d, want 10", got)
	}
}

// TestPollObservability_404FlipsObsSupportedOff asserts that one 404
// disables observability for the lifetime of the tray (avoiding a 5 s
// barrage against a legacy daemon).
func TestPollObservability_404FlipsObsSupportedOff(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.NotFound(w, nil)
	}))
	t.Cleanup(srv.Close)

	tr := &tray{cli: newTestClient(srv.URL), obsSupported: true}

	tr.pollObservability(context.Background(), &Snapshot{})
	tr.mu.Lock()
	if tr.obsSupported {
		t.Errorf("obsSupported should be false after 404")
	}
	tr.mu.Unlock()

	hitsAfter404 := hits.Load()
	// Second call must short-circuit before any HTTP request.
	tr.pollObservability(context.Background(), &Snapshot{})
	if hits.Load() != hitsAfter404 {
		t.Errorf("second poll after 404 should not make additional HTTP calls (was %d, now %d)",
			hitsAfter404, hits.Load())
	}
}
