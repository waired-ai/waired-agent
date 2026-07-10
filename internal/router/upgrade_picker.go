package router

import (
	"fmt"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// DefaultUpgradeSafetyMargin is the factor by which a candidate's
// PREDICTED throughput must clear the interactive floor before the
// agent suggests upgrading to it. The prediction is a bandwidth-scaling
// estimate, not a measurement — the margin absorbs its error so an
// accepted upgrade doesn't immediately re-trigger the lighter-model
// flow ("flapping").
const DefaultUpgradeSafetyMargin = 1.25

// ActiveWeightGB returns the bandwidth-relevant weight of a variant:
// the bytes that must stream through memory per decoded token. For
// MoE variants (ActiveParams > 0) that is the weight scaled by the
// active-parameter fraction; dense variants stream all weights.
// Returns 0 when the variant declares no weight (prediction
// impossible).
func ActiveWeightGB(v catalog.Variant) float64 {
	if v.EstimatedWeightGB <= 0 {
		return 0
	}
	if v.ActiveParams > 0 && v.ParamCount > 0 && v.ActiveParams < v.ParamCount {
		return v.EstimatedWeightGB * float64(v.ActiveParams) / float64(v.ParamCount)
	}
	return v.EstimatedWeightGB
}

// UpgradeInput parameterises UpgradeCandidate.
type UpgradeInput struct {
	// Pick supplies the catalog / hardware / engine context. Leave
	// PreferredModelID empty — an upgrade suggestion deliberately looks
	// across families.
	Pick PickInput

	// ActiveModelID / ActiveVariantID identify the currently-served
	// variant the measurement was taken against.
	ActiveModelID   string
	ActiveVariantID string

	// MeasuredTokps is the warm benchmark result for the active
	// variant; FloorTokps is the interactive floor it cleared.
	MeasuredTokps float64
	FloorTokps    float64

	// SafetyMargin overrides DefaultUpgradeSafetyMargin when > 0.
	SafetyMargin float64
}

// UpgradeCandidate is the inverse of LighterCandidate: when a warm
// benchmark shows the host has throughput headroom, it returns the
// HIGHEST-quality_tier fitting variant that is predicted to still
// clear the interactive floor (with margin). Decode is approximately
// memory-bandwidth-bound, so the prediction scales the measured tok/s
// by the ratio of active weights:
//
//	effBW        = MeasuredTokps × ActiveWeightGB(active)
//	predicted(v) = effBW / ActiveWeightGB(v)
//
// Unlike the lighter flow's single-step-down, this jumps straight to
// the best qualifying tier: each step is a multi-GB download plus an
// agent restart, so walking the ladder one rung at a time is far more
// expensive than a slightly bolder (margin-protected) prediction.
//
// ok=false when there is nothing to suggest: the active variant is
// unknown or carries no weight annotation, the measurement is missing,
// or no fitting candidate of a strictly higher tier clears the bar.
// Candidates come from RankModels, so the VRAM-residency gate has
// already excluded variants that would spill to the CPU (where the
// bandwidth model would be wildly optimistic).
func UpgradeCandidate(in UpgradeInput) (Pick, float64, bool) {
	if in.MeasuredTokps <= 0 || in.FloorTokps <= 0 {
		return Pick{}, 0, false
	}
	margin := in.SafetyMargin
	if margin <= 0 {
		margin = DefaultUpgradeSafetyMargin
	}

	active, ok := findCatalogVariant(in.Pick.Catalog, in.ActiveModelID, in.ActiveVariantID)
	if !ok {
		return Pick{}, 0, false
	}
	activeWeight := ActiveWeightGB(active)
	if activeWeight <= 0 {
		return Pick{}, 0, false
	}
	effBW := in.MeasuredTokps * activeWeight

	ranked, err := RankModels(in.Pick)
	if err != nil {
		return Pick{}, 0, false
	}

	bar := in.FloorTokps * margin
	// RankModels is sorted by quality_tier desc, so the first
	// qualifying candidate is the best one.
	for i := range ranked {
		c := ranked[i]
		if c.Manifest.ModelID == in.ActiveModelID && c.Variant.VariantID == in.ActiveVariantID {
			continue
		}
		if c.Variant.QualityTier <= active.QualityTier {
			break // sorted desc: nothing below this rank is an upgrade
		}
		w := ActiveWeightGB(c.Variant)
		if w <= 0 {
			continue // no weight annotation → no prediction
		}
		predicted := effBW / w
		if predicted < bar {
			continue
		}
		c.Reasons = []string{fmt.Sprintf(
			"upgrade headroom: measured %.0f tok/s on %s/%s; %s/%s (quality_tier=%d) predicted ~%.0f tok/s ≥ %.0f×%.2f floor",
			in.MeasuredTokps, in.ActiveModelID, in.ActiveVariantID,
			c.Manifest.ModelID, c.Variant.VariantID, c.Variant.QualityTier,
			predicted, in.FloorTokps, margin)}
		return c, predicted, true
	}
	return Pick{}, 0, false
}
