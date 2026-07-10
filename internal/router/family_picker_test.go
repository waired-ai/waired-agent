package router

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
)

// hostFamilyFixture returns synthetic manifests covering the family
// shapes the catalog endpoint cares about: ollama-only, vllm-only,
// dual-engine with multiple tiers.
func hostFamilyFixture() (ollamaOnly, vllmOnly, dual catalog.Manifest) {
	ollamaOnly = catalog.Manifest{
		ModelID:     "qwen3-4b-instruct",
		DisplayName: "Qwen3 4B Instruct",
		Variants: []catalog.Variant{{
			VariantID: "q4-gguf", Format: catalog.FormatOllamaTag,
			RuntimeSupport: []string{catalog.RuntimeOllama},
			MinRAMGB:       8, QualityTier: 35,
			Source: catalog.VariantSource{Type: catalog.SourceOllama, Tag: "qwen3:4b-q4"},
		}},
	}
	vllmOnly = catalog.Manifest{
		ModelID:     "qwen3-32b-instruct",
		DisplayName: "Qwen3 32B Instruct",
		Variants: []catalog.Variant{{
			VariantID: "awq-int4", Format: catalog.FormatSafetensors,
			Quantization:   "AWQ-int4",
			RuntimeSupport: []string{catalog.RuntimeVLLM},
			MinVRAMMB:      24576, QualityTier: 80,
			Source: catalog.VariantSource{Type: catalog.SourceHuggingFace, RepoID: "Qwen/Qwen3-32B-AWQ"},
		}},
	}
	dual = catalog.Manifest{
		ModelID:     "qwen3-8b-instruct",
		DisplayName: "Qwen3 8B Instruct",
		Variants: []catalog.Variant{
			{
				VariantID: "q4-gguf", Format: catalog.FormatOllamaTag,
				RuntimeSupport: []string{catalog.RuntimeOllama},
				MinRAMGB:       12, QualityTier: 50,
				Source: catalog.VariantSource{Type: catalog.SourceOllama, Tag: "qwen3:8b-q4"},
			},
			{
				VariantID: "awq-int4", Format: catalog.FormatSafetensors,
				Quantization:   "AWQ-int4",
				RuntimeSupport: []string{catalog.RuntimeVLLM},
				MinVRAMMB:      8000, QualityTier: 60,
				Source: catalog.VariantSource{Type: catalog.SourceHuggingFace, RepoID: "Qwen/Qwen3-8B-AWQ"},
			},
			{
				VariantID: "fp16", Format: catalog.FormatSafetensors,
				DType:          "float16",
				RuntimeSupport: []string{catalog.RuntimeVLLM},
				MinVRAMMB:      18000, QualityTier: 65,
				Source: catalog.VariantSource{Type: catalog.SourceHuggingFace, RepoID: "Qwen/Qwen3-8B"},
			},
		},
	}
	return
}

func TestFamilyBestFit_OllamaFits(t *testing.T) {
	o, _, _ := hostFamilyFixture()
	hw := hardware.Profile{RAMTotalGB: 32}
	got := FamilyBestFit(o, catalog.RuntimeOllama, "", hw)
	if !got.Fits {
		t.Fatalf("expected fit, got %+v", got)
	}
	if got.Variant.VariantID != "q4-gguf" {
		t.Errorf("variant: want q4-gguf, got %q", got.Variant.VariantID)
	}
	if got.DeficitLabel != "" {
		t.Errorf("deficit should be empty, got %q", got.DeficitLabel)
	}
}

func TestFamilyBestFit_OllamaShortRAM(t *testing.T) {
	o, _, _ := hostFamilyFixture()
	hw := hardware.Profile{RAMTotalGB: 4}
	got := FamilyBestFit(o, catalog.RuntimeOllama, "", hw)
	if got.Fits {
		t.Fatalf("expected no fit, got %+v", got)
	}
	if got.DeficitLabel == "" {
		t.Errorf("expected deficit label, got empty")
	}
	want := "needs 8 GB RAM (have 4 GB)"
	if got.DeficitLabel != want {
		t.Errorf("deficit: want %q, got %q", want, got.DeficitLabel)
	}
	// Even when no variant fits, the verdict carries the representative
	// variant so the catalog UI can still show recommended specs.
	if got.Variant.VariantID != "q4-gguf" {
		t.Errorf("no-fit representative variant: want q4-gguf, got %q", got.Variant.VariantID)
	}
}

func TestFamilyBestFit_VLLMPickHighestTier(t *testing.T) {
	_, _, d := hostFamilyFixture()
	// 24 GB host: both vllm variants fit (8000 MB and 18000 MB);
	// expect fp16 (tier=65) over awq-int4 (tier=60).
	hw := hardware.Profile{
		RAMTotalGB: 64,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 24576}},
	}
	got := FamilyBestFit(d, catalog.RuntimeVLLM, "", hw)
	if !got.Fits {
		t.Fatalf("expected fit, got %+v", got)
	}
	if got.Variant.VariantID != "fp16" {
		t.Errorf("variant: want fp16 (highest tier), got %q", got.Variant.VariantID)
	}
}

func TestFamilyBestFit_VLLMPickLowerTierWhenFP16Doesntfit(t *testing.T) {
	_, _, d := hostFamilyFixture()
	// 12 GB host: fp16 (18000 MB) doesn't fit, awq (8000 MB) does.
	hw := hardware.Profile{
		RAMTotalGB: 32,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 12288}},
	}
	got := FamilyBestFit(d, catalog.RuntimeVLLM, "", hw)
	if !got.Fits {
		t.Fatalf("expected fit, got %+v", got)
	}
	if got.Variant.VariantID != "awq-int4" {
		t.Errorf("variant: want awq-int4 (only fitting variant), got %q", got.Variant.VariantID)
	}
}

func TestFamilyBestFit_VLLMShortVRAM(t *testing.T) {
	_, v, _ := hostFamilyFixture()
	hw := hardware.Profile{
		RAMTotalGB: 32,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 8192}},
	}
	got := FamilyBestFit(v, catalog.RuntimeVLLM, "", hw)
	if got.Fits {
		t.Fatalf("expected no fit, got %+v", got)
	}
	want := "needs 24 GB VRAM (have 8 GB)"
	if got.DeficitLabel != want {
		t.Errorf("deficit: want %q, got %q", want, got.DeficitLabel)
	}
	if got.Variant.VariantID != "awq-int4" {
		t.Errorf("no-fit representative variant: want awq-int4, got %q", got.Variant.VariantID)
	}
}

// #678: FamilyBestFit budgets the TP aggregate on identical
// multi-NVIDIA hosts, both for the fit verdict and the deficit label.
func TestFamilyBestFit_VLLMMultiGPUAggregate(t *testing.T) {
	_, v, _ := hostFamilyFixture() // awq-int4 needs 24576 MB
	gpu := hardware.GPU{Vendor: "nvidia", Model: "NVIDIA RTX 4080", VRAMTotalMB: 16384}

	single := hardware.Profile{RAMTotalGB: 64, GPUs: []hardware.GPU{gpu}}
	if got := FamilyBestFit(v, catalog.RuntimeVLLM, "", single); got.Fits {
		t.Fatalf("24576 MB variant must not fit a single 16 GB GPU, got %+v", got)
	}

	// 2×16 GB: budget 2×(16384−1024) = 30720 MB ≥ 24576.
	dual := hardware.Profile{RAMTotalGB: 64, GPUs: []hardware.GPU{gpu, gpu}}
	if got := FamilyBestFit(v, catalog.RuntimeVLLM, "", dual); !got.Fits {
		t.Errorf("24576 MB variant should fit 2×16 GB via the TP=2 budget, got deficit %q", got.DeficitLabel)
	}
}

func TestFamilyBestFit_VLLMDeficitLabelAggregated(t *testing.T) {
	_, v, _ := hostFamilyFixture() // awq-int4 needs 24576 MB
	gpu := hardware.GPU{Vendor: "nvidia", Model: "NVIDIA GeForce RTX 3070", VRAMTotalMB: 8192}
	// 2×8 GB: budget 2×(8192−1024) = 14336 MB = 14 GB — still short.
	hw := hardware.Profile{RAMTotalGB: 32, GPUs: []hardware.GPU{gpu, gpu}}
	got := FamilyBestFit(v, catalog.RuntimeVLLM, "", hw)
	if got.Fits {
		t.Fatalf("expected no fit, got %+v", got)
	}
	want := "needs 24 GB VRAM (have 14 GB across 2 GPUs)"
	if got.DeficitLabel != want {
		t.Errorf("deficit: want %q, got %q", want, got.DeficitLabel)
	}
}

func TestFamilyBestFit_VLLMNoGPU(t *testing.T) {
	_, v, _ := hostFamilyFixture()
	hw := hardware.Profile{RAMTotalGB: 32}
	got := FamilyBestFit(v, catalog.RuntimeVLLM, "", hw)
	if got.Fits {
		t.Fatalf("expected no fit, got %+v", got)
	}
	want := "needs 24 GB VRAM (no GPU)"
	if got.DeficitLabel != want {
		t.Errorf("deficit: want %q, got %q", want, got.DeficitLabel)
	}
}

func TestFamilyBestFit_EngineNotSupportedByFamily(t *testing.T) {
	o, _, _ := hostFamilyFixture()
	// Asking for vllm on an ollama-only manifest.
	hw := hardware.Profile{RAMTotalGB: 64, GPUs: []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 40960}}}
	got := FamilyBestFit(o, catalog.RuntimeVLLM, "", hw)
	if got.Fits {
		t.Fatalf("expected no fit (engine mismatch), got %+v", got)
	}
	if got.DeficitLabel != "no variant supports vllm" {
		t.Errorf("deficit: want %q, got %q", "no variant supports vllm", got.DeficitLabel)
	}
	// No engine-supported variant exists, so there is no representative
	// variant to recommend.
	if got.Variant.VariantID != "" {
		t.Errorf("representative variant should be empty when engine unsupported, got %q", got.Variant.VariantID)
	}
}

func TestFamilyBestFit_OllamaUnknownRAMTreatedAsFit(t *testing.T) {
	// hostFits intentionally treats RAMTotalGB == 0 as "skip the
	// fit check rather than reject all" — verify FamilyBestFit
	// inherits that lenience instead of producing a misleading
	// "needs N GB RAM (have 0 GB)" deficit.
	o, _, _ := hostFamilyFixture()
	hw := hardware.Profile{RAMTotalGB: 0}
	got := FamilyBestFit(o, catalog.RuntimeOllama, "", hw)
	if !got.Fits {
		t.Fatalf("expected fit when RAM detection unavailable, got %+v", got)
	}
}
