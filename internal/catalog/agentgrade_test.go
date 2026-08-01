package catalog

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAgentGradesDecodes(t *testing.T) {
	set, err := AgentGrades()
	if err != nil {
		t.Fatalf("AgentGrades: %v", err)
	}
	if set.Schema != 1 {
		t.Errorf("schema = %d, want 1", set.Schema)
	}
	if strings.TrimSpace(set.Notes) == "" {
		t.Error("notes must explain what the store is; an undocumented verdict file rots")
	}
}

// A verdict has to carry enough provenance to be re-judged later: which
// engine produced it, at what fixture weight, and when. A bare
// pass/fail is the shape the `capabilities` field already has, and its
// uselessness is what this whole store exists to fix.
func TestAgentGradeEntriesCarryProvenance(t *testing.T) {
	set, err := AgentGrades()
	if err != nil {
		t.Fatalf("AgentGrades: %v", err)
	}
	for modelID, m := range set.Models {
		for variantID, rec := range m.Variants {
			where := modelID + "/" + variantID
			if rec.Verdict != AgentGradePass && rec.Verdict != AgentGradeFail {
				t.Errorf("%s: verdict = %q, want %q or %q", where, rec.Verdict, AgentGradePass, AgentGradeFail)
			}
			if rec.Engine == "" {
				t.Errorf("%s: engine is required — compliance is a property of (model, quantisation, engine)", where)
			}
			if rec.FixtureRevision == "" {
				t.Errorf("%s: fixture_revision is required — without it a verdict cannot be told from a stale one", where)
			}
			if rec.Retrieved == "" {
				t.Errorf("%s: retrieved is required — a verdict with no date cannot be aged out", where)
			}
			if len(rec.Retrieved) != len("2006-01-02") {
				t.Errorf("%s: retrieved = %q, want YYYY-MM-DD", where, rec.Retrieved)
			}
		}
	}
}

// Every unmeasurable declaration needs a reason. The point of the map is
// to force the decision — an entry with an empty reason is the silence
// it exists to prevent.
func TestUnmeasurableEntriesCarryReasons(t *testing.T) {
	set, err := AgentGrades()
	if err != nil {
		t.Fatalf("AgentGrades: %v", err)
	}
	bundled, err := BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	known := map[string]bool{}
	for _, m := range bundled {
		known[m.ModelID] = true
	}
	for modelID, reason := range set.Unmeasurable {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("%s: unmeasurable with no reason", modelID)
		}
		if !known[modelID] {
			t.Errorf("%s: declared unmeasurable but is not in the bundled catalog — "+
				"a stale exemption silently excuses nothing", modelID)
		}
	}
}

// The store must not name a model or variant the catalog does not ship:
// a verdict filed against a nonexistent key looks like coverage and is
// not.
func TestAgentGradeKeysExistInCatalog(t *testing.T) {
	set, err := AgentGrades()
	if err != nil {
		t.Fatalf("AgentGrades: %v", err)
	}
	bundled, err := BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	variants := map[string]bool{}
	for _, m := range bundled {
		for _, v := range m.Variants {
			variants[m.ModelID+"/"+v.VariantID] = true
		}
	}
	for modelID, m := range set.Models {
		for variantID := range m.Variants {
			if !variants[modelID+"/"+variantID] {
				t.Errorf("verdict recorded for %s/%s, which the bundled catalog does not ship",
					modelID, variantID)
			}
		}
	}
}

func TestCoverageGaps(t *testing.T) {
	manifests := []Manifest{{
		ModelID: "m1",
		Variants: []Variant{
			{VariantID: "ollama-a", RuntimeSupport: []string{RuntimeOllama}},
			{VariantID: "vllm-only", RuntimeSupport: []string{RuntimeVLLM}},
		},
	}, {
		ModelID:  "m2",
		Variants: []Variant{{VariantID: "ollama-b", RuntimeSupport: []string{RuntimeOllama}}},
	}}

	t.Run("missing verdict is a gap", func(t *testing.T) {
		set := AgentGradeSet{Models: map[string]ModelAgentGrade{}}
		gaps := set.CoverageGaps(manifests, "rev1")
		if len(gaps) != 2 {
			t.Fatalf("got %d gaps, want 2: %+v", len(gaps), gaps)
		}
	})

	t.Run("vLLM-only variants are not probed but do not create a gap when a sibling is covered", func(t *testing.T) {
		set := AgentGradeSet{Models: map[string]ModelAgentGrade{
			"m1": {Variants: map[string]VariantAgentGrade{
				"ollama-a": {Verdict: AgentGradePass, FixtureRevision: "rev1"},
			}},
			"m2": {Variants: map[string]VariantAgentGrade{
				"ollama-b": {Verdict: AgentGradePass, FixtureRevision: "rev1"},
			}},
		}}
		if gaps := set.CoverageGaps(manifests, "rev1"); len(gaps) != 0 {
			t.Errorf("want no gaps, got %+v", gaps)
		}
	})

	t.Run("a stale fixture revision is a gap, not a pass", func(t *testing.T) {
		set := AgentGradeSet{Models: map[string]ModelAgentGrade{
			"m1": {Variants: map[string]VariantAgentGrade{
				"ollama-a": {Verdict: AgentGradePass, FixtureRevision: "OLD"},
			}},
			"m2": {Variants: map[string]VariantAgentGrade{
				"ollama-b": {Verdict: AgentGradePass, FixtureRevision: "rev1"},
			}},
		}}
		gaps := set.CoverageGaps(manifests, "rev1")
		if len(gaps) != 1 || gaps[0].ModelID != "m1" {
			t.Fatalf("want one gap on m1, got %+v", gaps)
		}
		if !strings.Contains(gaps[0].Reason, "re-measure") {
			t.Errorf("reason should tell the reader what to do, got %q", gaps[0].Reason)
		}
	})

	t.Run("unmeasurable silences the model", func(t *testing.T) {
		set := AgentGradeSet{
			Models:       map[string]ModelAgentGrade{},
			Unmeasurable: map[string]string{"m1": "too large for any runner", "m2": "vLLM-only, no runner"},
		}
		if gaps := set.CoverageGaps(manifests, "rev1"); len(gaps) != 0 {
			t.Errorf("want no gaps, got %+v", gaps)
		}
	})

	// A manifest with no ollama variant must be reported, not skipped:
	// skipping is how a vLLM-only entry passes a coverage check that
	// looks complete.
	t.Run("a model with no ollama variant is a model-level gap", func(t *testing.T) {
		vllmOnly := []Manifest{{
			ModelID:  "big",
			Variants: []Variant{{VariantID: "fp8", RuntimeSupport: []string{RuntimeVLLM}}},
		}}
		set := AgentGradeSet{Models: map[string]ModelAgentGrade{}}
		gaps := set.CoverageGaps(vllmOnly, "rev1")
		if len(gaps) != 1 {
			t.Fatalf("want one gap, got %+v", gaps)
		}
		if gaps[0].VariantID != "" || !strings.Contains(gaps[0].Reason, "unmeasurable") {
			t.Errorf("gap should be model-level and name the escape hatch, got %+v", gaps[0])
		}
	})
}

func TestFailures(t *testing.T) {
	manifests := []Manifest{{
		ModelID:  "weak",
		Variants: []Variant{{VariantID: "q4", RuntimeSupport: []string{RuntimeOllama}}},
	}, {
		ModelID:  "good",
		Variants: []Variant{{VariantID: "q4", RuntimeSupport: []string{RuntimeOllama}}},
	}}
	set := AgentGradeSet{Models: map[string]ModelAgentGrade{
		"weak": {Variants: map[string]VariantAgentGrade{"q4": {
			Verdict: AgentGradeFail,
			Cases: map[string]string{
				"greeting":  "pass",
				"read-file": "fail_unstructured_tool_call",
			},
		}}},
		"good": {Variants: map[string]VariantAgentGrade{"q4": {Verdict: AgentGradePass}}},
	}}

	got := set.Failures(manifests)
	if len(got) != 1 || got[0].ModelID != "weak" {
		t.Fatalf("want one failure on weak, got %+v", got)
	}
	// The summary must name the failing case, not just say "failed":
	// which way a model fails is what decides whether it is worth
	// keeping for non-agent use.
	if !strings.Contains(got[0].Reason, "read-file=fail_unstructured_tool_call") {
		t.Errorf("reason should name the failing case, got %q", got[0].Reason)
	}
	if strings.Contains(got[0].Reason, "greeting") {
		t.Errorf("reason should not list passing cases, got %q", got[0].Reason)
	}
}

// The store is a JSON file people will read in review. Keep it parseable
// as the exact struct, so a typo'd key is caught here and not by a
// silently-zero field months later.
func TestAgentGradeJSONHasNoUnknownFields(t *testing.T) {
	dec := json.NewDecoder(strings.NewReader(string(agentGradeJSON)))
	dec.DisallowUnknownFields()
	var s AgentGradeSet
	if err := dec.Decode(&s); err != nil {
		t.Fatalf("agentgrade.json has a field the struct does not define: %v", err)
	}
}
