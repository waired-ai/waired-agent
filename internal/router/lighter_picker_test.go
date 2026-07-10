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
