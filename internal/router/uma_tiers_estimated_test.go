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
		// RESOLVED. This row was the sentinel for a separate defect: q27
		// sat below the install quality floor (30), so an 8 GB Mac
		// reported under-spec and installed no local engine at all — the
		// install path converting a quality judgement into a hard
		// outcome, against waired-ai/waired#1056. The comment said an
		// 8 GB Mac should be OFFERED this pick rather than dropped and
		// that this expectation was where it would show up first.
		//
		// #522 removed the floor (owner decision 2026-08-08). This pick
		// is now what the host installs, not what it is refused.
		{8, "qwen3.5-2b", "q4-gguf", 27,
			"#448: the 4b's real KV (32768) leaves ~120k on the 6144 MB budget, below the ~200k floor — 2b is the best fit that holds its window, and since #522 it is what this host installs"},
		{12, "qwen3.5-4b", "q4-gguf", 42,
			"#624: the 6.6 GB 9b fits by residency but its no-spill window on the 9216 MB budget is ~121k < floor (UMA gets no spill allowance) — 4b keeps the full window"},
		{16, "qwen3.5-9b", "q4-gguf", 52, "confirmed on real Apple M4 (16 GB); 9b's no-spill window ~318k clears the floor here"},
		// PROMOTED from qwen3.5-9b (q52) by waired-agent#1265, which is
		// the point of that lane: the ladder's own flagship now ships a
		// build this machine can hold. The Q4 builds of qwen3.6-35b-a3b
		// (22.6 / 23.9 GB) still leave no room for the ~200k window on
		// an 18432 MB carve-out; the Q2 build reads 12.6 GB and clears
		// both clauses, so a 24 GB Mac goes from a 9B to a 35B-A3B
		// without anything spilling.
		//
		// The dense 27Bs are still out for the reason this row always
		// gave: qwen3.6-27b (q70) is 131072-native, and qwen3.5-27b's
		// 17 GB of weights leave only ~38k of KV here.
		{24, "qwen3.6-35b-a3b", "q2-gguf", 86,
			"#1265: the Q2 build (12.6 GB) is the first 35B-A3B this budget can declare 200k with"},
		{32, "qwen3.6-35b-a3b", "mtp-q4-gguf", 90,
			"estimated; with 1024 MB overhead the mtp variant (resident 22325 MB) now fits the 24576 MB budget, beating q4 (q89); needs engine >= 0.30.0"},
		{64, "qwen3.6-35b-a3b", "mtp-q4-gguf", 90, "estimated; mtp needs engine >= 0.30.0"},
		{128, "qwen3.8-flash-next", "q2-gguf", 91,
			"MEASURED on a 128 GB AMD unified host, not estimated: 55.1 GB of weights served with size_vram == size, no spill (waired-agent#1192). This is the first budget where the large band beats 35b-a3b mtp — the 80b/120b/122b families all sit BELOW it on the ladder, and the 480b (q92) still needs ~283 GB resident"},
		{192, "qwen3.8-flash-next", "q2-gguf", 91, "same pick as 128 GB; 480b (q92) still over budget"},
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
// 1024 MB UMA overhead (down from the CUDA-calibrated 4096) lets a mid-sized
// model fit the 6144 MB budget — the models the box actually runs on Metal
// (UMA shares memory; ollama spills gracefully). Before #424 the 4 GB overhead
// pushed them past the budget and collapsed the auto-pick to the 1.9 GB
// qwen3.5-2b. If the UMA overhead is ever raised back, this catches it.
//
// AMENDED 2026-08-08 (#552). qwen3.5-4b left the list, and NOT because the
// overhead moved — the constant is asserted directly now, so #424's subject no
// longer rides on which model happens to clear a budget. The 4b is a
// 262144-native model and the gate prices it at the 200,704 rung: 7403 MiB
// against 6144.
//
// AMENDED AGAIN 2026-08-08 (#522). qwen2.5-coder-7b was the "better guard,
// being the larger model", and it retired with the 2025 generation. The
// largest ollama build that still fits this budget is qwen3.5-2b, so the
// fitting row is smaller than it was and has correspondingly more room for
// an overhead to hide in. That is why the overhead is asserted directly
// above rather than inferred from these rows: the direct assertion is what
// makes the guard survive its own fixtures being retired.
func TestUMA8GBFitsMidModelsOnMetal(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	// #424's actual subject, asserted as itself.
	if got := hostfit.OllamaVRAMOverheadUMAMB; got != 1024 {
		t.Errorf("UMA overhead = %d MB, want 1024 — #424 lowered it from the CUDA-calibrated 4096", got)
	}
	hw := syntheticAppleUMA(8, 0) // 6144 MB budget

	for _, tc := range []struct {
		id       string
		wantFits bool
		why      string
	}{
		{"qwen3.5-9b", false, "262144-native at 6.6 GB: nowhere near the 6144 MB budget at its rung"},
		{"qwen3.5-4b", false, "262144-native, so priced at the 200,704 rung: 7403 MiB (#552)"},
		{"qwen3.5-2b", true, "holds its full native window here, which is why the host keeps local inference"},
	} {
		m, ok := manifestByPrefix(manifests, tc.id)
		if !ok {
			t.Fatalf("catalog missing %s", tc.id)
		}
		fit := FamilyBestFit(m, catalog.RuntimeOllama, runtime.OllamaPinnedVersion, hw)
		if fit.Fits != tc.wantFits {
			t.Errorf("%s fits 8 GB = %v, want %v (deficit=%q) — %s",
				tc.id, fit.Fits, tc.wantFits, fit.DeficitLabel, tc.why)
		}
		t.Logf("8 GB: %s fits=%v — %s", tc.id, fit.Fits, tc.why)
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

// TestUMANothingFitsTinyPool documents that a sufficiently small unified
// host rejects the WHOLE catalog (ErrHardwareInsufficient), the genuine
// "nothing fits" case — distinct from the 8 GB Mac, which does fit.
//
// It is the POOL that decides this now, not the carve-out. Capacity is a
// certain-OOM question against total memory (waired-ai/waired#1056
// decision 1), and on Apple Silicon the "VRAM" figure is synthesized FROM
// that memory rather than withheld from it — so a machine with 8 GB of
// RAM and a 1 GB wired limit is a machine with 8 GB, running a model the
// OS pages rather than one it cannot load. This test used to assert the
// opposite (8 GB RAM, 1 GB budget → nothing fits), which is the
// carve-out-as-capacity rule that decision replaced.
//
// 2 GB of RAM leaves nothing once the OS allowance is served, so the
// whole catalog is genuinely out of reach.
func TestUMANothingFitsTinyPool(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	hw := syntheticAppleUMA(2, 0)
	_, err = PickModel(PickInput{
		Catalog: manifests, Hardware: hw, Engine: catalog.RuntimeOllama,
		EngineVersion: runtime.OllamaPinnedVersion,
	})
	if !errors.Is(err, ErrHardwareInsufficient) {
		t.Errorf("2 GB pool: err = %v, want ErrHardwareInsufficient", err)
	}

	// And the inversion this replaces, asserted the right way round: a
	// tight wired limit on a roomy pool is a slow configuration, not an
	// impossible one.
	if _, err := PickModel(PickInput{
		Catalog: manifests, Hardware: syntheticAppleUMA(8, 1024),
		Engine: catalog.RuntimeOllama, EngineVersion: runtime.OllamaPinnedVersion,
	}); err != nil {
		t.Errorf("8 GB pool with a 1 GB wired limit: err = %v, want a pick — "+
			"the pool is the capacity ceiling on Apple Silicon", err)
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
