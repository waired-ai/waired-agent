// Package scoring encodes the deterministic model-footprint formulas used to
// author bundled catalog manifests: weight size, KV-cache footprint, decode
// FLOPs, and the quality_tier composite. It is pure (no I/O) so the numbers
// it produces are trivially testable and re-derivable by a reviewer.
//
// The formulas and constants come verbatim from
// docs/reports/20260516-coding-model-scoring.md §2 (and §5.2 for the tier
// composite). See internal/catalog/scoring/scoring.go for the physics and
// tier.go for quality_tier derivation.
package scoring

import "strings"

// Quant describes a weight-quantization scheme: its effective bits-per-weight
// (bpw, used for weight-size estimation) and the catalog quantization_tier
// (the [1,8] precision ladder the Phase 7 router score multiplies by, see
// catalog.Variant.QuantizationTier).
//
// BPW values are the "effective" bits-per-weight from the scoring report §2.3
// (they include group scale/zero-point overhead where relevant). They model
// the QUANTIZED weight pool; for partially-quantized formats (AWQ / GPTQ /
// MXFP4) the real on-disk size is higher because embeddings and attention
// weights stay at higher precision — see WeightGB's doc comment.
type Quant struct {
	Name string
	BPW  float64 // effective bits per weight (scoring report §2.3)
	Tier int     // catalog quantization_tier, [1, 8]
}

// quants is the canonical table from scoring report §2.3, plus the
// quantization_tier ladder documented on catalog.Variant.QuantizationTier.
// Keyed by the normalized (uppercase, separator-folded) quant name.
var quants = []Quant{
	{Name: "BF16", BPW: 16.0, Tier: 8},
	{Name: "FP16", BPW: 16.0, Tier: 8},
	{Name: "Q8_0", BPW: 8.5, Tier: 8},
	{Name: "FP8", BPW: 8.0, Tier: 8},
	{Name: "Q6_K", BPW: 6.57, Tier: 6},
	{Name: "Q5_K_M", BPW: 5.69, Tier: 5},
	{Name: "Q4_K_M", BPW: 4.83, Tier: 4},
	{Name: "AWQ-int4", BPW: 4.5, Tier: 4},
	{Name: "GPTQ-int4", BPW: 4.5, Tier: 4},
	{Name: "Q4_0", BPW: 4.5, Tier: 4},
	{Name: "MXFP4", BPW: 4.25, Tier: 4},
}

// normalizeQuant folds the many spellings of a quantization name onto the
// canonical keys in `quants`: case-insensitive, with "_"/" " treated as "-"
// and a few common synonyms collapsed ("awq" -> "AWQ-int4").
func normalizeQuant(s string) string {
	u := strings.ToUpper(strings.TrimSpace(s))
	u = strings.ReplaceAll(u, " ", "-")
	switch u {
	case "AWQ", "AWQ-INT4", "AWQ_INT4", "INT4-AWQ":
		return "AWQ-int4"
	case "GPTQ", "GPTQ-INT4", "GPTQ_INT4":
		return "GPTQ-int4"
	case "Q4_K_M", "Q4-K-M", "Q4KM":
		return "Q4_K_M"
	case "Q5_K_M", "Q5-K-M", "Q5KM":
		return "Q5_K_M"
	case "Q6_K", "Q6-K":
		return "Q6_K"
	case "Q8_0", "Q8-0", "Q8":
		return "Q8_0"
	case "Q4_0", "Q4-0":
		return "Q4_0"
	case "MXFP4":
		return "MXFP4"
	case "FP8", "FP8-E4M3", "FP8-E5M2":
		return "FP8"
	case "BF16", "BFLOAT16":
		return "BF16"
	case "FP16", "FLOAT16", "F16":
		return "FP16"
	}
	return u
}

// QuantByName looks up a quantization scheme by (normalized) name. The bool is
// false when the name is unrecognized, so callers can fail loudly rather than
// guess a precision.
func QuantByName(name string) (Quant, bool) {
	target := normalizeQuant(name)
	for _, q := range quants {
		if strings.EqualFold(q.Name, target) {
			return q, true
		}
	}
	return Quant{}, false
}
