package catalog

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// agentGradeJSON is the tracked agent-harness verdict store, embedded so
// validation and tier tooling stay hermetic (no network at test/build
// time), exactly like benchmarks.json next to it.
//
//go:embed agentgrade.json
var agentGradeJSON []byte

// AgentGradeSet is the decoded agentgrade.json: per-variant verdicts on
// whether a model can drive a coding agent's tool-call format (#322),
// with the provenance behind each one.
//
// This lives in internal/catalog rather than in proto/catalog on
// purpose. The verdicts do not travel on any wire and no consumer reads
// them at runtime: their job is to decide what belongs in the bundled
// catalog at all, which is a maintenance-time question. Putting them in
// the frozen proto module would add a field every consumer must ignore,
// and — once the catalog holds only agent-grade models — that field
// would be true for every entry, which is precisely how the existing
// `capabilities` string became decorative.
type AgentGradeSet struct {
	Schema int    `json:"schema"`
	Notes  string `json:"notes,omitempty"`

	// Models maps model_id to its per-variant verdicts.
	Models map[string]ModelAgentGrade `json:"models"`

	// Unmeasurable declares models that no available runner can host,
	// so a missing verdict reads as a stated gap rather than an
	// oversight. Same discipline as the testnet package classification:
	// the guard forces the decision instead of letting silence pass.
	Unmeasurable map[string]string `json:"unmeasurable,omitempty"`
}

// ModelAgentGrade holds one model's verdicts, keyed by variant_id.
//
// Per-variant, not per-model: quantisation changes format compliance,
// and a family whose fp16 build complies while its q4 build does not is
// exactly the case a model-level verdict would get wrong in whichever
// direction it was recorded.
type ModelAgentGrade struct {
	Variants map[string]VariantAgentGrade `json:"variants"`
}

// VariantAgentGrade is one measured verdict plus everything needed to
// judge whether it still means anything.
type VariantAgentGrade struct {
	// Verdict is AgentGradePass or AgentGradeFail. There is no
	// "unknown" value: a run that could not obtain an answer is not
	// recorded at all, because a stored unknown is indistinguishable
	// from a stored failure to everyone downstream
	// (waired-ai/waired-agent#203).
	Verdict string `json:"verdict"`

	// Cases maps the probe case name to its verdict, so a reviewer can
	// see WHICH way the model failed without re-running it.
	Cases map[string]string `json:"cases,omitempty"`

	// Engine and EngineVersion identify the serving stack. Compliance
	// depends on the engine's template rendering and tool-call parser,
	// so a verdict is about (model, quantisation, engine) and not about
	// the weights alone.
	Engine        string `json:"engine"`
	EngineVersion string `json:"engine_version,omitempty"`

	// EngineTag is the engine-native tag actually served.
	//
	// Named EngineTag rather than Tag on purpose: the protoconsumer
	// guard matches proto-field writes BY NAME across cmd/ and
	// internal/, so a local field called Tag reads to it as a write to
	// catalog.VariantSource.Tag and silently invalidates that field's
	// receive-only exemption.
	EngineTag string `json:"engine_tag,omitempty"`

	// FixtureRevision is agentgrade.FixtureRevision() at measurement
	// time. Verdicts from different revisions were measured against
	// different weights and are not comparable.
	FixtureRevision string `json:"fixture_revision"`

	// Host is a hardware CLASS, never an identifier — this repository is
	// public. "nvidia-24gb-discrete", "apple-unified-64gb". It matters
	// because a model that spills most of its layers may answer
	// differently from the same model held resident.
	Host string `json:"host,omitempty"`

	// RunURL points at the CI run that produced the verdict, when it
	// came from one. Empty for a local measurement, which is honest
	// rather than missing: a local run is still a measurement, it just
	// has no artifact to link.
	RunURL string `json:"run_url,omitempty"`

	// Retrieved is the measurement date, YYYY-MM-DD.
	Retrieved string `json:"retrieved"`

	Notes string `json:"notes,omitempty"`
}

// Verdict values stored in agentgrade.json.
const (
	AgentGradePass = "pass"
	AgentGradeFail = "fail"
)

// AgentGrades decodes the embedded agentgrade.json.
func AgentGrades() (AgentGradeSet, error) {
	var s AgentGradeSet
	if err := json.Unmarshal(agentGradeJSON, &s); err != nil {
		return AgentGradeSet{}, fmt.Errorf("catalog: parse agentgrade.json: %w", err)
	}
	return s, nil
}

// Lookup returns the verdict for one (model, variant), if recorded.
func (s AgentGradeSet) Lookup(modelID, variantID string) (VariantAgentGrade, bool) {
	m, ok := s.Models[modelID]
	if !ok {
		return VariantAgentGrade{}, false
	}
	v, ok := m.Variants[variantID]
	return v, ok
}

// AgentGradeGap is one manifest variant with no usable verdict, and why.
type AgentGradeGap struct {
	ModelID   string
	VariantID string
	Reason    string
}

// CoverageGaps reports every ollama-servable variant of the given
// manifests that lacks a usable verdict at fixtureRevision.
//
// Only ollama variants are probed: the probe drives a serving engine,
// and the vLLM path has runner requirements no single host satisfies. A
// manifest with no ollama variant at all is therefore reported as ONE
// model-level gap rather than skipped — skipping it is how a vLLM-only
// entry would slip through a coverage check that looks complete.
//
// A model listed in Unmeasurable is not a gap; it is a stated one. A
// verdict from a DIFFERENT fixture revision IS a gap: it was measured
// against a weight this fixture no longer applies, and treating it as
// current is how a stale pass survives a fixture change.
func (s AgentGradeSet) CoverageGaps(manifests []Manifest, fixtureRevision string) []AgentGradeGap {
	var out []AgentGradeGap
	for _, m := range manifests {
		if _, ok := s.Unmeasurable[m.ModelID]; ok {
			continue
		}
		checked := 0
		for _, v := range m.Variants {
			if !variantServesOllama(v) {
				continue
			}
			checked++
			rec, ok := s.Lookup(m.ModelID, v.VariantID)
			switch {
			case !ok:
				out = append(out, AgentGradeGap{m.ModelID, v.VariantID, "no verdict recorded"})
			case rec.Verdict != AgentGradePass && rec.Verdict != AgentGradeFail:
				out = append(out, AgentGradeGap{m.ModelID, v.VariantID,
					fmt.Sprintf("verdict %q is not %q or %q", rec.Verdict, AgentGradePass, AgentGradeFail)})
			case rec.FixtureRevision != fixtureRevision:
				out = append(out, AgentGradeGap{m.ModelID, v.VariantID,
					fmt.Sprintf("measured at fixture revision %s, current is %s — re-measure",
						rec.FixtureRevision, fixtureRevision)})
			}
		}
		if checked == 0 {
			out = append(out, AgentGradeGap{m.ModelID, "",
				"no ollama variant to probe — record a GPU-lane verdict or declare it in \"unmeasurable\""})
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

// Failures reports every ollama-servable variant recorded as failing.
//
// It is the retirement worklist: under the decided policy the bundled
// catalog holds only models that can drive a coding agent, so a failing
// entry is one that should not be in it.
func (s AgentGradeSet) Failures(manifests []Manifest) []AgentGradeGap {
	var out []AgentGradeGap
	for _, m := range manifests {
		for _, v := range m.Variants {
			if !variantServesOllama(v) {
				continue
			}
			rec, ok := s.Lookup(m.ModelID, v.VariantID)
			if !ok || rec.Verdict != AgentGradeFail {
				continue
			}
			reason := "recorded as failing"
			if len(rec.Cases) > 0 {
				reason += " (" + failedCaseSummary(rec.Cases) + ")"
			}
			out = append(out, AgentGradeGap{m.ModelID, v.VariantID, reason})
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

func failedCaseSummary(cases map[string]string) string {
	names := make([]string, 0, len(cases))
	for name, verdict := range cases {
		if verdict != "pass" && verdict != "" {
			names = append(names, name+"="+verdict)
		}
	}
	if len(names) == 0 {
		return "no failing case recorded"
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func variantServesOllama(v Variant) bool {
	return slices.Contains(v.RuntimeSupport, RuntimeOllama)
}
