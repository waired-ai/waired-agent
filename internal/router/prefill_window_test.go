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

func TestPrefillWindow_RecordProbeKeepsThePublishedRungs(t *testing.T) {
	w, _ := newTestPrefillWindow(t)
	w.RecordProbe("peer-A", HealthStatus{
		CapacityUsed: 2,
		PrefillRate: &PrefillRate{VariantID: "q4-gguf", Rungs: []PrefillRung{
			{Depth: 4096, Tokps: 900},
			{Depth: 8192, Tokps: 800},
		}},
	})
	snap := w.Snapshot()
	got := snap["peer-A"]
	if got.VariantID != "q4-gguf" {
		t.Errorf("VariantID = %q, want q4-gguf", got.VariantID)
	}
	if got.CapacityUsed != 2 {
		t.Errorf("CapacityUsed = %d, want 2 — the congestion divisor", got.CapacityUsed)
	}
	if len(got.Rungs) != 2 || got.Rungs[8192].Tokps != 800 {
		t.Errorf("Rungs = %+v, want both published rungs", got.Rungs)
	}
}

// TestPrefillWindow_APeerThatSwitchedModelStartsOver: a rate is
// meaningless against another variant, so nothing carries across the
// switch — not even a rung the new model has not reached yet.
func TestPrefillWindow_APeerThatSwitchedModelStartsOver(t *testing.T) {
	w, _ := newTestPrefillWindow(t)
	w.RecordProbe("peer-A", HealthStatus{PrefillRate: &PrefillRate{
		VariantID: "q4-gguf", Rungs: []PrefillRung{{Depth: 8192, Tokps: 800}},
	}})
	w.RecordProbe("peer-A", HealthStatus{PrefillRate: &PrefillRate{
		VariantID: "q8-gguf", Rungs: []PrefillRung{{Depth: 4096, Tokps: 300}},
	}})
	got := w.Snapshot()["peer-A"]
	if _, stale := got.Rungs[8192]; stale {
		t.Error("a reading for the previous model must not survive the switch")
	}
	if got.Rungs[4096].Tokps != 300 || got.VariantID != "q8-gguf" {
		t.Errorf("got %+v, want only the new model's reading", got)
	}
}

// TestPrefillWindow_KeepsTheBestReading: a cold sample includes a model
// load and only ever understates, so one unlucky turn must not re-rank a
// peer for the whole window.
func TestPrefillWindow_KeepsTheBestReading(t *testing.T) {
	w, clk := newTestPrefillWindow(t)
	w.RecordObserved("peer-A", "q4-gguf", 30000, 45*time.Second) // 667 tok/s
	clk.advance(time.Minute)
	w.RecordObserved("peer-A", "q4-gguf", 30000, 90*time.Second) // 333 — a cold turn
	if got := w.Snapshot()["peer-A"].Rungs[32768].Tokps; got < 666 || got > 668 {
		t.Errorf("tokps = %v, want the better reading (~667)", got)
	}
	clk.advance(time.Minute)
	w.RecordObserved("peer-A", "q4-gguf", 30000, 30*time.Second) // 1000 — better still
	if got := w.Snapshot()["peer-A"].Rungs[32768].Tokps; got != 1000 {
		t.Errorf("tokps = %v, want 1000", got)
	}
}

// TestPrefillWindow_ObservationsLandOnlyOnARungTheyResemble is the
// comparability rule applied to real traffic. Prefill throughput falls
// with depth, so a 30k-token turn says nothing about a 4k rung.
func TestPrefillWindow_ObservationsLandOnlyOnARungTheyResemble(t *testing.T) {
	cases := []struct {
		name         string
		promptTokens int
		wantDepth    int
		wantKept     bool
	}{
		{"a real coding-agent first turn lands on the top rung", 30359, 32768, true},
		{"an exact rung", 8192, 8192, true},
		// Both rungs accept these, and the nearer one takes them:
		// 5,735 is 1.40x of 4,096 against 0.70x of 8,192.
		{"a depth two rungs accept goes to the nearer", 5735, 4096, true},
		{"and the same the other way", 6144, 8192, true},
		// 20,000 is 2.4x the 8,192 rung and 0.61 of the 32,768 one:
		// close enough to neither to describe either.
		{"between two rungs describes neither", 20000, 0, false},
		{"far below every rung", 500, 0, false},
		{"far above every rung", 200000, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, _ := newTestPrefillWindow(t)
			w.RecordObserved("peer-A", "q4-gguf", c.promptTokens, time.Second)
			rungs := w.Snapshot()["peer-A"].Rungs
			if !c.wantKept {
				if len(rungs) != 0 {
					t.Errorf("kept %+v; an uncomparable reading is worse than none", rungs)
				}
				return
			}
			if _, ok := rungs[c.wantDepth]; !ok {
				t.Errorf("rungs = %+v, want a reading at %d", rungs, c.wantDepth)
			}
		})
	}
}

// TestPrefillWindow_AMeasurementSupersedesABound, and never the reverse:
// a bound is what a host publishes when it could not finish.
func TestPrefillWindow_AMeasurementSupersedesABound(t *testing.T) {
	w, _ := newTestPrefillWindow(t)
	w.RecordProbe("peer-A", HealthStatus{PrefillRate: &PrefillRate{
		VariantID: "v", Rungs: []PrefillRung{{Depth: 4096, Tokps: 100, Bound: true}},
	}})
	// A slower MEASUREMENT still replaces the bound.
	w.RecordObserved("peer-A", "v", 4096, 100*time.Second) // ~41 tok/s
	got := w.Snapshot()["peer-A"].Rungs[4096]
	if got.Bound {
		t.Error("a measurement must supersede a bound")
	}
	if got.Tokps > 42 {
		t.Errorf("tokps = %v, want the measured ~41", got.Tokps)
	}
	// And a bound must not displace it again.
	w.RecordProbe("peer-A", HealthStatus{PrefillRate: &PrefillRate{
		VariantID: "v", Rungs: []PrefillRung{{Depth: 4096, Tokps: 900, Bound: true}},
	}})
	if w.Snapshot()["peer-A"].Rungs[4096].Bound {
		t.Error("a bound must never displace a measurement, however flattering")
	}
}

func TestPrefillWindow_ReadingsAgeOut(t *testing.T) {
	w, clk := newTestPrefillWindow(t)
	w.RecordProbe("peer-A", HealthStatus{CapacityUsed: 3, PrefillRate: &PrefillRate{
		VariantID: "v", Rungs: []PrefillRung{{Depth: 4096, Tokps: 900}},
	}})
	clk.advance(prefillWindowTTL - time.Second)
	if len(w.Snapshot()["peer-A"].Rungs) != 1 {
		t.Error("still inside the window")
	}
	clk.advance(2 * time.Second)
	if snap := w.Snapshot(); len(snap) != 0 {
		t.Errorf("snapshot = %+v, want the peer dropped entirely", snap)
	}
}

// TestRoundRung picks one depth for a whole selection round. It is not
// per pair: a pairwise "deepest common rung" is not a total order, and
// sort.SliceStable given a non-transitive comparison answers arbitrarily.
func TestRoundRung(t *testing.T) {
	at := func(depths ...int) PeerSpeed {
		r := map[int]PrefillRung{}
		for _, d := range depths {
			r[d] = PrefillRung{Depth: d, Tokps: 100}
		}
		return PeerSpeed{Rungs: r}
	}
	cases := []struct {
		name   string
		speeds []PeerSpeed
		want   int
		wantOK bool
	}{
		{"everyone reached the top", []PeerSpeed{at(4096, 8192, 32768), at(4096, 8192, 32768)}, 32768, true},
		{"one peer drags the round down", []PeerSpeed{at(4096, 8192, 32768), at(4096)}, 4096, true},
		{"a peer with no readings does not drag it", []PeerSpeed{at(4096, 8192, 32768), {}, at(8192, 32768)}, 32768, true},
		{"no shared depth", []PeerSpeed{at(4096), at(32768)}, 0, false},
		{"nobody has measured anything", []PeerSpeed{{}, {}}, 0, false},
		{"an empty round", nil, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := RoundRung(c.speeds)
			if got != c.want || ok != c.wantOK {
				t.Errorf("RoundRung = (%d, %v), want (%d, %v)", got, ok, c.want, c.wantOK)
			}
		})
	}
}

func TestPrefillWindow_IgnoresNonsense(t *testing.T) {
	w, _ := newTestPrefillWindow(t)
	w.RecordProbe("", HealthStatus{})
	w.RecordObserved("", "v", 4096, time.Second)
	w.RecordObserved("peer-A", "v", 0, time.Second)
	w.RecordObserved("peer-A", "v", 4096, 0)
	w.RecordProbe("peer-B", HealthStatus{PrefillRate: &PrefillRate{
		VariantID: "v", Rungs: []PrefillRung{{Depth: 0, Tokps: 900}, {Depth: 4096, Tokps: 0}},
	}})
	for id, s := range w.Snapshot() {
		if len(s.Rungs) != 0 {
			t.Errorf("%s kept %+v from nonsense input", id, s.Rungs)
		}
	}
}
