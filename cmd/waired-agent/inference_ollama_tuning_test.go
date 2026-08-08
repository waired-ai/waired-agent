package main

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// qwen3.6-35b-a3b-shaped manifest: the dogfood host's real numbers, so
// the table cases double as a sanity check of the sizing on known
// hardware.
func tuningTestManifest() catalog.Manifest {
	return catalog.Manifest{
		ModelID:       "test-moe-35b",
		ContextLength: 262144,
		Variants: []catalog.Variant{
			{
				VariantID:           "mtp-q4",
				RuntimeSupport:      []string{catalog.RuntimeOllama},
				EstimatedWeightGB:   22.0,
				KVBytesPerTokenFP16: 20480,
			},
			{
				VariantID:           "q4",
				RuntimeSupport:      []string{catalog.RuntimeOllama},
				EstimatedWeightGB:   21.0,
				KVBytesPerTokenFP16: 20480,
			},
		},
	}
}

func discrete24GB() hardware.Profile {
	return hardware.Profile{
		RAMTotalGB: 64,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 24576}},
	}
}

// tinyCoderManifest carries qwen2.5-coder-0.5b-instruct's measured
// numbers: the model whose llama-server segfaulted under q8_0 + flash
// attention on a CPU-only runner (waired-agent#29), which is what these
// cases are about.
//
// A FROZEN RECORD, not a catalog reference. That manifest was retired at
// #200 and the routing sentinel has pinned granite4-350m since #381; the
// numbers stay because they are the ones #29 was measured against, and
// re-basing them onto another model would quietly change what every
// assertion below is testing.
func tinyCoderManifest() catalog.Manifest {
	return catalog.Manifest{
		ModelID:       "qwen2.5-coder-0.5b-instruct",
		ContextLength: 32768,
		Variants: []catalog.Variant{{
			VariantID:           "q4_K_M",
			RuntimeSupport:      []string{catalog.RuntimeOllama},
			EstimatedWeightGB:   0.4,
			KVBytesPerTokenFP16: 12288,
		}},
	}
}

// ciRunner16GB is the hosted ubuntu-24.04 CI runner: CPU-only, 16 GB.
func ciRunner16GB() hardware.Profile {
	return hardware.Profile{RAMTotalGB: 16}
}

func TestComputeOllamaTuning(t *testing.T) {
	m := tuningTestManifest()

	t.Run("discrete-24gb-intentional-spill-to-floor", func(t *testing.T) {
		// Weight-scaled overhead (#624): 1024 + 40×22.0 = 1904 MiB →
		// no-spill window ≈ 173k < the 200704 coding floor, but the
		// expected spill at the floor (~3%) stays under the speed cap
		// (#670: OllamaIntentionalSpillCapExpected) → the floor is
		// served deliberately, parallel stays 1, and the warning is
		// informational (never reads as an error).
		got := computeOllamaTuning(m, m.Variants[0], discrete24GB(), "q8_0")
		if got.ContextLength != 200704 {
			t.Errorf("ContextLength = %d, want floor 200704", got.ContextLength)
		}
		if got.NumParallel != 1 {
			t.Errorf("NumParallel = %d, want 1 (spilling window must not double KV)", got.NumParallel)
		}
		if got.ExpectedSpillFraction <= 0 || got.ExpectedSpillFraction > router.OllamaIntentionalSpillCapExpected {
			t.Errorf("ExpectedSpillFraction = %.4f, want within (0, %.3f]",
				got.ExpectedSpillFraction, router.OllamaIntentionalSpillCapExpected)
		}
		if !strings.Contains(got.Warning, "expected to sit in system RAM") {
			t.Errorf("warning should state the planned spill: %q", got.Warning)
		}
		for _, bad := range []string{"error", "fail", "degraded"} {
			if strings.Contains(strings.ToLower(got.Warning), bad) {
				t.Errorf("warning must not read as an error (%q): %q", bad, got.Warning)
			}
		}
		if got.KVCacheType != "q8_0" {
			t.Errorf("KVCacheType = %q, want q8_0", got.KVCacheType)
		}
	})

	t.Run("discrete-24gb-nospill-clamp-above-floor", func(t *testing.T) {
		// A 21.5 GB variant's no-spill window (≈ 223k) already clears the
		// floor: plain clamp, no spill, no warning. Since #552 the clamp
		// lands on the top rung rather than on 223232 — a window between
		// the rungs is not one this product serves.
		v := m.Variants[0]
		v.EstimatedWeightGB = 21.5
		got := computeOllamaTuning(m, v, discrete24GB(), "q8_0")
		if got.ContextLength != hostfit.ServingWindow200k {
			t.Errorf("ContextLength = %d, want %d", got.ContextLength, hostfit.ServingWindow200k)
		}
		if got.ExpectedSpillFraction != 0 {
			t.Errorf("ExpectedSpillFraction = %.4f, want 0", got.ExpectedSpillFraction)
		}
		if got.Warning != "" {
			t.Errorf("unexpected warning: %q", got.Warning)
		}
	})

	t.Run("uma-below-floor-keeps-nospill-window", func(t *testing.T) {
		// UMA gets no bounded-spill allowance: a carve-out whose no-spill
		// window is under the floor keeps that window (no intentional
		// spill, no floor warning).
		hw := hardware.Profile{
			RAMTotalGB:    32,
			UnifiedMemory: true,
			UsableVRAMMB:  23552,
		}
		got := computeOllamaTuning(m, m.Variants[0], hw, "q8_0")
		// budget = (23552 − 1024) MiB ≈ 23.62 GB − 22 GB → ≈ 158k.
		if got.ContextLength >= 200704 || got.ContextLength < ollamaContextFloor {
			t.Errorf("ContextLength = %d, want the no-spill window below the floor", got.ContextLength)
		}
		if got.ExpectedSpillFraction != 0 {
			t.Errorf("ExpectedSpillFraction = %.4f, want 0 on UMA", got.ExpectedSpillFraction)
		}
	})

	t.Run("subfloor-manifest-spills-to-native-window", func(t *testing.T) {
		// A preferred 131072-native model gates on its own window: the
		// intentional spill aims at 131072, not 200704.
		sub := m
		sub.ContextLength = 131072
		hw := hardware.Profile{
			RAMTotalGB: 64,
			GPUs:       []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 23800}},
		}
		// no-spill ≈ (23800−1904) MiB ≈ 23.0 GB − 22 GB → ~93k < 131072;
		// expected spill at 131072 ≈ 3×(25.3−25.0)/25.3 ≈ 4.5% ≤ the
		// speed cap, so the native window is served in full.
		got := computeOllamaTuning(sub, sub.Variants[0], hw, "q8_0")
		if got.ContextLength != 131072 {
			t.Errorf("ContextLength = %d, want native 131072", got.ContextLength)
		}
		if got.ExpectedSpillFraction <= 0 {
			t.Error("expected an intentional-spill record for the sub-floor window")
		}
	})

	t.Run("spill-past-speed-cap-serves-capped-window", func(t *testing.T) {
		// #670/#765: when the floor window would spill past the speed
		// cap (single-thread CPU spill, #664), the tuner serves the
		// largest window that holds the cap instead of the full floor.
		// 23.7 GB weights on the 24 GiB card: floor spill ≈ 22% > the
		// 0.20 cap. (At the pre-#765 0.075 cap a 23 GB fixture spilling
		// ≈ 14.7% exercised this branch; gate-passing variants now
		// serve the full floor, so only heavier overshoots trim.)
		//
		// 28 GB of RAM rather than the 64 this used to carry. The cap is
		// no longer the last word: hostfit.OllamaPlannedWindow's rule 3
		// will not size a window below what the same machine would reach
		// with the card removed, and 60 GB of system RAM reaches the full
		// floor outright. Only a host whose RAM cannot reach it either
		// still exercises the trim, which is the case this test is for —
		// see the sibling below for what a roomy host does now.
		v := m.Variants[0]
		v.EstimatedWeightGB = 23.7
		hw := discrete24GB()
		hw.RAMTotalGB = 28
		got := computeOllamaTuning(m, v, hw, "q8_0")
		if got.ContextLength >= 200704 || got.ContextLength <= ollamaContextFloor {
			t.Errorf("ContextLength = %d, want a speed-capped window between %d and the floor",
				got.ContextLength, ollamaContextFloor)
		}
		if got.ExpectedSpillFraction <= 0 || got.ExpectedSpillFraction > router.OllamaIntentionalSpillCapExpected {
			t.Errorf("ExpectedSpillFraction = %.4f, want within (0, %.3f]",
				got.ExpectedSpillFraction, router.OllamaIntentionalSpillCapExpected)
		}
		if !strings.Contains(got.Warning, "below the ~200k coding target") ||
			!strings.Contains(got.Warning, "tok/s floor") {
			t.Errorf("warning should explain the speed-capped window: %q", got.Warning)
		}
		for _, bad := range []string{"error", "fail", "degraded"} {
			if strings.Contains(strings.ToLower(got.Warning), bad) {
				t.Errorf("warning must not read as an error (%q): %q", bad, got.Warning)
			}
		}
	})

	t.Run("discrete-16gb-weights-exceed-budget-floors", func(t *testing.T) {
		// mtp variant: 22 GB weights over a (16384 − 1904) MiB ≈ 15.2 GB
		// budget — even the floor spills, so keep the engine floor and
		// warn.
		//
		// 24 GB of RAM rather than 64, for the reason the sibling above
		// gives: with 60 GB of RAM behind the card the machine would
		// serve the full coding window from system memory with the card
		// removed, so rule 3 has it serve that window WITH the card too.
		// This branch is now the genuinely-cornered host — too little of
		// either kind of memory — which is what it always meant to
		// describe.
		hw := hardware.Profile{
			RAMTotalGB: 24,
			GPUs:       []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 16384}},
		}
		got := computeOllamaTuning(m, m.Variants[0], hw, "q8_0")
		if got.ContextLength != ollamaContextFloor {
			t.Errorf("ContextLength = %d, want floor %d", got.ContextLength, ollamaContextFloor)
		}
		if got.Warning == "" {
			t.Error("expected a spill warning when even the floor doesn't fit")
		}
		if got.NumParallel != 1 {
			t.Errorf("NumParallel = %d, want 1", got.NumParallel)
		}
	})

	t.Run("a-card-never-shrinks-the-window-below-the-card-less-host", func(t *testing.T) {
		// The same 16 GB card, on a machine with enough RAM that removing
		// the card entirely would still serve the full coding window.
		// hostfit.OllamaPlannedWindow's rule 3: fitting a card may not
		// cost the machine a window it would otherwise have had.
		//
		// It costs spill, and the figure is reported rather than hidden —
		// the verify pass widens its tolerance to twice the planned
		// fraction, so a plan that under-reported here would be restarted
		// straight back down to a smaller window.
		//
		// Ratifying source: owner statement on waired-ai/waired#1056
		// (2026-08-03 review, and the 2026-08-04 follow-up) that a host
		// with a GPU must not be offered less than the same host without
		// one, because GPU inference is the faster of the two.
		hw := hardware.Profile{
			RAMTotalGB: 64,
			GPUs:       []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 16384}},
		}
		cardless := hardware.Profile{RAMTotalGB: 64}

		bare := computeOllamaTuning(m, m.Variants[0], cardless, "q8_0")
		carded := computeOllamaTuning(m, m.Variants[0], hw, "q8_0")
		if bare.ContextLength < router.CodingAgentContextFloorTokens {
			t.Fatalf("fixture: the card-less host serves %d, so it does not exercise "+
				"the floor this test is about", bare.ContextLength)
		}
		// The coding window, not the card-less window. Rule 3 stops there
		// on purpose: 200,704 and the card-less host's 262,144 are the
		// same DECLARED window (hostfit.ServingWindow200k is what a
		// requester routes on), so the extra 61k would buy local context
		// nothing routes on at the price of more of the model sitting in
		// system RAM. Without the card this host would answer a coding
		// session; with it, it still must.
		if carded.ContextLength < router.CodingAgentContextFloorTokens {
			t.Errorf("with a 16 GB card the window is %d; with no card at all it is %d, "+
				"which clears the ~%d coding window. Fitting a card must not cost the "+
				"machine that window",
				carded.ContextLength, bare.ContextLength, router.CodingAgentContextFloorTokens)
		}
		if carded.ExpectedSpillFraction <= 0 {
			t.Error("the widened window is held partly in system RAM; the plan must say " +
				"so, or the verify pass will read the measured spill as a failure")
		}
	})

	t.Run("uma-96gb-full-window-and-parallel", func(t *testing.T) {
		hw := hardware.Profile{
			RAMTotalGB:    128,
			UnifiedMemory: true,
			UsableVRAMMB:  98304, // 96 GiB carve-out
		}
		got := computeOllamaTuning(m, m.Variants[0], hw, "q8_0")
		// The ceiling, not the manifest's 262144: #552 stopped serving
		// context the mesh cannot route on. The second slot still lands,
		// which is the thing this case is really about.
		if got.ContextLength != hostfit.OllamaCeilingWindow(m) {
			t.Errorf("ContextLength = %d, want ceiling %d", got.ContextLength, hostfit.OllamaCeilingWindow(m))
		}
		if got.NumParallel != 2 {
			t.Errorf("NumParallel = %d, want 2 (full window granted and 2× KV fits)", got.NumParallel)
		}
		if got.Warning != "" {
			t.Errorf("unexpected warning: %q", got.Warning)
		}
	})

	t.Run("cpu-only-uses-ram-budget", func(t *testing.T) {
		hw := hardware.Profile{RAMTotalGB: 32}
		got := computeOllamaTuning(m, m.Variants[1], hw, ollamaKVAuto)
		// budget = 28 GB; leftover 7 GB → q8_0 → ~683k tokens → capped
		// at the served ceiling (#552), not the manifest window.
		if got.ContextLength != hostfit.OllamaCeilingWindow(m) {
			t.Errorf("ContextLength = %d, want ceiling %d", got.ContextLength, hostfit.OllamaCeilingWindow(m))
		}
		// PRODUCT CONTRACT: a genuinely tight CPU host KEEPS the quantized KV
		// cache. f16 here affords only ~341k tokens, short of 2 x 262144, so
		// quantizing is buying real context. This pins that waired-agent#29's
		// fix is "only when it buys context", not "CPU means f16".
		if got.KVCacheType != "q8_0" || !got.FlashAttention {
			t.Errorf("KVCacheType/FlashAttention = %q/%v, want q8_0/true on a tight CPU host",
				got.KVCacheType, got.FlashAttention)
		}
	})

	// PRODUCT CONTRACT (waired-agent#29): the CI runner reproduced exactly.
	// Quantizing here saves ~400 MB of a 12 GB budget while forcing
	// llama.cpp's CPU + flash-attention + quantized-KV path, where the
	// llama-server segfault lives. Everything else about the sizing must be
	// untouched — especially NumParallel, which is why the f16 test uses the
	// same 2x threshold the slot grant does.
	t.Run("cpu-only-small-model-drops-quantized-kv", func(t *testing.T) {
		tm := tinyCoderManifest()
		got := computeOllamaTuning(tm, tm.Variants[0], ciRunner16GB(), ollamaKVAuto)
		if got.KVCacheType != "f16" {
			t.Errorf("KVCacheType = %q, want f16 (f16 affords ~943k tokens vs the 65k served)", got.KVCacheType)
		}
		if got.FlashAttention {
			t.Error("FlashAttention = true, want false: there is no quantized cache to protect")
		}
		if got.ContextLength != tm.ContextLength {
			t.Errorf("ContextLength = %d, want the full manifest window %d", got.ContextLength, tm.ContextLength)
		}
		if got.NumParallel != ollamaMaxAutoParallel {
			t.Errorf("NumParallel = %d, want %d — choosing f16 must not cost a request slot",
				got.NumParallel, ollamaMaxAutoParallel)
		}
		if got.NumBatch != 0 {
			t.Errorf("NumBatch = %d, want 0 on CPU-only", got.NumBatch)
		}
		if got.Warning != "" {
			t.Errorf("unexpected warning: %q", got.Warning)
		}
	})

	// PRODUCT CONTRACT: GPU hosts are bit-for-bit unchanged. The discrete
	// overhead model (proto/hostfit) is calibrated against a flash-attention
	// load, so dropping FA there would silently invalidate the spill
	// reservation.
	t.Run("gpu-auto-matches-pinned-q8", func(t *testing.T) {
		auto := computeOllamaTuning(m, m.Variants[1], discrete24GB(), ollamaKVAuto)
		pinned := computeOllamaTuning(m, m.Variants[1], discrete24GB(), "q8_0")
		if auto != pinned {
			t.Errorf("auto sizing on a discrete GPU differs from the q8_0 pin:\n auto   = %+v\n pinned = %+v", auto, pinned)
		}
		if !auto.FlashAttention {
			t.Error("FlashAttention = false on a discrete GPU, want true")
		}
	})

	t.Run("uma-auto-keeps-quantized-kv", func(t *testing.T) {
		// Reaches the VRAM branch via UsableVRAMMB, not GPUs.
		hw := hardware.Profile{RAMTotalGB: 128, UnifiedMemory: true, UsableVRAMMB: 98304}
		got := computeOllamaTuning(m, m.Variants[0], hw, ollamaKVAuto)
		if got.KVCacheType != "q8_0" || !got.FlashAttention {
			t.Errorf("KVCacheType/FlashAttention = %q/%v, want q8_0/true on UMA",
				got.KVCacheType, got.FlashAttention)
		}
	})

	// PRODUCT CONTRACT: an explicit f16 (the verify pass's degrade) must not
	// carry flash attention — there is no quantized cache left to protect.
	t.Run("explicit-f16-pin-drops-flash-attention", func(t *testing.T) {
		got := computeOllamaTuning(m, m.Variants[1], discrete24GB(), "f16")
		if got.KVCacheType != "f16" {
			t.Errorf("KVCacheType = %q, want the f16 pin honoured", got.KVCacheType)
		}
		if got.FlashAttention {
			t.Error("FlashAttention = true on an f16 pin, want false")
		}
	})

	// PRODUCT CONTRACT: never change behaviour on a host we cannot size.
	t.Run("unknown-sizing-keeps-quantized-kv", func(t *testing.T) {
		v := catalog.Variant{VariantID: "unknown", RuntimeSupport: []string{catalog.RuntimeOllama}}
		got := computeOllamaTuning(m, v, hardware.Profile{RAMTotalGB: 32}, ollamaKVAuto)
		if got.KVCacheType != "q8_0" || !got.FlashAttention {
			t.Errorf("KVCacheType/FlashAttention = %q/%v, want q8_0/true when the sizing inputs are unknown",
				got.KVCacheType, got.FlashAttention)
		}
		if got.ContextLength != 0 {
			t.Errorf("ContextLength = %d, want 0 (never guess a window)", got.ContextLength)
		}
	})

	t.Run("unknown-kv-leaves-context-unset", func(t *testing.T) {
		v := catalog.Variant{VariantID: "no-kv", EstimatedWeightGB: 21.0}
		got := computeOllamaTuning(m, v, discrete24GB(), "q8_0")
		if got.ContextLength != 0 {
			t.Errorf("ContextLength = %d, want 0 (never guess a window)", got.ContextLength)
		}
		if got.NumParallel != 1 || got.KVCacheType != "q8_0" {
			t.Errorf("KV type / parallel should still be set: %+v", got)
		}
	})

	t.Run("small-manifest-window-not-inflated", func(t *testing.T) {
		small := catalog.Manifest{ModelID: "coder-32k", ContextLength: 32768}
		v := catalog.Variant{VariantID: "q4", EstimatedWeightGB: 9.0, KVBytesPerTokenFP16: 49152}
		got := computeOllamaTuning(small, v, discrete24GB(), "q8_0")
		if got.ContextLength != 32768 {
			t.Errorf("ContextLength = %d, want manifest cap 32768", got.ContextLength)
		}
		// leftover ≈ 12.5 GB → q8_0 @ 24576 B/tok ≈ 507k ≥ 2×32768:
		// the full window is granted, so the surplus may buy parallelism.
		if got.NumParallel != 2 {
			t.Errorf("NumParallel = %d, want 2", got.NumParallel)
		}
	})

	t.Run("f16-pass-shrinks-window", func(t *testing.T) {
		// A 131072-native model on a UMA host: the ceiling is the model's
		// own window and there is no spill allowance to push either pass
		// up to it, so the two cache formats can be compared on their own
		// terms. On the 262144-native fixture both now clamp to the same
		// rung and the case would compare nothing (#552).
		sub := m
		sub.ContextLength = 131072
		uma := hardware.Profile{RAMTotalGB: 32, UnifiedMemory: true, UsableVRAMMB: 23552}
		q8 := computeOllamaTuning(sub, sub.Variants[1], uma, "q8_0")
		f16 := computeOllamaTuning(sub, sub.Variants[1], uma, "f16")
		if f16.ContextLength >= q8.ContextLength {
			t.Errorf("f16 sizing (%d) should be smaller than q8_0 (%d)",
				f16.ContextLength, q8.ContextLength)
		}
		if f16.KVCacheType != "f16" {
			t.Errorf("KVCacheType = %q, want f16", f16.KVCacheType)
		}
	})
}

// #642: the forced generation ubatch (num_batch=2048) is set only on the
// spilled discrete-GPU config — where Ollama's automatic batch sizing
// would otherwise fall back to 512. Every non-spilled path leaves it 0
// (automatic), and it is never delivered via env.
func TestComputeOllamaTuningNumBatch(t *testing.T) {
	m := tuningTestManifest()

	t.Run("spilled-discrete-forces-2048", func(t *testing.T) {
		got := computeOllamaTuning(m, m.Variants[0], discrete24GB(), "q8_0")
		if got.ExpectedSpillFraction <= 0 {
			t.Fatalf("fixture must take the intentional-spill branch: %+v", got.ModelTuning)
		}
		if got.NumBatch != ollamaLargeBatch {
			t.Errorf("NumBatch = %d, want %d on the spilled discrete config", got.NumBatch, ollamaLargeBatch)
		}
	})

	t.Run("nospill-discrete-leaves-auto", func(t *testing.T) {
		v := m.Variants[0]
		v.EstimatedWeightGB = 21.5 // no-spill window clears the floor
		got := computeOllamaTuning(m, v, discrete24GB(), "q8_0")
		if got.ExpectedSpillFraction != 0 {
			t.Fatalf("fixture should not spill: %+v", got.ModelTuning)
		}
		if got.NumBatch != 0 {
			t.Errorf("NumBatch = %d, want 0 (automatic) when GPU-resident", got.NumBatch)
		}
	})

	t.Run("uma-leaves-auto", func(t *testing.T) {
		hw := hardware.Profile{RAMTotalGB: 32, UnifiedMemory: true, UsableVRAMMB: 23552}
		got := computeOllamaTuning(m, m.Variants[0], hw, "q8_0")
		if got.NumBatch != 0 {
			t.Errorf("NumBatch = %d, want 0 on UMA (no spill semantics)", got.NumBatch)
		}
	})

	t.Run("cpu-only-leaves-auto", func(t *testing.T) {
		got := computeOllamaTuning(m, m.Variants[1], hardware.Profile{RAMTotalGB: 32}, "q8_0")
		if got.NumBatch != 0 {
			t.Errorf("NumBatch = %d, want 0 on CPU-only", got.NumBatch)
		}
	})

	t.Run("env-never-carries-num-batch", func(t *testing.T) {
		got := computeOllamaTuning(m, m.Variants[0], discrete24GB(), "q8_0")
		if got.NumBatch == 0 {
			t.Fatal("precondition: expected a forced batch on this config")
		}
		for _, kv := range got.Env() {
			if strings.Contains(strings.ToLower(kv), "batch") {
				t.Errorf("Env() must not deliver num_batch (delivered via derived model): %q", kv)
			}
		}
	})
}

func TestOllamaTuningEnv(t *testing.T) {
	m := tuningTestManifest()
	// q4 21 GB weights on the 24 GiB card: weight-scaled overhead leaves
	// ~275k tokens of KV headroom, so the manifest window (262144) is
	// granted in full (parallel stays 1 — 2× KV would not fit).
	tn := computeOllamaTuning(m, m.Variants[1], discrete24GB(), "q8_0")
	env := tn.Env()
	for _, want := range []string{
		fmt.Sprintf("OLLAMA_CONTEXT_LENGTH=%d", hostfit.OllamaCeilingWindow(m)),
		"OLLAMA_KV_CACHE_TYPE=q8_0",
		"OLLAMA_NUM_PARALLEL=1",
		"OLLAMA_FLASH_ATTENTION=1",
	} {
		found := false
		for _, kv := range env {
			if kv == want {
				found = true
			}
		}
		if !found {
			t.Errorf("Env() missing %q: %v", want, env)
		}
	}

	// Unknown sizing: the context var is omitted, everything else stays.
	unsized := computeOllamaTuning(m, catalog.Variant{VariantID: "no-kv"}, discrete24GB(), "q8_0")
	for _, kv := range unsized.Env() {
		if strings.HasPrefix(kv, "OLLAMA_CONTEXT_LENGTH=") {
			t.Errorf("context var should be omitted when sizing is unknown: %v", unsized.Env())
		}
	}

	// PRODUCT CONTRACT (waired-agent#29): on a host that does not need the
	// quantized cache, OLLAMA_FLASH_ATTENTION is not exported at all — the
	// engine picks. Exporting =0 was rejected: f16 + engine-chosen FA is the
	// upstream default that every non-waired user runs, i.e. the
	// best-exercised configuration.
	t.Run("cpu-only-auto-omits-flash-attention", func(t *testing.T) {
		tm := tinyCoderManifest()
		env := computeOllamaTuning(tm, tm.Variants[0], ciRunner16GB(), ollamaKVAuto).Env()
		for _, want := range []string{
			"OLLAMA_CONTEXT_LENGTH=32768",
			"OLLAMA_KV_CACHE_TYPE=f16",
			"OLLAMA_NUM_PARALLEL=2",
		} {
			if !slices.Contains(env, want) {
				t.Errorf("Env() missing %q: %v", want, env)
			}
		}
		for _, kv := range env {
			if strings.HasPrefix(kv, "OLLAMA_FLASH_ATTENTION=") {
				t.Errorf("flash attention must not be exported on an f16 cache: %v", env)
			}
		}
	})
}

// TestPlanOllamaKV is the seam-level table: the decision, isolated from the
// sizing that consumes it. Both sides of the boundary are pinned, because the
// whole point of the rule is that it turns over exactly where quantizing stops
// buying context.
func TestPlanOllamaKV(t *testing.T) {
	tm := tinyCoderManifest()
	tv := tm.Variants[0]
	m := tuningTestManifest()

	// want = 32768 (the manifest window), so the boundary is 2*32768 = 65536
	// f16 tokens. kv/tok = 12288 at f16, weights 0.4 GB, so a budget B gives
	// (B*1e9 - 0.4e9)/12288 tokens.
	//   65536 tokens exactly  -> 0.4e9 + 65536*12288 = 1.2054e9 -> 5.2054 GB
	//                            budget => RAMTotalGB 9.2054 (headroom 4)
	atBoundary := hardware.Profile{RAMTotalGB: 10} // budget 6 GB -> ~455k >= 65536
	belowBoundary := hardware.Profile{RAMTotalGB: 5}

	cases := []struct {
		name      string
		m         catalog.Manifest
		v         catalog.Variant
		hw        hardware.Profile
		requested string
		want      ollamaKVPlan
	}{
		{"pin-q8_0", tm, tv, ciRunner16GB(), "q8_0", ollamaKVPlan{Type: "q8_0", FlashAttention: true}},
		{"pin-q4_0", tm, tv, ciRunner16GB(), "q4_0", ollamaKVPlan{Type: "q4_0", FlashAttention: true}},
		{"pin-f16", tm, tv, ciRunner16GB(), "f16", ollamaKVPlan{Type: "f16"}},
		{"auto-cpu-roomy", tm, tv, ciRunner16GB(), ollamaKVAuto, ollamaKVPlan{Type: "f16"}},
		{"auto-cpu-at-boundary", tm, tv, atBoundary, ollamaKVAuto, ollamaKVPlan{Type: "f16"}},
		{"auto-cpu-below-boundary", tm, tv, belowBoundary, ollamaKVAuto, ollamaKVPlan{Type: "q8_0", FlashAttention: true}},
		{"auto-gpu-via-gpus", m, m.Variants[1], discrete24GB(), ollamaKVAuto, ollamaKVPlan{Type: "q8_0", FlashAttention: true}},
		{
			"auto-gpu-via-usable-vram", m, m.Variants[0],
			hardware.Profile{RAMTotalGB: 128, UnifiedMemory: true, UsableVRAMMB: 98304},
			ollamaKVAuto, ollamaKVPlan{Type: "q8_0", FlashAttention: true},
		},
		{
			"auto-unsizable-host", tm, catalog.Variant{VariantID: "no-kv"},
			hardware.Profile{RAMTotalGB: 16}, ollamaKVAuto,
			ollamaKVPlan{Type: "q8_0", FlashAttention: true},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := planOllamaKV(c.m, c.v, c.hw, c.requested); got != c.want {
				t.Errorf("planOllamaKV(%s) = %+v, want %+v", c.name, got, c.want)
			}
		})
	}
}

// TestOllamaKVRequest pins the nightly override seam that keeps q8_0 + flash
// attention exercised against a real engine (the GPU CI runner is vLLM-only,
// so without this the combination would have no real-engine coverage at all).
func TestOllamaKVRequest(t *testing.T) {
	cases := map[string]string{
		"":        ollamaKVAuto,
		"q8_0":    "q8_0",
		"f16":     "f16",
		"q4_0":    "q4_0",
		" Q8_0 ":  "q8_0",
		"garbage": ollamaKVAuto,
	}
	for in, want := range cases {
		t.Run("in="+in, func(t *testing.T) {
			t.Setenv(ollamaKVOverrideEnv, in)
			if got := ollamaKVRequest(); got != want {
				t.Errorf("ollamaKVRequest() with %q = %q, want %q", in, got, want)
			}
		})
	}
}

func TestResolveTuningTarget(t *testing.T) {
	manifests := []catalog.Manifest{
		tuningTestManifest(),
		{
			ModelID:       "bundled-default",
			ContextLength: 131072,
			Variants: []catalog.Variant{{
				VariantID:           "q4",
				RuntimeSupport:      []string{catalog.RuntimeOllama},
				EstimatedWeightGB:   9.0,
				KVBytesPerTokenFP16: 49152,
			}},
		},
	}

	t.Run("preferred-wins", func(t *testing.T) {
		cfg := agentconfig.InferenceConfig{PreferredModelID: "test-moe-35b", BundledModelID: "bundled-default"}
		m, v, ok := resolveTuningTarget(cfg, manifests, catalog.State{})
		if !ok || m.ModelID != "test-moe-35b" {
			t.Fatalf("got %q ok=%v, want test-moe-35b", m.ModelID, ok)
		}
		if v.VariantID != "mtp-q4" {
			t.Errorf("variant = %q, want first pullable mtp-q4", v.VariantID)
		}
	})

	t.Run("active-selection-and-ready-variant", func(t *testing.T) {
		cfg := agentconfig.InferenceConfig{BundledModelID: "bundled-default"}
		state := catalog.State{
			Active: &catalog.ActiveSelection{Runtime: catalog.RuntimeOllama, ModelID: "test-moe-35b"},
			Models: map[string]catalog.ModelState{
				// The pull that's actually on disk is the plain q4, not
				// the manifest-first mtp variant — size for what serves.
				"test-moe-35b": {VariantID: "q4", State: catalog.ModelStateReady},
			},
		}
		m, v, ok := resolveTuningTarget(cfg, manifests, state)
		if !ok || m.ModelID != "test-moe-35b" {
			t.Fatalf("got %q ok=%v, want test-moe-35b", m.ModelID, ok)
		}
		if v.VariantID != "q4" {
			t.Errorf("variant = %q, want ready q4", v.VariantID)
		}
	})

	// THE #320 DRIFT, stated as a transition. PRODUCT CONTRACT: the same
	// inputs resolve a DIFFERENT variant before and after the model lands
	// on disk, so a tuning computed pre-Ready is stale by construction and
	// something must recompute it. The reconcile fired from endPull is
	// that something; this pins why it has to exist.
	t.Run("the resolved variant changes on the ready transition", func(t *testing.T) {
		cfg := agentconfig.InferenceConfig{PreferredModelID: "test-moe-35b"}

		// Before the download: nothing on disk, so the guess is whatever
		// the manifest offers first for the pinned engine version.
		_, before, ok := resolveTuningTarget(cfg, manifests, catalog.State{})
		if !ok {
			t.Fatal("no target resolved before the download")
		}

		// After: the pull recorded the variant it actually fetched, which
		// need not be the guess — variant choice depends on the engine
		// version the pull saw, and fails closed before one is known.
		ready := catalog.State{Models: map[string]catalog.ModelState{
			"test-moe-35b": {VariantID: "q4", State: catalog.ModelStateReady},
		}}
		_, after, ok := resolveTuningTarget(cfg, manifests, ready)
		if !ok {
			t.Fatal("no target resolved after the download")
		}

		if before.VariantID != "mtp-q4" || after.VariantID != "q4" {
			t.Fatalf("before=%q after=%q, want mtp-q4 then q4 — if these ever "+
				"agree this subtest no longer demonstrates the drift it guards",
				before.VariantID, after.VariantID)
		}
	})

	t.Run("vllm-active-ignored", func(t *testing.T) {
		cfg := agentconfig.InferenceConfig{BundledModelID: "bundled-default"}
		state := catalog.State{
			Active: &catalog.ActiveSelection{Runtime: catalog.RuntimeVLLM, ModelID: "test-moe-35b"},
		}
		m, _, ok := resolveTuningTarget(cfg, manifests, state)
		if !ok || m.ModelID != "bundled-default" {
			t.Fatalf("got %q ok=%v, want bundled-default (vLLM active must not steer ollama tuning)", m.ModelID, ok)
		}
	})

	t.Run("nothing-resolvable", func(t *testing.T) {
		if _, _, ok := resolveTuningTarget(agentconfig.InferenceConfig{}, manifests, catalog.State{}); ok {
			t.Error("ok = true with no preferred/active/bundled model")
		}
	})
}

func TestModelDecisionReasons(t *testing.T) {
	m := tuningTestManifest()

	t.Run("intentional-spill", func(t *testing.T) {
		tn := computeOllamaTuning(m, m.Variants[0], discrete24GB(), "q8_0")
		reasons, extra := modelDecisionReasons(agentconfig.InferenceConfig{}, m, tn)
		if extra != "" {
			t.Errorf("no extra warning expected (the tuning already carries one): %q", extra)
		}
		if len(reasons) != 1 || !strings.Contains(reasons[0], "expected in system RAM") {
			t.Errorf("reasons = %v", reasons)
		}
	})

	t.Run("preferred-subfloor-override", func(t *testing.T) {
		sub := m
		sub.ContextLength = 131072
		tn := computeOllamaTuning(sub, sub.Variants[0], discrete24GB(), "q8_0")
		tn.ExpectedSpillFraction = 0 // isolate the native-floor case
		_, extra := modelDecisionReasons(agentconfig.InferenceConfig{PreferredModelID: sub.ModelID}, sub, tn)
		if !strings.Contains(extra, "overrides the ~200k coding-agent context floor") {
			t.Errorf("extra = %q", extra)
		}
	})

	t.Run("stale-config-subfloor", func(t *testing.T) {
		sub := m
		sub.ContextLength = 32768
		tn := computeOllamaTuning(sub, sub.Variants[0], discrete24GB(), "q8_0")
		tn.ExpectedSpillFraction = 0
		_, extra := modelDecisionReasons(agentconfig.InferenceConfig{}, sub, tn)
		if !strings.Contains(extra, "best-effort serving") {
			t.Errorf("extra = %q", extra)
		}
	})

	t.Run("full-window", func(t *testing.T) {
		tn := computeOllamaTuning(m, m.Variants[1], discrete24GB(), "q8_0") // 262144 granted
		reasons, extra := modelDecisionReasons(agentconfig.InferenceConfig{}, m, tn)
		if extra != "" || len(reasons) != 1 || !strings.Contains(reasons[0], "fully GPU-resident") {
			t.Errorf("reasons=%v extra=%q", reasons, extra)
		}
	})
}
