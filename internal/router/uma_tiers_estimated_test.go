package router

import (
	"errors"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/runtime"
)

// TestUMATierSelectionEstimated is the #415 "Tier B" deliverable: validate
// the Apple-Silicon model picks for the upper RAM tiers by COMPUTATION, not
// real hardware. It builds synthetic UMA (Apple Silicon) hardware profiles
// for each RAM tier — exactly what profiler_darwin.go's defaultUMA produces
// when iogpu.wired_limit_mb is UNSET, i.e. budget = RAMTotalGB * 3/4 * 1024
// MB — and asserts the picker's top auto-selection.
//
// The expected picks below are derived by hand from the bundled catalog's
// residency math (weightMiB + kvMiB(16384 tok) + 1024 MB UMA overhead, see
// ollamaFitsVRAM / ollamaVRAMOverheadUMAMB) and the quality_tier ranking.
// They are ESTIMATES for every tier except 16 GB, which is additionally
// confirmed against real Apple M4 hardware in selection_realhost_darwin_test.go
// + docs/records/20260619/. The 8 GB row is the edge case #415 flagged and
// #424 fixed: the Metal-aware 1024 MB overhead (down from the CUDA-calibrated
// 4096) now lets the 8 GB Mac pick the same coder-7b it actually runs on Metal.
//
// NOTE on the ">=32 GB engages MLX" path: that is Ollama's INTERNAL backend
// decision (Metal vs MLX), not something waired's picker selects — the
// picker always routes Apple Silicon to the ollama engine and lets it pick
// the backend (see ollama_backend.go). So this test asserts the catalog
// MODEL choice per tier; the Metal-vs-MLX backend is out of its scope.
func TestUMATierSelectionEstimated(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	engineVer := runtime.OllamaPinnedVersion

	cases := []struct {
		ramGB       int
		wantModelID string
		wantVariant string
		wantQuality int
		note        string
	}{
		{8, "qwen3.5-4b", "q4-gguf", 42,
			"#624: coder-7b (q45, 32k-native) is below the context floor; the 3.4 GB qwen3.5-4b serves the full ~200k on the 6144 MB budget (its KV is tiny). #424's coder-7b-fits finding still holds for explicit picks"},
		{12, "qwen3.5-4b", "q4-gguf", 42,
			"#624: the 6.6 GB 9b fits by residency but its no-spill window on the 9216 MB budget is ~121k < floor (UMA gets no spill allowance) — 4b keeps the full window"},
		{16, "qwen3.5-9b", "q4-gguf", 52, "confirmed on real Apple M4 (16 GB); 9b's no-spill window ~318k clears the floor here"},
		{24, "qwen3.5-9b", "q4-gguf", 52,
			"#624: qwen3.6-27b (q70) is 131072-native (excluded); qwen3.5-27b's 17 GB weights leave only ~38k of KV on the 18432 MB budget — 9b is the best floor-passing fit"},
		{32, "qwen3.6-35b-a3b", "mtp-q4-gguf", 90,
			"estimated; with 1024 MB overhead the mtp variant (resident 22325 MB) now fits the 24576 MB budget, beating q4 (q89); needs engine >= 0.30.0"},
		{64, "qwen3.6-35b-a3b", "mtp-q4-gguf", 90, "estimated; mtp needs engine >= 0.30.0"},
		{128, "qwen3.6-35b-a3b", "mtp-q4-gguf", 90,
			"estimated; the larger 80b/120b/122b families have LOWER quality_tier than 35b-a3b mtp, and the 480b (q92) needs ~283 GB resident, so 35b-a3b mtp stays the top fit"},
		{192, "qwen3.6-35b-a3b", "mtp-q4-gguf", 90, "estimated; 480b (q92) still over budget"},
	}

	var prevQuality int
	for _, tc := range cases {
		hw := syntheticAppleUMA(tc.ramGB, 0) // 0 => default budget (iogpu unset)
		pick, err := PickModel(PickInput{
			Catalog:       manifests,
			Hardware:      hw,
			Engine:        catalog.RuntimeOllama,
			EngineVersion: engineVer,
		})
		if err != nil {
			t.Errorf("%d GB: PickModel error: %v", tc.ramGB, err)
			continue
		}
		t.Logf("%3d GB (budget %5d MB): pick=%s/%s q%d  [%s]",
			tc.ramGB, hw.EffectiveVRAMMB(), pick.Manifest.ModelID, pick.Variant.VariantID,
			pick.Variant.QualityTier, tc.note)
		if pick.Manifest.ModelID != tc.wantModelID || pick.Variant.VariantID != tc.wantVariant {
			t.Errorf("%d GB: pick = %s/%s, want %s/%s",
				tc.ramGB, pick.Manifest.ModelID, pick.Variant.VariantID, tc.wantModelID, tc.wantVariant)
		}
		if pick.Variant.QualityTier != tc.wantQuality {
			t.Errorf("%d GB: pick quality = %d, want %d", tc.ramGB, pick.Variant.QualityTier, tc.wantQuality)
		}
		// Monotonicity: more RAM never lowers the auto-pick's quality tier.
		if pick.Variant.QualityTier < prevQuality {
			t.Errorf("%d GB: quality %d regressed below smaller tier's %d",
				tc.ramGB, pick.Variant.QualityTier, prevQuality)
		}
		prevQuality = pick.Variant.QualityTier
	}
}

// TestUMA8GBFitsMidModelsOnMetal is the #424 regression guard, the inverse of
// the old #415 finding-lock: on an 8 GB Apple Silicon Mac the Metal-aware
// 1024 MB UMA overhead (down from the CUDA-calibrated 4096) lets the 3.4 GB
// qwen3.5-4b and the 4.7 GB qwen2.5-coder-7b fit the 6144 MB budget — the
// models the box actually runs on Metal (UMA shares memory; ollama spills
// gracefully). Before #424 the 4 GB overhead pushed both just past the budget
// and collapsed the auto-pick to the 1.9 GB qwen3.5-2b. If the UMA overhead is
// ever raised back, this test catches the regression.
func TestUMA8GBFitsMidModelsOnMetal(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	hw := syntheticAppleUMA(8, 0) // 6144 MB budget

	for _, id := range []string{"qwen3.5-4b", "qwen2.5-coder-7b-instruct"} {
		m, ok := manifestByPrefix(manifests, id)
		if !ok {
			t.Fatalf("catalog missing %s", id)
		}
		fit := FamilyBestFit(m, catalog.RuntimeOllama, runtime.OllamaPinnedVersion, hw)
		if !fit.Fits {
			t.Errorf("%s does not fit 8 GB (deficit=%q) — UMA overhead may have been raised; #424 expects it to fit", id, fit.DeficitLabel)
		}
		t.Logf("8 GB: %s fits=%v (runs on Metal; #424 Metal-aware overhead)", id, fit.Fits)
	}
}

// TestUMABudgetGovernsNotRAM verifies the residency budget (UsableVRAMMB),
// not RAMTotalGB, is authoritative on UMA hosts: a 64 GB Mac whose operator
// capped iogpu.wired_limit_mb to 6144 MB picks the same model as an 8 GB Mac
// (qwen3.5-4b under the #624 context floor), not a model sized to its 64 GB
// of RAM. This guards the UMA fit path the issue cares about.
func TestUMABudgetGovernsNotRAM(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	hw := syntheticAppleUMA(64, 6144) // 64 GB RAM but only 6 GB GPU-addressable
	pick, err := PickModel(PickInput{
		Catalog: manifests, Hardware: hw, Engine: catalog.RuntimeOllama,
		EngineVersion: runtime.OllamaPinnedVersion,
	})
	if err != nil {
		t.Fatalf("PickModel: %v", err)
	}
	t.Logf("64 GB RAM / 6144 MB UMA budget: pick=%s/%s q%d",
		pick.Manifest.ModelID, pick.Variant.VariantID, pick.Variant.QualityTier)
	if pick.Manifest.ModelID != "qwen3.5-4b" {
		t.Errorf("budget-capped pick = %s, want qwen3.5-4b (UMA budget, not 64 GB RAM, must govern; #624 floor excludes the 32k coder-7b)", pick.Manifest.ModelID)
	}
}

// TestUMANothingFitsTinyBudget documents that a sufficiently small UMA budget
// rejects the WHOLE catalog (ErrHardwareInsufficient), the genuine "nothing
// fits" case — distinct from the 8 GB case, which does fit. The threshold
// dropped with #424's smaller UMA overhead: the smallest resident is now
// qwen3.5-0.8b at ~2170 MB (954 weight + 192 KV + 1024 overhead), so the
// budget must be below that to reject everything.
func TestUMANothingFitsTinyBudget(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	// Budget below the smallest resident. The catalog floor is now the
	// qwen2.5-coder-0.5b tiny model (~1.7 GB resident at its 32k context),
	// so a 1 GB budget still fits nothing.
	hw := syntheticAppleUMA(8, 1024)
	_, err = PickModel(PickInput{
		Catalog: manifests, Hardware: hw, Engine: catalog.RuntimeOllama,
		EngineVersion: runtime.OllamaPinnedVersion,
	})
	if !errors.Is(err, ErrHardwareInsufficient) {
		t.Errorf("1 GB budget: err = %v, want ErrHardwareInsufficient", err)
	}
}

// syntheticAppleUMA builds an Apple-Silicon UMA hardware profile. budgetMB
// overrides the GPU-addressable budget; 0 means "use the iogpu-unset default"
// of RAMTotalGB * 3/4 * 1024, matching profiler_darwin.go's defaultUMA.
func syntheticAppleUMA(ramGB, budgetMB int) hardware.Profile {
	if budgetMB == 0 {
		budgetMB = ramGB * 1024 * 3 / 4
	}
	return hardware.Profile{
		OS:            "darwin",
		Arch:          "arm64",
		RAMTotalGB:    ramGB,
		UnifiedMemory: true,
		UsableVRAMMB:  budgetMB,
		GPUs:          []hardware.GPU{{Vendor: "apple", Model: "Apple (synthetic)"}},
		Accelerators:  hardware.Accelerators{Metal: true},
	}
}

// manifestByPrefix returns the first manifest whose ModelID starts with
// prefix. Defined here (untagged) so both the cross-platform tier tests and
// the darwin-gated selection test can use it.
func manifestByPrefix(ms []catalog.Manifest, prefix string) (catalog.Manifest, bool) {
	for _, m := range ms {
		if strings.HasPrefix(m.ModelID, prefix) {
			return m, true
		}
	}
	return catalog.Manifest{}, false
}
