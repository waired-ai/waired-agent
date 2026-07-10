package catalog

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

// benchmarksJSON is the tracked benchmark-provenance store, embedded so tier
// derivation and CI stay hermetic (no network at test/build time).
//
//go:embed benchmarks.json
var benchmarksJSON []byte

// BenchmarkSet is the decoded benchmarks.json: per-model benchmark scores with
// the source provenance behind quality_tier auto-derivation (#133).
type BenchmarkSet struct {
	Schema int                        `json:"schema"`
	Notes  string                     `json:"notes,omitempty"`
	Models map[string]ModelBenchmarks `json:"models"`
}

// ModelBenchmarks holds one model's benchmark data. swe_bench_verified (0-100)
// is the primary quality signal; Secondary holds other cited benchmarks for
// cross-check. Sources is the provenance (>=1 URL + retrieved date). Variants
// optionally pins a per-variant tier_override.
type ModelBenchmarks struct {
	SWEBenchVerified float64                    `json:"swe_bench_verified"`
	Secondary        map[string]float64         `json:"secondary,omitempty"`
	Sources          []BenchmarkSource          `json:"sources"`
	CrossChecked     bool                       `json:"cross_checked"`
	Confidence       string                     `json:"confidence"`
	Variants         map[string]VariantBenchmrk `json:"variants,omitempty"`
}

// BenchmarkSource is one provenance record for a benchmark value.
type BenchmarkSource struct {
	URL       string  `json:"url"`
	Retrieved string  `json:"retrieved"` // YYYY-MM-DD
	Value     float64 `json:"value"`
}

// VariantBenchmrk carries per-variant overrides (e.g. pin a quality_tier).
type VariantBenchmrk struct {
	TierOverride int `json:"tier_override,omitempty"`
}

// Benchmarks decodes the embedded benchmarks.json.
func Benchmarks() (BenchmarkSet, error) {
	var bs BenchmarkSet
	if err := json.Unmarshal(benchmarksJSON, &bs); err != nil {
		return BenchmarkSet{}, fmt.Errorf("catalog: parse benchmarks.json: %w", err)
	}
	return bs, nil
}

// confidence levels used in benchmarks.json.
const (
	ConfidenceHigh   = "high"
	ConfidenceMedium = "medium"
	ConfidenceLow    = "low"
)
