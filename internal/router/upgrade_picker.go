package router

import (
	"fmt"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/proto/hostfit"
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
//
// The body moved to proto/hostfit, where the SELECTION-time decode
// estimate needs the identical quantity (#229). The two had converged on
// the same arithmetic independently — this picker deriving it from a
// measured throughput, the fit rules from an assumed bandwidth — which
// is the shape of duplication waired-agent#228 exists to prevent. It is
// also a good sign for both: the upgrade prediction has been in
// production use, so the fit estimate is not a new model, only the same
// one applied before any measurement exists.
func ActiveWeightGB(v catalog.Variant) float64 {
	return hostfit.ActiveBytesPerToken(v)
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
//
// The extrapolation assumes the candidate reads at the same effective
// bandwidth the measurement was taken at, which stops being true the
// moment a candidate spills to system memory: the measured host was
// reading from a graphics card, the candidate would read a large share
// over PCIe. That used to be handled for free, because RankModels'
// residency gate could not return a spilled candidate at all — the gate
// is gone (#229), so the prediction is capped by the candidate's own
// spill bound where one exists. Two estimates of the same quantity; the
// honest answer is the smaller.
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
		// The active MODEL, not just the active variant — the same rule
		// LighterCandidate carries, and for the same reasons (see the skip
		// there, waired-agent#754). Placed BEFORE the tier break so a
		// higher-tier sibling is stepped over rather than ending the
		// search: the shipped catalog has one, qwen3.6-27b's mtp-q4-gguf
		// at 69 above q4-gguf at 68.
		//
		// Defence in depth rather than a reported defect: this picker
		// returns early when the active variant does not resolve, so it
		// never reached #754's trigger, and tier ordering reaches a
		// different model first in the shipped catalog today.
		if c.Manifest.ModelID == in.ActiveModelID {
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
		// Cap by the spill bound: a candidate whose weights do not all
		// fit the card cannot reach the rate a resident measurement
		// extrapolates to, however much headroom that measurement showed.
		if est := c.DecodeEstimate; est.UpperBound && est.TokpsEstimate > 0 && est.TokpsEstimate < predicted {
			predicted = est.TokpsEstimate
		}
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
