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

	// Cases maps the probe case name to its outcome, so a reviewer can
	// see WHICH way the model failed, and HOW OFTEN, without re-running
	// it.
	Cases map[string]CaseOutcome `json:"cases,omitempty"`

	// Trials is how many times the case set ran to produce this verdict.
	// Recorded because a single-shot result is not a measurement at the
	// boundary: the first catalog sweep graded three models as failing
	// and an immediate re-run graded the same three as passing.
	Trials int `json:"trials,omitempty"`

	// Flaky names the cases that did not agree across trials. A model
	// listed here is unstable rather than simply bad, which is a
	// different decision — and one that a bare pass/fail hides.
	Flaky []string `json:"flaky,omitempty"`

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

	// AgentRevision is the waired-agent commit the probe ran from, and
	// Transport is the HTTP shape it drove — "unary", "stream", or
	// "stream+unary" when the verdict pools runs over both. Derived from
	// the reports actually imported, never typed: a value an operator
	// supplied could claim a path nobody drove.
	//
	// They record the HARNESS generation, which FixtureRevision does not
	// cover: it hashes the fixture, and the fixture is only half of what
	// a verdict depends on. #409 taught this the expensive way — the
	// gateway started recovering tool calls the engine had left in the
	// assistant text, four models went from failing every call to
	// passing, and every stored verdict still looked current because the
	// fixture had not moved. Deliberately NOT a staleness gate: unlike a
	// fixture change, an unrelated commit does not invalidate a
	// measurement, and treating it as one would put the whole file in
	// CoverageGaps on every merge.
	AgentRevision string `json:"agent_revision,omitempty"`
	Transport     string `json:"transport,omitempty"`

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

// CaseOutcome is one probe case's stored result.
//
// The ratio matters as much as the verdict. "Emitted the wrong syntax
// on all three trials" and "hit one engine parse error in three" are
// different models with different answers, and storing only the worst
// verdict collapses them — which is exactly what made the first
// single-shot sweep unusable. Keeping the counts means the grading
// POLICY can be changed without re-measuring the catalog.
type CaseOutcome struct {
	// Verdict is the worst outcome seen across trials.
	Verdict string `json:"verdict"`
	// Trials and Failed are how often the case ran and failed.
	Trials int `json:"trials,omitempty"`
	Failed int `json:"failed,omitempty"`
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

// Failures reports every ollama-servable variant that fails a probe
// case on EVERY trial.
//
// It is the retirement worklist, and the threshold is the whole design.
// A recorded Verdict of "fail" means "something went wrong at least
// once", which is the right signal for a user asking about their own
// model — but it is the wrong criterion for deleting a catalog entry,
// because the occasional single-trial failure is stochastic. Measured
// on the same catalog, same fixture, minutes apart: qwen3.5-9b failed
// one trial in one run and none in the next, and qwen3.5-35b-a3b did
// the reverse. Retiring on "failed once" would delete a different set
// of models every time the sweep runs.
//
// Failing every trial is a different claim, and the data separates
// cleanly at that line. All four qwen2.5-coder entries failed EVERY
// tool-requiring call on EVERY trial — the rc7 defect, deterministic —
// while nothing else in the catalog exceeded one failure in three.
//
// Verdicts recorded before per-trial counts existed have Trials == 0;
// those fall back to the stored verdict so an old record is not
// silently treated as clean.
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
			if !failsEveryTrial(rec) {
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

// failsEveryTrial reports whether some case failed on all of its
// trials. A record with no per-trial counts (pre-dating them) answers
// true from its verdict alone rather than being quietly excused.
func failsEveryTrial(rec VariantAgentGrade) bool {
	if len(rec.Cases) == 0 {
		return true
	}
	counted := false
	for _, o := range rec.Cases {
		if o.Trials <= 0 {
			continue
		}
		counted = true
		if o.Failed >= o.Trials {
			return true
		}
	}
	return !counted
}

func failedCaseSummary(cases map[string]CaseOutcome) string {
	names := make([]string, 0, len(cases))
	for name, o := range cases {
		if o.Verdict == "pass" || o.Verdict == "" {
			continue
		}
		if o.Trials > 0 {
			names = append(names, fmt.Sprintf("%s=%s [%d/%d]", name, o.Verdict, o.Failed, o.Trials))
			continue
		}
		names = append(names, name+"="+o.Verdict)
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
