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

	// Offered-only for the report: coverage and the retirement worklist
	// are questions about what we hand people, and a withheld model is
	// neither required to be agent-grade nor retirable for failing.
	bundled, err := catalog.BundledManifests()
	if err != nil {
		return fmt.Errorf("agentgrade: load bundled catalog: %w", err)
	}
	rev, err := agentgrade.FixtureRevision()
	if err != nil {
		return fmt.Errorf("agentgrade: fixture revision: %w", err)
	}

	if len(importPaths) > 0 {
		// The COMPLETE set for import: a withheld model is shipped, and
		// measuring one is how it earned the job. Resolving its tag
		// against the offered set would reject the verdict that
		// justified withholding it.
		all, err := catalog.BundledManifestsIncludingInternal()
		if err != nil {
			return fmt.Errorf("agentgrade: load bundled catalog: %w", err)
		}
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

	gaps := set.CoverageGaps(bundled, rev)
	failures := set.Failures(bundled)

	fmt.Printf("fixture revision: %s\n", rev)
	fmt.Printf("%d bundled manifests; %d with a recorded verdict; %d declared unmeasurable\n",
		len(bundled), len(set.Models), len(set.Unmeasurable))

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
