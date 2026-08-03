package catalog

import (
	"encoding/json"
	"math"
	"slices"
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
	bundled, err := BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatalf("BundledManifestsIncludingInternal: %v", err)
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
//
// SHIPPED, not offered — hence the complete accessor. A withheld model
// is shipped, and measuring one is how it earned the job: the CI
// fixture was picked precisely because its probe run came back clean.
// Checking against the offered set would reject that verdict as
// spurious, which is backwards.
func TestAgentGradeKeysExistInCatalog(t *testing.T) {
	set, err := AgentGrades()
	if err != nil {
		t.Fatalf("AgentGrades: %v", err)
	}
	bundled, err := BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatalf("BundledManifestsIncludingInternal: %v", err)
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

// The retirement worklist must separate a model that fails EVERY trial
// from one that blipped once. Measured minutes apart on the same
// catalog, the single-trial failures moved between models — retiring on
// those would delete a different set every sweep.
func TestFailures_onlyDeterministicFailuresAreRetirable(t *testing.T) {
	manifests := []Manifest{{
		ModelID:  "always",
		Variants: []Variant{{VariantID: "q4", RuntimeSupport: []string{RuntimeOllama}}},
	}, {
		ModelID:  "sometimes",
		Variants: []Variant{{VariantID: "q4", RuntimeSupport: []string{RuntimeOllama}}},
	}, {
		ModelID:  "legacy-no-counts",
		Variants: []Variant{{VariantID: "q4", RuntimeSupport: []string{RuntimeOllama}}},
	}}
	set := AgentGradeSet{Models: map[string]ModelAgentGrade{
		"always": {Variants: map[string]VariantAgentGrade{"q4": {
			Verdict: AgentGradeFail,
			Cases: map[string]CaseOutcome{
				"greeting":  {Verdict: "pass", Trials: 3},
				"read-file": {Verdict: "fail_unstructured_tool_call", Trials: 3, Failed: 3},
			},
		}}},
		"sometimes": {Variants: map[string]VariantAgentGrade{"q4": {
			Verdict: AgentGradeFail,
			Cases: map[string]CaseOutcome{
				"greeting":  {Verdict: "pass", Trials: 3},
				"read-file": {Verdict: "fail_no_tool_call", Trials: 3, Failed: 1},
			},
		}}},
		// No per-trial counts: predates them. Must NOT be silently
		// excused just because the ratio is unknown.
		"legacy-no-counts": {Variants: map[string]VariantAgentGrade{"q4": {
			Verdict: AgentGradeFail,
			Cases:   map[string]CaseOutcome{"read-file": {Verdict: "fail_no_tool_call"}},
		}}},
	}}

	got := set.Failures(manifests)
	names := map[string]bool{}
	for _, g := range got {
		names[g.ModelID] = true
	}
	if !names["always"] {
		t.Error("a model that fails every trial must be on the retirement worklist")
	}
	if names["sometimes"] {
		t.Error("a model that failed 1 of 3 trials must NOT be retired — that failure moves between runs")
	}
	if !names["legacy-no-counts"] {
		t.Error("a record with no per-trial counts must not be excused; its ratio is unknown, not clean")
	}
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
			Cases: map[string]CaseOutcome{
				"greeting":  {Verdict: "pass", Trials: 3, Failed: 0},
				"read-file": {Verdict: "fail_unstructured_tool_call", Trials: 3, Failed: 3},
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
	// The ratio has to survive into the worklist: "3/3" and "1/3" are
	// different retirement decisions.
	if !strings.Contains(got[0].Reason, "[3/3]") {
		t.Errorf("reason should carry the failed/total ratio, got %q", got[0].Reason)
	}
	if strings.Contains(got[0].Reason, "greeting") {
		t.Errorf("reason should not list passing cases, got %q", got[0].Reason)
	}
}

// The bound is the rule, so it is pinned against hand-computed values
// rather than against itself. Wilson score interval, one-sided 95%
// (z = 1.645).
func TestFailureRateLowerBound(t *testing.T) {
	cases := []struct {
		failed, trials int
		want           float64
	}{
		{0, 24, 0},
		{1, 24, 0.009350},
		{2, 24, 0.027968},
		{3, 24, 0.051076},
		{7, 24, 0.166559},
		{11, 24, 0.303894},
		{23, 24, 0.833134},
		{24, 24, 0.898674},
		{1, 3, 0.078257},
		{3, 3, 0.525760},
		{2, 2, 0.424987},
		{12, 12, 0.815992},
		// Degenerate inputs answer 0 rather than NaN: a bound derived
		// from nothing must not read as evidence of everything.
		{0, 0, 0},
		{5, 0, 0},
		{-1, 24, 0},
		// More failures than trials cannot happen, but a rule that
		// panics on it is worse than one that clamps.
		{5, 3, 0.525760},
	}
	for _, c := range cases {
		if got := failureRateLowerBound(c.failed, c.trials); math.Abs(got-c.want) > 1e-6 {
			t.Errorf("failureRateLowerBound(%d, %d) = %.6f, want %.6f", c.failed, c.trials, got, c.want)
		}
	}
}

// The line replaced "failed on EVERY trial". At the trial count every
// stored verdict was taken with, it must answer the same — otherwise
// this is a data change wearing a rule change's clothes.
func TestRetirementLineReproducesFailsEveryTrial(t *testing.T) {
	for n := 3; n <= 96; n++ {
		if got := failureRateLowerBound(n, n); got <= RetireFailureRate {
			t.Fatalf("%d of %d must stay retirable: bound %.4f is not above %.2f",
				n, n, got, RetireFailureRate)
		}
	}
	// Two trials is where it is STRICTER, and deliberately: DefaultTrials
	// is 3 because a smaller sample records a coin flip as a verdict, and
	// the importer already refuses a report below 2.
	if got := failureRateLowerBound(2, 2); got > RetireFailureRate {
		t.Errorf("2 of 2 is not enough evidence to delete a catalog entry: bound %.4f", got)
	}
}

// And where it is stricter than the old rule: one lucky draw used to
// take a hopeless model off the worklist entirely.
func TestRetirementLineCatchesNearTotalFailure(t *testing.T) {
	if got := failureRateLowerBound(23, 24); got <= RetireFailureRate {
		t.Errorf("23 of 24 must be retirable; the old rule excused it for one draw (bound %.4f)", got)
	}
	// The worst entry that is NOT retirable today, so the margin below
	// the line is pinned too. qwen3.5-9b's failures are an upstream
	// engine parser bug (ollama/ollama#16383), not the weights.
	if got := failureRateLowerBound(11, 24); got > RetireFailureRate {
		t.Errorf("11 of 24 must NOT be retirable (bound %.4f)", got)
	}
}

// The counts are the claim. A record whose stored verdict disagrees with
// its own numbers must be judged on the numbers: the verdict is the
// worst outcome across trials and therefore a function of the trial
// count, which is the whole reason this rule reads the ratio.
func TestFailuresReadsTheCountsNotTheVerdict(t *testing.T) {
	manifests := []Manifest{{
		ModelID:  "mislabelled",
		Variants: []Variant{{VariantID: "q4", RuntimeSupport: []string{RuntimeOllama}}},
	}}
	set := AgentGradeSet{Models: map[string]ModelAgentGrade{
		"mislabelled": {Variants: map[string]VariantAgentGrade{"q4": {
			Verdict: AgentGradePass,
			Cases: map[string]CaseOutcome{
				"greeting":  {Verdict: "pass", Trials: 24},
				"read-file": {Verdict: "fail_no_tool_call", Trials: 24, Failed: 24},
			},
		}}},
	}}
	if got := set.Failures(manifests); len(got) != 1 {
		t.Fatalf("a variant failing 24 of 24 belongs on the worklist whatever its verdict says, got %+v", got)
	}
}

// A golden on the shipped file: the next person to move the threshold
// finds out here which models they just proposed deleting.
//
// The offered list is empty, and #475 made that load-bearing —
// `catalog-tool agentgrade --require-pass` runs in ci.yml and reads
// exactly this. Before #475 the one entry here was
// qwen2.5-coder-0.5b-instruct/q4-gguf, which failed 24 of 24 on both
// tool-requiring cases; it is withheld rather than deleted, because
// deleting it answers a pinned user with ErrModelNotFound until #200's
// retired->successor map exists.
//
// Empty is therefore two different facts — "nothing is above the line"
// and "everything above the line is withheld" — and only the second is
// true today. The withheld half below keeps the distinction visible, so
// an entry cannot be quietly parked in internal_only and forgotten.
func TestFailuresOnTheShippedCatalog(t *testing.T) {
	set, err := AgentGrades()
	if err != nil {
		t.Fatalf("AgentGrades: %v", err)
	}
	bundled, err := BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	if got := worklistNames(set.Failures(bundled)); len(got) != 0 {
		t.Errorf("offered retirement worklist = %v, want none — ci.yml's "+
			"`agentgrade --require-pass` fails while this is non-empty", got)
	}

	all, err := BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatalf("BundledManifestsIncludingInternal: %v", err)
	}
	var withheld []Manifest
	for _, m := range all {
		if m.InternalOnly != "" {
			withheld = append(withheld, m)
		}
	}
	got := set.Failures(withheld)
	want := []string{"qwen2.5-coder-0.5b-instruct/q4-gguf"}
	if names := worklistNames(got); !slices.Equal(names, want) {
		t.Errorf("withheld-and-above-the-line = %v, want %v", names, want)
	}
	if len(got) == 1 {
		if !strings.Contains(got[0].Reason, "95% confidence") {
			t.Errorf("the worklist must show the evidence behind the claim, got %q", got[0].Reason)
		}
		m, ok := LookupByAlias(got[0].ModelID, withheld)
		if !ok {
			t.Fatalf("%s is not in the withheld set", got[0].ModelID)
		}
		// The reason string is the only record of WHY the entry is out of
		// the offered catalog, and of the fact that this is a stop on the
		// way to deletion rather than the destination.
		for _, want := range []string{"RETIREMENT", "#200"} {
			if !strings.Contains(m.InternalOnly, want) {
				t.Errorf("internal_only reason for %s does not mention %q: %q",
					got[0].ModelID, want, m.InternalOnly)
			}
		}
	}
}

func worklistNames(gaps []AgentGradeGap) []string {
	var names []string
	for _, g := range gaps {
		names = append(names, g.ModelID+"/"+g.VariantID)
	}
	return names
}

// The three rates RetireFailureRate's doc comment cites as the reason the
// line sits at half, read back off the shipped store.
//
// A record of today's measurement, not a product contract: if a sweep
// moves one of these, the number here moves with it. The point is that it
// cannot move SILENTLY. #467 re-measured the whole catalog and left that
// paragraph quoting the previous sweep — 3b at 17%, granite4-350m at 11%,
// qwen3.5-9b at 30% — so the justification for the threshold described a
// catalog that no longer existed. This is the cheapest thing that turns
// that into a failing test instead of a stale sentence.
func TestRatesCitedByRetireFailureRate(t *testing.T) {
	set, err := AgentGrades()
	if err != nil {
		t.Fatalf("AgentGrades: %v", err)
	}
	cited := []struct {
		model, variant string
		wantPct        int
	}{
		{"qwen2.5-coder-3b-instruct", "q4-gguf", 23}, // defines the install quality floor
		{"granite4-350m", "bf16-gguf", 14},           // the CI fixture
		{"qwen3.5-9b", "q4-gguf", 5},                 // ollama/ollama#16383, retried since #458
	}
	for _, c := range cited {
		rec, ok := set.Lookup(c.model, c.variant)
		if !ok {
			t.Errorf("%s/%s: no verdict, but RetireFailureRate's doc cites its rate",
				c.model, c.variant)
			continue
		}
		worst, counted := rec.WorstCase()
		if !counted {
			t.Errorf("%s/%s: no per-trial counts, but RetireFailureRate's doc cites its rate",
				c.model, c.variant)
			continue
		}
		if got := int(math.Round(worst.LowerBound * 100)); got != c.wantPct {
			t.Errorf("%s/%s: worst case %s at %d of %d is %d%%, but RetireFailureRate's "+
				"doc comment says %d%% — update the comment (and this table) to match the data",
				c.model, c.variant, worst.Case, worst.Failed, worst.Trials, got, c.wantPct)
		}
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
