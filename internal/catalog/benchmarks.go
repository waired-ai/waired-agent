package catalog

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
)

// benchmarksJSON is the tracked benchmark-evidence store, embedded so tier
// derivation and CI stay hermetic (no network at test/build time).
//
//go:embed benchmarks.json
var benchmarksJSON []byte

// BenchmarkSet is the decoded benchmarks.json.
//
// It is NOT an input to the quality composite. quality_tier is the parameter
// ordering of the generations we carry, and the only maintainer judgment in it
// is where a model from another family or generation sits; this file is where
// the evidence behind such a judgment lives
// (docs/decisions/20260805/1427-quality-tier-is-a-curated-ladder.md).
//
// Because these numbers never rank anything, rows may carry different
// benchmarks from one another — there is no aggregation across them and
// nothing is calibrated against anything else.
type BenchmarkSet struct {
	Schema int    `json:"schema"`
	Notes  string `json:"notes,omitempty"`

	// AcceptedSources are the leaderboards whose numbers may back a
	// tier_override. Everything else may be recorded, but cannot justify a
	// placement.
	AcceptedSources []AcceptedSource `json:"accepted_sources"`

	// Models is keyed by the EXACT Manifest.ModelID. Not an alias: the scoring
	// path looks rows up by exact id, so an alias-keyed row would be invisible
	// to it while still looking present here.
	Models map[string]ModelBenchmarks `json:"models"`
}

// BenchmarkSchema is the schema version this build understands.
const BenchmarkSchema = 2

// AcceptedSource is one leaderboard we accept as evidence.
//
// Two properties are required of it and neither is negotiable by convenience:
// it must still be updated, and it must run the models itself. A frozen
// leaderboard cannot rank a model released after it froze, and a vendor's own
// number is not comparable to another vendor's — gpt-oss-120b reads 82.7,
// 81.9, 42.68 and 83.2 on one benchmark depending on who ran it and at what
// reasoning effort.
//
// LastUpdated is that source's own last refresh, recorded so a source going
// quiet is visible here rather than discovered later. It is deliberately not a
// test failure on a timer: a gate that reddens on a calendar reddens on days
// nobody changed anything.
type AcceptedSource struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	URL         string `json:"url"`
	LastUpdated string `json:"last_updated"` // YYYY-MM-DD
	Notes       string `json:"notes,omitempty"`
}

// ModelBenchmarks is one model's recorded evidence.
type ModelBenchmarks struct {
	// Scores is keyed by an identifier of our own choosing that names the
	// benchmark and, where the benchmark has versions or windows, pins them
	// (e.g. "livebench_2026_06_25_code_generation").
	Scores map[string]BenchmarkScore `json:"scores,omitempty"`

	// Variants pins a per-variant quality_tier and says why.
	Variants map[string]VariantBenchmark `json:"variants,omitempty"`

	Notes string `json:"notes,omitempty"`
}

// BenchmarkScore is one recorded number.
//
// Source names an AcceptedSource.ID when the number came from one. An empty
// Source is legal and means "recorded, but not something a placement may rest
// on" — a vendor card, a paper, a leaderboard that has stopped moving. Keeping
// those visible is the point: deleting them would hide that we looked.
type BenchmarkScore struct {
	Value     float64 `json:"value"`
	Source    string  `json:"source,omitempty"`
	URL       string  `json:"url"`
	Retrieved string  `json:"retrieved"` // YYYY-MM-DD
	Notes     string  `json:"notes,omitempty"`
}

// VariantBenchmark pins a variant's quality_tier.
//
// Reason is required on every override. An exclusion or a promotion nobody had
// to justify is one nobody revisits, and the three overrides this file carried
// before 2026-08-05 carried no reason at all.
//
// Evidence lists Scores keys, and every one of them must name an
// AcceptedSource. An override is by definition a claim that the ordering the
// composite produced is wrong, and that claim is exactly the kind this repo
// requires a citable source for.
type VariantBenchmark struct {
	TierOverride int      `json:"tier_override,omitempty"`
	Reason       string   `json:"reason"`
	Evidence     []string `json:"evidence,omitempty"`
}

// Benchmarks decodes the embedded benchmarks.json.
func Benchmarks() (BenchmarkSet, error) {
	var bs BenchmarkSet
	if err := json.Unmarshal(benchmarksJSON, &bs); err != nil {
		return BenchmarkSet{}, fmt.Errorf("catalog: parse benchmarks.json: %w", err)
	}
	if bs.Schema != BenchmarkSchema {
		return BenchmarkSet{}, fmt.Errorf("catalog: benchmarks.json schema %d, want %d", bs.Schema, BenchmarkSchema)
	}
	return bs, nil
}

// AcceptedSourceIDs returns the ids a score may cite to back an override.
func (s BenchmarkSet) AcceptedSourceIDs() []string {
	out := make([]string, 0, len(s.AcceptedSources))
	for _, a := range s.AcceptedSources {
		out = append(out, a.ID)
	}
	sort.Strings(out)
	return out
}

// Override returns the pin recorded for one variant, if any.
func (s BenchmarkSet) Override(modelID, variantID string) (VariantBenchmark, bool) {
	mb, ok := s.Models[modelID]
	if !ok {
		return VariantBenchmark{}, false
	}
	vb, ok := mb.Variants[variantID]
	return vb, ok
}

// EvidenceIssue is one way the store fails the rules its own doc states.
type EvidenceIssue struct {
	Key    string // "model_id" or "model_id/variant_id"
	Reason string
}

// CheckEvidence reports every override that is not justified the way
// VariantBenchmark's doc requires, plus rows that name a model the given
// manifests do not offer.
//
// It takes the manifests rather than reading the bundled catalog so a test can
// hand it a fixture; the shipped-catalog check is one caller among several.
func (s BenchmarkSet) CheckEvidence(manifests []Manifest) []EvidenceIssue {
	accepted := map[string]bool{}
	for _, a := range s.AcceptedSources {
		accepted[a.ID] = true
	}
	known := map[string]map[string]bool{}
	for _, m := range manifests {
		vs := map[string]bool{}
		for _, v := range m.Variants {
			vs[v.VariantID] = true
		}
		known[m.ModelID] = vs
	}

	var issues []EvidenceIssue
	for _, id := range sortedKeys(s.Models) {
		mb := s.Models[id]
		variants, offered := known[id]
		if !offered {
			issues = append(issues, EvidenceIssue{id, "no manifest has this exact model_id (an alias-keyed row is invisible to tier assignment)"})
			continue
		}
		for _, vid := range sortedKeys(mb.Variants) {
			vb := mb.Variants[vid]
			key := id + "/" + vid
			if !variants[vid] {
				issues = append(issues, EvidenceIssue{key, "no such variant in the manifest"})
				continue
			}
			if vb.TierOverride == 0 {
				continue
			}
			if vb.Reason == "" {
				issues = append(issues, EvidenceIssue{key, "tier_override without a reason"})
			}
			if len(vb.Evidence) == 0 {
				issues = append(issues, EvidenceIssue{key, "tier_override without evidence from an accepted source"})
				continue
			}
			for _, ev := range vb.Evidence {
				sc, ok := mb.Scores[ev]
				switch {
				case !ok:
					issues = append(issues, EvidenceIssue{key, fmt.Sprintf("evidence %q is not a key in this model's scores", ev)})
				case sc.Source == "":
					issues = append(issues, EvidenceIssue{key, fmt.Sprintf("evidence %q names no source, so it cannot back a placement", ev)})
				case !accepted[sc.Source]:
					issues = append(issues, EvidenceIssue{key, fmt.Sprintf("evidence %q cites %q, which is not an accepted source", ev, sc.Source)})
				}
			}
		}
	}
	return issues
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// BenchmarkSource is one provenance record for a researched benchmark value.
// The catalog-radar pipeline still emits these; the store itself now carries
// provenance per score (BenchmarkScore).
type BenchmarkSource struct {
	URL       string  `json:"url"`
	Retrieved string  `json:"retrieved"` // YYYY-MM-DD
	Value     float64 `json:"value"`
}

// confidence levels the catalog-radar research records use.
const (
	ConfidenceHigh   = "high"
	ConfidenceMedium = "medium"
	ConfidenceLow    = "low"
)
