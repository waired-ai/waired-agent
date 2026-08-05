package scoring

import "math"

// Coefficients for the quality_tier composite:
//
//	composite = tierParamWeight·log10(total_params)
//	          − tierVRAMWeight·log10(footprint_mb)
//
// The parameter term spans ~22 across 3B..480B and ~28 across the offered
// range; the memory-footprint term is a mild penalty (~11 across the same
// range), so of two variants of similar size the one that fits in less memory
// ranks higher, but a footprint difference does not overturn a size
// difference. Worked numbers and the directional checks are in
// docs/knowledges/20260805/1427-catalog-scoring-formula.md.
//
// A third term, 0.3·swe_bench_verified, was removed on 2026-08-05
// (docs/decisions/20260805/1427-quality-tier-is-a-curated-ladder.md). It was
// deleted rather than repointed at another benchmark: no actively-maintained
// independent leaderboard covers this catalog's size range, so 17 of 21
// entries supplied a zero that entered the composite as a 30-point penalty
// rather than as "unknown". quality_tier is now the parameter ordering of the
// generations we carry, and a placement the ordering gets wrong is corrected
// by a tier_override citing its source (see internal/catalog/benchmarks.go).
//
// NOTE on params: this uses the TOTAL parameter count. Capability tracks the
// full pool (see catalog.Variant.ParamCount's doc and the Phase 7 router score
// = ParamCount × QuantizationTier). Using active params would rank a 30B-A3B
// MoE below a 7B dense model, contradicting both the curated ladder and the
// router score — but note that the same choice is why a MoE outranks a dense
// model of similar total size here whatever their measured order, which is one
// of the things a tier_override exists to correct.
//
// Origin of the formula: waired-ai/waired#133 (private). Written bare as "#133"
// until 2026-08-05, which in this public repo resolves to an unrelated issue.
const (
	tierParamWeight = 10.0
	tierVRAMWeight  = 5.0
)

// CompositeScore returns the continuous quality score for one variant.
// totalParams must be > 0; footprintMB is the variant's memory threshold
// (min_vram_mb, or min_ram_gb×1024 for CPU runtimes). The ABSOLUTE value is
// meaningless — only the order across variants matters; catalog.AssignTiers
// maps it to unique integer tiers.
func CompositeScore(totalParams int64, footprintMB int) float64 {
	if totalParams <= 0 {
		return 0
	}
	pTerm := tierParamWeight * math.Log10(float64(totalParams))
	// floor: avoid log10(0) and over-rewarding a tiny declared footprint.
	fMB := max(footprintMB, 1024)
	vTerm := tierVRAMWeight * math.Log10(float64(fMB))
	return pTerm - vTerm
}
