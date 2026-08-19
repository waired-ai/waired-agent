package main

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/hardware"
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
		got := computeOllamaTuningOpts(m, v, discrete24GB(), "q8_0", 0, 0, ollamaObservedServe{})
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
		got := computeOllamaTuningOpts(m, v, discrete24GB(), "q8_0", 0, 1, ollamaObservedServe{})
		if got.NumParallel != 1 {
			t.Errorf("NumParallel = %d, want 1", got.NumParallel)
		}
		if strings.Contains(got.Warning, "recommended max") {
			t.Errorf("a target at the recommendation must not warn: %q", got.Warning)
		}
	})

	t.Run("override-above-recommendation-honored-and-warns", func(t *testing.T) {
		got := computeOllamaTuningOpts(m, v, discrete24GB(), "q8_0", 0, 8, ollamaObservedServe{})
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

	// PRODUCT CONTRACT (waired-agent#29): dropping the quantized KV cache on a
	// roomy CPU host must not cost a request slot. planOllamaKV's f16
	// threshold is deliberately the same 2x the slot grant uses, which is what
	// makes this a proof rather than a coincidence.
	t.Run("auto-cpu-host-keeps-parallelism", func(t *testing.T) {
		tm := tinyCoderManifest()
		got := computeOllamaTuningOpts(tm, tm.Variants[0], ciRunner16GB(), ollamaKVAuto, 0, 0, ollamaObservedServe{})
		if got.KVCacheType != "f16" {
			t.Fatalf("precondition: KVCacheType = %q, want f16", got.KVCacheType)
		}
		if got.NumParallel != ollamaMaxAutoParallel {
			t.Errorf("NumParallel = %d, want %d", got.NumParallel, ollamaMaxAutoParallel)
		}
		// The operator override still wins, and still warns above the
		// recommendation.
		over := computeOllamaTuningOpts(tm, tm.Variants[0], ciRunner16GB(), ollamaKVAuto, 0, 8, ollamaObservedServe{})
		if over.NumParallel != 8 {
			t.Errorf("NumParallel = %d, want the operator override 8", over.NumParallel)
		}
	})

	t.Run("override-honored-in-spill-regime", func(t *testing.T) {
		// The 22 GB variant spills to the coding floor (recommended = 1). The
		// override is still honored, with the concurrency warning joined onto the
		// existing spill warning.
		got := computeOllamaTuningOpts(m, m.Variants[0], discrete24GB(), "q8_0", 0, 4, ollamaObservedServe{})
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

// umaTwoSlotHost is the shape that earns a second request slot: a unified
// pool large enough to hold two full windows of KV un-spilled. It is also
// the shape a Windows Ryzen AI Max host produces once the carve-out stops
// being read as the budget (waired-ai/waired-agent#863), which is why the
// engine's refusal became reachable there again.
func umaTwoSlotHost() hardware.Profile {
	return hardware.Profile{RAMTotalGB: 128, UnifiedMemory: true, UsableVRAMMB: 98304}
}

// TestComputeOllamaTuning_ObservedParallelIsTheCeiling pins the feedback
// from waired-ai/waired-agent#846: our per-slot price and the engine's
// disagree, the engine is right about its own allocations, and once it has
// answered for this model at this window we stop re-asking.
//
// A record of today's behaviour. The gap itself — the runner also charges
// a prompt cache, context checkpoints and a vision tower that the sizing
// does not model — is still open on #846; this only stops the refusal from
// repeating every reconcile.
func TestComputeOllamaTuning_ObservedParallelIsTheCeiling(t *testing.T) {
	m := tuningTestManifest()
	v := m.Variants[0]
	hw := umaTwoSlotHost()

	auto := computeOllamaTuningOpts(m, v, hw, "q8_0", 0, 0, ollamaObservedServe{})
	if auto.NumParallel != 2 {
		t.Fatalf("NumParallel = %d without an observation, want 2: the rest of this "+
			"test asserts a clamp that would be invisible without one", auto.NumParallel)
	}
	if auto.RecommendedMaxParallel < 2 {
		t.Fatalf("RecommendedMaxParallel = %d, want at least 2", auto.RecommendedMaxParallel)
	}
	served := ollamaObservedServe{
		ModelID:       m.ModelID,
		VariantID:     v.VariantID,
		ContextLength: auto.ContextLength,
		NumParallel:   1,
	}

	t.Run("the engine's answer lowers both figures", func(t *testing.T) {
		got := computeOllamaTuningOpts(m, v, hw, "q8_0", 0, 0, served)
		if got.NumParallel != 1 {
			t.Errorf("NumParallel = %d, want 1: the runner declined the second slot", got.NumParallel)
		}
		if got.RecommendedMaxParallel != 1 {
			t.Errorf("RecommendedMaxParallel = %d, want 1: advising an operator toward a "+
				"slot the engine refused is the same error one level up", got.RecommendedMaxParallel)
		}
	})

	t.Run("an observation never raises the slot count", func(t *testing.T) {
		generous := served
		generous.NumParallel = 8
		got := computeOllamaTuningOpts(m, v, hw, "q8_0", 0, 0, generous)
		if got.NumParallel != auto.NumParallel {
			t.Errorf("NumParallel = %d, want the auto-sized %d: the observation is a "+
				"ceiling, not a target", got.NumParallel, auto.NumParallel)
		}
	})

	t.Run("an observation for another target is ignored", func(t *testing.T) {
		otherModel := served
		otherModel.ModelID = "some-other-model"
		otherVariant := served
		otherVariant.VariantID = "q4"
		otherWindow := served
		otherWindow.ContextLength = auto.ContextLength / 2
		unobserved := served
		unobserved.NumParallel = 0

		for name, o := range map[string]ollamaObservedServe{
			"another model":    otherModel,
			"another variant":  otherVariant,
			"another window":   otherWindow,
			"nothing observed": unobserved,
		} {
			got := computeOllamaTuningOpts(m, v, hw, "q8_0", 0, 0, o)
			if got.NumParallel != 2 {
				t.Errorf("%s: NumParallel = %d, want 2: a refusal elsewhere says nothing "+
					"about this sizing", name, got.NumParallel)
			}
		}
	})

	t.Run("the operator override still wins", func(t *testing.T) {
		got := computeOllamaTuningOpts(m, v, hw, "q8_0", 0, 2, served)
		if got.NumParallel != 2 {
			t.Errorf("NumParallel = %d, want the operator's 2: the observation sizes the "+
				"default, it does not overrule an informed override", got.NumParallel)
		}
		if !strings.Contains(got.Warning, "above this host's recommended max of 1") {
			t.Errorf("Warning = %q, want it to name the recommended max of 1", got.Warning)
		}
	})
}
