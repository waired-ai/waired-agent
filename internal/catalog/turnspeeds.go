package catalog

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
)

// Recorded speeds of catalog variants on one reference host, in the
// product's own unit: the seconds one 32,768-token request takes
// (TurnSeconds, docs/decisions/20260913/2245-speed-is-one-request-at-32768-tokens.md).
//
// What the record is for: the step-down. When a host measures its model
// over the line, it is offered the first ranked candidate that is fully
// resident here and at least 5% faster on this host class (decisions 2
// and 5 of docs/decisions/20260916/0340-catalog-reference-host-rank-and-admission.md).
// The first pick never reads it — decision 4 of
// docs/decisions/20260804/1937 keeps speed predictions out of the pick.
//
// "Reference host" is avoided in the names here on purpose: the host
// cutoff probe already calls its CPU anchor that (proto/hostfit
// host_cutoff.go). This file names the host by its class.

//go:embed turnspeeds.json
var turnSpeedJSON []byte

// TurnSpeedSet is the whole store.
type TurnSpeedSet struct {
	Schema int    `json:"schema"`
	Notes  string `json:"notes,omitempty"`

	// HostClass is the one host class every record was taken on
	// (catalog.HostClasses). One class per store: comparing seconds
	// across machines is exactly what the step-down must not do.
	HostClass string `json:"host_class"`

	// Models is model_id -> variant_id -> record.
	Models map[string]ModelTurnSpeeds `json:"models"`
}

// ModelTurnSpeeds groups one model's variant records.
type ModelTurnSpeeds struct {
	Variants map[string]VariantTurnSpeed `json:"variants"`
}

// Record methods.
const (
	// TurnSpeedMeasured is the median of repeated product measurements
	// on the host class.
	TurnSpeedMeasured = "measured"
	// TurnSpeedEstimated is computed offline for a variant that has no
	// measurement of its own and no measured ollama sibling (decision 2
	// of docs/decisions/20260916/0340). Labelled so a reader never
	// mistakes it for a measurement.
	TurnSpeedEstimated = "estimate"
)

// VariantTurnSpeed is one variant's record.
type VariantTurnSpeed struct {
	// VariantSHA pins which build was measured. A record whose SHA no
	// longer matches the shipped variant is stale.
	VariantSHA string `json:"variant_sha"`
	// SourceDigest is the pulled manifest digest when the variant pins
	// one (Source.Digest). Outside VariantSHA on purpose, as there.
	SourceDigest string `json:"source_digest,omitempty"`

	Method        string `json:"method"`
	Engine        string `json:"engine"`
	EngineVersion string `json:"engine_version"`
	// Backend is what the engine ran on (e.g. "vulkan", "rocm", "cuda").
	Backend string `json:"backend,omitempty"`

	// TurnSeconds is the median across Samples.
	TurnSeconds  float64 `json:"turn_seconds"`
	PrefillTokps float64 `json:"prefill_tokps,omitempty"`
	DecodeTokps  float64 `json:"decode_tokps,omitempty"`
	// SpreadPct is (max-min)/median of TurnSeconds across Samples.
	SpreadPct float64 `json:"spread_pct,omitempty"`
	Samples   int     `json:"samples,omitempty"`

	// The serving configuration the figure belongs to, as the product
	// recorded it (catalog.VariantMeasurement).
	DepthTokens   int    `json:"depth_tokens,omitempty"`
	AppliedWindow int    `json:"applied_window,omitempty"`
	KVCacheType   string `json:"kv_cache_type,omitempty"`
	NumParallel   int    `json:"num_parallel,omitempty"`

	AgentRevision string `json:"agent_revision,omitempty"`
	Retrieved     string `json:"retrieved"`
	Notes         string `json:"notes,omitempty"`
}

// TurnSpeeds decodes the embedded store.
func TurnSpeeds() (TurnSpeedSet, error) {
	var s TurnSpeedSet
	if err := json.Unmarshal(turnSpeedJSON, &s); err != nil {
		return TurnSpeedSet{}, fmt.Errorf("decode turnspeeds.json: %w", err)
	}
	return s, nil
}

// Lookup returns one variant's record.
func (s TurnSpeedSet) Lookup(modelID, variantID string) (VariantTurnSpeed, bool) {
	m, ok := s.Models[modelID]
	if !ok {
		return VariantTurnSpeed{}, false
	}
	rec, ok := m.Variants[variantID]
	return rec, ok
}

// TurnSpeedSource says where a variant's seconds came from.
type TurnSpeedSource string

const (
	// FromOwnMeasurement: the variant was measured.
	FromOwnMeasurement TurnSpeedSource = "measured"
	// FromOllamaDefault: the variant has no measurement, and its model's
	// default ollama variant does (decision 2 of
	// docs/decisions/20260916/0340: borrow the ollama value).
	FromOllamaDefault TurnSpeedSource = "ollama_default"
	// FromEstimate: an offline estimate recorded for the variant.
	FromEstimate TurnSpeedSource = "estimate"
)

// For returns the seconds the step-down compares for (m, v), in the order
// the owner decided (decision 2 of docs/decisions/20260916/0340): the
// variant's own measurement, then the measurement of the same model's
// default ollama variant, then an estimate recorded for the variant.
//
// A record answers only for the build it was taken on: a VariantSHA that
// no longer matches is skipped as if absent, so a changed source never
// borrows an old build's seconds.
func (s TurnSpeedSet) For(m Manifest, v Variant) (float64, TurnSpeedSource, bool) {
	sha := VariantSHA(v)
	if rec, ok := s.Lookup(m.ModelID, v.VariantID); ok && rec.VariantSHA == sha &&
		rec.Method == TurnSpeedMeasured && rec.TurnSeconds > 0 {
		return rec.TurnSeconds, FromOwnMeasurement, true
	}
	if def := m.DefaultVariant[RuntimeOllama]; def != "" && def != v.VariantID {
		for _, sib := range m.Variants {
			if sib.VariantID != def {
				continue
			}
			if rec, ok := s.Lookup(m.ModelID, def); ok && rec.VariantSHA == VariantSHA(sib) &&
				rec.Method == TurnSpeedMeasured && rec.TurnSeconds > 0 {
				return rec.TurnSeconds, FromOllamaDefault, true
			}
		}
	}
	if rec, ok := s.Lookup(m.ModelID, v.VariantID); ok && rec.VariantSHA == sha &&
		rec.Method == TurnSpeedEstimated && rec.TurnSeconds > 0 {
		return rec.TurnSeconds, FromEstimate, true
	}
	return 0, "", false
}

// TurnSpeedGaps lists the offered ollama variants with no current measured
// record, as "model_id/variant_id", sorted. Every ollama variant can be
// measured on the host class; a vLLM variant is not listed, because the
// product's vLLM path does not run there (decision 5 of
// docs/decisions/20260916/0340) and it borrows or estimates instead.
func TurnSpeedGaps(s TurnSpeedSet, manifests []Manifest) []string {
	var gaps []string
	for _, m := range manifests {
		for _, v := range m.Variants {
			if !contains(v.RuntimeSupport, RuntimeOllama) {
				continue
			}
			rec, ok := s.Lookup(m.ModelID, v.VariantID)
			if !ok || rec.Method != TurnSpeedMeasured || rec.VariantSHA != VariantSHA(v) || rec.TurnSeconds <= 0 {
				gaps = append(gaps, m.ModelID+"/"+v.VariantID)
			}
		}
	}
	sort.Strings(gaps)
	return gaps
}
