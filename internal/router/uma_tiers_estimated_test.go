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
		// RECORD OF TODAY'S BEHAVIOUR, not a contract — and it is a
		// DEMOTION from qwen3.5-4b (q42), taken deliberately in #448.
		//
		// The 4b's KV was annotated at 12288 B/token, the value of its 2b
		// sibling; its architecture derives 32768 (8 full-attention layers
		// x 4 KV heads x 256 head_dim x 2 x 2 bytes) and a real engine load
		// measures exactly that. On the 6144 MB budget the honest figure
		// leaves the 4b holding ~120k, not the ~200k the old number
		// claimed — so the row above did not describe a machine that could
		// serve the floor window, it described an under-stated input.
		//
		// The consequence is NOT contained here: q27 is below
		// InstallQualityFloorTier (30), so an 8 GB Mac now reports
		// under-spec and installs no local engine at all. That is the
		// install path converting a quality judgement into a hard
		// outcome, which is a separate defect — waired-ai/waired#1056.
		// This row is the sentinel for it: when #1056 lands, an 8 GB Mac
		// should be OFFERED this pick rather than dropped, and this
		// expectation is where that shows up first.
		{8, "qwen3.5-2b", "q4-gguf", 27,
			"#448: the 4b's real KV (32768) leaves ~120k on the 6144 MB budget, below the ~200k floor — 2b is the best floor-passing fit. Below InstallQualityFloorTier, so SelectInstallModel calls this host under-spec (waired-ai/waired#1056)"},
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
// capped iogpu.wired_limit_mb to 6144 MB picks what an 8 GB Mac picks, not a
// model sized to its 64 GB of RAM. This guards the UMA fit path the issue
// cares about.
//
// Asserted as an EQUALITY against the 8 GB Mac's own pick rather than
// against a model id. The property is "the budget governs"; naming the
// model made this test restate the tier table above, so a catalog change
// that moved both picks identically still had to be edited in two places
// (#448 was such a change). Only a divergence between the two hosts can
// fail it now, which is the thing that would actually mean RAM had crept
// back into the decision.
func TestUMABudgetGovernsNotRAM(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	pick := func(hw hardware.Profile) Pick {
		t.Helper()
		p, err := PickModel(PickInput{
			Catalog: manifests, Hardware: hw, Engine: catalog.RuntimeOllama,
			EngineVersion: runtime.OllamaPinnedVersion,
		})
		if err != nil {
			t.Fatalf("PickModel: %v", err)
		}
		return p
	}
	capped := pick(syntheticAppleUMA(64, 6144)) // 64 GB RAM, 6 GB GPU-addressable
	small := pick(syntheticAppleUMA(8, 0))      // 8 GB RAM, same 6144 MB budget
	uncapped := pick(syntheticAppleUMA(64, 0))  // what 64 GB of RAM would reach

	t.Logf("64 GB RAM / 6144 MB budget: %s/%s q%d | 8 GB Mac: %s/%s q%d | uncapped 64 GB: %s/%s q%d",
		capped.Manifest.ModelID, capped.Variant.VariantID, capped.Variant.QualityTier,
		small.Manifest.ModelID, small.Variant.VariantID, small.Variant.QualityTier,
		uncapped.Manifest.ModelID, uncapped.Variant.VariantID, uncapped.Variant.QualityTier)

	if capped.Manifest.ModelID != small.Manifest.ModelID ||
		capped.Variant.VariantID != small.Variant.VariantID {
		t.Errorf("budget-capped 64 GB Mac picked %s/%s but an 8 GB Mac on the same 6144 MB "+
			"budget picked %s/%s — the UMA budget, not RAMTotalGB, must govern",
			capped.Manifest.ModelID, capped.Variant.VariantID,
			small.Manifest.ModelID, small.Variant.VariantID)
	}
	// Without this the equality above would also hold if the budget were
	// ignored entirely and every Mac picked the same model.
	if uncapped.Manifest.ModelID == capped.Manifest.ModelID {
		t.Errorf("an uncapped 64 GB Mac picked %s too, so this test cannot tell a governing "+
			"budget from a catalog with only one viable model", uncapped.Manifest.ModelID)
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
