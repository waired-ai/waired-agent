package router

import "testing"

// TestDeepestCommonRung is the comparison rule of waired-agent#1127.
//
// Prefill throughput falls as the prompt grows — 833 tok/s at 11,526
// tokens against 583 at 21,247, one machine and one model, measured in
// docs/knowledges/20260805/1830-ollama-prompt-depth-two-traps.md — so two
// readings taken at different depths say more about the depths than about
// the hosts. Every host climbs the same fixed rungs; two peers are
// compared at the deepest one they BOTH reached, and at no other.
func TestDeepestCommonRung(t *testing.T) {
	fast := &PrefillRate{Rungs: []PrefillRung{
		{Depth: 4096, Tokps: 900},
		{Depth: 8192, Tokps: 800},
		{Depth: 32768, Tokps: 690},
	}}
	middling := &PrefillRate{Rungs: []PrefillRung{
		{Depth: 4096, Tokps: 200},
		{Depth: 8192, Tokps: 150},
	}}
	slow := &PrefillRate{Rungs: []PrefillRung{
		{Depth: 4096, Tokps: 80},
	}}
	bounded := &PrefillRate{Rungs: []PrefillRung{
		{Depth: 4096, Tokps: 22, Bound: true},
	}}
	deepOnly := &PrefillRate{Rungs: []PrefillRung{
		{Depth: 32768, Tokps: 690},
	}}

	cases := []struct {
		name       string
		a, b       *PrefillRate
		wantDepth  int
		wantATokps float64
		wantBTokps float64
		wantOK     bool
	}{
		{"two fast peers meet at the top", fast, fast, 32768, 690, 690, true},
		{"a fast and a middling peer meet at 8,192", fast, middling, 8192, 800, 150, true},
		{"a fast and a slow peer meet at the shallowest", fast, slow, 4096, 900, 80, true},
		{"a bound is still a rung", fast, bounded, 4096, 900, 22, true},
		{"no rung in common is not comparable", deepOnly, slow, 0, 0, 0, false},
		{"nothing published on one side", fast, nil, 0, 0, 0, false},
		{"nothing published on either side", nil, nil, 0, 0, 0, false},
		{"an empty list is not a rung", fast, &PrefillRate{}, 0, 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ra, rb, ok := DeepestCommonRung(c.a, c.b)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			if ra.Depth != c.wantDepth || rb.Depth != c.wantDepth {
				t.Errorf("depths = %d/%d, want %d on both sides", ra.Depth, rb.Depth, c.wantDepth)
			}
			if ra.Tokps != c.wantATokps || rb.Tokps != c.wantBTokps {
				t.Errorf("rates = %v/%v, want %v/%v", ra.Tokps, rb.Tokps, c.wantATokps, c.wantBTokps)
			}
		})
	}
}

// TestDeepestCommonRung_NeverComparesUnlikeDepths is the property the
// table above encodes, stated on its own because it is the whole point:
// a rung is only ever paired with the same depth on the other side.
func TestDeepestCommonRung_NeverComparesUnlikeDepths(t *testing.T) {
	a := &PrefillRate{Rungs: []PrefillRung{{Depth: 4096, Tokps: 900}, {Depth: 32768, Tokps: 690}}}
	b := &PrefillRate{Rungs: []PrefillRung{{Depth: 8192, Tokps: 150}}}
	if _, _, ok := DeepestCommonRung(a, b); ok {
		t.Error("peers whose rungs do not overlap must be reported as not comparable")
	}
}

// TestPrefillRate_RungAt_IgnoresAZeroReading: a rung recorded with no
// rate says nothing, and must not be offered as a comparison point.
func TestPrefillRate_RungAt_IgnoresAZeroReading(t *testing.T) {
	p := &PrefillRate{Rungs: []PrefillRung{{Depth: 4096, Tokps: 0}}}
	if _, ok := p.RungAt(4096); ok {
		t.Error("a zero rate is not a reading")
	}
	var nilRate *PrefillRate
	if _, ok := nilRate.RungAt(4096); ok {
		t.Error("nil must miss")
	}
}
