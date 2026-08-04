package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/waired-ai/waired-agent/internal/agentgrade"
	"github.com/waired-ai/waired-agent/internal/catalog"
)

func init() {
	subcommands["agentgrade"] = subcommand{
		run:     runAgentGrade,
		summary: "report / check / import coding-agent tool-format verdicts (#322)",
	}
}

// agentGradePath is where the verdict store lives. Written only by
// --import: a verdict is a measurement, and hand-editing this file is
// how it would quietly become an opinion again.
const agentGradePath = "internal/catalog/agentgrade.json"

func runAgentGrade(args []string) error {
	fs := flag.NewFlagSet("agentgrade", flag.ContinueOnError)
	check := fs.Bool("check", false, "exit non-zero when a bundled manifest has no usable verdict")
	requirePass := fs.Bool("require-pass", false,
		"additionally fail when any bundled manifest is recorded as failing "+
			"(the end state: the catalog holds only models that can drive a coding agent)")
	var importPaths repeatedPath
	fs.Var(&importPaths, "import",
		"fold a probe report (make e2e-agentgrade JSON=...) into "+agentGradePath+
			"; repeat it to POOL several runs of the same model into one verdict")
	engineVersion := fs.String("engine-version", "", "engine version the report was measured on (with --import)")
	host := fs.String("host", "", "hardware CLASS the report was measured on, never an identifier, e.g. nvidia-24gb-discrete (with --import)")
	runURL := fs.String("run-url", "", "CI run URL, when the report came from one (with --import)")
	retrieved := fs.String("retrieved", "", "measurement date YYYY-MM-DD (with --import; required)")
	notes := fs.String("notes", "", "free-text note to store with the verdict (with --import)")
	fixtureBytes := fs.Bool("fixture-bytes", false,
		"print the probe fixture's whole-request size in bytes and exit "+
			"(the drift canary compares it against the real client's)")
	sweepTargets := fs.Bool("sweep-targets", false,
		"print `<model_id> <variant_id> <engine tag>` for every variant a "+
			"catalog sweep should measure, and exit (scripts/dev/agentgrade-sweep.sh)")
	recompute := fs.Bool("recompute", false,
		"re-derive every stored grade from the per-class tallies under the "+
			"CURRENT rules, and exit — how a change to Verdict.IsFailure or the "+
			"severity ladder reaches the catalog without a GPU sweep")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *fixtureBytes {
		// Read the size off the request the probe actually builds, so the
		// canary can never compare against a number that has drifted from
		// what is sent.
		n, err := agentgrade.RequestBytes()
		if err != nil {
			return err
		}
		fmt.Println(n)
		return nil
	}

	// Offered-only for coverage and the gate: both are questions about
	// what we hand people, and a withheld model is neither required to
	// be agent-grade nor blocking on it. The REPORT still shows the
	// withheld ones (printWithheldPendingRetirement) — silence is how a
	// withheld entry becomes a permanent one.
	bundled, err := catalog.BundledManifests()
	if err != nil {
		return fmt.Errorf("agentgrade: load bundled catalog: %w", err)
	}
	// The COMPLETE set: a withheld model is shipped, and measuring one is
	// how it earned the job. Import resolves its engine tag against this
	// set — the offered set would reject the very verdict that justified
	// withholding it — and the report reads it for the withheld section.
	all, err := catalog.BundledManifestsIncludingInternal()
	if err != nil {
		return fmt.Errorf("agentgrade: load bundled catalog: %w", err)
	}
	rev, err := agentgrade.FixtureRevision()
	if err != nil {
		return fmt.Errorf("agentgrade: fixture revision: %w", err)
	}

	if len(importPaths) > 0 {
		return importAgentGrade(importPaths, importOpts{
			EngineVersion: *engineVersion,
			Host:          *host,
			RunURL:        *runURL,
			Retrieved:     *retrieved,
			Notes:         *notes,
			Revision:      rev,
			Bundled:       all,
		})
	}

	set, err := catalog.AgentGrades()
	if err != nil {
		return err
	}

	if *sweepTargets {
		printSweepTargets(set, all)
		return nil
	}

	if *recompute {
		changed, skipped := recomputeAgentGrades(set)
		if err := writeAgentGrades(set); err != nil {
			return err
		}
		fmt.Printf("recomputed %s: %d variant(s) rewritten\n", agentGradePath, changed)
		if skipped > 0 {
			fmt.Printf("%d case(s) have no per-class tally and were left as measured — "+
				"they predate the counter and cannot be re-graded without a sweep\n", skipped)
		}
		return nil
	}

	gaps := set.CoverageGaps(bundled, rev)
	failures := set.Failures(bundled)

	fmt.Printf("fixture revision: %s\n", rev)
	fmt.Printf("%d bundled manifests; %d with a recorded verdict; %d declared unmeasurable\n",
		len(bundled), len(set.Models), len(set.Unmeasurable))

	printFailureRates(set, bundled)
	printWithheldPendingRetirement(set, all)

	if len(failures) > 0 {
		fmt.Printf("\nrecorded as FAILING (%d) — cannot drive a coding agent:\n", len(failures))
		for _, f := range failures {
			fmt.Printf("  %-34s %-18s %s\n", f.ModelID, f.VariantID, f.Reason)
		}
	}
	if len(gaps) > 0 {
		fmt.Printf("\nno usable verdict (%d):\n", len(gaps))
		for _, g := range gaps {
			fmt.Printf("  %-34s %-18s %s\n", g.ModelID, g.VariantID, g.Reason)
		}
	}
	if len(gaps) == 0 && len(failures) == 0 {
		fmt.Println("\nok: every bundled manifest has a passing, current verdict")
	}

	if *check && len(gaps) > 0 {
		return fmt.Errorf("agentgrade: %d bundled variant(s) have no usable verdict — "+
			"measure them (make e2e-agentgrade MODEL=<tag>) and import the report, or declare "+
			"them in the \"unmeasurable\" map with a reason", len(gaps))
	}
	if *requirePass && len(failures) > 0 {
		return fmt.Errorf("agentgrade: %d bundled variant(s) are recorded as failing; "+
			"the catalog is meant to hold only models that can drive a coding agent", len(failures))
	}
	return nil
}

// printSweepTargets prints what a whole-catalog re-measurement has to
// cover, one `model_id variant_id tag` per line.
//
// Derived, never typed. A sweep is 17 variants over two transports, and
// the way one gets missed is a hand-maintained list that a new manifest
// does not reach — the same failure the coverage guard exists to catch,
// arriving through the back door. The rule is the same one --check
// applies: every ollama-servable bundled variant that is not declared
// unmeasurable.
//
// The COMPLETE set, including internal_only. A withheld model is still
// shipped, and its verdict is the record justifying the withholding
// (qwen2.5-coder-0.5b, #475) or the reason it was chosen as the CI
// fixture (granite4-350m) — letting either go stale is how the
// justification quietly stops being true.
func printSweepTargets(set catalog.AgentGradeSet, all []catalog.Manifest) {
	for _, m := range all {
		if _, ok := set.Unmeasurable[m.ModelID]; ok {
			continue
		}
		for _, v := range m.Variants {
			if !slices.Contains(v.RuntimeSupport, catalog.RuntimeOllama) {
				continue
			}
			if v.Source.Type != catalog.SourceOllama || v.Source.Tag == "" {
				continue
			}
			fmt.Printf("%s %s %s\n", m.ModelID, v.VariantID, v.Source.Tag)
		}
	}
}

// printFailureRates lists every recorded variant by its worst case's
// measured failure rate, worst first.
//
// The store's own notes field has to shout "READ THE RATIO, NOT THE
// VERDICT" because the verdict is the worst outcome across every trial
// and therefore a function of the trial count: entries reading "fail" on
// one bad trial in 24 sit beside entries that failed 24 of 24. Telling a
// reader that in prose and then printing only the worklist leaves them
// to open the JSON to find out which they are looking at. This prints
// the ratio, so the instruction is unnecessary.
func printFailureRates(set catalog.AgentGradeSet, manifests []catalog.Manifest) {
	type row struct {
		model, variant string
		worst          catalog.CaseFailureRate
		classes        map[string]int
	}
	var rows []row
	for _, m := range manifests {
		for _, v := range m.Variants {
			rec, ok := set.Lookup(m.ModelID, v.VariantID)
			if !ok {
				continue
			}
			if worst, counted := rec.WorstCase(); counted {
				rows = append(rows, row{m.ModelID, v.VariantID, worst,
					rec.Cases[worst.Case].Verdicts})
			}
		}
	}
	if len(rows) == 0 {
		return
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].worst.LowerBound != rows[j].worst.LowerBound {
			return rows[i].worst.LowerBound > rows[j].worst.LowerBound
		}
		return rows[i].model < rows[j].model
	})

	fmt.Printf("\nmeasured failure rate, worst case per variant "+
		"(retirement line: lower bound above %.0f%%):\n", catalog.RetireFailureRate*100)
	for _, r := range rows {
		mark := "  "
		if r.worst.LowerBound > catalog.RetireFailureRate {
			mark = "→ "
		}
		fmt.Printf("  %s%-30s %-14s %-17s %3d/%-3d  >= %3.0f%%   %s\n",
			mark, r.model, r.variant, r.worst.Case,
			r.worst.Failed, r.worst.Trials, r.worst.LowerBound*100,
			describeClasses(r.classes))
	}
}

// describeClasses renders one case's per-class tally, worst class first.
//
// The ratio column pools every failing class into one number and names
// only the worst, so two variants reading 3/24 can be three engine parse
// errors or three different defects — and a variant reading 0/24 can
// still have produced warnings that a policy change would turn into
// failures. This is the column that answers those without opening the
// JSON.
//
// "(not counted)" rather than an empty cell: a record measured before
// the probe tallied classes knows nothing about them, which is different
// from a case that produced exactly one.
func describeClasses(counts map[string]int) string {
	if len(counts) == 0 {
		return "(not counted)"
	}
	// Rank by the CANONICAL class. A store that has not been through
	// --recompute still holds pre-rename spellings, and an unknown class
	// ranks at the pass end of the ladder — so a renamed failure would
	// print last, beside the clean trials, exactly where a reader is
	// least likely to look at it.
	rank := func(s string) int {
		return agentgrade.Severity(agentgrade.CanonicalVerdict(agentgrade.Verdict(s)))
	}
	classes := slices.SortedFunc(maps.Keys(counts), func(a, b string) int {
		if d := rank(b) - rank(a); d != 0 {
			return d
		}
		return strings.Compare(a, b)
	})
	parts := make([]string, 0, len(classes))
	for _, c := range classes {
		parts = append(parts, fmt.Sprintf("%s×%d", c, counts[c]))
	}
	return strings.Join(parts, " ")
}

// printWithheldPendingRetirement lists the models held out of the offered
// catalog that are ALSO above the retirement line.
//
// Withholding a failing model removes it from every offered-only view at
// once — the rate table above, the worklist, the docs table, `models ls`.
// That is the point, and it is also how such an entry stops being
// revisited: the next reader sees a clean report and no reason to think
// anything is queued. The exemption pattern this repo uses elsewhere
// (agentgrade.json's "unmeasurable", InternalOnly itself) is "an
// exemption nobody has to justify is an exemption nobody revisits", and
// a reason string only satisfies that while someone is reading the file.
// So the report says it out loud, with the rate and the reason — which
// carries the tracking issue — on every run.
//
// Reported, never gated: `--require-pass` stays offered-only. A gate here
// would be a red that no PR can clear (deleting the entry needs #200's
// retired->successor map), and a permanent red is ignored, which is the
// outcome this section exists to prevent.
//
// Reuses Failures rather than re-deriving the rate: one rule, two
// audiences. What differs is the consequence, not the threshold.
func printWithheldPendingRetirement(set catalog.AgentGradeSet, all []catalog.Manifest) {
	withheld := make([]catalog.Manifest, 0, len(all))
	reason := make(map[string]string, len(all))
	for _, m := range all {
		if m.InternalOnly == "" {
			continue
		}
		withheld = append(withheld, m)
		reason[m.ModelID] = m.InternalOnly
	}
	rows := set.Failures(withheld)
	if len(rows) == 0 {
		return
	}
	fmt.Printf("\nWITHHELD, PENDING RETIREMENT (%d) — above the retirement line and "+
		"held out of the offered catalog, so nothing above lists them:\n", len(rows))
	for _, r := range rows {
		fmt.Printf("  %-34s %-18s %s\n", r.ModelID, r.VariantID, r.Reason)
		fmt.Printf("    withheld because: %s\n", reason[r.ModelID])
	}
}

type importOpts struct {
	EngineVersion string
	Host          string
	RunURL        string
	Retrieved     string
	Notes         string
	Revision      string
	Bundled       []catalog.Manifest
}

// repeatedPath collects a flag given more than once, in order.
type repeatedPath []string

func (p *repeatedPath) String() string     { return strings.Join(*p, ", ") }
func (p *repeatedPath) Set(v string) error { *p = append(*p, v); return nil }

// probeReport is the subset of the probe's JSON output the store keeps.
type probeReport struct {
	Model           string       `json:"model"`
	Grade           string       `json:"grade"`
	FixtureRevision string       `json:"fixture_revision"`
	AgentRevision   string       `json:"agent_revision"`
	Transport       string       `json:"transport"`
	Trials          int          `json:"trials"`
	Flaky           []string     `json:"flaky"`
	Results         []caseResult `json:"results"`
}

type caseResult struct {
	Case         string `json:"case"`
	Verdict      string `json:"verdict"`
	Trials       int    `json:"trials"`
	FailedTrials int    `json:"failed_trials"`
	// Verdicts is the per-class tally. Absent from reports written
	// before the probe counted classes, which is why it is carried
	// alongside FailedTrials rather than replacing it here: an old report
	// must still import, and it can only supply the pooled number.
	Verdicts map[string]int `json:"verdicts,omitempty"`
}

// pool merges several runs of the same model into one verdict.
//
// #426 measures every model over both transports, and those two runs are
// two samples of one thing: the gateway answers a given engine reply
// identically on both paths (pinned by TestProbeTransportsAgree with a
// canned engine), so what differs between the runs is the model's own
// sampling. Recording only one of them would throw away half the
// evidence, and picking "the one that disagreed" would record noise as a
// finding — a case failing 1 trial in 12 on one path and 0 in 12 on the
// other has said nothing except that 1/12 is small.
//
// So they are summed, and the resulting failure ratio is what a
// retirement decision reads. What must NOT be pooled is anything that
// makes two runs incomparable: a different model, fixture or harness. Those
// are refused rather than averaged, because a pooled verdict spanning two
// gateways is exactly the silent mixing #426 exists to end.
func pool(reps []probeReport) (probeReport, error) {
	out := reps[0]
	out.Results = append([]caseResult(nil), reps[0].Results...)
	for i := range out.Results {
		// The slice copy above is shallow, so every Verdicts map is still
		// the input report's. Summing into it would edit the caller's data
		// — and --import is given the same report twice often enough
		// (a re-run of one transport) that this would show up as doubled
		// counts rather than as a crash.
		out.Results[i].Verdicts = maps.Clone(out.Results[i].Verdicts)
	}
	out.Flaky = append([]string(nil), reps[0].Flaky...)
	transports := map[string]bool{reps[0].Transport: true}

	for _, r := range reps[1:] {
		for _, m := range []struct{ what, a, b string }{
			{"model", out.Model, r.Model},
			{"fixture_revision", out.FixtureRevision, r.FixtureRevision},
			{"agent_revision", out.AgentRevision, r.AgentRevision},
		} {
			if m.a != m.b {
				return probeReport{}, fmt.Errorf(
					"agentgrade: cannot pool reports measured at different %s (%q vs %q) — "+
						"they are not samples of the same thing", m.what, m.a, m.b)
			}
		}
		transports[r.Transport] = true
		out.Trials += r.Trials
		for _, rc := range r.Results {
			i := slices.IndexFunc(out.Results, func(c caseResult) bool { return c.Case == rc.Case })
			if i < 0 {
				out.Results = append(out.Results, rc)
				continue
			}
			out.Results[i].Trials += rc.Trials
			out.Results[i].FailedTrials += rc.FailedTrials
			// Class counts pool only when BOTH runs carry them. One old
			// report in the set would otherwise produce a tally that sums
			// to fewer trials than Trials says, which reads as "some trials
			// were unclassified" instead of "one run predates the counter".
			if len(out.Results[i].Verdicts) > 0 && len(rc.Verdicts) > 0 {
				for v, n := range rc.Verdicts {
					out.Results[i].Verdicts[v] += n
				}
			} else {
				out.Results[i].Verdicts = nil
			}
			out.Results[i].Verdict = string(agentgrade.Worse(
				agentgrade.Verdict(out.Results[i].Verdict), agentgrade.Verdict(rc.Verdict)))
		}
		for _, f := range r.Flaky {
			if !slices.Contains(out.Flaky, f) {
				out.Flaky = append(out.Flaky, f)
			}
		}
	}

	// The grade follows the pooled cases, not the individual runs': a
	// model that passed one run and failed the other failed.
	out.Grade = string(agentgrade.GradePass)
	for _, c := range out.Results {
		if agentgrade.Verdict(c.Verdict).IsFailure() {
			out.Grade = string(agentgrade.GradeFail)
			break
		}
	}

	// Transport is DERIVED from what was actually driven — never typed.
	// An operator-supplied value could claim a path nobody ran, which is
	// the same class of error as a hand-maintained revision string.
	names := slices.Sorted(maps.Keys(transports))
	out.Transport = strings.Join(names, "+")
	slices.Sort(out.Flaky)
	return out, nil
}

// importAgentGrade folds one or more probe reports of the same model
// into the store, pooling them.
//
// The report names an ENGINE tag; the store is keyed by (model_id,
// variant_id). Resolving that mapping here, from the bundled catalog,
// is deliberate: asking the operator to type both would let a verdict
// be filed against the wrong variant, and a verdict filed against the
// wrong variant is worse than a missing one.
func importAgentGrade(paths []string, o importOpts) error {
	if o.Retrieved == "" {
		return fmt.Errorf("agentgrade: --retrieved YYYY-MM-DD is required with --import " +
			"(a verdict with no date cannot be aged out)")
	}

	reps := make([]probeReport, 0, len(paths))
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("agentgrade: read report: %w", err)
		}
		var r probeReport
		if err := json.Unmarshal(raw, &r); err != nil {
			return fmt.Errorf("agentgrade: parse %s: %w", path, err)
		}
		// Validate BEFORE pooling: an ungraded run summed into a graded
		// one would vanish into the total, and "half of this verdict came
		// from a run that never completed" is not recoverable afterwards.
		if err := checkReport(r, path, o); err != nil {
			return err
		}
		reps = append(reps, r)
	}

	rep, err := pool(reps)
	if err != nil {
		return err
	}

	modelID, variantID, err := resolveTag(rep.Model, o.Bundled)
	if err != nil {
		return err
	}

	set, err := catalog.AgentGrades()
	if err != nil {
		return err
	}
	if set.Models == nil {
		set.Models = map[string]catalog.ModelAgentGrade{}
	}
	entry, ok := set.Models[modelID]
	if !ok || entry.Variants == nil {
		entry = catalog.ModelAgentGrade{Variants: map[string]catalog.VariantAgentGrade{}}
	}

	cases := make(map[string]catalog.CaseOutcome, len(rep.Results))
	for _, r := range rep.Results {
		cases[r.Case] = catalog.CaseOutcome{
			Verdict: r.Verdict, Trials: r.Trials, Failed: r.FailedTrials,
			Verdicts: r.Verdicts,
		}
	}
	entry.Variants[variantID] = catalog.VariantAgentGrade{
		Verdict:         rep.Grade,
		Cases:           cases,
		Trials:          rep.Trials,
		Flaky:           rep.Flaky,
		Engine:          catalog.RuntimeOllama,
		EngineVersion:   o.EngineVersion,
		EngineTag:       rep.Model,
		FixtureRevision: o.Revision,
		AgentRevision:   rep.AgentRevision,
		Transport:       rep.Transport,
		Host:            o.Host,
		RunURL:          o.RunURL,
		Retrieved:       o.Retrieved,
		Notes:           o.Notes,
	}
	set.Models[modelID] = entry

	if err := writeAgentGrades(set); err != nil {
		return err
	}
	fmt.Printf("imported: %s / %s = %s (tag %s, %d trials over %s, agent %s)\n",
		modelID, variantID, rep.Grade, rep.Model, rep.Trials, rep.Transport, rep.AgentRevision)
	return nil
}

// checkReport rejects a report that is not a measurement, or not a
// measurement of what the store claims to hold.
func checkReport(rep probeReport, path string, o importOpts) error {
	switch rep.Grade {
	case string(agentgrade.GradePass), string(agentgrade.GradeFail):
	case string(agentgrade.GradeUnknown), "":
		// Storing an ungraded run would be indistinguishable from a
		// failure to every consumer (waired-ai/waired-agent#203).
		return fmt.Errorf("agentgrade: %s: report grade is %q — a run that could not be "+
			"completed is not a verdict and is not stored; fix the engine and re-run", path, rep.Grade)
	default:
		return fmt.Errorf("agentgrade: unknown grade %q in report", rep.Grade)
	}

	if rep.Trials < 2 {
		return fmt.Errorf("agentgrade: report ran %d trial(s); a single run is not a "+
			"measurement at the boundary (the first catalog sweep graded three models "+
			"as failing and an immediate re-run passed all three) — re-measure with at "+
			"least 2", rep.Trials)
	}

	// A report carrying a different fixture revision was measured
	// against a different request weight. Importing it would file a
	// verdict that looks current and is not.
	if rep.FixtureRevision != "" && rep.FixtureRevision != o.Revision {
		return fmt.Errorf("agentgrade: report was measured at fixture revision %s, "+
			"current is %s — re-measure rather than importing a stale verdict",
			rep.FixtureRevision, o.Revision)
	}

	// The harness generation is as load-bearing as the fixture revision
	// and has no default that could be right. #409 changed the gateway's
	// answer for the same model at the same fixture revision, so a
	// verdict that cannot name the code it was measured against is a
	// verdict nobody can re-take a decision on later.
	if rep.AgentRevision == "" || rep.Transport == "" {
		return fmt.Errorf("agentgrade: report carries agent_revision=%q transport=%q — "+
			"measure through `make e2e-agentgrade`, which stamps both; a bare `go test` "+
			"produces a report whose harness cannot be identified afterwards",
			rep.AgentRevision, rep.Transport)
	}
	if strings.HasSuffix(rep.AgentRevision, "-dirty") {
		return fmt.Errorf("agentgrade: report was measured at %s — a verdict taken on an "+
			"uncommitted tree cannot be reproduced from anything in the repository; "+
			"commit (or stash) and re-measure", rep.AgentRevision)
	}

	return nil
}

// resolveTag maps an engine-native tag back to (model_id, variant_id).
func resolveTag(tag string, bundled []catalog.Manifest) (string, string, error) {
	var matches []string
	var modelID, variantID string
	for _, m := range bundled {
		for _, v := range m.Variants {
			if v.Source.Tag == tag {
				matches = append(matches, m.ModelID+"/"+v.VariantID)
				modelID, variantID = m.ModelID, v.VariantID
			}
		}
	}
	switch len(matches) {
	case 0:
		return "", "", fmt.Errorf("agentgrade: tag %q matches no variant in the bundled catalog — "+
			"the store is keyed by catalog membership, so a tag nobody ships has nowhere to go", tag)
	case 1:
		return modelID, variantID, nil
	default:
		sort.Strings(matches)
		return "", "", fmt.Errorf("agentgrade: tag %q matches %d variants (%s) — "+
			"cannot decide which one the verdict belongs to",
			tag, len(matches), strings.Join(matches, ", "))
	}
}

// recomputeAgentGrades re-reads the stored per-class tallies under the
// CURRENT grading rules and rewrites everything derived from them.
//
// This is what makes the tally load-bearing rather than decorative.
// `failed`, a case's `verdict` and a variant's grade are all outputs of
// a policy — Verdict.IsFailure and the severity ladder — that is
// expected to change; before the tally existed the only way to apply a
// change was to re-measure the catalog on a GPU, which meant a policy
// question could sit undecided for as long as the hardware was busy.
// #455 sat that way for two sweeps. Now a policy change is a code change
// plus this command.
//
// It also canonicalises renamed classes. A verdict string is stored, so
// a rename leaves old spellings behind, and an unknown class is silently
// the most harmless thing in the package (IsFailure says no, Severity
// returns the pass rank) — a renamed failure would stop counting with no
// error anywhere.
//
// Records with no tally are left ALONE, not zeroed: they predate the
// counter, so their `failed` is the only evidence they have, taken under
// whatever rule was in force. Reporting how many were skipped is the
// point — a silent partial recompute would leave the file half under one
// policy and half under another.
func recomputeAgentGrades(set catalog.AgentGradeSet) (changed, skipped int) {
	for modelID, m := range set.Models {
		for variantID, rec := range m.Variants {
			anyFailure, touched := false, false
			for name, c := range rec.Cases {
				if len(c.Verdicts) == 0 {
					skipped++
					// Still consult the stored verdict for the grade: an
					// uncounted case that was recorded as failing has not
					// stopped failing just because it cannot be recounted.
					anyFailure = anyFailure || agentgrade.Verdict(c.Verdict).IsFailure()
					continue
				}
				tally := make(map[string]int, len(c.Verdicts))
				failed, worst := 0, agentgrade.VerdictPass
				for class, n := range c.Verdicts {
					v := agentgrade.CanonicalVerdict(agentgrade.Verdict(class))
					tally[string(v)] += n
					if v.IsFailure() {
						failed += n
						anyFailure = true
					}
					if agentgrade.Severity(v) > agentgrade.Severity(worst) {
						worst = v
					}
				}
				next := catalog.CaseOutcome{
					Verdict: string(worst), Trials: c.Trials, Failed: failed, Verdicts: tally,
				}
				if !sameOutcome(c, next) {
					touched = true
				}
				rec.Cases[name] = next
			}
			grade := catalog.AgentGradePass
			if anyFailure {
				grade = catalog.AgentGradeFail
			}
			if rec.Verdict != grade {
				rec.Verdict, touched = grade, true
			}
			if touched {
				changed++
			}
			m.Variants[variantID] = rec
		}
		set.Models[modelID] = m
	}
	return changed, skipped
}

func sameOutcome(a, b catalog.CaseOutcome) bool {
	return a.Verdict == b.Verdict && a.Trials == b.Trials && a.Failed == b.Failed &&
		maps.Equal(a.Verdicts, b.Verdicts)
}

func writeAgentGrades(set catalog.AgentGradeSet) error {
	b, err := json.MarshalIndent(set, "", "  ")
	if err != nil {
		return fmt.Errorf("agentgrade: encode store: %w", err)
	}
	if err := os.WriteFile(agentGradePath, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("agentgrade: write %s: %w", agentGradePath, err)
	}
	return nil
}
