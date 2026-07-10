package scoring

import "math"

// Coefficients for the quality_tier composite (scoring report §5.2, issue
// #133):
//
//	composite = tierParamWeight·log10(total_params)
//	          + tierBenchWeight·swe_bench_verified
//	          − tierVRAMWeight·log10(footprint_mb)
//
// Chosen so the SWE-bench term (0..30 over a 0..100 score) is comparable in
// magnitude to the parameter-size term (~22 across 3B..480B) — a strong
// benchmark meaningfully outranks raw size — while the memory-footprint term is
// a mild penalty (~11). The rationale and directional checks are recorded in
// docs/knowledges/.../catalog-scoring-formula.md.
//
// NOTE on params: this uses the TOTAL parameter count. Capability tracks the
// full pool (see catalog.Variant.ParamCount's doc and the Phase 7 router score
// = ParamCount × QuantizationTier). The report §5.2 wording ("active_params")
// is superseded here because using active params would rank a 30B-A3B MoE below
// a 7B dense model, contradicting both the curated ladder and the router score.
const (
	tierParamWeight = 10.0
	tierBenchWeight = 0.3
	tierVRAMWeight  = 5.0
)

// CompositeScore returns the continuous quality score for one variant.
// totalParams must be > 0; sweBenchVerified is on a 0..100 scale (0 when
// unknown); footprintMB is the variant's memory threshold (min_vram_mb, or
// min_ram_gb×1024 for CPU runtimes). The ABSOLUTE value is meaningless — only
// the order across variants matters; catalog.AssignTiers maps it to unique
// integer tiers.
func CompositeScore(totalParams int64, sweBenchVerified float64, footprintMB int) float64 {
	if totalParams <= 0 {
		return 0
	}
	pTerm := tierParamWeight * math.Log10(float64(totalParams))
	bTerm := tierBenchWeight * sweBenchVerified
	// floor: avoid log10(0) and over-rewarding a tiny declared footprint.
	fMB := max(footprintMB, 1024)
	vTerm := tierVRAMWeight * math.Log10(float64(fMB))
	return pTerm + bTerm - vTerm
}
