package router

import (
	"fmt"
	"sync"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// FasterStepFactor is how much faster a candidate must be to be offered:
// its seconds on the host class must be at most this fraction of the
// active variant's. 5% is the owner's (decision 2 of
// docs/decisions/20260916/0340-catalog-reference-host-rank-and-admission.md,
// 2026-09-16: 「5%ぐらいにする」). It keeps a candidate that is only as fast
// within measurement noise from being offered as an improvement.
const FasterStepFactor = 0.95

// FasterCandidate returns the model a host that measured its model over
// the line should be offered instead, or (Pick{}, false) when there is
// none. It backs the #133 step-down: the daemon calls it when one request
// took longer than the line (docs/decisions/20260913/2245).
//
// The rule is the owner's (decision 2 of
// docs/decisions/20260916/0340-catalog-reference-host-rank-and-admission.md):
// among the candidates that are fully resident on THIS host and at least
// 5% faster than the active variant on the reference host class, the first
// in rank order. "Faster" reads recorded seconds (catalog.TurnSpeeds), not
// weights: a lighter model is not a faster one — a 3B-active mixture of
// experts outruns a dense 27B of lower weight — and the rule used to offer
// the lighter one.
//
// What it keeps from the rule it replaces
// (docs/decisions/20260820/1130-step-down-walks-the-selection-ladder.md):
//   - the first candidate in rank order, so the step-down walks the same
//     ladder the pick does (waired-agent#834);
//   - never another variant of the active model (waired-agent#754);
//   - never a variant this host already measured over the line
//     (waired-agent#784).
//
// Each of those is checked here rather than trusted to RankModels: its
// narrowing passes stand down when they would empty the set, so a ranked
// candidate can be one this host cannot hold or has measured slow.
//
// The chain terminates: every accepted step lowers the seconds being
// compared against by at least 5%, and there are finitely many variants.
//
// A variant with no recorded seconds — neither its own, nor its model's
// default ollama variant's, nor an estimate (catalog.TurnSpeedSet.For) —
// cannot be judged faster or slower, so it is never offered, and an active
// variant with none gets no offer.
//
// The baseline is the active variant looked up in in.Catalog. When it is
// not in the catalog (a stale or externally pinned selection), the top
// ranked pick stands in for it, and the different-model skip is what keeps
// that fallback from offering the model the host already runs.
func FasterCandidate(in PickInput, activeModelID, activeVariantID string) (Pick, bool) {
	ranked, err := RankModels(in)
	if err != nil || len(ranked) == 0 {
		return Pick{}, false
	}
	speedFor := in.TurnSpeedFor
	if speedFor == nil {
		speedFor = shippedTurnSpeedFor
	}

	baseManifest, baseVariant, ok := findCatalogPair(in.Catalog, activeModelID, activeVariantID)
	if !ok {
		baseManifest, baseVariant = ranked[0].Manifest, ranked[0].Variant
	}
	baseSeconds, ok := speedFor(baseManifest, baseVariant)
	if !ok {
		return Pick{}, false
	}

	for i := range ranked {
		best := ranked[i]
		// Another MODEL, never another build of the active one
		// (waired-agent#754; decision 4 of
		// docs/decisions/20260913/2355-catalog-variant-kv-and-residency-rulings.md).
		// This path accepts a model-level switch and offers to remove the
		// model it moved off, which would delete the weights still serving.
		if best.Manifest.ModelID == activeModelID {
			continue
		}
		// Fully resident here (decision 10 of the same record).
		if !best.Recommendation.Fits {
			continue
		}
		// Not measured over the line on this host (waired-agent#784).
		if in.TurnBudgetSeconds > 0 && best.MeasuredTurnSeconds > in.TurnBudgetSeconds {
			continue
		}
		seconds, ok := speedFor(best.Manifest, best.Variant)
		if !ok || seconds > baseSeconds*FasterStepFactor {
			continue
		}
		best.Reasons = []string{
			fmt.Sprintf("recommend %s/%s (quality_tier=%d) — highest-ranked candidate that fits this host and takes %.0f s per request on the reference host class against %.0f s for %s/%s",
				best.Manifest.ModelID, best.Variant.VariantID, best.Variant.QualityTier,
				seconds, baseSeconds, activeModelID, activeVariantID),
		}
		return best, true
	}
	return Pick{}, false
}

// shippedTurnSpeeds decodes the embedded store once per process. The
// store is asked for every candidate of every pick, and decoding the
// whole JSON each time cost a dozen decodes per recommendation
// (waired-ai/waired-agent#1400 review). It is embedded data, so the
// answer cannot change while the process runs.
var shippedTurnSpeeds = sync.OnceValues(catalog.TurnSpeeds)

// shippedTurnSpeedFor reads the embedded store. A store that does not
// decode answers nothing, which disarms the step-down rather than
// inventing an ordering.
func shippedTurnSpeedFor(m catalog.Manifest, v catalog.Variant) (float64, bool) {
	s, err := shippedTurnSpeeds()
	if err != nil {
		return 0, false
	}
	seconds, _, ok := s.For(m, v)
	return seconds, ok
}

// findCatalogPair locates a (modelID, variantID) across the catalog and
// returns its manifest with it.
func findCatalogPair(cat []catalog.Manifest, modelID, variantID string) (catalog.Manifest, catalog.Variant, bool) {
	for _, m := range cat {
		if m.ModelID != modelID {
			continue
		}
		if v, ok := findVariant(m, variantID); ok {
			return m, v, true
		}
		return catalog.Manifest{}, catalog.Variant{}, false
	}
	return catalog.Manifest{}, catalog.Variant{}, false
}
