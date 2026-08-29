package catalog

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
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
	seen := 0
	for modelID, m := range set.Models {
		for variantID, rec := range m.Variants {
			seen++
			where := modelID + "/" + variantID
			if rec.Verdict != AgentGradePass && rec.Verdict != AgentGradeFail {
				t.Errorf("%s: verdict = %q, want %q or %q", where, rec.Verdict, AgentGradePass, AgentGradeFail)
			}
			if rec.Engine == "" {
				t.Errorf("%s: engine is required — compliance is a property of (model, quantisation, engine)", where)
			}
			// The build, not just the engine name: ollama 0.32.13 refuses
			// a request shape 0.32.14 tolerates and 0.32.15 merges, so a
			// verdict that cannot name its build cannot be re-decided.
			// Derived by the importer since waired-agent#1117.
			if rec.EngineVersion == "" {
				t.Errorf("%s: engine_version is required — the same model and the same request "+
					"answer differently on different engine builds", where)
			}
			if rec.FixtureRevision == "" {
				t.Errorf("%s: fixture_revision is required — without it a verdict cannot be told from a stale one", where)
			}
			if rec.Retrieved == "" {
				t.Errorf("%s: retrieved is required — a verdict with no date cannot be aged out", where)
			}
			if !retrievedDate.MatchString(rec.Retrieved) {
				t.Errorf("%s: retrieved = %q, want YYYY-MM-DD", where, rec.Retrieved)
			}
			// A machine name in a public repository is the failure this
			// field's doc comment warns about, and prose was the only
			// thing enforcing it.
			if !ValidHostClass(rec.Host) {
				t.Errorf("%s: host = %q is not a declared hardware class (one of: %s)",
					where, rec.Host, HostClassList())
			}
		}
	}
	if seen == 0 {
		t.Fatal("no verdicts in the store — this guard is checking nothing")
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
		// SHIPPED OR RETIRED, for the same reason
		// TestAgentGradeKeysExistInCatalog takes retired keys: an
		// unmeasurable declaration is the record of WHY a model was never
		// measured, and a retirement whose reason says "unmeasurable on
		// every runner we have" needs that record to still exist. #522
		// retired three of them at once (glm-4.5-air-106b-a12b,
		// qwen3-coder-480b-a35b-instruct, qwen3-coder-next-80b-a3b-instruct).
		//
		// A typo is still caught: a name that is neither shipped nor in
		// the retirement table fails exactly as before.
		if _, gone := LookupRetirement(modelID); gone {
			continue
		}
		if !known[modelID] {
			t.Errorf("%s: declared unmeasurable but is neither in the bundled catalog nor "+
				"in the retirement table — a stale exemption silently excuses nothing", modelID)
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
//
// SHIPPED OR RETIRED (#200). A retired entry's record is not stale data
// left behind — it is the evidence the retirement was carried out on,
// and qwen2.5-coder-0.5b's 24-of-24 is the only place the 90% bound that
// justified deleting it is written down. Deleting the record with the
// manifest would leave the retirement's own reason string citing numbers
// nothing in the tree still holds. A typo is still caught: a name that is
// neither shipped nor in the retirement table fails exactly as before.
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
	retired := 0
	for modelID, m := range set.Models {
		if _, gone := LookupRetirement(modelID); gone {
			retired++
			continue
		}
		for variantID := range m.Variants {
			if !variants[modelID+"/"+variantID] {
				t.Errorf("verdict recorded for %s/%s, which the bundled catalog neither "+
					"ships nor records as retired", modelID, variantID)
			}
		}
	}
	// The exemption above is only worth its complexity while something
	// uses it; if the last retired record ever goes, take the branch out
	// rather than leaving a door nobody walks through.
	if retired == 0 && len(Retirements()) > 0 {
		t.Log("no retired model has a retained verdict; the retirement branch above is unused")
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
// Both lists are empty now, and each is empty for a different reason.
//
// The OFFERED list is load-bearing: `catalog-tool agentgrade
// --require-pass` runs in ci.yml and reads exactly this (#475). The
// WITHHELD list used to hold qwen2.5-coder-0.5b-instruct/q4-gguf, which
// failed 24 of 24 on both tool-requiring cases. #475 withheld it rather
// than deleting it, because deleting it answered a pinned user with
// ErrModelNotFound; #200 built the retired->successor map and deleted it,
// so it is now out of the catalog entirely.
//
// That means "withheld and above the line" is empty for the RIGHT reason
// — the entry left through the exit the withhold was holding open, not
// because somebody quietly relaxed the line. The successor half below is
// what says so, and it is the assertion that goes red if a future entry
// is parked in internal_only and forgotten instead.
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
	if names := worklistNames(set.Failures(withheld)); len(names) != 0 {
		t.Errorf("withheld-and-above-the-line = %v, want none: an entry above the "+
			"retirement line is either offered and failing (impossible, checked above) "+
			"or on its way out through catalog.Retirements()", names)
	}

	// The entry that WAS on this list left through the retirement map, and
	// its evidence has to survive the manifest that carried it. Everything
	// retired keeps a resolvable successor and a record of what it did.
	//
	// The reason requirement used to be the literals "95% confidence" and
	// "#200", which encoded the shape of the ONE retirement that existed:
	// a model deleted for a measured failure rate, decided in #200. #522
	// retired seven at once for a reason that is not a measurement — they
	// are not the generation the catalog carries (#518) — and three of
	// them are `unmeasurable`, so demanding a confidence bound would have
	// forced a number nobody has.
	//
	// What survives is the part that was actually load-bearing: a reason
	// must CITE WHERE IT WAS DECIDED, so a reader can find the argument
	// rather than take the sentence on trust. A measurement, where one
	// exists, is evidence inside that argument and not a substitute for
	// it — hence the second check, which holds every retirement that
	// claims a bound to stating it properly rather than waving at one.
	if len(Retirements()) == 0 {
		t.Fatal("nothing has ever been retired — this half is asserting nothing")
	}
	issueRef := regexp.MustCompile(`#\d+`)
	for _, r := range Retirements() {
		if _, ok := LookupByAlias(r.SuccessorModelID, all); !ok {
			t.Errorf("retired %v points at successor %q, which the catalog does not ship",
				r.Names, r.SuccessorModelID)
		}
		if !issueRef.MatchString(r.Reason) {
			t.Errorf("retirement of %v does not cite where it was decided — no issue "+
				"reference in %q", r.Names, r.Reason)
		}
		// A claimed rate has to carry its bound. "failed 12 of 72" without
		// one is a number with no interval, which is what #475 was opened
		// about; a generation reason makes no rate claim and is exempt.
		if strings.Contains(r.Reason, "failure rate") && !strings.Contains(r.Reason, "confidence") {
			t.Errorf("retirement of %v claims a failure rate without a confidence bound: %q",
				r.Names, r.Reason)
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
//
// It pins the STORE against this table, not the prose against the store,
// so a rate cited a SECOND time elsewhere is still on its own: Failures'
// doc went on calling granite4-350m "the next-worst at 17%" through
// #483, which had moved it to 38%. When a number here changes, grep the
// file for the old one. TestWithheldReasonsQuoteTheStore covers the
// same rot in the manifests.
func TestRatesCitedByRetireFailureRate(t *testing.T) {
	set, err := AgentGrades()
	if err != nil {
		t.Fatalf("AgentGrades: %v", err)
	}
	cited := []struct {
		model, variant string
		wantPct        int
	}{
		{"granite4-350m", "bf16-gguf", 38},           // the CI fixture
		{"qwen2.5-coder-3b-instruct", "q4-gguf", 23}, // defines the install quality floor
		{"qwen3.5-35b-a3b", "q4-gguf", 8},            // a current recommendation
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

// Every withheld model's reason string, held against the store it
// describes.
//
// TestRatesCitedByRetireFailureRate covers the same rot one file over,
// and covering only that file is how this one was missed: granite4-350m's
// internal_only said it "emits real structured tool calls, clean on
// greeting and search" — true of the three trials that had been run when
// it was written, and false by 13 of 24 on search-then-edit once the
// catalog was swept at 24 (#479) and the invalid-arguments class was
// promoted (#483). Nothing read it, so nothing said so. It is a
// load-bearing sentence: it was the argument that a future CI leg could
// drive a real tool call on this model.
//
// Two properties, both cheap:
//
//   - the reason quotes the variant's CURRENT worst-case bound, so a
//     sweep that moves the rate cannot leave the prose behind;
//   - it says which KIND of withholding this is. The report prints
//     permanent exemptions (granite4-350m, the CI fixture) beside
//     transitional ones (qwen2.5-coder-0.5b, awaiting #200) and the
//     reason string is the only thing that distinguishes them.
//
// A record of today's behaviour, not a product contract: when the store
// moves, the expected number moves with it — visibly, in a diff someone
// signs off on.
func TestWithheldReasonsQuoteTheStore(t *testing.T) {
	set, err := AgentGrades()
	if err != nil {
		t.Fatalf("AgentGrades: %v", err)
	}
	all, err := BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatalf("BundledManifestsIncludingInternal: %v", err)
	}
	withheld := 0
	for _, m := range all {
		if m.InternalOnly == "" {
			continue
		}
		withheld++
		lower := strings.ToLower(m.InternalOnly)
		if !strings.Contains(lower, "permanent") && !strings.Contains(lower, "pending retirement") {
			t.Errorf("internal_only for %s says neither \"permanent\" nor \"pending retirement\"; "+
				"the report cannot tell a standing exemption from a stop on the way to deletion, "+
				"and an unlabelled one defaults to permanent by never being revisited", m.ModelID)
		}
		for _, v := range m.Variants {
			rec, ok := set.Lookup(m.ModelID, v.VariantID)
			if !ok {
				continue
			}
			worst, counted := rec.WorstCase()
			if !counted {
				continue
			}
			want := fmt.Sprintf("%.0f%%", worst.LowerBound*100)
			if !strings.Contains(m.InternalOnly, want) {
				t.Errorf("%s/%s is measured at a %s lower bound (%s failed %d of %d) but its "+
					"internal_only reason never says %s — re-measure or rewrite the reason, "+
					"whichever is out of date",
					m.ModelID, v.VariantID, want, worst.Case, worst.Failed, worst.Trials, want)
			}
		}
	}
	if withheld == 0 {
		t.Fatal("no withheld models in the bundled catalog — this guard is checking nothing")
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
