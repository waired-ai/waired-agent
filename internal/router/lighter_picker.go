package router

import (
	"fmt"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// LighterCandidate returns the highest-ranked fitting variant that is
// genuinely LIGHTER than the active (activeModelID, activeVariantID),
// or (Pick{}, false) when none exists. It backs the issue #133
// "benchmark below interactive floor → recommend a lighter model" flow:
// the daemon calls it when the boot benchmark measured throughput below
// the interactive floor.
//
// "Lighter" is decided by the engine-appropriate resource footprint,
// compared with deterministic tiebreaks (see footprintCmp):
//
//  1. EstimatedWeightGB (primary — what actually drives load/throughput;
//     skipped when either side is 0/unknown so an un-annotated variant
//     isn't mistaken for a tiny one)
//  2. MinVRAMMB (vllm) / MinRAMGB (ollama)
//  3. ParamCount
//
// Among the candidates strictly lighter than active it returns the
// HIGHEST-RANKED one. RankModels is sorted by quality_tier desc, so that
// is simply the first admitted candidate.
//
// The rank ladder is the one the rest of this flow already walks: the
// auto-picker sorts by it (model_picker.go), UpgradeCandidate walks it in
// the opposite direction, and the CLI decides whether anything is ranked
// below the offer on it (cmd/waired/init_modelselect.go's
// isLightestOfferedModel — "An ORDERING, not a floor"). Selecting the
// step-down by footprint instead made the two halves of one flow disagree,
// and on the shipped catalog it traded 17 quality_tier points for 0.1 GB:
// qwen3.5-35b-a3b (tier 73, 24.0 GB) beat qwen3.6-35b-a3b (tier 89/90 at
// 23.9/22.6 GB) below an 81.0 GB baseline (waired-agent#834, reported in
// the v0.0.3-rc2 owner review waired-ai/waired#1223).
//
// Still one step at a time: re-benchmarking the lighter model after the
// user accepts the switch chains naturally to a further step if it is
// still below the floor — no need to evaluate the whole ladder up front.
// The chain terminates because each accepted step lowers the baseline
// footprint, so the admitted set shrinks strictly.
//
// The candidate is always a DIFFERENT model, never another variant of
// the active one — see the skip inside the loop.
//
// The baseline is the active variant looked up in in.Catalog. When the
// active variant is not in the catalog (e.g. a stale or externally-pinned
// selection), the top fitting pick (RankModels[0]) is used as the
// baseline so a lighter alternative can still be offered. That fallback
// is what made waired-agent#754 reachable: the baseline becomes some
// OTHER model, and the active model's own variant is then "strictly
// lighter" than it. The different-model skip below is what keeps the
// fallback from recommending the host what it is already running.
//
// Note: callers typically pass an EMPTY PickInput.PreferredModelID even
// when a model is pinned, so a pinned-but-too-heavy model can still be
// stepped down across families.
func LighterCandidate(in PickInput, activeModelID, activeVariantID string) (Pick, bool) {
	ranked, err := RankModels(in)
	if err != nil || len(ranked) == 0 {
		return Pick{}, false
	}

	// Resolve the baseline footprint (the active variant), falling back
	// to the top pick when active isn't in the catalog.
	baseline, ok := findCatalogVariant(in.Catalog, activeModelID, activeVariantID)
	if !ok {
		baseline = ranked[0].Variant
	}

	for i := range ranked {
		best := ranked[i]
		// Skip the active MODEL, not just the active variant
		// (waired-agent#754). Two reasons, either one sufficient:
		//
		// Matching on the pair meant an unresolvable activeVariantID
		// disarmed the skip entirely — an empty variant_id in state.json,
		// or one the catalog has since renamed, matches nothing — and the
		// pick then landed on the very variant the host was serving. The
		// offer rendered as "Qwen3.6 27B → Qwen3.6 27B".
		//
		// And a sibling variant is not a step down to begin with. Everything
		// downstream of this pick is keyed by model id: the label
		// (cmd/waired/init_modelselect.go), the accept API
		// (management.PreferredModelRequest carries no variant), the
		// residency check, and the "remove the model we moved off" offer —
		// which would delete the weights of the model still serving. The
		// catalog's siblings differ by engine feature rather than weight
		// class anyway: qwen3.6-35b-a3b's LIGHTER variant carries the
		// HIGHER quality_tier.
		if best.Manifest.ModelID == activeModelID {
			continue
		}
		// Must be strictly lighter than the baseline.
		if footprintCmp(best.Variant, baseline, in.Engine) >= 0 {
			continue
		}
		// ranked is quality_tier desc, so the first candidate that clears
		// both tests above is the best one — the same "first qualifying in
		// rank order" rule UpgradeCandidate applies going the other way.
		best.Reasons = []string{
			fmt.Sprintf("recommend lighter %s/%s (quality_tier=%d) — highest-ranked candidate lighter than %s/%s that fits the host",
				best.Manifest.ModelID, best.Variant.VariantID, best.Variant.QualityTier,
				activeModelID, activeVariantID),
		}
		return best, true
	}
	return Pick{}, false
}

// findCatalogVariant locates a (modelID, variantID) across the catalog.
// Returns the zero Variant and false when absent. (findVariant in
// endpoint_router.go searches within a single manifest.)
func findCatalogVariant(cat []catalog.Manifest, modelID, variantID string) (catalog.Variant, bool) {
	for _, m := range cat {
		if m.ModelID != modelID {
			continue
		}
		return findVariant(m, variantID)
	}
	return catalog.Variant{}, false
}

// footprintCmp returns -1, 0, or 1 as a is lighter than, equal to, or
// heavier than b for the given engine. The EstimatedWeightGB axis is
// only consulted when both sides declare it (> 0); otherwise it falls
// through to the engine's hard resource minimum and finally ParamCount,
// both of which the catalog Validate guarantees for real variants.
func footprintCmp(a, b catalog.Variant, engine string) int {
	if a.EstimatedWeightGB > 0 && b.EstimatedWeightGB > 0 && a.EstimatedWeightGB != b.EstimatedWeightGB {
		if a.EstimatedWeightGB < b.EstimatedWeightGB {
			return -1
		}
		return 1
	}
	if engine == catalog.RuntimeVLLM {
		if a.MinVRAMMB != b.MinVRAMMB {
			if a.MinVRAMMB < b.MinVRAMMB {
				return -1
			}
			return 1
		}
	} else {
		if a.MinRAMGB != b.MinRAMGB {
			if a.MinRAMGB < b.MinRAMGB {
				return -1
			}
			return 1
		}
	}
	if a.ParamCount != b.ParamCount {
		if a.ParamCount < b.ParamCount {
			return -1
		}
		return 1
	}
	return 0
}
