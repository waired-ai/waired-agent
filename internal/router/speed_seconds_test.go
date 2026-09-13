package router

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// Peer selection in seconds per request (waired-agent#1341; decision 9 of
// docs/decisions/20260913/2245-speed-is-one-request-at-32768-tokens.md).

func speedAt(turn float64, at time.Time) *PeerSpeedReading {
	return &PeerSpeedReading{
		VariantID: "v", DepthTokens: hostfit.SpeedMeasurementDepthTokens,
		PrefillTokps: 250, DecodeTokps: 15.8,
		TurnSeconds: turn, MeasuredAt: at.Format(time.RFC3339Nano),
	}
}

func turnOf(t *testing.T, w *PrefillWindow, peer string) *PeerTurn {
	t.Helper()
	return w.Snapshot()[peer].Turn
}

func TestHealthStatus_DecodesTheSpeedObject(t *testing.T) {
	body := `{"engine_ready":true,"speed":{"variant_id":"q4","depth_tokens":32768,` +
		`"prefill_tokps":252.9,"decode_tokps":15.8,"turn_seconds":228,` +
		`"measured_at":"2026-09-13T12:00:00Z"}}`
	var s HealthStatus
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		t.Fatal(err)
	}
	want := PeerSpeedReading{VariantID: "q4", DepthTokens: 32768, PrefillTokps: 252.9,
		DecodeTokps: 15.8, TurnSeconds: 228, MeasuredAt: "2026-09-13T12:00:00Z"}
	if s.Speed == nil || *s.Speed != want {
		t.Errorf("speed = %+v, want %+v", s.Speed, want)
	}
	var old HealthStatus
	if err := json.Unmarshal([]byte(`{"engine_ready":true}`), &old); err != nil || old.Speed != nil {
		t.Errorf("an agent predating the field decoded a speed: %+v (err %v)", old.Speed, err)
	}
}

// PRODUCT CONTRACT (decision 9: "新しい計測が古い計測を置き換える"): a
// newer reading replaces an older one even when it is slower — the case the
// keep-best rule used to ignore until the window expired.
func TestPrefillWindow_ANewerSecondsReadingReplacesAFasterOlderOne(t *testing.T) {
	w, clk := newTestPrefillWindow(t)
	t0 := clk.Now()
	w.RecordProbe("peer-A", HealthStatus{Speed: speedAt(70, t0)})
	clk.advance(time.Minute)
	w.RecordProbe("peer-A", HealthStatus{Speed: speedAt(228, clk.Now())})
	if got := turnOf(t, w, "peer-A"); got == nil || got.TurnSeconds != 228 {
		t.Fatalf("turn = %+v, want the newer, slower 228 s", got)
	}

	// A probe answered out of order carries the OLDER measurement: it does
	// not overturn the newer one.
	w.RecordProbe("peer-A", HealthStatus{Speed: speedAt(70, t0)})
	if got := turnOf(t, w, "peer-A"); got.TurnSeconds != 228 {
		t.Errorf("an older measurement overturned a newer one: %+v", got)
	}

	// A newer bound replaces a finished figure too: the peer is measuring
	// again and is already over the line.
	clk.advance(time.Minute)
	bound := speedAt(0, clk.Now())
	bound.TurnFloorSeconds = 195
	w.RecordProbe("peer-A", HealthStatus{Speed: bound})
	if got := turnOf(t, w, "peer-A"); got.TurnSeconds != 0 || got.TurnFloorSeconds != 195 {
		t.Errorf("turn = %+v, want the newer bound", got)
	}
}

func TestPrefillWindow_SecondsReadingsAgeOutAndResetOnASwitch(t *testing.T) {
	w, clk := newTestPrefillWindow(t)
	w.RecordProbe("peer-A", HealthStatus{Speed: speedAt(70, clk.Now())})
	if turnOf(t, w, "peer-A") == nil {
		t.Fatal("a peer with only a seconds reading was dropped from the snapshot")
	}

	// A peer that switched model starts over.
	other := speedAt(0, clk.Now())
	other.VariantID = "other"
	other.TurnFloorSeconds = 0
	w.RecordProbe("peer-A", HealthStatus{Speed: other})
	if got := turnOf(t, w, "peer-A"); got != nil {
		t.Errorf("a reading for the previous model survived the switch: %+v", got)
	}

	w.RecordProbe("peer-B", HealthStatus{Speed: speedAt(70, clk.Now())})
	clk.advance(prefillWindowTTL + time.Second)
	if snap := w.Snapshot(); len(snap) != 0 {
		t.Errorf("snapshot = %+v, want every reading aged out", snap)
	}

	// An aged-out reading is replaced even by an older measurement.
	w.RecordProbe("peer-C", HealthStatus{Speed: speedAt(228, clk.Now())})
	clk.advance(prefillWindowTTL + time.Second)
	w.RecordProbe("peer-C", HealthStatus{Speed: speedAt(70, clk.Now().Add(-time.Hour))})
	if got := turnOf(t, w, "peer-C"); got == nil || got.TurnSeconds != 70 {
		t.Errorf("turn = %+v, want the only fresh reading", got)
	}
}

// RecordObserved replaces only the prefill term, only at a depth resembling
// 32,768, and only when the peer published a decode rate to finish the sum.
func TestPrefillWindow_AnObservedTurnReplacesThePrefillTerm(t *testing.T) {
	cases := []struct {
		name         string
		published    bool
		decode       float64
		promptTokens int
		ttft         time.Duration
		wantTurn     float64 // 0 = the published figure stands
	}{
		{"a coding-agent first turn near 32k", true, 15.8, 30000, 100 * time.Second,
			hostfit.TurnSecondsAt(32768, 300, 15.8)},
		{"the band's low edge", true, 15.8, 23000, 100 * time.Second,
			hostfit.TurnSecondsAt(32768, 230, 15.8)},
		{"a turn at 8k depth resembles another rung", true, 15.8, 8192, 10 * time.Second, 0},
		{"no published decode rate", true, 0, 30000, 100 * time.Second, 0},
		{"nothing published at all", false, 0, 30000, 100 * time.Second, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, clk := newTestPrefillWindow(t)
			if c.published {
				r := speedAt(228, clk.Now())
				r.DecodeTokps = c.decode
				w.RecordProbe("peer-A", HealthStatus{Speed: r})
			}
			clk.advance(time.Minute)
			w.RecordObserved("peer-A", "v", c.promptTokens, c.ttft)
			got := turnOf(t, w, "peer-A")
			switch {
			case c.wantTurn == 0 && !c.published:
				if got != nil {
					t.Errorf("turn = %+v, want none", got)
				}
			case c.wantTurn == 0:
				if got == nil || got.TurnSeconds != 228 {
					t.Errorf("turn = %+v, want the published 228 s to stand", got)
				}
			default:
				if got == nil || math.Abs(got.TurnSeconds-c.wantTurn) > 1e-6 {
					t.Errorf("turn = %+v, want %.2f", got, c.wantTurn)
				}
			}
		})
	}

	t.Run("a newer publication wins over an older observation", func(t *testing.T) {
		w, clk := newTestPrefillWindow(t)
		w.RecordProbe("peer-A", HealthStatus{Speed: speedAt(228, clk.Now())})
		clk.advance(time.Minute)
		w.RecordObserved("peer-A", "v", 30000, 100*time.Second)
		clk.advance(time.Minute)
		w.RecordProbe("peer-A", HealthStatus{Speed: speedAt(150, clk.Now())})
		if got := turnOf(t, w, "peer-A"); got.TurnSeconds != 150 {
			t.Errorf("turn = %+v, want the peer's newer 150 s", got)
		}
	})
}

// A #1341 peer on a window too small for 32,768 publishes its one rung at a
// shallower depth; an older round still has to be able to compare it.
func TestPrefillWindow_AMeasuredDepthIsFiledUnderTheRungItResembles(t *testing.T) {
	w, _ := newTestPrefillWindow(t)
	w.RecordProbe("peer-A", HealthStatus{PrefillRate: &PrefillRate{VariantID: "v", Rungs: []PrefillRung{
		{Depth: 30592, Tokps: 900},
		{Depth: 20000, Tokps: 950}, // resembles no rung: dropped
	}}})
	rungs := w.Snapshot()["peer-A"].Rungs
	if r, ok := rungs[32768]; !ok || r.Tokps != 900 || len(rungs) != 1 {
		t.Errorf("rungs = %+v, want the 30,592 reading at 32,768 and nothing else", rungs)
	}
}

func turnSpeed(turn, floor float64, capacityUsed int) PeerSpeed {
	return PeerSpeed{VariantID: "v", CapacityUsed: capacityUsed,
		Turn: &PeerTurn{TurnSeconds: turn, TurnFloorSeconds: floor}}
}

func withRung(s PeerSpeed, tokps float64) PeerSpeed {
	s.Rungs = map[int]PrefillRung{32768: {Depth: 32768, Tokps: tokps}}
	return s
}

func bucketsOf(cands []meshCandidate) map[string]int {
	out := map[string]int{}
	for _, c := range cands {
		out[c.deviceID] = c.speedBucket
	}
	return out
}

func cands(ids ...string) []meshCandidate {
	out := make([]meshCandidate, 0, len(ids))
	for _, id := range ids {
		out = append(out, meshCandidate{deviceID: id})
	}
	return out
}

// PRODUCT CONTRACT (decision 9): when the whole round publishes seconds, a
// request's cost orders it — (capacity_used + 1) × TurnSeconds, 25 % bands.
func TestAssignSpeedRanks_SecondsOrderTheRound(t *testing.T) {
	cs := cands("m5-35b", "m5-27b", "busy")
	assignSpeedRanks(cs, map[string]PeerSpeed{
		"m5-35b": turnSpeed(70, 0, 0),
		"m5-27b": turnSpeed(228, 0, 0),
		// 70 s with two requests already running costs 210 s for this one.
		"busy": turnSpeed(70, 0, 2),
	})
	b := bucketsOf(cs)
	if b["m5-35b"] >= b["busy"] || b["m5-35b"] >= b["m5-27b"] {
		t.Errorf("buckets = %v, want the 70 s peer ahead of both 228 s and 3×70 s", b)
	}
	if b["busy"] != speedBucketOf(210) || b["m5-27b"] != speedBucketOf(228) {
		t.Errorf("buckets = %v, want congestion to multiply the seconds", b)
	}
}

// PRODUCT CONTRACT (decision 9): a peer known only to be over the line sits
// below every measured one; one with no reading keeps the best bucket.
func TestAssignSpeedRanks_ABoundSitsBelowTheMeasured(t *testing.T) {
	cs := cands("fast", "slow", "bound-near", "bound-far", "unmeasured")
	assignSpeedRanks(cs, map[string]PeerSpeed{
		"fast":       turnSpeed(70, 0, 0),
		"slow":       turnSpeed(228, 0, 0),
		"bound-near": turnSpeed(0, 195, 0), // under slow's own figure, still below it
		"bound-far":  turnSpeed(0, 5000, 0),
	})
	b := bucketsOf(cs)
	if b["bound-near"] != b["slow"]+1 {
		t.Errorf("buckets = %v, want the bound one bucket below the slowest measured", b)
	}
	if b["bound-far"] != speedBucketOf(5000) || b["bound-far"] <= b["bound-near"] {
		t.Errorf("buckets = %v, want a far bound at its own, lower bucket", b)
	}
	if b["unmeasured"] != b["fast"] {
		t.Errorf("buckets = %v, want the unmeasured peer at the best known bucket", b)
	}

	t.Run("bounds alone still order, and the unmeasured takes the best of them", func(t *testing.T) {
		cs := cands("b1", "b2", "unmeasured")
		assignSpeedRanks(cs, map[string]PeerSpeed{
			"b1": turnSpeed(0, 200, 0),
			"b2": turnSpeed(0, 900, 0),
		})
		b := bucketsOf(cs)
		if b["b1"] >= b["b2"] || b["unmeasured"] != b["b1"] {
			t.Errorf("buckets = %v", b)
		}
	})
}

// PRODUCT CONTRACT (the rollout rule): a round that mixes an older agent
// (prefill rungs only) with #1341 agents ranks on the rungs every one of them
// publishes. Reading the old peer as "unmeasured, best bucket" would put it
// level with the fastest new peer and above every slower one.
func TestAssignSpeedRanks_AMixedRoundFallsBackToTheRungs(t *testing.T) {
	cs := cands("new-fast", "new-slow", "old")
	assignSpeedRanks(cs, map[string]PeerSpeed{
		"new-fast": withRung(turnSpeed(70, 0, 0), 900),
		"new-slow": withRung(turnSpeed(228, 0, 0), 250),
		"old":      withRung(PeerSpeed{VariantID: "v"}, 400),
	})
	b := bucketsOf(cs)
	if b["old"] == b["new-fast"] {
		t.Errorf("buckets = %v: the old peer shares the best bucket as though unmeasured", b)
	}
	if b["new-fast"] >= b["old"] || b["old"] >= b["new-slow"] {
		t.Errorf("buckets = %v, want the rung order 900 > 400 > 250 tok/s", b)
	}
	if b["new-fast"] != speedBucketOf(1.0/900) {
		t.Errorf("new-fast bucket = %d, want its rung bucket %d", b["new-fast"], speedBucketOf(1.0/900))
	}
}

// A #1341 peer whose rung an old peer never reached, in a mixed round: no
// common depth, so speed does not order the round — today's behaviour.
func TestAssignSpeedRanks_AMixedRoundWithNoCommonRungOrdersNothing(t *testing.T) {
	cs := cands("new", "old")
	assignSpeedRanks(cs, map[string]PeerSpeed{
		"new": withRung(turnSpeed(70, 0, 0), 900),
		"old": {VariantID: "v", Rungs: map[int]PrefillRung{4096: {Depth: 4096, Tokps: 1200}}},
	})
	if b := bucketsOf(cs); b["new"] != 0 || b["old"] != 0 {
		t.Errorf("buckets = %v, want no speed key", b)
	}
}

func TestRoundSpeeds_FoldsTheLocalSecondsReading(t *testing.T) {
	ln := localFor("qwen3:8b-q4_K_M")
	ln.CapacityUsed = 1
	ln.Speed = &PeerSpeedReading{VariantID: "q4-gguf", TurnSeconds: 70, MeasuredAt: "2026-09-13T12:00:00Z"}
	self := roundSpeeds(nil, ln)["self"]
	if self.Turn == nil || self.Turn.TurnSeconds != 70 || self.Turn.TurnFloorSeconds != 0 {
		t.Fatalf("turn = %+v, want the local 70 s", self.Turn)
	}
	if got := localRTT(ln); got != 0 {
		t.Errorf("localRTT with only a seconds reading = %d, want 0", got)
	}

	ln.Speed = &PeerSpeedReading{TurnFloorSeconds: 195}
	if self := roundSpeeds(nil, ln)["self"]; self.Turn == nil || self.Turn.TurnFloorSeconds != 195 {
		t.Errorf("turn = %+v, want the local bound", self.Turn)
	}

	// The local device ranks in the same arm as its peers.
	cs := cands("self", "peer")
	assignSpeedRanks(cs, roundSpeeds(map[string]PeerSpeed{"peer": turnSpeed(228, 0, 0)}, func() LocalNode {
		l := localFor("qwen3:8b-q4_K_M")
		l.Speed = &PeerSpeedReading{TurnSeconds: 70}
		return l
	}()))
	if b := bucketsOf(cs); b["self"] >= b["peer"] {
		t.Errorf("buckets = %v, want this 70 s device ahead of the 228 s peer", b)
	}
}
