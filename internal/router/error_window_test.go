package router

import (
	"math"
	"sync"
	"testing"
	"time"
)

func approxEqual(a, b float32) bool {
	return float64(math.Abs(float64(a-b))) < 1e-6
}

// TestErrorWindow_SuccessOnlyReportsZero confirms an all-2xx peer
// shows up with rate 0.0, not "no entry". The Selector reads the
// rate during tie-break and "0.0 errors" is meaningfully different
// from "no observations".
func TestErrorWindow_SuccessOnlyReportsZero(t *testing.T) {
	clk := newMockClock(time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC))
	w := NewErrorWindow(clk.Now)

	for i := 0; i < 10; i++ {
		w.Record("peer-a", true)
	}
	got := w.Snapshot()
	if !approxEqual(got["peer-a"], 0.0) {
		t.Errorf("peer-a after 10 successes: rate=%f, want 0.0 (zero failures over 10 samples)", got["peer-a"])
	}
}

// TestErrorWindow_HalfFailureRate verifies the basic ratio math.
// 4 failures + 6 successes = 0.4 failure fraction.
func TestErrorWindow_HalfFailureRate(t *testing.T) {
	clk := newMockClock(time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC))
	w := NewErrorWindow(clk.Now)

	for i := 0; i < 6; i++ {
		w.Record("peer-a", true)
	}
	for i := 0; i < 4; i++ {
		w.Record("peer-a", false)
	}
	got := w.Snapshot()
	if !approxEqual(got["peer-a"], 0.4) {
		t.Errorf("rate=%f, want 0.4 (4 failures of 10)", got["peer-a"])
	}
}

// TestErrorWindow_RotatesBucketsAfter15s exercises the ring head
// advance. Sample, advance the clock past one bucket, sample again —
// both samples must remain in the window (60 s total). After 60 s,
// the first sample must have aged out.
func TestErrorWindow_RotatesBucketsAfter15s(t *testing.T) {
	clk := newMockClock(time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC))
	w := NewErrorWindow(clk.Now)

	w.Record("peer-a", false) // bucket 0

	clk.advance(20 * time.Second) // bucket 1
	w.Record("peer-a", true)

	clk.advance(20 * time.Second) // bucket 2
	w.Record("peer-a", true)

	// At t=40 s, all three samples are still in the 60 s window.
	got := w.Snapshot()
	if !approxEqual(got["peer-a"], 1.0/3.0) {
		t.Errorf("at t=40 s: rate=%f, want 1/3 = %f", got["peer-a"], 1.0/3.0)
	}

	// At t=61 s, the original failure bucket has aged out — only
	// the two successes remain.
	clk.advance(21 * time.Second) // now at t=61 s
	got = w.Snapshot()
	if !approxEqual(got["peer-a"], 0.0) {
		t.Errorf("at t=61 s: rate=%f, want 0.0 (failure aged out)", got["peer-a"])
	}
}

// TestErrorWindow_FullDrainAfterMinute checks the fast-path that
// resets the whole ring when an idle peer's most recent bucket is
// older than the entire window. Without that branch the loop in
// drainLocked would walk N buckets on every Record after a long
// idle, costing nothing functionally but masking the "snap to fresh"
// intent.
func TestErrorWindow_FullDrainAfterMinute(t *testing.T) {
	clk := newMockClock(time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC))
	w := NewErrorWindow(clk.Now)

	for i := 0; i < 5; i++ {
		w.Record("peer-a", false)
	}
	// Sanity: 100% failure right now.
	if got := w.Snapshot()["peer-a"]; !approxEqual(got, 1.0) {
		t.Fatalf("baseline rate=%f, want 1.0", got)
	}

	clk.advance(2 * time.Minute) // > 60 s window
	w.Record("peer-a", true)
	got := w.Snapshot()
	if !approxEqual(got["peer-a"], 0.0) {
		t.Errorf("after long idle: rate=%f, want 0.0 (only the new success counts)", got["peer-a"])
	}
}

// TestErrorWindow_OmitsZeroSnapshot ensures peers with no recent
// observations are dropped from Snapshot(). The NetworkMap broadcast
// is signed and grows linearly with map size — keeping it compact
// matters at scale.
func TestErrorWindow_OmitsZeroSnapshot(t *testing.T) {
	clk := newMockClock(time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC))
	w := NewErrorWindow(clk.Now)

	// peer-a has one record; peer-b never observed.
	w.Record("peer-a", true)
	got := w.Snapshot()
	if _, ok := got["peer-b"]; ok {
		t.Errorf("Snapshot must omit peers with no observations; got %+v", got)
	}

	// peer-a after a full window drain returns to "no recent data".
	clk.advance(2 * time.Minute)
	got = w.Snapshot()
	if _, ok := got["peer-a"]; ok {
		t.Errorf("peer-a after long idle should be dropped from Snapshot; got %+v", got)
	}
	if got != nil {
		t.Errorf("Snapshot with no active peers should be nil for compact JSON; got %+v", got)
	}
}

// TestErrorWindow_EmptyDeviceIDIgnored guards the defensive no-op in
// Record. The Selector should never produce an empty deviceID, but
// silently corrupting the empty-string key would be hard to debug.
func TestErrorWindow_EmptyDeviceIDIgnored(t *testing.T) {
	clk := newMockClock(time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC))
	w := NewErrorWindow(clk.Now)

	w.Record("", false)
	w.Record("", true)
	if got := w.Snapshot(); len(got) != 0 {
		t.Errorf("empty deviceID created entry: %+v", got)
	}
}

// TestErrorWindow_ConcurrentRecord exercises the lock. Run with -race.
func TestErrorWindow_ConcurrentRecord(t *testing.T) {
	clk := newMockClock(time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC))
	w := NewErrorWindow(clk.Now)

	const workers = 16
	const iter = 500
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(id int) {
			defer wg.Done()
			device := "peer-" + string(rune('A'+id%4))
			for j := 0; j < iter; j++ {
				w.Record(device, j%3 == 0)
			}
		}(i)
	}
	wg.Wait()
	// Don't assert exact rates (race-y by design); just confirm the
	// snapshot is well-formed and rates are in [0, 1].
	got := w.Snapshot()
	for k, v := range got {
		if v < 0 || v > 1 {
			t.Errorf("Snapshot[%s] = %f out of [0,1]", k, v)
		}
	}
}

// mockClock advances on demand. Useful here because the sliding
// window depends on wall-clock and we want hermetic tests.
type mockClock struct {
	mu sync.Mutex
	t  time.Time
}

func newMockClock(start time.Time) *mockClock { return &mockClock{t: start} }
func (c *mockClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *mockClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}
