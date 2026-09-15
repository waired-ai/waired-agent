package router

import (
	"testing"
	"time"
)

func newTestPrefillWindow(t *testing.T) (*PrefillWindow, *mockClock) {
	t.Helper()
	clk := newMockClock(time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC))
	return NewPrefillWindow(clk.Now), clk
}

func TestPrefillWindow_RecordProbeKeepsThePublishedReading(t *testing.T) {
	w, clk := newTestPrefillWindow(t)
	r := speedAt(70, clk.Now())
	r.VariantID = "q4-gguf"
	w.RecordProbe("peer-A", HealthStatus{CapacityUsed: 2, Speed: r})
	got := w.Snapshot()["peer-A"]
	if got.VariantID != "q4-gguf" {
		t.Errorf("VariantID = %q, want q4-gguf", got.VariantID)
	}
	if got.CapacityUsed != 2 {
		t.Errorf("CapacityUsed = %d, want 2 — the congestion multiplier", got.CapacityUsed)
	}
	if got.Turn == nil || got.Turn.TurnSeconds != 70 {
		t.Errorf("Turn = %+v, want the published 70 s", got.Turn)
	}

	t.Run("a peer that has measured nothing still reports its in-flight count", func(t *testing.T) {
		// The multiplier is read whether or not the peer has a figure: an
		// unmeasured peer takes the best known bucket, and a busy one must
		// still be seen as busy once it publishes.
		w, _ := newTestPrefillWindow(t)
		w.RecordProbe("peer-B", HealthStatus{CapacityUsed: 1})
		got, ok := w.Snapshot()["peer-B"]
		if !ok || got.CapacityUsed != 1 || got.Turn != nil {
			t.Errorf("snapshot = %+v (present %v), want the in-flight count and no reading", got, ok)
		}
	})
}

func TestPrefillWindow_IgnoresNonsense(t *testing.T) {
	w, clk := newTestPrefillWindow(t)
	w.RecordProbe("", HealthStatus{Speed: speedAt(70, clk.Now())})
	w.RecordProbe("peer-A", HealthStatus{Speed: speedAt(70, clk.Now())})
	w.RecordObserved("", "v", 30000, time.Second)
	w.RecordObserved("peer-A", "v", 0, time.Second)
	w.RecordObserved("peer-A", "v", 30000, 0)
	// A reading that claims nothing: no figure and no bound.
	w.RecordProbe("peer-B", HealthStatus{Speed: &PeerSpeedReading{VariantID: "v", DecodeTokps: 15.8}})
	snap := w.Snapshot()
	if _, ok := snap[""]; ok {
		t.Error("a probe with no device id was recorded")
	}
	if got := snap["peer-A"].Turn; got == nil || got.TurnSeconds != 70 {
		t.Errorf("peer-A turn = %+v, want the published 70 s untouched by the nonsense turns", got)
	}
	if got := snap["peer-B"].Turn; got != nil {
		t.Errorf("peer-B turn = %+v from a reading that claims nothing", got)
	}
}
