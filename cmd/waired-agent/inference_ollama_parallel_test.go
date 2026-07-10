package main

import (
	"strings"
	"testing"
)

// TestRecommendedParallel pins the VRAM-safe ceiling: floor(maxCtx/ctx) in the
// no-spill regime, floored at 1, and 1 whenever spilling (maxCtx < ctx) or
// unsizable (a zero input).
func TestRecommendedParallel(t *testing.T) {
	cases := []struct {
		maxCtx, ctx, want int
	}{
		{200000, 40960, 4}, // four full-window slots fit
		{81920, 40960, 2},  // exactly two fit
		{45000, 40960, 1},  // barely over one → 1 (never fractional)
		{40960, 40960, 1},  // window fills the budget → single slot
		{20000, 40960, 1},  // spilling (maxCtx < ctx) → 1
		{0, 40960, 1},      // unsizable → 1
		{40960, 0, 1},      // guard against divide-by-zero → 1
	}
	for _, c := range cases {
		if got := recommendedParallel(c.maxCtx, c.ctx); got != c.want {
			t.Errorf("recommendedParallel(%d, %d) = %d, want %d", c.maxCtx, c.ctx, got, c.want)
		}
	}
}

// TestComputeOllamaTuning_OperatorOverride covers the concurrency knob: the
// operator target replaces the auto-sized NumParallel and, above the
// recommended max, attaches a warning; a 0 target keeps auto sizing while still
// reporting the recommendation.
func TestComputeOllamaTuning_OperatorOverride(t *testing.T) {
	m := tuningTestManifest()
	// The 21.5 GB variant's no-spill window fills VRAM (ctx == maxCtx), so the
	// recommended max is a single slot — any override > 1 must warn.
	v := m.Variants[0]
	v.EstimatedWeightGB = 21.5

	t.Run("auto-reports-recommendation", func(t *testing.T) {
		got := computeOllamaTuningOpts(m, v, discrete24GB(), "q8_0", true, 0)
		if got.NumParallel != 1 {
			t.Errorf("NumParallel = %d, want auto 1", got.NumParallel)
		}
		if got.RecommendedMaxParallel != 1 {
			t.Errorf("RecommendedMaxParallel = %d, want 1", got.RecommendedMaxParallel)
		}
		if strings.Contains(got.Warning, "recommended max") {
			t.Errorf("auto sizing must not attach the override warning: %q", got.Warning)
		}
	})

	t.Run("override-at-recommendation-no-warning", func(t *testing.T) {
		got := computeOllamaTuningOpts(m, v, discrete24GB(), "q8_0", true, 1)
		if got.NumParallel != 1 {
			t.Errorf("NumParallel = %d, want 1", got.NumParallel)
		}
		if strings.Contains(got.Warning, "recommended max") {
			t.Errorf("a target at the recommendation must not warn: %q", got.Warning)
		}
	})

	t.Run("override-above-recommendation-honored-and-warns", func(t *testing.T) {
		got := computeOllamaTuningOpts(m, v, discrete24GB(), "q8_0", true, 8)
		if got.NumParallel != 8 {
			t.Errorf("NumParallel = %d, want the operator override 8", got.NumParallel)
		}
		if got.RecommendedMaxParallel != 1 {
			t.Errorf("RecommendedMaxParallel = %d, want 1", got.RecommendedMaxParallel)
		}
		if !strings.Contains(got.Warning, "above this host's recommended max") {
			t.Errorf("an over-recommendation override must warn: %q", got.Warning)
		}
	})

	t.Run("override-honored-in-spill-regime", func(t *testing.T) {
		// The 22 GB variant spills to the coding floor (recommended = 1). The
		// override is still honored, with the concurrency warning joined onto the
		// existing spill warning.
		got := computeOllamaTuningOpts(m, m.Variants[0], discrete24GB(), "q8_0", true, 4)
		if got.NumParallel != 4 {
			t.Errorf("NumParallel = %d, want 4", got.NumParallel)
		}
		if got.RecommendedMaxParallel != 1 {
			t.Errorf("RecommendedMaxParallel = %d, want 1 (spilling)", got.RecommendedMaxParallel)
		}
		if !strings.Contains(got.Warning, "system RAM") ||
			!strings.Contains(got.Warning, "above this host's recommended max") {
			t.Errorf("spill + override warnings should both be present: %q", got.Warning)
		}
	})
}
