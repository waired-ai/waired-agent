package router

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
)

func TestLighterCandidate_StepsDownFromHeaviest(t *testing.T) {
	// 24 GB vLLM host, active = large-vllm/awq-int4 (the heaviest fit).
	// The only lighter fitting variant is mid-vllm/awq-int4.
	hw := hardware.Profile{
		RAMTotalGB: 64,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 24467}},
	}
	pick, ok := LighterCandidate(
		PickInput{Catalog: fixtureCatalog(), Hardware: hw, Engine: "vllm"},
		"large-vllm", "awq-int4")
	if !ok {
		t.Fatalf("LighterCandidate = !ok, want a lighter pick")
	}
	if pick.Manifest.ModelID != "mid-vllm" || pick.Variant.VariantID != "awq-int4" {
		t.Errorf("got %s/%s, want mid-vllm/awq-int4", pick.Manifest.ModelID, pick.Variant.VariantID)
	}
}

func TestLighterCandidate_AlreadyLightest(t *testing.T) {
	// active = mid-vllm/awq-int4 (the lightest vLLM fit at 24 GB).
	hw := hardware.Profile{
		RAMTotalGB: 64,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 24467}},
	}
	_, ok := LighterCandidate(
		PickInput{Catalog: fixtureCatalog(), Hardware: hw, Engine: "vllm"},
		"mid-vllm", "awq-int4")
	if ok {
		t.Errorf("LighterCandidate = ok, want !ok (already lightest fitting)")
	}
}

func TestLighterCandidate_CPUSingleFit(t *testing.T) {
	// 6 GB RAM ollama host: only tiny-ollama fits (mid needs 12 GB).
	hw := hardware.Profile{RAMTotalGB: 6}
	_, ok := LighterCandidate(
		PickInput{Catalog: fixtureCatalog(), Hardware: hw, Engine: "ollama"},
		"tiny-ollama", "q4-gguf")
	if ok {
		t.Errorf("LighterCandidate = ok, want !ok (single fitting variant)")
	}
}

func TestLighterCandidate_CPUStepsDown(t *testing.T) {
	// 16 GB RAM ollama host: tiny + mid fit. From mid, step down to tiny.
	hw := hardware.Profile{RAMTotalGB: 16}
	pick, ok := LighterCandidate(
		PickInput{Catalog: fixtureCatalog(), Hardware: hw, Engine: "ollama"},
		"mid-ollama", "q4-gguf")
	if !ok {
		t.Fatalf("LighterCandidate = !ok, want tiny-ollama")
	}
	if pick.Manifest.ModelID != "tiny-ollama" {
		t.Errorf("got %s, want tiny-ollama", pick.Manifest.ModelID)
	}
}

func TestLighterCandidate_ActiveNotInCatalog(t *testing.T) {
	// active unknown → baseline is the top pick (large-vllm); the lighter
	// alternative mid-vllm/awq-int4 is still offered.
	hw := hardware.Profile{
		RAMTotalGB: 64,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 24467}},
	}
	pick, ok := LighterCandidate(
		PickInput{Catalog: fixtureCatalog(), Hardware: hw, Engine: "vllm"},
		"ghost-model", "ghost-variant")
	if !ok {
		t.Fatalf("LighterCandidate = !ok, want a lighter pick via baseline fallback")
	}
	if pick.Manifest.ModelID != "mid-vllm" {
		t.Errorf("got %s, want mid-vllm", pick.Manifest.ModelID)
	}
}

func TestFootprintCmp(t *testing.T) {
	v := func(w float64, vram, ram int, params int64) catalog.Variant {
		return catalog.Variant{EstimatedWeightGB: w, MinVRAMMB: vram, MinRAMGB: ram, ParamCount: params}
	}
	// Primary weight axis.
	if got := footprintCmp(v(5, 0, 0, 0), v(9, 0, 0, 0), "vllm"); got != -1 {
		t.Errorf("weight 5 vs 9 = %d, want -1", got)
	}
	// Weight axis skipped when either side is 0 → fall through to MinVRAMMB.
	if got := footprintCmp(v(0, 8000, 0, 0), v(5, 12000, 0, 0), "vllm"); got != -1 {
		t.Errorf("weight-unknown fallthrough to VRAM = %d, want -1", got)
	}
	// ollama uses MinRAMGB as the secondary axis.
	if got := footprintCmp(v(0, 0, 8, 0), v(0, 0, 4, 0), "ollama"); got != 1 {
		t.Errorf("RAM 8 vs 4 = %d, want 1", got)
	}
	// ParamCount final tiebreak.
	if got := footprintCmp(v(5, 8000, 0, 3_000_000_000), v(5, 8000, 0, 7_000_000_000), "vllm"); got != -1 {
		t.Errorf("param tiebreak = %d, want -1", got)
	}
	// Fully equal.
	if got := footprintCmp(v(5, 8000, 0, 3), v(5, 8000, 0, 3), "vllm"); got != 0 {
		t.Errorf("equal = %d, want 0", got)
	}
}

// siblingVariantCatalog mirrors the shape the shipped catalog actually
// has, and that waired-agent#754 was reported against: ONE manifest
// carrying two ollama variants that differ by engine feature rather
// than by weight class. qwen3.6-27b ships mtp-q4-gguf (18.0 GB,
// quality_tier 71) beside q4-gguf (16.3 GB, tier 70); the numbers here
// are scaled down so the fixture fits the same 16 GB host the other
// CPU cases in this file use, but the ordering is the reported one.
//
// light-ollama is the genuinely different, genuinely lighter model a
// step-down is supposed to land on.
func siblingVariantCatalog() []catalog.Manifest {
	return []catalog.Manifest{
		{
			ModelID: "dual-ollama", ContextLength: 32768,
			Capabilities: []string{"chat", "tool_use"},
			Variants: []catalog.Variant{
				{
					VariantID: "mtp-q4-gguf", Format: "ollama-tag",
					Quantization: "Q4_K_M", RuntimeSupport: []string{"ollama"},
					EstimatedWeightGB: 5.0, MinRAMGB: 12, QualityTier: 71,
					ParamCount: 27_000_000_000,
					Source:     catalog.VariantSource{Type: "ollama", Tag: "dual:27b-mtp-q4_K_M"},
				},
				{
					VariantID: "q4-gguf", Format: "ollama-tag",
					Quantization: "Q4_K_M", RuntimeSupport: []string{"ollama"},
					EstimatedWeightGB: 4.5, MinRAMGB: 12, QualityTier: 70,
					ParamCount: 27_000_000_000,
					Source:     catalog.VariantSource{Type: "ollama", Tag: "dual:27b-q4_K_M"},
				},
			},
		},
		{
			ModelID: "light-ollama", ContextLength: 32768,
			Capabilities: []string{"chat", "tool_use"},
			Variants: []catalog.Variant{{
				VariantID: "q4-gguf", Format: "ollama-tag",
				Quantization: "Q4_K_M", RuntimeSupport: []string{"ollama"},
				EstimatedWeightGB: 1.5, MinRAMGB: 4, QualityTier: 18,
				ParamCount: 2_000_000_000,
				Source:     catalog.VariantSource{Type: "ollama", Tag: "light:2b-q4_K_M"},
			}},
		},
	}
}

func TestLighterCandidate_NeverRecommendsTheActiveModel(t *testing.T) {
	// waired-agent#754: the sibling variant is the heaviest candidate
	// that is strictly lighter than the active one, so single-step-down
	// used to pick it — and every consumer of the pick is keyed by model
	// id, so the offer rendered as "X → X" and could not be applied.
	// A step-down has to land on a different model.
	hw := hardware.Profile{RAMTotalGB: 16}
	pick, ok := LighterCandidate(
		PickInput{Catalog: siblingVariantCatalog(), Hardware: hw, Engine: "ollama"},
		"dual-ollama", "mtp-q4-gguf")
	if !ok {
		t.Fatalf("LighterCandidate = !ok, want light-ollama")
	}
	if pick.Manifest.ModelID == "dual-ollama" {
		t.Errorf("recommended %s/%s as a lighter replacement for its own model",
			pick.Manifest.ModelID, pick.Variant.VariantID)
	}
	if pick.Manifest.ModelID != "light-ollama" {
		t.Errorf("got %s, want light-ollama", pick.Manifest.ModelID)
	}
}

func TestLighterCandidate_SiblingVariantIsNotAStepDown(t *testing.T) {
	// Same catalog with the alternative removed: the only thing lighter
	// than the active variant is another variant of the same model, and
	// that is not a step down — the two differ by engine feature, not
	// weight class. No recommendation at all is the right answer.
	cat := siblingVariantCatalog()[:1]
	hw := hardware.Profile{RAMTotalGB: 16}
	pick, ok := LighterCandidate(
		PickInput{Catalog: cat, Hardware: hw, Engine: "ollama"},
		"dual-ollama", "mtp-q4-gguf")
	if ok {
		t.Errorf("LighterCandidate = %s/%s, want !ok (the only lighter variant is a sibling)",
			pick.Manifest.ModelID, pick.Variant.VariantID)
	}
}

func TestLighterCandidate_RealCatalogReportedHost(t *testing.T) {
	// waired-agent#754 against the SHIPPED catalog and the host it was
	// reported on: an NVIDIA 24 GB card with 121 GB of system memory,
	// serving qwen3.6-27b at 20 tok/s under an ollama new enough for the
	// mtp variants.
	//
	// The trigger is an active variant this catalog cannot resolve — an
	// empty variant_id in state.json, or one that has since been renamed.
	// findCatalogVariant misses, the baseline falls back to ranked[0]
	// (qwen3.6-35b-a3b/mtp-q4-gguf at 22.6 GB — a different, HEAVIER
	// model), and the host's own 18.0 GB variant is then strictly lighter
	// than that baseline and wins the single-step-down. The offer named
	// the model the host was already running.
	//
	// The assertion is the invariant, not a named target: which model the
	// step-down lands on moves with the catalog, and pinning it here would
	// fail on the next catalog edit for no defect.
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	in := PickInput{
		Catalog: manifests,
		Hardware: hardware.Profile{
			RAMTotalGB: 121,
			GPUs:       []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 24564}},
		},
		Engine:        "ollama",
		EngineVersion: "0.31.2",
	}

	// Anti-vacuity: the fallback only reaches the active model when
	// ranked[0] is some heavier OTHER model. If the shipped catalog stops
	// ranking one there, this test no longer covers what it claims to.
	ranked, err := RankModels(in)
	if err != nil || len(ranked) == 0 {
		t.Fatalf("RankModels: %v (ranked=%d)", err, len(ranked))
	}
	if ranked[0].Manifest.ModelID == "qwen3.6-27b" {
		t.Fatalf("the shipped catalog now ranks the active model first on this host, "+
			"so the baseline fallback cannot reach it and this test proves nothing "+
			"(ranked[0]=%s/%s)", ranked[0].Manifest.ModelID, ranked[0].Variant.VariantID)
	}

	for _, activeVariantID := range []string{"", "mtp-q4-gguf", "retired-variant-id"} {
		pick, ok := LighterCandidate(in, "qwen3.6-27b", activeVariantID)
		if !ok {
			t.Errorf("active variant %q: no lighter candidate at all on the reported host",
				activeVariantID)
			continue
		}
		if pick.Manifest.ModelID == "qwen3.6-27b" {
			t.Errorf("active variant %q: recommended qwen3.6-27b/%s as a lighter "+
				"replacement for qwen3.6-27b", activeVariantID, pick.Variant.VariantID)
		}
	}
}
