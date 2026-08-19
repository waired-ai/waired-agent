package router

import (
	"errors"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/proto/hostfit"
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

// TestRankModels_MultiGPU_OllamaKeepsWhatOneCardRefuses is #264's
// end-to-end claim: the wizard stops refusing a model the machine runs.
//
// Deliberately built on a local catalog rather than fixtureCatalog,
// whose ollama variants are all small enough to be resident on one card
// — the bug needs a model that spills on one and pools onto two. The
// small variant is load-bearing too: RankModels' narrow() falls through
// when nothing passes, so an exclusion only bites while something else
// still qualifies.
func TestRankModels_MultiGPU_OllamaKeepsWhatOneCardRefuses(t *testing.T) {
	// 33 GB of weights: past one 24 GB card, comfortably inside the
	// 2×24467−1024 = 47910 MiB pool. Sized so the machine genuinely
	// serves the coding floor on two cards — the point is that the
	// judgment was wrong, not that the gate is being loosened.
	big := catalog.Manifest{
		ModelID: "big-moe-ollama", ContextLength: 262144,
		Capabilities: []string{"chat", "tool_use"},
		Variants: []catalog.Variant{{
			VariantID: "q4-gguf", Format: "ollama-tag",
			Quantization: "Q4_K_M", RuntimeSupport: []string{"ollama"},
			EstimatedWeightGB: 33.0, MinRAMGB: 64, QualityTier: 88,
			ParamCount: 70_000_000_000, ActiveParams: 10_000_000_000,
			KVBytesPerTokenFP16: 24576,
			Source:              catalog.VariantSource{Type: "ollama", Tag: "big:70b-a10b-q4_K_M"},
		}},
	}
	small := catalog.Manifest{
		ModelID: "small-ollama", ContextLength: 262144,
		Capabilities: []string{"chat", "tool_use"},
		Variants: []catalog.Variant{{
			VariantID: "q4-gguf", Format: "ollama-tag",
			Quantization: "Q4_K_M", RuntimeSupport: []string{"ollama"},
			EstimatedWeightGB: 5.0, MinRAMGB: 12, QualityTier: 42,
			ParamCount: 8_000_000_000, KVBytesPerTokenFP16: 8192,
			Source: catalog.VariantSource{Type: "ollama", Tag: "small:8b-q4_K_M"},
		}},
	}
	cat := []catalog.Manifest{big, small}

	gpu := hardware.GPU{Vendor: "nvidia", Model: "RTX PRO 4000 Blackwell", VRAMTotalMB: 24467}
	one := hardware.Profile{RAMTotalGB: 256, GPUs: []hardware.GPU{gpu}}
	two := hardware.Profile{RAMTotalGB: 256, GPUs: []hardware.GPU{gpu, gpu}}

	kept := func(hw hardware.Profile) bool {
		t.Helper()
		ranked, err := RankModels(PickInput{Catalog: cat, Hardware: hw, Engine: "ollama"})
		if err != nil {
			t.Fatalf("RankModels: %v", err)
		}
		for _, p := range ranked {
			if p.Manifest.ModelID == "big-moe-ollama" {
				return true
			}
		}
		return false
	}

	if kept(one) {
		t.Fatal("one 24 GB card no longer refuses the 81 GB variant, so this test " +
			"proves nothing — re-pick the weights against the current constants")
	}
	if !kept(two) {
		t.Error("two 24 GB cards still refuse the 81 GB variant: the picker is " +
			"judging the host as if the second card were not there (#264)")
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
//
// The card is the one this project measures on, and until #823 it paired
// with qwen3.6-27b/awq-int4 (tier 72). That pairing was never fetchable:
// Qwen/Qwen3.6-27B-AWQ is not on Hugging Face. With the variant
// repointed at the official FP8 build the 27B band starts at 38912 MB,
// and no catalog variant fits 24 GB under vLLM.
//
// The assertion is kept — inverted, not deleted — because the pairing it
// used to state is exactly what a reader would assume still holds. On
// this host the product serves through ollama, which PickEngine picks
// without being asked; #575 tracks giving the band a vLLM build a common
// card can hold.
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
	if !errors.Is(err, ErrHardwareInsufficient) {
		t.Fatalf("Blackwell 24 GB under vllm: want ErrHardwareInsufficient, got pick=%s/%s err=%v",
			pick.Manifest.ModelID, pick.Variant.VariantID, err)
	}

	// The same card still has a model on ollama — the engine PickEngine
	// names for it — which is what makes the row above a change of
	// engine rather than a loss. Which model is HardwareTiers' question,
	// not this one's; all that matters here is that there is one and
	// that it is not a model we decline to recommend.
	pick, err = PickModel(PickInput{Catalog: ms, Hardware: hw, Engine: "ollama",
		EngineVersion: runtime.OllamaPinnedVersion})
	if err != nil {
		t.Fatalf("Blackwell 24 GB under ollama: %v", err)
	}
	if pick.Manifest.ManualOnly != "" {
		t.Errorf("Blackwell 24 GB on ollama picked %s, which is manual_only: %s",
			pick.Manifest.ModelID, pick.Manifest.ManualOnly)
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
		// wantNoFit expects ErrHardwareInsufficient: this engine has no
		// build this host can run. Distinct from a wrong pick, and the
		// only honest expectation once a class of hardware loses its
		// last variant on an engine.
		wantNoFit bool
	}{
		{
			// These two picked qwen2.5-coder's awq-int4 builds (8000 and
			// 16000 MB of VRAM) until #522 retired the 2025 generation.
			// Every vLLM build under 24 GB went with it — the smallest
			// left is qwen3.6-27b/awq-int4 at 24000 MB — so vLLM has
			// nothing for a card this size and the picker says so.
			//
			// A user never reaches this state: PickEngine will not name
			// an engine the catalog cannot feed (#572), so these hosts
			// serve on ollama. That is asserted in
			// TestPickEngine_ShippedCatalog_TodaysVerdicts; here the
			// subject is the picker with the engine already forced, which
			// is what an explicit `--prefer vllm` does.
			//
			// #575 tracks adding vLLM builds for the qwen3.5 line, which
			// would give these rows a model again.
			name: "8GB NVIDIA dGPU (RTX 3060/4060), vllm forced",
			hw: hardware.Profile{
				RAMTotalGB: 32,
				GPUs:       []hardware.GPU{{Vendor: "nvidia", Model: "RTX 4060", VRAMTotalMB: 8000}},
			},
			engine:    "vllm",
			wantNoFit: true,
		},
		{
			name: "16GB NVIDIA dGPU (RTX 4060 Ti), vllm forced",
			hw: hardware.Profile{
				RAMTotalGB: 32,
				GPUs:       []hardware.GPU{{Vendor: "nvidia", Model: "RTX 4060 Ti", VRAMTotalMB: 16000}},
			},
			engine:    "vllm",
			wantNoFit: true,
		},
		{
			name: "24GB NVIDIA dGPU (RTX 4090)",
			hw: hardware.Profile{
				RAMTotalGB: 64,
				GPUs:       []hardware.GPU{{Vendor: "nvidia", Model: "RTX 4090", VRAMTotalMB: 24000}},
			},
			engine: "vllm",
			// This row answered qwen3.6-27b/awq-int4 until #823. That
			// build was sourced from Qwen/Qwen3.6-27B-AWQ, which is not
			// on Hugging Face — a 24 GB card auto-picking vLLM resolved
			// to weights it could not fetch. Repointed at the official
			// Qwen/Qwen3.6-27B-FP8 the same model needs 38912 MB, and
			// nothing else in the catalog fits a 24 GB card under vLLM,
			// so the honest answer here is now "no fit".
			//
			// It is not a loss of local inference: PickEngine would not
			// have named vllm for this host in the first place (see
			// engine_picker_feedable_test.go), and this row forces the
			// engine. #575 tracks the coverage gap.
			wantNoFit: true,
		},
		{
			name: "80GB NVIDIA H100",
			hw: hardware.Profile{
				RAMTotalGB: 256,
				GPUs:       []hardware.GPU{{Vendor: "nvidia", Model: "H100 80GB HBM3", VRAMTotalMB: 80000}},
			},
			engine: "vllm",
			// #624: gpt-oss-120b (tier 88) is 131072-native and excluded
			// by the context floor. qwen3-coder-next-80b (tier 82) was
			// the best 262144-native vllm fit until #522 retired it, so
			// the answer steps down to the only vLLM build the pinned
			// generation ships. #823 made that build qwen3.8-27b/fp8:
			// the 27B band moved a generation, and qwen3.6-27b is
			// manual_only behind it.
			wantModel:   "qwen3.8-27b",
			wantVariant: "fp8",
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
			name: "Strix Halo 96 GB UMA carve-out on Linux (Ryzen AI Max+ 395)",
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
			name: "Strix Halo carve-out on Linux with Ollama 0.31 → mtp variant",
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
			// The SAME machine as the two rows above, profiled on Windows,
			// where the carve-out is not the budget: a graphics allocation
			// carries a system-memory backing store of equal size, so what
			// a model can occupy is the 31 GB the OS still sees minus its
			// reserve (waired-ai/waired-agent#863). The pick must not move
			// — this host ran qwen3.6-35b-a3b at 74.27 tok/s with 96 GB
			// carved out, and a fix to the arithmetic that took its model
			// away would be a regression, not a correction.
			name: "Strix Halo 96 GB carve-out on Windows keeps the same pick",
			hw: hardware.Profile{
				OS:             "windows",
				RAMTotalGB:     31,
				UnifiedMemory:  true,
				UsableVRAMMB:   29 * 1024,
				CarveOutVRAMMB: 0,
				CPU:            CPUInfoForTest("AMD RYZEN AI MAX+ 395 w/ Radeon 8060S"),
				GPUs:           []hardware.GPU{{Vendor: "amd", Model: "Radeon 8060S", VRAMTotalMB: 96 * 1024}},
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
			engine: "ollama",
			// qwen3-coder-480b (tier 92) held this row until #522. The
			// biggest ollama build the pinned generation ships is
			// qwen3.6-35b-a3b (tier 90) — a 512 GB Mac steps down two
			// tier points and loses nothing else.
			wantModel: "qwen3.6-35b-a3b",
			// q4-gguf, not mtp: this row sets no engineVersion, and the
			// mtp build carries a MinEngineVersion floor that an unknown
			// version fails closed on.
			wantVariant: "q4-gguf",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pick, err := PickModel(PickInput{Catalog: ms, Hardware: c.hw, Engine: c.engine, EngineVersion: c.engineVersion})
			if c.wantNoFit {
				if !errors.Is(err, ErrHardwareInsufficient) {
					t.Fatalf("want ErrHardwareInsufficient, got pick=%s/%s err=%v",
						pick.Manifest.ModelID, pick.Variant.VariantID, err)
				}
				return
			}
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
		// A Linux Strix Halo shape: there a carve-out reading IS the budget
		// and IS additive. On Windows the same machine now publishes the
		// OS-visible leftover instead (waired-ai/waired-agent#863), so this
		// pair pins the arithmetic given a host, not the host itself.
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
		// The shape the #67 fallbacks can produce: the card is named but
		// its capacity is not readable (Linux procfs, or a Windows driver
		// that publishes no registry memory value). Product contract: an
		// unknown budget suspends the residency gate rather than failing
		// it — ollama offloads what fits and runs the rest from RAM, so
		// rejecting the catalog here would be strictly worse than the
		// CPU-only host this machine was mistaken for.
		{"detected GPU with an unreadable VRAM figure does not reject the catalog",
			catalog.Variant{EstimatedWeightGB: 62.0, KVBytesPerTokenFP16: 65536},
			hardware.Profile{RAMTotalGB: 120,
				GPUs: []hardware.GPU{{Vendor: "nvidia", Model: "NVIDIA GeForce RTX 3060 Ti"}}}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := ollamaFitsVRAM(c.v, c.hw); got != c.want {
				t.Errorf("ollamaFitsVRAM = %v, want %v", got, c.want)
			}
		})
	}
}

// TestPickModel_GPUHostBigRAM_ollama: a 62 GB-weight model on a 120 GB
// host with a 24 GB GPU runs — ollama spills most layers to the CPU —
// but must not be what the host is POINTED AT, because those weights are
// re-read from system RAM on every token. The picker takes the highest
// tier whose weights stay resident instead.
//
// Capacity admits it, and that is the layering: 62 GB fits 120 GB of RAM
// plus 24 GB of VRAM, so refusing it would be refusing something the
// machine can do (waired-ai/waired#1056 decision 1). The recommendation
// is what declines it.
//
// Both manifests declare 262144 rather than 131072 as they once did: the
// recommendation now asks whether the host can declare the ~200k coding
// window, and a 131072-native model answers no on any hardware, so with
// the old fixture BOTH candidates were unrecommended and the pass fell
// through without discriminating. The window class is not what this test
// is about.
func TestPickModel_GPUHostBigRAM_ollama(t *testing.T) {
	cat := []catalog.Manifest{
		{
			ModelID: "huge-moe-ollama", ContextLength: 262144,
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
			ModelID: "dense-27b-ollama", ContextLength: 262144,
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

// TestRankModels_SpilledFlagshipsAreNotPreselected is the router half of
// #229, re-based on the rule that replaced the speed pass.
//
// hostFits does not require GPU residency, because requiring it as
// CAPACITY meant adding a graphics card removed models. That leaves the
// ranking exposed: a model with a higher quality tier survives the filter
// even when the card holds almost none of it, and tier-desc sorting would
// hand it the auto-selection.
//
// The speed pass used to stop that. It no longer exists: it excluded on
// an estimate over BandwidthSystemRAMGBs, the same population constant
// ClassCPUOnly is exempt from, so it refused a 19.96 tok/s host while
// admitting a 17.65 tok/s one — and it prices decode only, while a coding
// agent's load is roughly 21:1 prefill. Speed returns as a recommendation
// input when it is measured (waired-ai/waired-agent#466).
//
// What stops it now is the recommendation's residency clause: weights and
// overhead must fit GPU-addressable memory. Neither 81 GB model does on a
// 24 GB card, so NEITHER is preselected and the pass falls through to
// tier order — which is the honest answer, because the thing that would
// separate them is a speed claim this stage no longer makes.
//
// Record of today's behaviour, not a rule: the ratifying decision
// (waired-ai/waired#1056, 2026-08-03) settles that speed is soft and
// measured, not that tier order is the right tie-break among models a
// host cannot hold.
func TestRankModels_SpilledFlagshipsAreNotPreselected(t *testing.T) {
	// 128 GB of RAM behind a 24 GB card: both models clear the RAM gate,
	// neither is resident, so capacity alone cannot separate them.
	hw := hardware.Profile{
		RAMTotalGB: 128,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", Model: "RTX 4090", VRAMTotalMB: 24564}},
	}
	dense := catalog.Manifest{
		ModelID: "spilled-dense", ContextLength: 262144,
		Capabilities: []string{"chat", "tool_use"},
		Variants: []catalog.Variant{{
			VariantID: "q4-gguf", Format: "ollama-tag", RuntimeSupport: []string{"ollama"},
			EstimatedWeightGB: 81.0, MinRAMGB: 96, QualityTier: 95,
			ParamCount: 122_000_000_000, ActiveParams: 122_000_000_000,
			KVBytesPerTokenFP16: 24576,
			Source:              catalog.VariantSource{Type: "ollama", Tag: "dense:122b"},
		}},
	}
	moe := catalog.Manifest{
		ModelID: "spilled-moe", ContextLength: 262144,
		Capabilities: []string{"chat", "tool_use"},
		Variants: []catalog.Variant{{
			VariantID: "q4-gguf", Format: "ollama-tag", RuntimeSupport: []string{"ollama"},
			EstimatedWeightGB: 81.0, MinRAMGB: 96, QualityTier: 90,
			ParamCount: 122_000_000_000, ActiveParams: 3_300_000_000,
			KVBytesPerTokenFP16: 24576,
			Source:              catalog.VariantSource{Type: "ollama", Tag: "moe:122b"},
		}},
	}

	// Both fit: the capacity gate is the RAM gate on a discrete host now.
	for _, m := range []catalog.Manifest{dense, moe} {
		if !hostFits(catalog.RuntimeOllama, m, m.Variants[0], hw) {
			t.Fatalf("%s does not fit; the capacity gate is still requiring residency", m.ModelID)
		}
	}

	// Neither is preselected: a 24 GB card holds neither 81 GB of weights.
	ranked, err := RankModels(PickInput{
		Catalog: []catalog.Manifest{dense, moe}, Hardware: hw, Engine: catalog.RuntimeOllama,
	})
	if err != nil {
		t.Fatalf("RankModels: %v", err)
	}
	for _, p := range ranked {
		if p.Recommendation.Fits {
			t.Errorf("%s is preselected on a 24 GB card holding none of its 81 GB of "+
				"weights; the residency clause is not reaching the picker", p.Manifest.ModelID)
		}
		if p.Recommendation.Reason != hostfit.ReasonWeightsSpill {
			t.Errorf("%s: not-preselected reason = %q, want %q — the operator has to be "+
				"told it is the weights, not the window",
				p.Manifest.ModelID, p.Recommendation.Reason, hostfit.ReasonWeightsSpill)
		}
	}

	// The roofline still SAYS which of the two the machine would read
	// seven times faster, on every surface. It just does not decide.
	byID := map[string]hostfit.Estimate{}
	for _, p := range ranked {
		byID[p.Manifest.ModelID] = p.DecodeEstimate
	}
	if byID["spilled-moe"].TokpsEstimate <= byID["spilled-dense"].TokpsEstimate {
		t.Errorf("the 3.3B-active mixture of experts is estimated at %.2f tok/s and the "+
			"122B-active dense one at %.2f; the estimate has stopped discriminating",
			byID["spilled-moe"].TokpsEstimate, byID["spilled-dense"].TokpsEstimate)
	}

	// The user may still ASK for either: an explicit preference bypasses
	// every pass, exactly as it bypasses the #624 context floor.
	pinned, err := PickModel(PickInput{
		Catalog: []catalog.Manifest{dense, moe}, Hardware: hw,
		Engine: catalog.RuntimeOllama, PreferredModelID: "spilled-dense",
	})
	if err != nil {
		t.Fatalf("PickModel(pinned): %v", err)
	}
	if pinned.Manifest.ModelID != "spilled-dense" {
		t.Errorf("pinned ModelID = %q, want spilled-dense — a preference must survive "+
			"the recommendation pass the way it survives the context floor", pinned.Manifest.ModelID)
	}
}

// TestHostFitsIsMonotoneInHardware is the router-level statement of the
// #229 invariant: whatever a host can serve, the same host with a
// graphics card added can serve too. hostFits is the predicate
// RankModels filters on, so an inversion here is an inversion in
// everything downstream — including what the wizard offers, since the
// control plane calls the same rule.
func TestHostFitsIsMonotoneInHardware(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	for _, ram := range []int{8, 16, 32, 64, 128, 512} {
		for _, vram := range []int{4096, 8192, 12288, 16384, 24564} {
			bare := hardware.Profile{RAMTotalGB: ram}
			carded := hardware.Profile{
				RAMTotalGB: ram,
				GPUs:       []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: vram}},
			}
			for _, m := range manifests {
				for _, v := range m.Variants {
					if !engineSupports(v, catalog.RuntimeOllama) {
						continue
					}
					if !hostFits(catalog.RuntimeOllama, m, v, bare) {
						continue
					}
					if !hostFits(catalog.RuntimeOllama, m, v, carded) {
						t.Fatalf("%s/%s: %d GB of RAM serves it, but the same host with a "+
							"%d MB card does not", m.ModelID, v.VariantID, ram, vram)
					}
				}
			}
		}
	}
}

// TestRankModels_ResidentWeightsBeatASpilledFlagship is the
// waired-ai/waired#986 host, and the router half of
// waired-ai/waired#988.
//
// A 22.6 GB mixture of experts was being served on a 16 GB card with
// 37.7 % of its weights in system RAM; a ~30k-token coding prompt
// prefilled at 388 tok/s. The roofline cannot see that, because a MoE
// reads only its ACTIVE weights per token and prefill touches all of
// them — so the model cleared the decode floor, carried the highest
// tier, and won.
//
// Product contract: on a discrete card the auto-selection is a model
// whose WEIGHTS the card holds, and the one it passes over is reported
// with a reason rather than hidden.
func TestRankModels_ResidentWeightsBeatASpilledFlagship(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	hw := hardware.Profile{
		OS: "windows", Arch: "x86_64", RAMTotalGB: 64,
		GPUs: []hardware.GPU{{Vendor: "nvidia", Model: "RTX 5080", VRAMTotalMB: 16303}},
	}
	in := PickInput{Catalog: manifests, Hardware: hw, Engine: catalog.RuntimeOllama}

	pick, err := PickModel(in)
	if err != nil {
		t.Fatalf("PickModel: %v", err)
	}
	if pick.Manifest.ModelID == "qwen3.6-35b-a3b" {
		t.Fatal("a 16 GB card is still auto-selecting a 22.6 GB mixture of experts")
	}
	if !pick.Recommendation.Fits {
		t.Errorf("the winner %s is itself not recommended (%+v); the gate fell through, "+
			"so this host has no resident option at all", pick.Manifest.ModelID, pick.Recommendation)
	}
	if pick.Manifest.ModelID != "qwen3.5-9b" {
		t.Errorf("auto-selection = %s, want qwen3.5-9b — the highest tier that both holds "+
			"its weights in a 16 GB card and serves the ~200k coding window",
			pick.Manifest.ModelID)
	}

	// Capacity still admits it — that is the layering, and hiding a
	// model the host can run is the #229 bug.
	if !hostFits(catalog.RuntimeOllama, manifestByID(t, manifests, "qwen3.6-35b-a3b"),
		manifestVariant(t, manifests, "qwen3.6-35b-a3b"), hw) {
		t.Error("qwen3.6-35b-a3b no longer fits a 64 GB host with a 16 GB card; " +
			"the recommendation gate has leaked into capacity")
	}

	// And the pass-over is explained rather than silent, which is the
	// half of this the review actually hit. Both quality gates are stood
	// down to enumerate: RankModels returns the NARROWED set, so a model
	// dropped by the #624 context floor never reaches the caller either
	// way.
	ungated := in
	ungated.NoRecommendGate = true
	ungated.NoContextFloor = true
	ranked, err := RankModels(ungated)
	if err != nil {
		t.Fatalf("RankModels (ungated): %v", err)
	}
	var seen bool
	for _, p := range ranked {
		if p.Manifest.ModelID != "qwen3.6-35b-a3b" {
			continue
		}
		seen = true
		if p.Recommendation.Fits {
			t.Errorf("qwen3.6-35b-a3b reports itself recommendable on a 16 GB card (%+v)",
				p.Recommendation)
		}
		if p.Recommendation.Reason != hostfit.ReasonWeightsSpill {
			t.Errorf("Reason = %q, want %q", p.Recommendation.Reason, hostfit.ReasonWeightsSpill)
		}
		if p.Recommendation.NeedMB <= p.Recommendation.HaveMB {
			t.Errorf("shortfall reads need=%d have=%d, which is not a shortfall",
				p.Recommendation.NeedMB, p.Recommendation.HaveMB)
		}
		var explained bool
		for _, r := range p.Reasons {
			if strings.Contains(r, "not preselected here") {
				explained = true
			}
			if strings.Contains(r, "cannot run") || strings.Contains(r, "does not fit") {
				t.Errorf("reason claims the model does not run: %q", r)
			}
		}
		if !explained {
			t.Errorf("no reason line explains why it was passed over: %q", p.Reasons)
		}
	}
	if !seen {
		t.Error("qwen3.6-35b-a3b disappeared from the ranking entirely; capacity still admits it")
	}
}

// manifestVariant returns the first ollama variant of modelID in the
// bundled catalog, failing the test when the model is gone.
func manifestVariant(t *testing.T, manifests []catalog.Manifest, modelID string) catalog.Variant {
	t.Helper()
	for _, m := range manifests {
		if m.ModelID != modelID {
			continue
		}
		for _, v := range m.Variants {
			if engineSupports(v, catalog.RuntimeOllama) {
				return v
			}
		}
	}
	t.Fatalf("%s is not in the bundled catalog with an ollama variant", modelID)
	return catalog.Variant{}
}

// manifestByID returns the bundled manifest for modelID, failing the
// test when the model is gone. The capacity rule prices the window a
// model would actually be given, so its manifest is an input.
func manifestByID(t *testing.T, manifests []catalog.Manifest, modelID string) catalog.Manifest {
	t.Helper()
	for _, m := range manifests {
		if m.ModelID == modelID {
			return m
		}
	}
	t.Fatalf("%s is not in the bundled catalog", modelID)
	return catalog.Manifest{}
}
