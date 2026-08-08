package hostfit_test

import (
	"testing"

	"github.com/waired-ai/waired-agent/proto/catalog"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// rungManifest is the anchor-shaped 262144-native manifest the tuning
// tests size everywhere: 22 GB of weights, 20480 B/tok of fp16 KV.
func rungManifest() (catalog.Manifest, catalog.Variant) {
	m := catalog.Manifest{
		ModelID:       "rung-moe-35b",
		ContextLength: 262144,
		Variants: []catalog.Variant{{
			VariantID:           "q4",
			RuntimeSupport:      []string{catalog.RuntimeOllama},
			EstimatedWeightGB:   22.0,
			KVBytesPerTokenFP16: 20480,
		}},
	}
	return m, m.Variants[0]
}

// TestOllamaPlannedRung pins the product contract from the 2026-08-08
// owner rulings on waired-ai/waired#1067 (waired-ai/waired-agent#587):
// the engine is started at a rung of OllamaServedWindows — the highest
// one the reachability rules pass, or the lowest one with Fits=false
// when none does — and never at a window between them.
func TestOllamaPlannedRung(t *testing.T) {
	m, v := rungManifest()

	t.Run("outright fit lands on the rung, not above it", func(t *testing.T) {
		// 24 GiB card, 21.5 GB weights: the no-spill window (~223k)
		// clears the rung, and the plan is the rung — the surplus is
		// reported as capacity, not served as window.
		lite := v
		lite.EstimatedWeightGB = 21.5
		h := hostfit.Host{RAMTotalGB: 64, GPUCount: 1, VRAM0MB: 24576}
		plan := hostfit.OllamaPlannedRung(m, lite, h, hostfit.OllamaKVFactorQ8_0, 0)
		if !plan.Fits || plan.ContextLength != hostfit.ServingWindow200k {
			t.Errorf("plan = %+v, want the fitting %d rung", plan, hostfit.ServingWindow200k)
		}
		if plan.ExpectedSpillFraction != 0 {
			t.Errorf("ExpectedSpillFraction = %v, want 0 for an outright fit", plan.ExpectedSpillFraction)
		}
		if plan.NoSpillCapacityTokens <= hostfit.ServingWindow200k {
			t.Errorf("NoSpillCapacityTokens = %d, want the surplus preserved", plan.NoSpillCapacityTokens)
		}
	})

	t.Run("bounded spill reaches the rung on a discrete card", func(t *testing.T) {
		// The anchor shape: 22 GB on the 24 GiB card spills a few percent
		// at the rung — within OllamaMaxExpectedSpillFraction, so the
		// rung is reachable and the cost is reported.
		h := hostfit.Host{RAMTotalGB: 64, GPUCount: 1, VRAM0MB: 24576}
		plan := hostfit.OllamaPlannedRung(m, v, h, hostfit.OllamaKVFactorQ8_0, 0)
		if !plan.Fits || plan.ContextLength != hostfit.ServingWindow200k {
			t.Errorf("plan = %+v, want the rung via bounded spill", plan)
		}
		if plan.ExpectedSpillFraction <= 0 || plan.ExpectedSpillFraction > hostfit.OllamaMaxExpectedSpillFraction {
			t.Errorf("ExpectedSpillFraction = %v, want within (0, %v]",
				plan.ExpectedSpillFraction, hostfit.OllamaMaxExpectedSpillFraction)
		}
	})

	t.Run("a card never shrinks the window below the card-less host", func(t *testing.T) {
		// 16 GiB card that cannot hold the weights, behind 64 GB of RAM
		// that alone reaches the rung: rule 3 keeps the rung reachable,
		// and the (large) spill is reported rather than hidden.
		h := hostfit.Host{RAMTotalGB: 64, GPUCount: 1, VRAM0MB: 16384}
		plan := hostfit.OllamaPlannedRung(m, v, h, hostfit.OllamaKVFactorQ8_0, 0)
		if !plan.Fits || plan.ContextLength != hostfit.ServingWindow200k {
			t.Errorf("plan = %+v, want the rung via the card-less floor", plan)
		}
		if plan.ExpectedSpillFraction <= 0 {
			t.Error("the rung is held partly in system RAM; the plan must say so")
		}
	})

	t.Run("uma below the rung is forced to it, not trimmed under it", func(t *testing.T) {
		// The carve-out holds ~158k beside the weights and unified memory
		// gets no spill rules — no rung passes, and the plan is the
		// lowest rung with Fits=false instead of a served 158k.
		h := hostfit.Host{RAMTotalGB: 32, GPUCount: 1, UnifiedMemory: true, UsableVRAMMB: 23552}
		plan := hostfit.OllamaPlannedRung(m, v, h, hostfit.OllamaKVFactorQ8_0, 0)
		if plan.Fits || plan.ContextLength != hostfit.ServingWindow200k {
			t.Errorf("plan = %+v, want the forced lowest rung with Fits=false", plan)
		}
		if plan.ExpectedSpillFraction <= 0 {
			t.Error("the forced rung oversubscribes the carve-out; the plan must say so")
		}
	})

	t.Run("cpu-only host that cannot hold the weights is forced to the rung", func(t *testing.T) {
		h := hostfit.Host{RAMTotalGB: 24}
		plan := hostfit.OllamaPlannedRung(m, v, h, hostfit.OllamaKVFactorQ8_0, 0)
		if plan.Fits || plan.ContextLength != hostfit.ServingWindow200k {
			t.Errorf("plan = %+v, want the forced lowest rung with Fits=false", plan)
		}
		if plan.ExpectedSpillFraction != 0 {
			t.Errorf("ExpectedSpillFraction = %v, want 0 — no accelerator to overflow", plan.ExpectedSpillFraction)
		}
	})

	t.Run("cpu-only fit charges the OS deduction and engine reservation", func(t *testing.T) {
		// 32 GB CPU-only: budget = 32 − 2 (OS deduction, unmeasured) −
		// ~1.9 (engine reservation at 21 GB) − 21 of weights ≈ 7 GB of
		// KV — hundreds of thousands of tokens, so the rung fits.
		lite := v
		lite.EstimatedWeightGB = 21.0
		h := hostfit.Host{RAMTotalGB: 32}
		plan := hostfit.OllamaPlannedRung(m, lite, h, hostfit.OllamaKVFactorQ8_0, 0)
		if !plan.Fits || plan.ContextLength != hostfit.ServingWindow200k {
			t.Errorf("plan = %+v, want the fitting rung", plan)
		}
	})

	t.Run("a 1M model steps down the ladder", func(t *testing.T) {
		mm, mv := rungManifest()
		mm.ContextLength = hostfit.ServingWindow1M
		mv.EstimatedWeightGB = 8.0
		mv.KVBytesPerTokenFP16 = 12288

		// 64 GB CPU-only holds the full 1M rung.
		big := hostfit.OllamaPlannedRung(mm, mv, hostfit.Host{RAMTotalGB: 64}, hostfit.OllamaKVFactorQ8_0, 0)
		if !big.Fits || big.ContextLength != hostfit.ServingWindow1M {
			t.Errorf("64 GB plan = %+v, want the 1M rung", big)
		}
		// An 8 GiB card that cannot hold even the weights, behind the
		// same RAM: 1M is only reachable outright (rules 2/3 stop at the
		// effective floor), so the plan steps down to the 200k rung —
		// which rule 3 keeps reachable.
		carded := hostfit.OllamaPlannedRung(mm, mv,
			hostfit.Host{RAMTotalGB: 64, GPUCount: 1, VRAM0MB: 8192}, hostfit.OllamaKVFactorQ8_0, 0)
		if !carded.Fits || carded.ContextLength != hostfit.ServingWindow200k {
			t.Errorf("8 GiB-card plan = %+v, want the 200k rung", carded)
		}
	})

	t.Run("sub-200k native model has one rung: its own window", func(t *testing.T) {
		sub, sv := rungManifest()
		sub.ContextLength = 131072
		h := hostfit.Host{RAMTotalGB: 64, GPUCount: 1, VRAM0MB: 24576}
		plan := hostfit.OllamaPlannedRung(sub, sv, h, hostfit.OllamaKVFactorQ8_0, 0)
		if plan.ContextLength != 131072 {
			t.Errorf("plan = %+v, want the native 131072 rung", plan)
		}
	})

	t.Run("ceiling caps the ladder for the verify step-down", func(t *testing.T) {
		mm, mv := rungManifest()
		mm.ContextLength = hostfit.ServingWindow1M
		mv.EstimatedWeightGB = 8.0
		mv.KVBytesPerTokenFP16 = 12288
		h := hostfit.Host{RAMTotalGB: 64}

		capped := hostfit.OllamaPlannedRung(mm, mv, h, hostfit.OllamaKVFactorQ8_0, hostfit.ServingWindow200k)
		if !capped.Fits || capped.ContextLength != hostfit.ServingWindow200k {
			t.Errorf("capped plan = %+v, want the 200k rung (host fits 1M, ceiling forbids it)", capped)
		}
		// A ceiling below the lowest rung keeps the lowest rung: the
		// ladder never empties.
		below := hostfit.OllamaPlannedRung(mm, mv, h, hostfit.OllamaKVFactorQ8_0, 32768)
		if below.ContextLength != hostfit.ServingWindow200k {
			t.Errorf("below-ladder ceiling plan = %+v, want the lowest rung kept", below)
		}
	})

	t.Run("unknown inputs plan nothing", func(t *testing.T) {
		h := hostfit.Host{RAMTotalGB: 64}
		for name, tc := range map[string]struct {
			m catalog.Manifest
			v catalog.Variant
			h hostfit.Host
		}{
			"no window annotation": {catalog.Manifest{ModelID: "x"}, v, h},
			"no weight":            {m, catalog.Variant{KVBytesPerTokenFP16: 20480}, h},
			"no kv figure":         {m, catalog.Variant{EstimatedWeightGB: 22}, h},
			"no memory at all":     {m, v, hostfit.Host{}},
		} {
			if plan := hostfit.OllamaPlannedRung(tc.m, tc.v, tc.h, hostfit.OllamaKVFactorQ8_0, 0); plan != (hostfit.OllamaRungPlan{}) {
				t.Errorf("%s: plan = %+v, want the zero plan", name, plan)
			}
		}
	})
}

// TestOllamaDeclaresWindow_RungGate pins the declaration side of the
// rung contract (waired-ai/waired#1031 window contract; waired#1067
// rulings): a forced rung is SERVED but never DECLARED — Fits=false
// reads as "does not declare", so the mesh never routes a 200k session
// to a host that cannot hold one.
func TestOllamaDeclaresWindow_RungGate(t *testing.T) {
	m, v := rungManifest()

	fits := hostfit.Host{RAMTotalGB: 64, GPUCount: 1, VRAM0MB: 24576}
	if !hostfit.OllamaDeclaresWindow(m, v, fits, hostfit.ServingWindow200k) {
		t.Error("a host that reaches the rung within the rules must declare it")
	}
	forced := hostfit.Host{RAMTotalGB: 32, GPUCount: 1, UnifiedMemory: true, UsableVRAMMB: 23552}
	if hostfit.OllamaDeclaresWindow(m, v, forced, hostfit.ServingWindow200k) {
		t.Error("a forced rung (Fits=false) must not be declared to the mesh")
	}
}
