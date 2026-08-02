package main

import (
	"strings"
	"testing"
)

func TestFormatCatalogDetail_VLLMHost(t *testing.T) {
	c := catalogDetailResp{
		Engine: "vllm",
	}
	c.Host.RAMTotalGB = 32
	c.Host.VRAMTotalMB = 12288
	c.Host.GPUModel = "RTX 3060"
	c.Families = []catalogDetailFamily{
		{
			ModelID: "qwen3-8b-instruct", Active: true, Downloaded: true, Fits: true,
			Recommended: &catalogDetailSpec{VariantID: "awq", MinVRAMMB: 8000, QualityTier: 60, ParamCount: 7_610_000_000},
		},
		{
			ModelID: "qwen3-32b-instruct", Fits: false, DeficitLabel: "needs 24 GB VRAM (have 12 GB)",
			Recommended: &catalogDetailSpec{VariantID: "awq-int4", MinVRAMMB: 24576, QualityTier: 80, ParamCount: 32_000_000_000},
		},
		{
			// MoE: active count differs from total.
			ModelID: "qwen3-coder-30b-a3b-instruct", Preferred: true, Fits: true,
			Recommended: &catalogDetailSpec{VariantID: "awq", MinVRAMMB: 24000, QualityTier: 68, ParamCount: 30_000_000_000, ActiveParams: 3_300_000_000},
		},
	}

	out := formatCatalogDetail(c)

	for _, want := range []string{
		"engine=vllm",
		"RTX 3060 12 GB VRAM / 32 GB RAM",
		"● qwen3-8b-instruct",
		"8 GB VRAM", // 8000 MB rounded up
		"✓ fits",
		"24 GB VRAM", // 24576 MB rounded up
		"✗ needs 24 GB VRAM (have 12 GB)",
		"→ qwen3-coder-30b-a3b-instruct",
		"30B (3.3B act)",
		"infer --explain",
		"reference/model-catalog",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
	// On a vllm host the recommended column reports VRAM; the only RAM
	// mention is the host header's total ("32 GB RAM"), never a per-model
	// recommendation derived from min_ram_gb.
	if strings.Contains(out, "8 GB RAM") {
		t.Errorf("vllm host should report VRAM, not a RAM recommendation:\n%s", out)
	}
}

func TestFormatCatalogDetail_OllamaHostShowsRAM(t *testing.T) {
	c := catalogDetailResp{Engine: "ollama"}
	c.Host.RAMTotalGB = 16
	c.Families = []catalogDetailFamily{
		{
			ModelID: "qwen2.5-coder-7b-instruct", Fits: true, Downloaded: true,
			Recommended: &catalogDetailSpec{VariantID: "q4-gguf", MinRAMGB: 8, QualityTier: 45, ParamCount: 7_610_000_000},
		},
	}
	out := formatCatalogDetail(c)
	if !strings.Contains(out, "16 GB RAM (no GPU)") {
		t.Errorf("expected no-GPU host header, got:\n%s", out)
	}
	if !strings.Contains(out, "8 GB RAM") {
		t.Errorf("ollama host should show RAM recommendation, got:\n%s", out)
	}
	if strings.Contains(out, "VRAM") {
		t.Errorf("ollama host should not mention VRAM, got:\n%s", out)
	}
	if !strings.Contains(out, "7.6B") {
		t.Errorf("expected humanized 7.6B params, got:\n%s", out)
	}
}

func TestHumanizeParams(t *testing.T) {
	cases := map[int64]string{
		7_610_000_000:   "7.6B",
		3_300_000_000:   "3.3B",
		30_000_000_000:  "30B",
		116_800_000_000: "117B",
		800_000_000:     "800M",
	}
	for in, want := range cases {
		if got := humanizeParams(in); got != want {
			t.Errorf("humanizeParams(%d) = %q, want %q", in, got, want)
		}
	}
}

// Product contract (waired-agent#321): NEEDS is the RESIDENT requirement
// — weights + the reserved KV budget + engine overhead, what the fit rule
// actually compares — not min_ram_gb. The two differ here on purpose so a
// regression to the old field is visible in the assertion.
func TestFormatCatalogDetail_NeedsColumnIsTheResidentFigure(t *testing.T) {
	c := catalogDetailResp{Engine: "ollama"}
	c.Host.RAMTotalGB = 64
	c.Host.VRAMTotalMB = 16303
	c.Host.GPUModel = "RTX 5080"
	c.Families = []catalogDetailFamily{
		{
			ModelID: "qwen3.6-27b", Fits: true, Downloaded: true,
			Recommended: &catalogDetailSpec{MinRAMGB: 32, QualityTier: 62},
			Fit:         &catalogDetailFit{Runnable: true, RequiredResidentMB: 18 * 1024, QualityTier: 62},
		},
	}
	out := formatCatalogDetail(c)
	if !strings.Contains(out, "18 GB VRAM") {
		t.Errorf("NEEDS must be the resident figure, got:\n%s", out)
	}
	if strings.Contains(out, "32 GB RAM") {
		t.Errorf("NEEDS must not fall back to min_ram_gb when a resident figure exists:\n%s", out)
	}
}

// Product contract, and the visibility gap #321 closes on this surface:
// waired-ai/waired#988 shipped the rule that keeps a weights-spilling
// model out of the automatic pick, and this view printed a bare "✓ fits"
// for exactly those models. The row stays RUNNABLE — annotated, never
// hidden and never marked unusable, which is what waired-agent#229
// removed.
func TestFormatCatalogDetail_MarksTheHostsOwnPickAndItsDemotions(t *testing.T) {
	c := catalogDetailResp{Engine: "ollama"}
	c.Host.RAMTotalGB = 64
	c.Host.VRAMTotalMB = 16303
	c.Host.GPUModel = "RTX 5080"
	c.Families = []catalogDetailFamily{
		{
			ModelID: "qwen3.5-9b", Fits: true, Downloaded: true, RecommendedPick: true,
			Fit: &catalogDetailFit{Runnable: true, RequiredResidentMB: 9 * 1024, QualityTier: 55},
		},
		{
			ModelID: "qwen3.6-35b-a3b", Fits: true,
			Fit: &catalogDetailFit{
				Runnable: true, RequiredResidentMB: 24 * 1024, QualityTier: 65,
				NotRecommended: true, NotRecommendedReason: "weights_spill",
			},
		},
		{
			ModelID: "deepseek-v4-flash", Fits: false, DeficitLabel: "no variant supports ollama",
			Fit: &catalogDetailFit{Reason: "no_variant_for_engine", QualityTier: 80},
		},
	}
	out := formatCatalogDetail(c)
	for _, want := range []string{
		"✓ fits · recommended",
		"✓ fits · not recommended (weights spill)",
		"✗ not available on this computer",
		"tier", // the raw quality number now has a column of its own
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
	// The blocked row must not print the wire's own sentence, which names
	// an engine and a "variant" to somebody who has heard of neither.
	if strings.Contains(out, "no variant supports") {
		t.Errorf("blocked row leaked internal vocabulary:\n%s", out)
	}
}
