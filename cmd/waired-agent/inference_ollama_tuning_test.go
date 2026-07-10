package main

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/router"
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
		// floor: plain clamp, no spill, no warning.
		v := m.Variants[0]
		v.EstimatedWeightGB = 21.5
		got := computeOllamaTuning(m, v, discrete24GB(), "q8_0")
		if got.ContextLength != 223232 {
			t.Errorf("ContextLength = %d, want 223232", got.ContextLength)
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
		// #670: when the floor window would spill past the speed cap
		// (single-thread CPU spill, #664), the tuner serves the largest
		// window that holds the cap instead of the full floor. 23 GB
		// weights on the 24 GiB card: floor spill ≈ 14.7% > cap.
		v := m.Variants[0]
		v.EstimatedWeightGB = 23.0
		got := computeOllamaTuning(m, v, discrete24GB(), "q8_0")
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
		// budget — even the floor spills, so keep the floor and warn.
		hw := hardware.Profile{
			RAMTotalGB: 64,
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

	t.Run("uma-96gb-full-window-and-parallel", func(t *testing.T) {
		hw := hardware.Profile{
			RAMTotalGB:    128,
			UnifiedMemory: true,
			UsableVRAMMB:  98304, // 96 GiB carve-out
		}
		got := computeOllamaTuning(m, m.Variants[0], hw, "q8_0")
		if got.ContextLength != m.ContextLength {
			t.Errorf("ContextLength = %d, want manifest %d", got.ContextLength, m.ContextLength)
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
		got := computeOllamaTuning(m, m.Variants[1], hw, "q8_0")
		// budget = 28 GB; leftover 7 GB → q8_0 → ~683k tokens → capped
		// at the manifest window.
		if got.ContextLength != m.ContextLength {
			t.Errorf("ContextLength = %d, want manifest %d", got.ContextLength, m.ContextLength)
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
		q8 := computeOllamaTuning(m, m.Variants[1], discrete24GB(), "q8_0")
		f16 := computeOllamaTuning(m, m.Variants[1], discrete24GB(), "f16")
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
		"OLLAMA_CONTEXT_LENGTH=262144",
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
