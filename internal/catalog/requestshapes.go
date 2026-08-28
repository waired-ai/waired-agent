package catalog

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
)

// Recorded answers to "does this model, on this engine build, render the
// message shapes a coding agent sends?" (waired-agent#1095).
//
// Separate from agentgrade.json rather than a field on
// VariantAgentGrade, for two reasons that are not stylistic. The
// agent-grade importer builds a fresh VariantAgentGrade literal on every
// import, so a field added there is dropped the next time anybody folds
// in a measurement. And every agent-grade record is required to carry a
// tool-call verdict, engine and retrieval date; a shape record has no
// verdict to carry, and bending one of the two to fit the other is how
// they would start disagreeing about which models were measured.
//
// What a record means: the engine answered these shapes this way, on
// this build. It is NOT a claim about the model's quality — that is the
// agent grade — and it is not a claim that a GPU produced it. Nothing in
// a public repository can prove the latter; what the importer can do,
// and does, is refuse everything a fabricated record would have to get
// wrong.

//go:embed requestshapes.json
var requestShapeJSON []byte

// RequestShapeSet is the whole store.
type RequestShapeSet struct {
	Schema int    `json:"schema"`
	Notes  string `json:"notes,omitempty"`

	// Models is model_id -> variant_id -> record.
	Models map[string]ModelRequestShapes `json:"models"`

	// Baseline exempts the variants that were already in the catalog
	// when this check was added: they carry no shape record and are not
	// required to. Keyed "model_id/variant_id" and valued by the
	// variant's VariantSHA.
	//
	// Keyed per VARIANT, not per model, because shape acceptance is a
	// property of the renderer the tag selects. A model-level exemption
	// would silence every future variant of that model, and models here
	// do grow variants. Valued by VariantSHA so that moving a variant's
	// source tag under the same ids retires the exemption rather than
	// carrying it to a renderer nobody measured — and note VariantSHA
	// excludes MinEngineVersion, so an engine pin bump deliberately
	// leaves an exemption, and a record, standing.
	Baseline map[string]string `json:"baseline,omitempty"`
}

// ModelRequestShapes groups one model's variant records.
type ModelRequestShapes struct {
	Variants map[string]VariantRequestShapes `json:"variants"`
}

// VariantRequestShapes is one variant's matrix, as measured.
type VariantRequestShapes struct {
	// VariantSHA pins which variant was measured. A record whose SHA no
	// longer matches the shipped variant is stale, not current.
	VariantSHA string `json:"variant_sha"`

	Engine string `json:"engine"`

	// EngineVersion is required. The same model and the same shape
	// answer differently on different builds — ollama 0.32.13 rejects a
	// non-leading instruction turn on qwen3.8, 0.32.14 tolerates it and
	// 0.32.15 merges it — so a record without a version says nothing.
	EngineVersion string `json:"engine_version"`

	// Shapes is shape name -> outcome.
	Shapes map[string]ShapeOutcome `json:"shapes"`

	AgentRevision string `json:"agent_revision"`
	Host          string `json:"host,omitempty"`
	RunURL        string `json:"run_url,omitempty"`
	Retrieved     string `json:"retrieved"`
	Notes         string `json:"notes,omitempty"`
}

// ShapeOutcome is one recorded row.
type ShapeOutcome struct {
	// Digest identifies the shape row this answers. A record whose
	// digest no longer matches the shipped table answers a question the
	// table no longer asks.
	Digest string `json:"digest"`

	// Outcome is "accepted" or "rejected". "error" is never stored: a
	// run that could not obtain an answer is refused at import.
	Outcome string `json:"outcome"`

	Status int `json:"status"`

	// Marker classifies a rejection. The engine's own error text is not
	// stored — it is upstream's unstable wording, and an engine can echo
	// request content into it.
	Marker string `json:"marker,omitempty"`

	// EngineSawRoles is the message-role sequence the engine received.
	// Measured engine-direct it equals what was sent, and that equality
	// is the claim that the row is the model's answer rather than ours.
	EngineSawRoles []string `json:"engine_saw_roles"`
}

// Recorded outcomes. Mirrors agentgrade's ShapeOutcome vocabulary; the
// probe's third value ("error") is deliberately not representable here.
const (
	ShapeAccepted = "accepted"
	ShapeRejected = "rejected"
)

// RequestShapes decodes the embedded store.
func RequestShapes() (RequestShapeSet, error) {
	var s RequestShapeSet
	if err := json.Unmarshal(requestShapeJSON, &s); err != nil {
		return RequestShapeSet{}, fmt.Errorf("decode requestshapes.json: %w", err)
	}
	return s, nil
}

// Lookup returns one variant's record.
func (s RequestShapeSet) Lookup(modelID, variantID string) (VariantRequestShapes, bool) {
	m, ok := s.Models[modelID]
	if !ok {
		return VariantRequestShapes{}, false
	}
	rec, ok := m.Variants[variantID]
	return rec, ok
}

// BaselineKey is how a variant is named in the Baseline map.
func BaselineKey(modelID, variantID string) string {
	return modelID + "/" + variantID
}

// ShapeRef names one row of the shipped shape table.
//
// Passed in rather than read here: internal/gateway owns the table and
// already imports this package, so reading it from here would close a
// cycle. The caller that has both — cmd/catalog-tool — supplies it, the
// same way CoverageGaps takes the fixture revision it cannot compute.
type ShapeRef struct {
	Name   string
	Digest string
}

// RequestShapeGap is one variant with no usable shape evidence, and why.
type RequestShapeGap struct {
	ModelID   string
	VariantID string
	Reason    string
}

// RequestShapeGaps reports every offered ollama-servable variant that
// carries neither a current shape record nor a standing exemption.
//
// Two kinds of exemption, both stated rather than silent. A model in
// unmeasurable (the agent-grade store's map, reused so the two cannot
// drift about which models no runner can host) is skipped. A variant in
// Baseline whose VariantSHA still matches is skipped.
//
// A manifest with no ollama variant at all is ONE model-level gap
// rather than a skip: a vLLM-only entry with nothing to probe would
// otherwise pass a coverage check having proved nothing, which is how
// deepseek-v4-flash and glm-5.2 are shaped today.
func (s RequestShapeSet) RequestShapeGaps(manifests []Manifest, unmeasurable map[string]string, want []ShapeRef) []RequestShapeGap {
	var out []RequestShapeGap
	for _, m := range manifests {
		if _, ok := unmeasurable[m.ModelID]; ok {
			continue
		}
		checked := 0
		for _, v := range m.Variants {
			if !variantServesOllama(v) {
				continue
			}
			checked++
			sha := VariantSHA(v)
			key := BaselineKey(m.ModelID, v.VariantID)

			if pinned, exempt := s.Baseline[key]; exempt {
				if pinned == sha {
					continue
				}
				out = append(out, RequestShapeGap{m.ModelID, v.VariantID,
					"the baseline exemption was granted to a different variant — measure this one or drop the exemption"})
				continue
			}

			rec, ok := s.Lookup(m.ModelID, v.VariantID)
			if !ok {
				out = append(out, RequestShapeGap{m.ModelID, v.VariantID, "no shape record"})
				continue
			}
			if reason := rec.stale(sha, want); reason != "" {
				out = append(out, RequestShapeGap{m.ModelID, v.VariantID, reason})
			}
		}
		if checked == 0 {
			out = append(out, RequestShapeGap{m.ModelID, "",
				"no ollama variant to probe — record a GPU-lane matrix or declare it in the agent-grade \"unmeasurable\" map"})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ModelID != out[j].ModelID {
			return out[i].ModelID < out[j].ModelID
		}
		return out[i].VariantID < out[j].VariantID
	})
	return out
}

// stale says why a record does not answer the current table, or "".
func (rec VariantRequestShapes) stale(sha string, want []ShapeRef) string {
	if rec.VariantSHA != sha {
		return "recorded against a different variant — the source moved; re-measure"
	}
	if rec.EngineVersion == "" {
		return "no engine version recorded"
	}
	for _, w := range want {
		got, ok := rec.Shapes[w.Name]
		if !ok {
			return fmt.Sprintf("shape %q was never measured", w.Name)
		}
		if got.Digest != w.Digest {
			return fmt.Sprintf("shape %q was measured at digest %s, current is %s — re-measure that row",
				w.Name, got.Digest, w.Digest)
		}
	}
	return ""
}

// RequestShapeRejection names a shape an offered variant's own record
// says the engine refused.
type RequestShapeRejection struct {
	ModelID       string
	VariantID     string
	Shape         string
	Status        int
	Marker        string
	EngineVersion string
}

// RejectedShapes reports every offered variant whose record contains a
// refused shape.
//
// This is the difference between "there is evidence" and "the model
// works", and RequestShapeGaps only ever asked the first question. The
// record's own outcome field was never read: a variant could be measured
// rejecting the very shape Claude Code sends and still report no gap.
//
// That is not hypothetical. qwen3.8-27b is offered today, and it is the
// model this whole table exists for — it shipped with a passing
// agent-harness verdict and then failed every real Claude Code turn on a
// 24 GB host (waired-agent#1035). Its matrix on ollama 0.32.13 refuses
// three of the six rows. Importing that matrix under a
// presence-only check would have filed the defect and shipped it.
//
// There is deliberately NO exemption map here. A model that cannot
// render the shape a coding agent sends is a model this project cannot
// offer, and the catalog already has the vocabulary for that decision:
// move the engine pin, or stop offering the variant. An exemption map
// would be a third answer invented before any case needed one.
func (s RequestShapeSet) RejectedShapes(manifests []Manifest, unmeasurable map[string]string) []RequestShapeRejection {
	var out []RequestShapeRejection
	for _, m := range manifests {
		if _, ok := unmeasurable[m.ModelID]; ok {
			continue
		}
		for _, v := range m.Variants {
			if !variantServesOllama(v) {
				continue
			}
			rec, ok := s.Lookup(m.ModelID, v.VariantID)
			if !ok {
				// Absence is RequestShapeGaps' business, not this
				// one's. Reporting it here too would make one missing
				// record read as two independent findings.
				continue
			}
			for name, outcome := range rec.Shapes {
				if outcome.Outcome != ShapeRejected {
					continue
				}
				out = append(out, RequestShapeRejection{
					ModelID: m.ModelID, VariantID: v.VariantID,
					Shape: name, Status: outcome.Status, Marker: outcome.Marker,
					EngineVersion: rec.EngineVersion,
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ModelID != out[j].ModelID {
			return out[i].ModelID < out[j].ModelID
		}
		if out[i].VariantID != out[j].VariantID {
			return out[i].VariantID < out[j].VariantID
		}
		return out[i].Shape < out[j].Shape
	})
	return out
}

// StaleEngineVersions reports records measured on an engine build other
// than the current pin.
//
// Deliberately NOT a gap. A pin bump does not re-open the question for a
// model already measured: no engine bump on record has broken a shape a
// model previously accepted, and re-measuring the catalog on every bump
// would put a GPU run in front of a one-line version change. What it
// does do is refuse to be silent — the count is printed, so a pin bump
// is a decision somebody takes with the drift in front of them.
func (s RequestShapeSet) StaleEngineVersions(pinned string) []RequestShapeGap {
	var out []RequestShapeGap
	for modelID, m := range s.Models {
		for variantID, rec := range m.Variants {
			if rec.EngineVersion != pinned {
				out = append(out, RequestShapeGap{modelID, variantID,
					fmt.Sprintf("measured on engine %s, current pin is %s", rec.EngineVersion, pinned)})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ModelID != out[j].ModelID {
			return out[i].ModelID < out[j].ModelID
		}
		return out[i].VariantID < out[j].VariantID
	})
	return out
}
