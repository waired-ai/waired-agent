package router

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
)

func TestLighterCandidate_StepsDownToTheBestLighterFit(t *testing.T) {
	// 24 GB vLLM host, active = large-vllm/awq-int4 (the heaviest fit).
	// The only lighter fitting variant is mid-vllm/awq-int4.
	//
	// Renamed from _StepsDownFromHeaviest with waired-agent#834: the
	// assertion is unchanged (fixtureCatalog's tiers and weights are
	// co-monotone, so both rules agree here), but the old name described
	// the rule this test never actually distinguished. The rule itself is
	// pinned by _PrefersRankOverWeight and _ShippedCatalogStepsDownByRank.
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

// invertedLadderCatalog anti-correlates the two ladders below the active
// model. fixtureCatalog and siblingVariantCatalog do not: in both of those
// heavier is always higher-tier, so weight order and rank order return the
// same answer and neither fixture could ever see waired-agent#834. That
// co-monotonicity is why the defect shipped with the picker fully covered.
//
// The shape is the shipped catalog's, scaled to the 16 GB ollama host the
// other CPU cases in this file use: qwen3.6-35b-a3b/q4-gguf (23.9 GB,
// quality_tier 89) beside qwen3.5-35b-a3b/q4-gguf (24.0 GB, tier 73), both
// below qwen3.5-122b-a10b (81.0 GB, tier 83).
func invertedLadderCatalog() []catalog.Manifest {
	ollama := func(modelID string, weightGB float64, tier int) catalog.Manifest {
		return catalog.Manifest{
			ModelID: modelID, ContextLength: 32768,
			Capabilities: []string{"chat", "tool_use"},
			Variants: []catalog.Variant{{
				VariantID: "q4-gguf", Format: "ollama-tag",
				Quantization: "Q4_K_M", RuntimeSupport: []string{"ollama"},
				EstimatedWeightGB: weightGB, MinRAMGB: 12, QualityTier: tier,
				ParamCount: 8_000_000_000,
				Source:     catalog.VariantSource{Type: "ollama", Tag: modelID + ":q4_K_M"},
			}},
		}
	}
	return []catalog.Manifest{
		ollama("heavy-ollama", 8.0, 83),   // the active model (the baseline)
		ollama("worse-heavier", 5.1, 73),  // heaviest lighter — today's winner
		ollama("better-lighter", 5.0, 89), // highest-ranked lighter
	}
}

// TestLighterCandidate_PrefersRankOverWeight is the deterministic half of
// waired-agent#834: where the two ladders disagree, the step-down follows
// the rank ladder.
//
// Product contract (ratifying source: waired-agent#834, reported in the
// v0.0.3-rc2 owner review waired-ai/waired#1223).
func TestLighterCandidate_PrefersRankOverWeight(t *testing.T) {
	hw := hardware.Profile{RAMTotalGB: 16}
	in := PickInput{Catalog: invertedLadderCatalog(), Hardware: hw, Engine: "ollama"}

	// Anti-vacuity: both alternatives must reach the ranked set, and they
	// must still invert, or this asserts nothing about the choice.
	ranked, err := RankModels(in)
	if err != nil {
		t.Fatalf("RankModels: %v", err)
	}
	tiers := map[string]int{}
	for _, c := range ranked {
		tiers[c.Manifest.ModelID] = c.Variant.QualityTier
	}
	for _, id := range []string{"better-lighter", "worse-heavier"} {
		if _, ok := tiers[id]; !ok {
			t.Fatalf("%s did not reach the ranked set on this host; the fixture no longer "+
				"presents a choice (ranked=%d)", id, len(ranked))
		}
	}
	if tiers["better-lighter"] <= tiers["worse-heavier"] {
		t.Fatalf("the fixture no longer inverts the ladders (better-lighter tier %d, "+
			"worse-heavier tier %d)", tiers["better-lighter"], tiers["worse-heavier"])
	}

	pick, ok := LighterCandidate(in, "heavy-ollama", "q4-gguf")
	if !ok {
		t.Fatalf("LighterCandidate = !ok, want better-lighter")
	}
	if pick.Manifest.ModelID != "better-lighter" {
		t.Errorf("got %s (tier %d), want better-lighter (tier %d) — the step-down walks the "+
			"rank ladder, not the weight ladder (waired-agent#834)",
			pick.Manifest.ModelID, pick.Variant.QualityTier, tiers["better-lighter"])
	}
}

// TestLighterCandidate_ChainTerminates pins the property the doc comment
// claims: accepting the offer and re-benchmarking chains to a further step
// rather than looping, because each accepted step lowers the baseline
// footprint and so shrinks the admitted set.
//
// Record of today's behaviour, on the shipped catalog. Worth pinning because
// the old rule made the chain nearly useless on this host: heaviest-lighter
// walked 81.0 -> 24.0 -> 23.9 GB, spending a whole download-and-restart cycle
// on a 0.1 GB "step".
func TestLighterCandidate_ChainTerminates(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	in := PickInput{
		Catalog:       manifests,
		Hardware:      reviewHostStrixHalo(),
		Engine:        catalog.RuntimeOllama,
		EngineVersion: "0.32.13",
	}

	modelID, variantID := "qwen3.5-122b-a10b", "q4-gguf"
	prev, ok := findCatalogVariant(manifests, modelID, variantID)
	if !ok {
		t.Fatalf("the catalog no longer carries %s/%s", modelID, variantID)
	}
	steps := 0
	for {
		pick, ok := LighterCandidate(in, modelID, variantID)
		if !ok {
			break
		}
		steps++
		if steps > len(manifests)+1 {
			t.Fatalf("the chain did not terminate in %d steps (now at %s/%s)",
				steps, pick.Manifest.ModelID, pick.Variant.VariantID)
		}
		if footprintCmp(pick.Variant, prev, in.Engine) >= 0 {
			t.Fatalf("step %d moved from %s/%s to %s/%s without getting lighter",
				steps, modelID, variantID, pick.Manifest.ModelID, pick.Variant.VariantID)
		}
		modelID, variantID, prev = pick.Manifest.ModelID, pick.Variant.VariantID, pick.Variant
	}
	// Anti-vacuity: a chain of zero steps would satisfy every assert above.
	if steps < 2 {
		t.Errorf("chain length %d — the shipped catalog no longer offers a multi-step "+
			"ladder below the baseline on this host, so nothing above was exercised", steps)
	}
}

// reviewHostStrixHalo is the v0.0.3-rc2 review host (waired-ai/waired#1223):
// a Windows Ryzen AI Max 395 with a Radeon 8060S iGPU, 128 GB installed and a
// 96 GB GPU budget. It is the host that served qwen3.5-122b-a10b (81.0 GB,
// quality_tier 83) below the coding floor and was offered qwen3.5-35b-a3b
// (24.0 GB, tier 73) as the step-down — 17 tier points to save 0.1 GB over
// qwen3.6-35b-a3b/q4-gguf (23.9 GB, tier 89).
func reviewHostStrixHalo() hardware.Profile {
	return hardware.Profile{
		OS: "windows", Arch: "amd64",
		RAMTotalGB:    128,
		UnifiedMemory: true,
		UsableVRAMMB:  96 * 1024,
		GPUs:          []hardware.GPU{{Vendor: "amd", Model: "Radeon 8060S (synthetic)"}},
	}
}

// TestLighterCandidate_ShippedCatalogStepsDownByRank is waired-agent#834
// against the SHIPPED catalog and the host it was reported on.
//
// Product contract (ratifying source: waired-agent#834, reported in the
// v0.0.3-rc2 owner review waired-ai/waired#1223): the step-down offers the
// highest-ranked candidate that is lighter than the active variant. It walks
// the same ladder RankModels sorts by and the same one the CLI's
// isLightestOfferedModel compares on (cmd/waired/init_modelselect.go — "An
// ORDERING, not a floor"), so the two halves of one flow cannot disagree.
//
// The table runs TWO engine versions on purpose. qwen3.6-35b-a3b's mtp
// variant (22.6 GB, tier 90) carries min_engine_version 0.30.0, so which
// candidate is admitted depends on the engine — pinning one version would
// leave the other case untested and would make the reproduction condition
// depend on a value the test never varied.
func TestLighterCandidate_ShippedCatalogStepsDownByRank(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}

	const (
		activeModelID   = "qwen3.5-122b-a10b"
		activeVariantID = "q4-gguf"
		// The symptom the review reported, named so a regression reads as
		// itself rather than merely "not the expected id".
		reportedWrongPick = "qwen3.5-35b-a3b"
	)

	cases := []struct {
		engineVersion string
		wantModelID   string
		wantVariantID string
		why           string
	}{
		{"0.32.13", "qwen3.6-35b-a3b", "mtp-q4-gguf",
			"tier 90 at 22.6 GB — the highest-ranked candidate lighter than the 81.0 GB baseline"},
		{"0.29.0", "qwen3.6-35b-a3b", "q4-gguf",
			"the mtp variant is below its min_engine_version 0.30.0 floor here, so tier 89 at 23.9 GB is the highest-ranked lighter candidate"},
	}

	for _, tc := range cases {
		in := PickInput{
			Catalog:       manifests,
			Hardware:      reviewHostStrixHalo(),
			Engine:        catalog.RuntimeOllama,
			EngineVersion: tc.engineVersion,
		}

		// Anti-vacuity: the two rules only differ where the ladders
		// disagree. If the shipped catalog ever stops offering a lighter
		// candidate that outranks the HEAVIEST lighter one, this row
		// cannot tell rank-order from weight-order and proves nothing.
		baseline, ok := findCatalogVariant(manifests, activeModelID, activeVariantID)
		if !ok {
			t.Fatalf("engine %s: the catalog no longer carries %s/%s as the baseline",
				tc.engineVersion, activeModelID, activeVariantID)
		}
		ranked, err := RankModels(in)
		if err != nil || len(ranked) == 0 {
			t.Fatalf("engine %s: RankModels: %v (ranked=%d)", tc.engineVersion, err, len(ranked))
		}
		var byRank, byWeight *Pick
		for i := range ranked {
			c := ranked[i]
			if c.Manifest.ModelID == activeModelID {
				continue
			}
			if footprintCmp(c.Variant, baseline, in.Engine) >= 0 {
				continue
			}
			if byRank == nil {
				cp := c
				byRank = &cp // ranked is tier-desc, so the first admitted is the best
			}
			if byWeight == nil || footprintCmp(c.Variant, byWeight.Variant, in.Engine) > 0 {
				cp := c
				byWeight = &cp
			}
		}
		if byRank == nil || byWeight == nil {
			t.Fatalf("engine %s: no candidate is lighter than the %.1f GB baseline at all",
				tc.engineVersion, baseline.EstimatedWeightGB)
		}
		if byRank.Variant.QualityTier <= byWeight.Variant.QualityTier {
			t.Fatalf("engine %s: the shipped catalog no longer inverts the two ladders below "+
				"the baseline (rank-order picks %s/%s tier %d, weight-order picks %s/%s tier %d), "+
				"so this row cannot distinguish the rules",
				tc.engineVersion,
				byRank.Manifest.ModelID, byRank.Variant.VariantID, byRank.Variant.QualityTier,
				byWeight.Manifest.ModelID, byWeight.Variant.VariantID, byWeight.Variant.QualityTier)
		}

		pick, ok := LighterCandidate(in, activeModelID, activeVariantID)
		if !ok {
			t.Errorf("engine %s: no lighter candidate on the reported host", tc.engineVersion)
			continue
		}
		if pick.Manifest.ModelID == reportedWrongPick {
			t.Errorf("engine %s: offered %s/%s (tier %d) — the demotion waired-agent#834 reported",
				tc.engineVersion, pick.Manifest.ModelID, pick.Variant.VariantID, pick.Variant.QualityTier)
		}
		if pick.Manifest.ModelID != tc.wantModelID || pick.Variant.VariantID != tc.wantVariantID {
			t.Errorf("engine %s: got %s/%s (tier %d), want %s/%s — %s",
				tc.engineVersion, pick.Manifest.ModelID, pick.Variant.VariantID,
				pick.Variant.QualityTier, tc.wantModelID, tc.wantVariantID, tc.why)
		}
	}
}
