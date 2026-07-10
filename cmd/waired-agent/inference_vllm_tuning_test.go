package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/catalog/scoring"
	"github.com/waired-ai/waired-agent/internal/hardware"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

func vllmTuningFixture() (catalog.Manifest, catalog.Variant, hardware.Profile) {
	m := catalog.Manifest{ModelID: "gpt-oss-20b", ContextLength: 131072}
	v := catalog.Variant{
		VariantID:           "mxfp4-safetensors",
		EstimatedWeightGB:   14.0,
		KVBytesPerTokenFP16: 73728,
	}
	hw := hardware.Profile{
		GPUs: []hardware.GPU{{Vendor: "nvidia", Model: "NVIDIA L4", VRAMTotalMB: 23034}},
	}
	return m, v, hw
}

func TestComputeVLLMTuning_ClampsBelowNative(t *testing.T) {
	m, v, hw := vllmTuningFixture()
	// 1×L4 @ 0.85: ~19.5 GB budget (util×VRAM − per-GPU overhead) −
	// 14×1.15 GB weights → ~45k tokens (see router.TestVLLMMaxModelLen).
	maxLen, mt := computeVLLMTuning(m, v, hw, 1, 0.85, scoring.KVFactorF16)
	if maxLen != 45056 {
		t.Fatalf("maxLen = %d, want 45056", maxLen)
	}
	if mt.ContextLength != maxLen {
		t.Errorf("ModelTuning.ContextLength = %d, want %d", mt.ContextLength, maxLen)
	}
	if mt.ModelID != "gpt-oss-20b" || mt.VariantID != "mxfp4-safetensors" {
		t.Errorf("identity fields not filled: %+v", mt)
	}
	if !strings.Contains(mt.Warning, "clamped to 45056 tokens") ||
		!strings.Contains(mt.Warning, "131072") {
		t.Errorf("clamp warning should name both windows, got %q", mt.Warning)
	}
	// 131072-native manifests are below the coding-agent native floor,
	// so no sub-floor phrasing is appended for them (the floor gate never
	// admitted this model; the clamp itself is the whole story).
	if strings.Contains(mt.Warning, "coding") {
		t.Errorf("sub-native-floor manifest must not carry the coding-target phrasing, got %q", mt.Warning)
	}
}

func TestComputeVLLMTuning_SubFloorClampNamesCodingTarget(t *testing.T) {
	m, v, hw := vllmTuningFixture()
	m.ContextLength = 262144 // above the native floor → floor phrasing applies
	maxLen, mt := computeVLLMTuning(m, v, hw, 1, 0.85, scoring.KVFactorF16)
	if maxLen != 45056 {
		t.Fatalf("maxLen = %d, want 45056", maxLen)
	}
	if !strings.Contains(mt.Warning, "~200k coding") {
		t.Errorf("expected the ~200k coding-target phrasing, got %q", mt.Warning)
	}
}

func TestComputeVLLMTuning_NoClampWhenBudgetCovers(t *testing.T) {
	m, v, hw := vllmTuningFixture()
	// TP=2 doubles the budget past the 131072 native window.
	hw.GPUs = append(hw.GPUs, hw.GPUs[0])
	maxLen, mt := computeVLLMTuning(m, v, hw, 2, 0.85, scoring.KVFactorF16)
	if maxLen != m.ContextLength {
		t.Fatalf("maxLen = %d, want native %d", maxLen, m.ContextLength)
	}
	if mt.Warning != "" {
		t.Errorf("no warning expected when the native window fits, got %q", mt.Warning)
	}
	if mt.ContextLength != m.ContextLength {
		t.Errorf("ModelTuning.ContextLength = %d, want %d", mt.ContextLength, m.ContextLength)
	}
}

func TestComputeVLLMTuning_UnknownInputsPassThrough(t *testing.T) {
	m, v, hw := vllmTuningFixture()
	v.KVBytesPerTokenFP16 = 0 // sizing unknown → never guess
	maxLen, mt := computeVLLMTuning(m, v, hw, 1, 0.85, scoring.KVFactorF16)
	if maxLen != m.ContextLength {
		t.Fatalf("maxLen = %d, want manifest window %d", maxLen, m.ContextLength)
	}
	if mt.Warning != "" {
		t.Errorf("unknown inputs must not warn, got %q", mt.Warning)
	}
}

func TestComputeVLLMTuning_WeightsExceedBudgetWarns(t *testing.T) {
	m, v, hw := vllmTuningFixture()
	v.EstimatedWeightGB = 40.0 // padded weights alone exceed a single L4
	maxLen, mt := computeVLLMTuning(m, v, hw, 1, 0.85, scoring.KVFactorF16)
	if maxLen != m.ContextLength {
		t.Fatalf("maxLen = %d, want manifest window %d (no invented clamp)", maxLen, m.ContextLength)
	}
	if !strings.Contains(mt.Warning, "exceed") || !strings.Contains(mt.Warning, "engine.log") {
		t.Errorf("expected a weights-exceed-budget warning pointing at engine.log, got %q", mt.Warning)
	}
}

func TestComputeVLLMTuning_FP8DoublesTheClampedWindow(t *testing.T) {
	m, v, hw := vllmTuningFixture()
	m.ContextLength = 262144
	f16, _ := computeVLLMTuning(m, v, hw, 1, 0.85, scoring.KVFactorF16)
	fp8, mt := computeVLLMTuning(m, v, hw, 1, 0.85, scoring.KVFactorFP8)
	if fp8 <= f16 {
		t.Fatalf("fp8 window %d should exceed the f16 window %d", fp8, f16)
	}
	if fp8 != 90112 {
		t.Errorf("fp8 clamp = %d, want 90112 (halved KV → ~2× f16 45056)", fp8)
	}
	if mt.ContextLength != fp8 {
		t.Errorf("ModelTuning.ContextLength = %d, want %d", mt.ContextLength, fp8)
	}
}

func TestResolveVLLMKVCache(t *testing.T) {
	ada := hardware.Profile{GPUs: []hardware.GPU{
		{Vendor: "nvidia", Model: "NVIDIA L4", VRAMTotalMB: 23034, ComputeCap: "8.9"},
	}}
	ampere := hardware.Profile{GPUs: []hardware.GPU{
		{Vendor: "nvidia", Model: "NVIDIA A100", VRAMTotalMB: 81920, ComputeCap: "8.0"},
	}}

	t.Run("Ada default-on selects fp8", func(t *testing.T) {
		dtype, factor := resolveVLLMKVCache(ada, false)
		if dtype != "fp8" || factor != scoring.KVFactorFP8 {
			t.Errorf("Ada: got (%q, %v), want (fp8, %v)", dtype, factor, scoring.KVFactorFP8)
		}
	})
	t.Run("Ada opt-out falls back to fp16", func(t *testing.T) {
		dtype, factor := resolveVLLMKVCache(ada, true)
		if dtype != "" || factor != scoring.KVFactorF16 {
			t.Errorf("Ada opt-out: got (%q, %v), want (\"\", %v)", dtype, factor, scoring.KVFactorF16)
		}
	})
	t.Run("Ampere never engages fp8 even without opt-out", func(t *testing.T) {
		dtype, factor := resolveVLLMKVCache(ampere, false)
		if dtype != "" || factor != scoring.KVFactorF16 {
			t.Errorf("Ampere: got (%q, %v), want (\"\", %v)", dtype, factor, scoring.KVFactorF16)
		}
	})
}

func TestVLLMKVCacheDType(t *testing.T) {
	if got := vllmKVCacheDType(true); got != "fp8" {
		t.Errorf("vllmKVCacheDType(true) = %q, want \"fp8\"", got)
	}
	if got := vllmKVCacheDType(false); got != "" {
		t.Errorf("vllmKVCacheDType(false) = %q, want \"\" (omit flag)", got)
	}
}

func TestVLLMSpeculativeConfigJSON(t *testing.T) {
	if got := vllmSpeculativeConfigJSON(false); got != "" {
		t.Errorf("disabled must omit the flag, got %q", got)
	}
	got := vllmSpeculativeConfigJSON(true)
	if !strings.Contains(got, `"method":"ngram"`) {
		t.Errorf("enabled config must select the ngram method, got %q", got)
	}
	if !json.Valid([]byte(got)) {
		t.Errorf("speculative config must be valid JSON, got %q", got)
	}
}

func TestParseVLLMKVCapacityTokens(t *testing.T) {
	cases := []struct {
		name string
		log  string
		want int
	}{
		{
			"v1 kv_cache_utils line with thousands separators",
			"INFO 07-05 12:00:01 [kv_cache_utils.py:1087] GPU KV cache size: 152,192 tokens\n" +
				"INFO 07-05 12:00:01 [kv_cache_utils.py:1091] Maximum concurrency for 131,072 tokens per request: 1.16x\n",
			152192,
		},
		{"plain number", "GPU KV cache size: 45056 tokens", 45056},
		{
			"last occurrence wins across restarts",
			"GPU KV cache size: 10,240 tokens\nGPU KV cache size: 20,480 tokens\n",
			20480,
		},
		{"absent", "vllm serving started\n", 0},
		{"garbage number", "GPU KV cache size: many tokens", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseVLLMKVCapacityTokens(tc.log); got != tc.want {
				t.Errorf("parseVLLMKVCapacityTokens = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestApplyVLLMTuningVerification(t *testing.T) {
	base := infruntime.ModelTuning{ModelID: "m", VariantID: "v", ContextLength: 45056}

	t.Run("capacity line marks verified", func(t *testing.T) {
		mt := applyVLLMTuningVerification(base, "GPU KV cache size: 80,000 tokens\n")
		if !mt.Verified {
			t.Errorf("expected Verified=true")
		}
		if mt.Warning != base.Warning {
			t.Errorf("warning must not change when capacity covers the window, got %q", mt.Warning)
		}
	})

	t.Run("capacity below the window appends a warning", func(t *testing.T) {
		mt := applyVLLMTuningVerification(base, "GPU KV cache size: 40,960 tokens\n")
		if !mt.Verified {
			t.Errorf("expected Verified=true")
		}
		if !strings.Contains(mt.Warning, "40960") {
			t.Errorf("expected the measured capacity in the warning, got %q", mt.Warning)
		}
	})

	t.Run("absent line is inconclusive", func(t *testing.T) {
		mt := applyVLLMTuningVerification(base, "no capacity line here\n")
		if mt.Verified {
			t.Errorf("expected Verified=false on an inconclusive log")
		}
	})
}
