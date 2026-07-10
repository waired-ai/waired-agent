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
// v1 is single-step-down: among the candidates strictly lighter than
// active it returns the HEAVIEST one (the smallest drop). Re-benchmarking
// the lighter model after the user accepts the switch chains naturally to
// a further step if it is still below the floor — no need to evaluate the
// whole ladder up front.
//
// The baseline is the active variant looked up in in.Catalog. When the
// active variant is not in the catalog (e.g. a stale or externally-pinned
// selection), the top fitting pick (RankModels[0]) is used as the
// baseline so a lighter alternative can still be offered.
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

	var best *Pick
	for i := range ranked {
		c := ranked[i]
		// Skip the active variant itself.
		if c.Manifest.ModelID == activeModelID && c.Variant.VariantID == activeVariantID {
			continue
		}
		// Must be strictly lighter than the baseline.
		if footprintCmp(c.Variant, baseline, in.Engine) >= 0 {
			continue
		}
		// Single-step-down: keep the heaviest lighter candidate (the one
		// closest to the baseline).
		if best == nil || footprintCmp(c.Variant, best.Variant, in.Engine) > 0 {
			cp := c
			best = &cp
		}
	}
	if best == nil {
		return Pick{}, false
	}
	best.Reasons = []string{
		fmt.Sprintf("recommend lighter %s/%s (quality_tier=%d) — smallest step down from %s/%s that fits the host",
			best.Manifest.ModelID, best.Variant.VariantID, best.Variant.QualityTier,
			activeModelID, activeVariantID),
	}
	return *best, true
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
