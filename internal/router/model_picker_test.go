package router

import (
	"errors"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
)

// fixtureCatalog returns a small synthetic catalog that exercises the
// dimensions the picker is supposed to discriminate on: tier ladder,
// engine-mismatch filter, VRAM/RAM fit, and capability filter.
func fixtureCatalog() []catalog.Manifest {
	return []catalog.Manifest{
		{
			ModelID: "tiny-ollama", ContextLength: 32768,
			Capabilities: []string{"chat"},
			Variants: []catalog.Variant{{
				VariantID: "q4-gguf", Format: "ollama-tag",
				Quantization: "Q4_K_M", RuntimeSupport: []string{"ollama"},
				EstimatedWeightGB: 1.5, MinRAMGB: 4, QualityTier: 18,
				Source: catalog.VariantSource{Type: "ollama", Tag: "tiny:1.7b"},
			}},
		},
		{
			ModelID: "mid-ollama", ContextLength: 32768,
			Capabilities: []string{"chat", "tool_use"},
			Variants: []catalog.Variant{{
				VariantID: "q4-gguf", Format: "ollama-tag",
				Quantization: "Q4_K_M", RuntimeSupport: []string{"ollama"},
				EstimatedWeightGB: 5.0, MinRAMGB: 12, QualityTier: 42,
				Source: catalog.VariantSource{Type: "ollama", Tag: "mid:8b-q4_K_M"},
			}},
		},
		{
			ModelID: "mid-vllm", ContextLength: 32768,
			Capabilities: []string{"chat", "json_mode", "tool_use"},
			Variants: []catalog.Variant{
				{
					VariantID: "awq-int4", Format: "safetensors",
					Quantization: "AWQ-int4", RuntimeSupport: []string{"vllm"},
					EstimatedWeightGB: 9.5, MinVRAMMB: 12000, QualityTier: 60,
					Source: catalog.VariantSource{Type: "huggingface", RepoID: "Qwen/Mid-AWQ"},
				},
				{
					VariantID: "fp16-safetensors", Format: "safetensors",
					DType: "float16", RuntimeSupport: []string{"vllm"},
					EstimatedWeightGB: 28.0, MinVRAMMB: 32000, QualityTier: 65,
					Source: catalog.VariantSource{Type: "huggingface", RepoID: "Qwen/Mid"},
				},
			},
		},
		{
			ModelID: "large-vllm", ContextLength: 32768,
			Capabilities: []string{"chat", "tool_use"},
			Variants: []catalog.Variant{{
				VariantID: "awq-int4", Format: "safetensors",
				Quantization: "AWQ-int4", RuntimeSupport: []string{"vllm"},
				EstimatedWeightGB: 22.0, MinVRAMMB: 24000, QualityTier: 78,
				Source: catalog.VariantSource{Type: "huggingface", RepoID: "Qwen/Large-AWQ"},
			}},
		},
		{
			ModelID: "huge-vllm", ContextLength: 32768,
			Capabilities: []string{"chat", "tool_use"},
			Variants: []catalog.Variant{{
				VariantID: "awq-int4", Format: "safetensors",
				Quantization: "AWQ-int4", RuntimeSupport: []string{"vllm"},
				EstimatedWeightGB: 120.0, MinVRAMMB: 130000, QualityTier: 95,
				Source: catalog.VariantSource{Type: "huggingface", RepoID: "Qwen/Huge-AWQ"},
			}},
		},
	}
}

func TestPickModel_Blackwell24GB_vllm(t *testing.T) {
	hw := hardware.Profile{
		RAMTotalGB: 64,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", Model: "RTX PRO 4000 Blackwell", VRAMTotalMB: 24467}},
	}
	pick, err := PickModel(PickInput{
		Catalog:  fixtureCatalog(),
		Hardware: hw,
		Engine:   "vllm",
	})
	if err != nil {
		t.Fatalf("PickModel: %v", err)
	}
	if pick.Manifest.ModelID != "large-vllm" {
		t.Errorf("ModelID = %q, want large-vllm (tier 78 fits 24467 MB)", pick.Manifest.ModelID)
	}
	if pick.Variant.VariantID != "awq-int4" {
		t.Errorf("VariantID = %q, want awq-int4", pick.Variant.VariantID)
	}
}

// #678: on identical multi-NVIDIA hosts the vllm fit gate budgets the
// TP-aggregated VRAM, so variants beyond a single device become
// selectable.
func TestRankModels_MultiGPU_VLLMBudgetAggregates(t *testing.T) {
	gpu := hardware.GPU{Vendor: "nvidia", Model: "RTX PRO 4000 Blackwell", VRAMTotalMB: 24467}
	single := hardware.Profile{RAMTotalGB: 64, GPUs: []hardware.GPU{gpu}}
	dual := hardware.Profile{RAMTotalGB: 64, GPUs: []hardware.GPU{gpu, gpu}}

	hasVariant := func(hw hardware.Profile, modelID, variantID string) bool {
		t.Helper()
		ranked, err := RankModels(PickInput{Catalog: fixtureCatalog(), Hardware: hw, Engine: "vllm"})
		if err != nil {
			t.Fatalf("RankModels: %v", err)
		}
		for _, p := range ranked {
			if p.Manifest.ModelID == modelID && p.Variant.VariantID == variantID {
				return true
			}
		}
		return false
	}

	// mid-vllm/fp16-safetensors needs 32000 MB: over a single 24 GB
	// device, within the 2×(24467−1024)=46886 MB TP=2 budget.
	if hasVariant(single, "mid-vllm", "fp16-safetensors") {
		t.Errorf("fp16-safetensors (32000 MB) should NOT fit a single 24 GB GPU")
	}
	if !hasVariant(dual, "mid-vllm", "fp16-safetensors") {
		t.Errorf("fp16-safetensors (32000 MB) should fit 2×24 GB via the TP=2 aggregate budget")
	}
	// huge-vllm (130000 MB) stays out of reach either way.
	if hasVariant(dual, "huge-vllm", "awq-int4") {
		t.Errorf("huge-vllm (130000 MB) must not fit 2×24 GB")
	}
}

// #678: the winner trace reports the aggregated budget on TP>1 hosts
// instead of the misleading single-GPU figure.
func TestPickModel_MultiGPU_VLLMReasonReportsTPBudget(t *testing.T) {
	gpu := hardware.GPU{Vendor: "nvidia", Model: "RTX PRO 4000 Blackwell", VRAMTotalMB: 24467}
	pick, err := PickModel(PickInput{
		Catalog:  fixtureCatalog(),
		Hardware: hardware.Profile{RAMTotalGB: 64, GPUs: []hardware.GPU{gpu, gpu}},
		Engine:   "vllm",
	})
	if err != nil {
		t.Fatalf("PickModel: %v", err)
	}
	if pick.Manifest.ModelID != "large-vllm" {
		t.Errorf("ModelID = %q, want large-vllm (tier 78 still the highest fitting tier)", pick.Manifest.ModelID)
	}
	found := false
	for _, r := range pick.Reasons {
		if strings.Contains(r, "TP=2") && strings.Contains(r, "46886 MB") {
			found = true
		}
	}
	if !found {
		t.Errorf("reasons missing the TP-aggregated budget line; got %q", pick.Reasons)
	}
}

func TestPickModel_CPUHostMid_ollama(t *testing.T) {
	hw := hardware.Profile{RAMTotalGB: 16, GPUs: nil}
	pick, err := PickModel(PickInput{
		Catalog:  fixtureCatalog(),
		Hardware: hw,
		Engine:   "ollama",
	})
	if err != nil {
		t.Fatalf("PickModel: %v", err)
	}
	if pick.Manifest.ModelID != "mid-ollama" {
		t.Errorf("ModelID = %q, want mid-ollama (tier 42, fits 16 GB RAM)", pick.Manifest.ModelID)
	}
}

func TestPickModel_CPULowEnd_ollama(t *testing.T) {
	hw := hardware.Profile{RAMTotalGB: 6, GPUs: nil}
	pick, err := PickModel(PickInput{
		Catalog:  fixtureCatalog(),
		Hardware: hw,
		Engine:   "ollama",
	})
	if err != nil {
		t.Fatalf("PickModel: %v", err)
	}
	if pick.Manifest.ModelID != "tiny-ollama" {
		t.Errorf("ModelID = %q, want tiny-ollama (mid-ollama needs 12 GB)", pick.Manifest.ModelID)
	}
}

func TestPickModel_CapabilityFilter(t *testing.T) {
	// require json_mode → only mid-vllm has it; with 24 GB VRAM
	// mid-vllm/awq-int4 (tier 60) wins (large-vllm lacks json_mode).
	hw := hardware.Profile{
		RAMTotalGB: 64,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 24467}},
	}
	pick, err := PickModel(PickInput{
		Catalog:           fixtureCatalog(),
		Hardware:          hw,
		Engine:            "vllm",
		RequireCapability: []string{"json_mode"},
	})
	if err != nil {
		t.Fatalf("PickModel: %v", err)
	}
	if pick.Manifest.ModelID != "mid-vllm" || pick.Variant.VariantID != "awq-int4" {
		t.Errorf("got %s/%s, want mid-vllm/awq-int4", pick.Manifest.ModelID, pick.Variant.VariantID)
	}
}

func TestPickModel_VRAMFit_FallsToLowerTier(t *testing.T) {
	// 12 GB GPU: large-vllm (24 GB) is too big, mid-vllm/awq-int4 (12 GB) wins.
	hw := hardware.Profile{
		RAMTotalGB: 32,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 12288}},
	}
	pick, err := PickModel(PickInput{
		Catalog:  fixtureCatalog(),
		Hardware: hw,
		Engine:   "vllm",
	})
	if err != nil {
		t.Fatalf("PickModel: %v", err)
	}
	if pick.Manifest.ModelID != "mid-vllm" || pick.Variant.VariantID != "awq-int4" {
		t.Errorf("got %s/%s, want mid-vllm/awq-int4 (12 GB GPU rejects large-vllm)", pick.Manifest.ModelID, pick.Variant.VariantID)
	}
}

func TestPickModel_ExplicitModelOverride(t *testing.T) {
	// PreferredModelID forces a specific manifest; if that manifest has
	// multiple variants, the highest-tier fitting one is still chosen.
	hw := hardware.Profile{
		RAMTotalGB: 64,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 24467}},
	}
	pick, err := PickModel(PickInput{
		Catalog:          fixtureCatalog(),
		Hardware:         hw,
		Engine:           "vllm",
		PreferredModelID: "mid-vllm",
	})
	if err != nil {
		t.Fatalf("PickModel: %v", err)
	}
	if pick.Manifest.ModelID != "mid-vllm" || pick.Variant.VariantID != "awq-int4" {
		t.Errorf("got %s/%s, want mid-vllm/awq-int4 (fp16 needs 32 GB)", pick.Manifest.ModelID, pick.Variant.VariantID)
	}
}

func TestPickModel_PreferredMissing(t *testing.T) {
	hw := hardware.Profile{
		RAMTotalGB: 64,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 24467}},
	}
	_, err := PickModel(PickInput{
		Catalog:          fixtureCatalog(),
		Hardware:         hw,
		Engine:           "vllm",
		PreferredModelID: "does-not-exist",
	})
	if !errors.Is(err, ErrModelNotFound) {
		t.Errorf("err = %v, want ErrModelNotFound", err)
	}
}

func TestPickModel_NothingFits(t *testing.T) {
	// 0.5 GB GPU: every vllm variant rejected.
	hw := hardware.Profile{
		RAMTotalGB: 8,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 512}},
	}
	_, err := PickModel(PickInput{
		Catalog:  fixtureCatalog(),
		Hardware: hw,
		Engine:   "vllm",
	})
	if !errors.Is(err, ErrHardwareInsufficient) {
		t.Errorf("err = %v, want ErrHardwareInsufficient", err)
	}
}

func TestPickModel_EmptyEngine(t *testing.T) {
	_, err := PickModel(PickInput{
		Catalog:  fixtureCatalog(),
		Hardware: hardware.Profile{RAMTotalGB: 64},
		Engine:   "",
	})
	if err == nil {
		t.Errorf("expected error when Engine is empty")
	}
}

func TestPickModel_Reasons(t *testing.T) {
	hw := hardware.Profile{
		RAMTotalGB: 64,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 24467}},
	}
	pick, err := PickModel(PickInput{
		Catalog:  fixtureCatalog(),
		Hardware: hw,
		Engine:   "vllm",
	})
	if err != nil {
		t.Fatalf("PickModel: %v", err)
	}
	if len(pick.Reasons) == 0 {
		t.Errorf("expected non-empty Reasons trace")
	}
	// At least one reason should mention the chosen tier so an operator
	// reading "waired runtimes status" can audit the decision.
	gotTier := false
	for _, r := range pick.Reasons {
		if strings_contains_lower(r, "tier") {
			gotTier = true
			break
		}
	}
	if !gotTier {
		t.Errorf("Reasons should mention quality_tier: %+v", pick.Reasons)
	}
}

// TestPickModel_BundledCatalog_Blackwell ties the picker to the real
// bundled catalog so a future refactor of either side breaks loudly.
// A 24 GB Blackwell card pairs with qwen3.6-27b/awq-int4 (tier 72):
// the manifest's context_length was corrected to the HF-card native
// 262,144 (#670), so the #624 floor no longer excludes it and the
// highest-tier vLLM fit wins again.
func TestPickModel_BundledCatalog_Blackwell(t *testing.T) {
	ms, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	hw := hardware.Profile{
		RAMTotalGB: 64,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", Model: "RTX PRO 4000 Blackwell", VRAMTotalMB: 24467}},
	}
	pick, err := PickModel(PickInput{Catalog: ms, Hardware: hw, Engine: "vllm"})
	if err != nil {
		t.Fatalf("PickModel: %v", err)
	}
	if pick.Manifest.ModelID != "qwen3.6-27b" || pick.Variant.VariantID != "awq-int4" {
		t.Errorf("Blackwell 24 GB picked %s/%s, want qwen3.6-27b/awq-int4 (262144-native after the #670 manifest fix)",
			pick.Manifest.ModelID, pick.Variant.VariantID)
	}
}

// TestPickModel_BundledCatalog_HardwareTiers exercises the picker
// against the real coding-agent bundled catalog across every host
// class the Auto Selector is supposed to handle. Failures here mean
// either the catalog ranking is off or the picker's fit logic drifted
// — both are operator-visible regressions worth pinning.
//
// The expected outcome at each tier is derived from
// docs/reports/20260516-coding-model-scoring.md §4.4
// (hardware-tier → manifest mapping). The picker's quality_tier-desc
// ordering should land on the highest-tier variant that fits the
// VRAM / RAM envelope.
func TestPickModel_BundledCatalog_HardwareTiers(t *testing.T) {
	ms, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	cases := []struct {
		name          string
		hw            hardware.Profile
		engine        string
		engineVersion string
		wantModel     string
		wantVariant   string
	}{
		{
			name: "8GB NVIDIA dGPU (RTX 3060/4060)",
			hw: hardware.Profile{
				RAMTotalGB: 32,
				GPUs:       []hardware.GPU{{Vendor: "nvidia", Model: "RTX 4060", VRAMTotalMB: 8000}},
			},
			engine:      "vllm",
			wantModel:   "qwen2.5-coder-7b-instruct",
			wantVariant: "awq-int4",
		},
		{
			name: "16GB NVIDIA dGPU (RTX 4060 Ti)",
			hw: hardware.Profile{
				RAMTotalGB: 32,
				GPUs:       []hardware.GPU{{Vendor: "nvidia", Model: "RTX 4060 Ti", VRAMTotalMB: 16000}},
			},
			engine:      "vllm",
			wantModel:   "qwen2.5-coder-14b-instruct",
			wantVariant: "awq-int4",
		},
		{
			name: "24GB NVIDIA dGPU (RTX 4090)",
			hw: hardware.Profile{
				RAMTotalGB: 64,
				GPUs:       []hardware.GPU{{Vendor: "nvidia", Model: "RTX 4090", VRAMTotalMB: 24000}},
			},
			engine: "vllm",
			// #670: qwen3.6-27b is 262144-native (the earlier 131072 in
			// the manifest was wrong — HF card says 262,144), so the
			// context floor keeps it and the tier-72 AWQ build wins over
			// the 30B coder MoE (tier 68).
			wantModel:   "qwen3.6-27b",
			wantVariant: "awq-int4",
		},
		{
			name: "80GB NVIDIA H100",
			hw: hardware.Profile{
				RAMTotalGB: 256,
				GPUs:       []hardware.GPU{{Vendor: "nvidia", Model: "H100 80GB HBM3", VRAMTotalMB: 80000}},
			},
			engine: "vllm",
			// #624: gpt-oss-120b (tier 88) is 131072-native and excluded
			// by the context floor; the best 262144-native vllm fit is
			// qwen3-coder-next-80b (tier 82).
			wantModel:   "qwen3-coder-next-80b-a3b-instruct",
			wantVariant: "awq-int4",
		},
		{
			// Real Ryzen AI Max+ 395 carve-out: 128 GB installed, 96 GB
			// fixed to the iGPU at the BIOS level, so the OS sees only
			// ~31 GB as system RAM. The MinRAMGB gate is skipped on UMA
			// hosts (else every 96/128 GB-min MoE would be rejected by the
			// 31 GB system RAM) and the residency check against the 96 GB
			// pool governs. The coding-first Ollama lineup makes
			// qwen3.6-35b-a3b (SWE-bench V 73.4%, 3B active) the highest
			// quality_tier that fits — its faster mtp variant (tier 90) is
			// floored to Ollama >= 0.30 and excluded here because the test
			// supplies no EngineVersion, so the plain q4-gguf (tier 89)
			// wins. At runtime, with a known engine version >= 0.30, the
			// mtp variant is selected instead.
			name: "Strix Halo 96 GB UMA carve-out (Ryzen AI Max+ 395)",
			hw: hardware.Profile{
				RAMTotalGB:    31,
				UnifiedMemory: true,
				UsableVRAMMB:  96 * 1024,
				CPU:           CPUInfoForTest("AMD RYZEN AI MAX+ 395 w/ Radeon 8060S"),
				GPUs:          []hardware.GPU{{Vendor: "amd", Model: "Radeon 8060S", VRAMTotalMB: 96 * 1024}},
			},
			engine:      "ollama",
			wantModel:   "qwen3.6-35b-a3b",
			wantVariant: "q4-gguf",
		},
		{
			// Same carve-out host but with a known recent engine version:
			// the mtp variant (tier 90, min_engine_version 0.30.0) is no
			// longer floored out and wins as the fastest top-tier coder.
			name: "Strix Halo carve-out with Ollama 0.31 → mtp variant",
			hw: hardware.Profile{
				RAMTotalGB:    31,
				UnifiedMemory: true,
				UsableVRAMMB:  96 * 1024,
				CPU:           CPUInfoForTest("AMD RYZEN AI MAX+ 395 w/ Radeon 8060S"),
				GPUs:          []hardware.GPU{{Vendor: "amd", Model: "Radeon 8060S", VRAMTotalMB: 96 * 1024}},
			},
			engine:        "ollama",
			engineVersion: "0.31.0",
			wantModel:     "qwen3.6-35b-a3b",
			wantVariant:   "mtp-q4-gguf",
		},
		{
			name: "M4 Ultra 512 GB UMA (Apple Silicon)",
			hw: hardware.Profile{
				RAMTotalGB:    512,
				UnifiedMemory: true,
				UsableVRAMMB:  384 * 1024,
				GPUs:          []hardware.GPU{{Vendor: "apple", Model: "Apple M4 Ultra"}},
			},
			engine:      "ollama",
			wantModel:   "qwen3-coder-480b-a35b-instruct",
			wantVariant: "q4-gguf",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pick, err := PickModel(PickInput{Catalog: ms, Hardware: c.hw, Engine: c.engine, EngineVersion: c.engineVersion})
			if err != nil {
				t.Fatalf("PickModel: %v (reasons may show why no variant fit)", err)
			}
			if pick.Manifest.ModelID != c.wantModel {
				t.Errorf("picked %s/%s, want %s/%s",
					pick.Manifest.ModelID, pick.Variant.VariantID, c.wantModel, c.wantVariant)
			}
			if c.wantVariant != "" && pick.Variant.VariantID != c.wantVariant {
				t.Errorf("picked variant = %s, want %s", pick.Variant.VariantID, c.wantVariant)
			}
		})
	}
}

// CPUInfoForTest builds a hardware.CPUInfo with the given model name.
// Defined here (rather than as a literal struct in each table row)
// because hardware.CPUInfo's Cores field is unused by the picker but
// would otherwise produce noisy diffs if its zero value changes.
func CPUInfoForTest(model string) hardware.CPUInfo {
	return hardware.CPUInfo{Model: model, Cores: 16}
}

func TestPickModel_BundledCatalog_CPUOnly(t *testing.T) {
	ms, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	hw := hardware.Profile{RAMTotalGB: 16, GPUs: nil}
	pick, err := PickModel(PickInput{Catalog: ms, Hardware: hw, Engine: "ollama"})
	if err != nil {
		t.Fatalf("PickModel: %v", err)
	}
	// 16 GB RAM: gpt-oss-20b (tier 60) and the 14B coder (tier 55)
	// both fit by RAM but are 131k/32k-native — excluded by the #624
	// context floor. The best 262144-native fit is qwen3.5-9b (tier 52).
	if pick.Manifest.ModelID != "qwen3.5-9b" {
		t.Errorf("16 GB CPU picked %s, want qwen3.5-9b (highest-tier 200k-native ollama variant)", pick.Manifest.ModelID)
	}
}

// strings_contains_lower is a tiny helper to avoid importing strings
// just for one check inside a test file.
func strings_contains_lower(s, sub string) bool {
	return len(sub) <= len(s) && (indexCI(s, sub) >= 0)
}

func indexCI(s, sub string) int {
	if sub == "" {
		return 0
	}
outer:
	for i := 0; i+len(sub) <= len(s); i++ {
		for j := 0; j < len(sub); j++ {
			a := s[i+j]
			b := sub[j]
			if a >= 'A' && a <= 'Z' {
				a += 'a' - 'A'
			}
			if b >= 'A' && b <= 'Z' {
				b += 'a' - 'A'
			}
			if a != b {
				continue outer
			}
		}
		return i
	}
	return -1
}

// TestVariantSupportedByVendor verifies the new vendor compatibility
// filter: variants flagged as "unsupported" on the host's GPU vendor /
// engine combination are excluded from PickModel. Permissive defaults
// (nil VendorSupport, empty cell) keep the variant in play.
func TestVariantSupportedByVendor(t *testing.T) {
	cases := []struct {
		name   string
		v      catalog.Variant
		engine string
		gpu    hardware.GPU
		want   bool
	}{
		{
			name:   "nil VendorSupport is permissive",
			v:      catalog.Variant{},
			engine: catalog.RuntimeVLLM,
			gpu:    hardware.GPU{Vendor: "nvidia", VRAMTotalMB: 24000},
			want:   true,
		},
		{
			name: "empty cell defaults to stable",
			v: catalog.Variant{VendorSupport: &catalog.VendorSupportMatrix{
				Nvidia: catalog.VendorRuntimeSupport{}, // VLLM == ""
			}},
			engine: catalog.RuntimeVLLM,
			gpu:    hardware.GPU{Vendor: "nvidia", VRAMTotalMB: 24000},
			want:   true,
		},
		{
			name: "explicit stable accepted",
			v: catalog.Variant{VendorSupport: &catalog.VendorSupportMatrix{
				Nvidia: catalog.VendorRuntimeSupport{VLLM: catalog.VendorSupportStable},
			}},
			engine: catalog.RuntimeVLLM,
			gpu:    hardware.GPU{Vendor: "nvidia", VRAMTotalMB: 24000},
			want:   true,
		},
		{
			name: "unsupported on this vendor/engine excluded",
			v: catalog.Variant{VendorSupport: &catalog.VendorSupportMatrix{
				AMD: catalog.VendorRuntimeSupport{VLLM: catalog.VendorSupportUnsupported},
			}},
			engine: catalog.RuntimeVLLM,
			gpu:    hardware.GPU{Vendor: "amd", VRAMTotalMB: 24000},
			want:   false,
		},
		{
			name: "unsupported on Mac MLX filters when host is Apple",
			v: catalog.Variant{VendorSupport: &catalog.VendorSupportMatrix{
				Mac: catalog.VendorRuntimeSupport{Ollama: catalog.VendorSupportUnsupported},
			}},
			engine: catalog.RuntimeOllama,
			gpu:    hardware.GPU{Vendor: "apple", VRAMTotalMB: 96000},
			want:   false,
		},
		{
			name: "unsupported on AMD does not filter NVIDIA host",
			v: catalog.Variant{VendorSupport: &catalog.VendorSupportMatrix{
				AMD: catalog.VendorRuntimeSupport{VLLM: catalog.VendorSupportUnsupported},
			}},
			engine: catalog.RuntimeVLLM,
			gpu:    hardware.GPU{Vendor: "nvidia", VRAMTotalMB: 24000},
			want:   true,
		},
		{
			name: "CPU-only host is not vendor-filtered",
			v: catalog.Variant{VendorSupport: &catalog.VendorSupportMatrix{
				AMD: catalog.VendorRuntimeSupport{Ollama: catalog.VendorSupportUnsupported},
			}},
			engine: catalog.RuntimeOllama,
			// no GPU
			want: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hw := hardware.Profile{}
			if c.gpu.Vendor != "" {
				hw.GPUs = []hardware.GPU{c.gpu}
			}
			got := variantSupportedByVendor(c.v, c.engine, hw)
			if got != c.want {
				t.Errorf("variantSupportedByVendor = %v, want %v", got, c.want)
			}
		})
	}
}

// TestEffectiveVRAMMB_UMA verifies the Profile helper that hostFits now
// consults: UMA hosts get UsableVRAMMB, discrete-GPU hosts get
// GPUs[0].VRAMTotalMB.
func TestEffectiveVRAMMB_UMA(t *testing.T) {
	cases := []struct {
		name string
		p    hardware.Profile
		want int
	}{
		{
			name: "discrete GPU returns VRAMTotalMB",
			p: hardware.Profile{
				GPUs: []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 24000}},
			},
			want: 24000,
		},
		{
			name: "Apple Silicon UMA returns UsableVRAMMB",
			p: hardware.Profile{
				UnifiedMemory: true,
				UsableVRAMMB:  96000,
				GPUs:          []hardware.GPU{{Vendor: "apple", VRAMTotalMB: 128000}},
			},
			want: 96000,
		},
		{
			name: "UMA flag without UsableVRAMMB falls back to VRAMTotalMB",
			p: hardware.Profile{
				UnifiedMemory: true,
				UsableVRAMMB:  0,
				GPUs:          []hardware.GPU{{Vendor: "amd", VRAMTotalMB: 80000}},
			},
			want: 80000,
		},
		{
			name: "CPU-only returns 0",
			p:    hardware.Profile{},
			want: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.p.EffectiveVRAMMB(); got != c.want {
				t.Errorf("EffectiveVRAMMB = %d, want %d", got, c.want)
			}
		})
	}
}

func TestRankModels_HeadMatchesPickModel(t *testing.T) {
	hw := hardware.Profile{
		RAMTotalGB: 64,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 24467}},
	}
	in := PickInput{Catalog: fixtureCatalog(), Hardware: hw, Engine: "vllm"}
	pick, err := PickModel(in)
	if err != nil {
		t.Fatalf("PickModel: %v", err)
	}
	ranked, err := RankModels(in)
	if err != nil {
		t.Fatalf("RankModels: %v", err)
	}
	if len(ranked) == 0 {
		t.Fatalf("RankModels returned empty slice")
	}
	if ranked[0].Manifest.ModelID != pick.Manifest.ModelID ||
		ranked[0].Variant.VariantID != pick.Variant.VariantID {
		t.Errorf("RankModels[0] = %s/%s, want PickModel head %s/%s",
			ranked[0].Manifest.ModelID, ranked[0].Variant.VariantID,
			pick.Manifest.ModelID, pick.Variant.VariantID)
	}
}

func TestRankModels_FullOrdering(t *testing.T) {
	// 24 GB vLLM host: large-vllm/awq-int4 (tier 78) then mid-vllm/awq-int4
	// (tier 60). mid-vllm/fp16 (32 GB) and huge-vllm (130 GB) don't fit.
	hw := hardware.Profile{
		RAMTotalGB: 64,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 24467}},
	}
	ranked, err := RankModels(PickInput{Catalog: fixtureCatalog(), Hardware: hw, Engine: "vllm"})
	if err != nil {
		t.Fatalf("RankModels: %v", err)
	}
	want := []string{"large-vllm/awq-int4", "mid-vllm/awq-int4"}
	if len(ranked) != len(want) {
		t.Fatalf("RankModels len = %d, want %d (%+v)", len(ranked), len(want), ranked)
	}
	for i, w := range want {
		got := ranked[i].Manifest.ModelID + "/" + ranked[i].Variant.VariantID
		if got != w {
			t.Errorf("ranked[%d] = %s, want %s", i, got, w)
		}
	}
}

func TestRankModels_EmptyEngine(t *testing.T) {
	_, err := RankModels(PickInput{Catalog: fixtureCatalog(), Hardware: hardware.Profile{}})
	if err == nil {
		t.Errorf("RankModels with empty engine = nil error, want error")
	}
}

// TestOllamaFitsVRAM pins the discrete-GPU residency gate added for
// the "120 GB RAM host with a 24 GB card auto-picks a 62 GB model"
// CPU-spill trap.
func TestOllamaFitsVRAM(t *testing.T) {
	gpu24 := hardware.Profile{
		RAMTotalGB: 120,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 24467}},
	}
	for _, c := range []struct {
		name string
		v    catalog.Variant
		hw   hardware.Profile
		want bool
	}{
		{"16GB weights fit a 24GB card",
			catalog.Variant{EstimatedWeightGB: 16.3, KVBytesPerTokenFP16: 65536}, gpu24, true},
		{"62GB weights rejected on a 24GB card",
			catalog.Variant{EstimatedWeightGB: 62.0, KVBytesPerTokenFP16: 65536}, gpu24, false},
		{"24GB weights rejected on a 24GB card (KV + overhead)",
			catalog.Variant{EstimatedWeightGB: 24.0, KVBytesPerTokenFP16: 65536}, gpu24, false},
		{"CPU-only host falls back to the RAM gate",
			catalog.Variant{EstimatedWeightGB: 62.0}, hardware.Profile{RAMTotalGB: 120}, true},
		{"UMA host: 62GB weights fit the 96GB pool (residency check, not RAM gate)",
			catalog.Variant{EstimatedWeightGB: 62.0, KVBytesPerTokenFP16: 65536},
			hardware.Profile{RAMTotalGB: 31, UnifiedMemory: true, UsableVRAMMB: 98304,
				GPUs: []hardware.GPU{{Vendor: "amd", VRAMTotalMB: 96 * 1024}}}, true},
		// 110 GB weights (~102.5 GiB) exceed the 96 GiB (98304 MiB) pool on
		// their own, so this stays rejected under the smaller UMA overhead
		// (#424). 100 GB weights are only ~93 GiB and now fit with room.
		{"UMA host: 110GB weights rejected (exceeds 96GB pool)",
			catalog.Variant{EstimatedWeightGB: 110.0, KVBytesPerTokenFP16: 24576},
			hardware.Profile{RAMTotalGB: 31, UnifiedMemory: true, UsableVRAMMB: 98304,
				GPUs: []hardware.GPU{{Vendor: "amd", VRAMTotalMB: 96 * 1024}}}, false},
		{"variant without a weight annotation is not rejected",
			catalog.Variant{}, gpu24, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := ollamaFitsVRAM(c.v, c.hw); got != c.want {
				t.Errorf("ollamaFitsVRAM = %v, want %v", got, c.want)
			}
		})
	}
}

// TestPickModel_GPUHostBigRAM_ollama: the RAM-only gate used to let a
// 62 GB-weight model "fit" a 120 GB-RAM host with a 24 GB GPU; ollama
// then spilled most layers to the CPU and decode collapsed. The picker
// must choose the highest tier that stays GPU-resident instead.
func TestPickModel_GPUHostBigRAM_ollama(t *testing.T) {
	cat := []catalog.Manifest{
		{
			ModelID: "huge-moe-ollama", ContextLength: 131072,
			Capabilities: []string{"chat", "tool_use"},
			Variants: []catalog.Variant{{
				VariantID: "gguf", Format: "ollama-tag",
				RuntimeSupport:    []string{"ollama"},
				EstimatedWeightGB: 62.0, MinRAMGB: 96, QualityTier: 85,
				KVBytesPerTokenFP16: 65536,
				Source:              catalog.VariantSource{Type: "ollama", Tag: "huge:120b"},
			}},
		},
		{
			ModelID: "dense-27b-ollama", ContextLength: 131072,
			Capabilities: []string{"chat", "tool_use"},
			Variants: []catalog.Variant{{
				VariantID: "q4-gguf", Format: "ollama-tag",
				RuntimeSupport:    []string{"ollama"},
				EstimatedWeightGB: 16.3, MinRAMGB: 24, QualityTier: 70,
				KVBytesPerTokenFP16: 65536,
				Source:              catalog.VariantSource{Type: "ollama", Tag: "dense:27b-q4_K_M"},
			}},
		},
	}
	hw := hardware.Profile{
		RAMTotalGB: 120,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", Model: "RTX PRO 4000 Blackwell", VRAMTotalMB: 24467}},
	}
	pick, err := PickModel(PickInput{Catalog: cat, Hardware: hw, Engine: "ollama"})
	if err != nil {
		t.Fatalf("PickModel: %v", err)
	}
	if pick.Manifest.ModelID != "dense-27b-ollama" {
		t.Errorf("ModelID = %q, want dense-27b-ollama (62 GB must not fit a 24 GB card)", pick.Manifest.ModelID)
	}
}

// #624: the discrete overhead reservation scales with model weight
// (base 1024 MiB + 40 MiB/GB, single-point calibrated against the
// 22.62 GB / ~1.9 GB measurement in
// docs/reports/20260704-mtp-vs-spill-24gb.md). UMA stays flat.
func TestOllamaVRAMOverheadMB(t *testing.T) {
	discrete := hardware.Profile{GPUs: []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 24467}}}
	uma := hardware.Profile{UnifiedMemory: true, UsableVRAMMB: 24576}

	cases := []struct {
		name     string
		hw       hardware.Profile
		weightGB float64
		want     int
	}{
		{"discrete-anchor-22.62gb", discrete, 22.62, 1024 + 904},
		{"discrete-small-model", discrete, 4.7, 1024 + 188},
		{"discrete-unknown-weight-conservative", discrete, 0, 4096},
		{"uma-flat-regardless-of-weight", uma, 22.62, 1024},
		{"uma-flat-unknown-weight", uma, 0, 1024},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := OllamaVRAMOverheadMB(c.hw, c.weightGB); got != c.want {
				t.Errorf("OllamaVRAMOverheadMB(%v, %v) = %d, want %d", c.hw.UnifiedMemory, c.weightGB, got, c.want)
			}
		})
	}
}
