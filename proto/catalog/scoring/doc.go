// Package scoring encodes the deterministic model-footprint formulas used to
// author bundled catalog manifests: weight size, KV-cache footprint, decode
// FLOPs, and the quality_tier composite. It is pure (no I/O) so the numbers
// it produces are trivially testable and re-derivable by a reviewer.
//
// The formulas and constants come verbatim from
// docs/reports/20260516-coding-model-scoring.md §2 (and §5.2 for the tier
// composite). See scoring.go for the physics and tier.go for quality_tier
// derivation.
//
// It lives in the proto module so the control plane can price a model a
// person imports with the same formulas (waired-ai/waired#1476):
// EstimateKVFromConfig and EstimateKVFromGGUF, which answer "unknown"
// rather than 0 for a shape they cannot price. Like the rest of proto it
// depends on the standard library and other proto packages only.
package scoring
