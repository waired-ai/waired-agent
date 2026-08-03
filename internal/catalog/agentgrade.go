package catalog

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
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

// RetireFailureRate is the per-call failure rate a variant must be shown
// to EXCEED before it lands on the retirement worklist.
//
// Half. Deleting a catalog entry is a claim that the model fails more
// often than it works, which is a sentence someone can defend in a
// review. Every lower line the measured catalog offers would sweep in a
// model there is a reason to keep: qwen2.5-coder-3b defines the install
// quality floor and sits at 23%, granite4-350m is the CI fixture at 14%,
// and qwen3.5-9b's 5% is the residue of an upstream engine parser bug
// (ollama/ollama#16383) that #458 taught the gateway to retry, not the
// weights.
//
// Those three rates are the whole justification for the line, so
// TestRatesCitedByRetireFailureRate reads them back off the shipped
// store. They have gone stale once already: #467 re-measured the catalog
// and left this paragraph quoting the previous sweep (3b at 17%,
// granite4-350m at 11%, qwen3.5-9b at 30%).
const RetireFailureRate = 0.5

// Failures reports every ollama-servable variant whose measured failure
// rate is high enough to justify deleting it from the catalog.
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
// The line is a CONFIDENCE BOUND on the rate rather than the rate
// itself: a case is retirable when failureRateLowerBound exceeds
// RetireFailureRate. That distinction is a correction this line has
// already needed once. Its first form was "failed on EVERY trial",
// which reads like a rate and is not one — 2 of 2 and 24 of 24 are both
// 100%, and only one of them is evidence — and it drew the line between
// 24 of 24 and 23 of 24, where nothing separates. The bound is stricter
// than the old rule at two trials, identical from three (where
// DefaultTrials sits), and catches the near-total failure the old rule
// excused for one lucky draw.
//
// The measured store puts exactly ONE variant above the line:
// qwen2.5-coder-0.5b, at a bound of 90%, against a next-worst of
// qwen2.5-coder-3b at 23%. Nearly four times the margin the line needs,
// and the widest gap in the file. That entry is now withheld (#475), so
// on the OFFERED catalog this returns nothing and `catalog-tool
// agentgrade --require-pass` gates CI; the record itself still says 90%,
// and the report's withheld section keeps saying so until #200's
// retirement machinery can delete it.
//
// 0.5b is a different defect and no parser reaches it: its "tool calls"
// name a daemon and a file path rather than any offered tool, so the
// gateway correctly leaves them as text. #409 taught the gateway to
// recover a call the engine had left in the assistant text, and three of
// the four qwen2.5-coder sizes came back — 7b went from failing every
// tool call to 1 of 24 on each of the two tool-requiring cases. A
// worklist keyed on the stored verdict rather than on this threshold
// would have proposed deleting all four.
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
			if !ok {
				continue
			}
			worst, counted := rec.WorstCase()
			if !counted {
				// No per-trial counts to reason from. The ratio is
				// unknown, not clean, so the stored verdict still
				// stands on its own.
				if rec.Verdict == AgentGradeFail {
					out = append(out, AgentGradeGap{m.ModelID, v.VariantID,
						legacyFailureReason(rec)})
				}
				continue
			}
			if worst.LowerBound <= RetireFailureRate {
				continue
			}
			out = append(out, AgentGradeGap{m.ModelID, v.VariantID,
				fmt.Sprintf("%s — %s failed %d of %d, so its true rate is at least %.0f%% (95%% confidence)",
					failedCaseSummary(rec.Cases), worst.Case, worst.Failed, worst.Trials,
					worst.LowerBound*100)})
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

func legacyFailureReason(rec VariantAgentGrade) string {
	const tail = "recorded as failing before per-trial counts existed; the ratio is unknown"
	if len(rec.Cases) == 0 {
		return tail
	}
	return failedCaseSummary(rec.Cases) + " — " + tail
}

// CaseFailureRate is one probe case's measured failure rate together
// with the sample it was measured from.
type CaseFailureRate struct {
	Case           string
	Failed, Trials int

	// LowerBound is the one-sided 95% Wilson lower bound on the true
	// per-call failure rate behind Failed/Trials.
	LowerBound float64
}

// WorstCase returns the probe case whose failure rate has the highest
// lower bound. ok is false for a record predating per-trial counts,
// where no rate can be derived at all.
//
// Per case, never pooled across cases: the three differ in difficulty by
// design, and averaging them hides the finding. qwen2.5-coder-0.5b is
// 48 of 72 pooled but 24 of 24 on both tool-requiring cases — "cannot
// call a tool at all", which the pooled number reads as two thirds.
func (r VariantAgentGrade) WorstCase() (worst CaseFailureRate, ok bool) {
	names := make([]string, 0, len(r.Cases))
	for n := range r.Cases {
		names = append(names, n)
	}
	sort.Strings(names) // ties resolve the same way on every run
	for _, n := range names {
		o := r.Cases[n]
		if o.Trials <= 0 {
			continue
		}
		c := CaseFailureRate{
			Case: n, Failed: o.Failed, Trials: o.Trials,
			LowerBound: failureRateLowerBound(o.Failed, o.Trials),
		}
		if !ok || c.LowerBound > worst.LowerBound {
			worst = c
		}
		ok = true
	}
	return worst, ok
}

// failureRateLowerBound returns the one-sided 95% Wilson lower bound on
// the true per-call failure rate behind failed-out-of-trials.
//
// A bound rather than the ratio, because the ratio alone does not say
// how hard anyone looked. The finding this whole store exists to record
// is that a verdict taken at three trials means "no draw went wrong",
// not "this model is clean" — and a threshold written against
// failed/trials repeats that mistake one level up. Wilson puts the
// sample size inside the rule instead of in a comment beside it, which
// is why raising WAIRED_AGENTGRADE_TRIALS can only ever move an entry
// ONTO the worklist, never off it by measuring less.
func failureRateLowerBound(failed, trials int) float64 {
	if trials <= 0 || failed <= 0 {
		return 0
	}
	if failed > trials {
		failed = trials
	}
	// z for a one-sided 95% bound. Two-sided 95% (1.96) would be the
	// reflex and is the wrong test: nobody retires a model for failing
	// too little.
	const z = 1.645
	n, k := float64(trials), float64(failed)
	den := n + z*z
	centre := (k + z*z/2) / den
	half := z / den * math.Sqrt(k*(n-k)/n+z*z/4)
	return math.Max(0, centre-half)
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
