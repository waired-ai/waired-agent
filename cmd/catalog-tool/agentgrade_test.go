package main

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/agentgrade"
	"github.com/waired-ai/waired-agent/internal/catalog"
)

// writeReport spills a probe report to a temp file and returns its path.
func writeReport(t *testing.T, rep map[string]any) string {
	t.Helper()
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	p := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatalf("write report: %v", err)
	}
	return p
}

// validReport is a report that would import cleanly, so each test below
// can break exactly one thing.
func validReport(t *testing.T) map[string]any {
	t.Helper()
	rev, err := agentgrade.FixtureRevision()
	if err != nil {
		t.Fatalf("FixtureRevision: %v", err)
	}
	return map[string]any{
		"model":            "granite4:350m",
		"grade":            "pass",
		"fixture_revision": rev,
		"agent_revision":   "0123456789ab",
		"transport":        agentgrade.TransportStream,
		"trials":           12,
	}
}

func importWith(t *testing.T, rep map[string]any, o importOpts) error {
	t.Helper()
	rev, err := agentgrade.FixtureRevision()
	if err != nil {
		t.Fatalf("FixtureRevision: %v", err)
	}
	o.Revision = rev
	if o.Retrieved == "" {
		o.Retrieved = "2026-08-02"
	}
	return importAgentGrade([]string{writeReport(t, rep)}, o)
}

// Product contract (#426): FixtureRevision covers the fixture and nothing
// else, so a report that cannot name the gateway it was measured against
// is not filed. #409 changed four models' verdicts without moving the
// fixture by a byte.
func TestImportAgentGrade_RefusesUnstampedHarness(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field string
	}{
		{"no agent revision", "agent_revision"},
		{"no transport", "transport"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rep := validReport(t)
			rep[tc.field] = ""
			err := importWith(t, rep, importOpts{})
			if err == nil {
				t.Fatal("imported a report with no harness provenance")
			}
			if !strings.Contains(err.Error(), "make e2e-agentgrade") {
				t.Errorf("error does not say how to fix it: %v", err)
			}
		})
	}
}

// A verdict taken on an uncommitted tree names code that exists nowhere
// but that machine, so nobody can re-take the decision from it later.
func TestImportAgentGrade_RefusesDirtyTree(t *testing.T) {
	rep := validReport(t)
	rep["agent_revision"] = "0123456789ab-dirty"
	err := importWith(t, rep, importOpts{})
	if err == nil {
		t.Fatal("imported a report measured on a dirty tree")
	}
	if !strings.Contains(err.Error(), "cannot be reproduced") {
		t.Errorf("error = %v", err)
	}
}

// caseList builds the results block of a report.
func caseList(t *testing.T, cs ...caseResult) []any {
	t.Helper()
	out := make([]any, 0, len(cs))
	for _, c := range cs {
		m := map[string]any{
			"case": c.Case, "verdict": c.Verdict,
			"trials": c.Trials, "failed_trials": c.FailedTrials,
		}
		// Omitted when absent, so a case built without a tally round-trips
		// as a pre-counter report rather than as an empty one.
		if c.Verdicts != nil {
			m["verdicts"] = c.Verdicts
		}
		out = append(out, m)
	}
	return out
}

func reportWith(t *testing.T, transport string, cs ...caseResult) probeReport {
	t.Helper()
	m := validReport(t)
	m["transport"] = transport
	m["results"] = caseList(t, cs...)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var r probeReport
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return r
}

// Product contract (#426): two runs of the same model over the two
// transports are two samples of one thing, so their trials add up and the
// stored ratio spans both. Recording one and discarding the other would
// halve the evidence a retirement decision reads.
func TestPool_SumsTrialsAndKeepsTheWorstVerdict(t *testing.T) {
	u := reportWith(t, agentgrade.TransportUnary,
		caseResult{Case: "greeting", Verdict: "fail_malformed_tool_call", Trials: 12, FailedTrials: 1},
		caseResult{Case: "read-file", Verdict: "pass", Trials: 12, FailedTrials: 0})
	s := reportWith(t, agentgrade.TransportStream,
		caseResult{Case: "greeting", Verdict: "warn_unprompted_tool_call", Trials: 12, FailedTrials: 0},
		caseResult{Case: "read-file", Verdict: "pass", Trials: 12, FailedTrials: 0})

	got, err := pool([]probeReport{u, s})
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if got.Trials != 24 {
		t.Errorf("trials = %d, want 24", got.Trials)
	}
	if got.Transport != "stream+unary" {
		t.Errorf("transport = %q, want %q", got.Transport, "stream+unary")
	}
	greeting := got.Results[0]
	if greeting.Verdict != "fail_malformed_tool_call" {
		t.Errorf("greeting verdict = %q — the worse of the two must survive", greeting.Verdict)
	}
	if greeting.Trials != 24 || greeting.FailedTrials != 1 {
		t.Errorf("greeting = %d/%d, want 1/24", greeting.FailedTrials, greeting.Trials)
	}
	// A case that failed on either run failed: the grade follows the
	// POOLED cases, not either run's own grade.
	if got.Grade != string(agentgrade.GradeFail) {
		t.Errorf("grade = %q, want fail", got.Grade)
	}
}

// A pass on both stays a pass — the pooling must not manufacture failure
// out of two clean runs.
func TestPool_TwoCleanRunsStayClean(t *testing.T) {
	c := caseResult{Case: "read-file", Verdict: "pass", Trials: 12, FailedTrials: 0}
	got, err := pool([]probeReport{
		reportWith(t, agentgrade.TransportUnary, c),
		reportWith(t, agentgrade.TransportStream, c),
	})
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if got.Grade != string(agentgrade.GradePass) || got.Results[0].FailedTrials != 0 {
		t.Errorf("grade=%q failed=%d, want pass/0", got.Grade, got.Results[0].FailedTrials)
	}
}

// Pooling across a different model, fixture or harness would produce a
// verdict spanning two things — the silent mixing #426 exists to end.
func TestPool_RefusesIncomparableRuns(t *testing.T) {
	base := reportWith(t, agentgrade.TransportUnary, caseResult{Case: "read-file", Verdict: "pass", Trials: 12, FailedTrials: 0})
	for _, tc := range []struct {
		name   string
		mutate func(*probeReport)
		want   string
	}{
		{"different model", func(r *probeReport) { r.Model = "gpt-oss:20b" }, "model"},
		{"different fixture", func(r *probeReport) { r.FixtureRevision = "deadbeefcafe" }, "fixture_revision"},
		{"different harness", func(r *probeReport) { r.AgentRevision = "ffffffffffff" }, "agent_revision"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			other := reportWith(t, agentgrade.TransportStream, caseResult{Case: "read-file", Verdict: "pass", Trials: 12, FailedTrials: 0})
			tc.mutate(&other)
			_, err := pool([]probeReport{base, other})
			if err == nil {
				t.Fatal("pooled two runs that are not samples of the same thing")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not name %s: %v", tc.want, err)
			}
		})
	}
}

// A single report still pools, and reports its own transport verbatim.
func TestPool_SingleReportIsUnchanged(t *testing.T) {
	r := reportWith(t, agentgrade.TransportStream, caseResult{Case: "read-file", Verdict: "pass", Trials: 12, FailedTrials: 0})
	got, err := pool([]probeReport{r})
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if got.Transport != agentgrade.TransportStream || got.Trials != 12 {
		t.Errorf("transport=%q trials=%d", got.Transport, got.Trials)
	}
}

// Pooling two runs adds their per-class tallies, the same way it adds
// their trial counts. Anything less and a stored tally would describe
// one transport while the ratio beside it describes both.
func TestPool_SumsVerdictClasses(t *testing.T) {
	u := reportWith(t, agentgrade.TransportUnary, caseResult{
		Case: "read-file", Verdict: "fail_no_tool_call", Trials: 12, FailedTrials: 1,
		Verdicts: map[string]int{"pass": 9, "warn_invalid_tool_arguments": 2, "fail_no_tool_call": 1},
	})
	s := reportWith(t, agentgrade.TransportStream, caseResult{
		Case: "read-file", Verdict: "warn_invalid_tool_arguments", Trials: 12, FailedTrials: 0,
		Verdicts: map[string]int{"pass": 8, "warn_invalid_tool_arguments": 4},
	})

	got, err := pool([]probeReport{u, s})
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	want := map[string]int{"pass": 17, "warn_invalid_tool_arguments": 6, "fail_no_tool_call": 1}
	if !maps.Equal(got.Results[0].Verdicts, want) {
		t.Errorf("verdicts = %v, want %v", got.Results[0].Verdicts, want)
	}
	// The tally has to keep agreeing with the totals beside it.
	sum := 0
	for _, n := range got.Results[0].Verdicts {
		sum += n
	}
	if sum != got.Results[0].Trials {
		t.Errorf("tally sums to %d but trials = %d", sum, got.Results[0].Trials)
	}
}

// pool must not write through into the reports it was handed. The
// unpooled report is what scripts/dev/agentgrade-pool.py prints for the
// side-by-side view, and --import is given the same file twice often
// enough (a re-run of one transport) that an aliased map would show up as
// doubled counts rather than as a crash.
func TestPool_DoesNotMutateItsInput(t *testing.T) {
	c := caseResult{
		Case: "read-file", Verdict: "pass", Trials: 12,
		Verdicts: map[string]int{"pass": 12},
	}
	u := reportWith(t, agentgrade.TransportUnary, c)
	s := reportWith(t, agentgrade.TransportStream, c)

	if _, err := pool([]probeReport{u, s}); err != nil {
		t.Fatalf("pool: %v", err)
	}
	if got := u.Results[0].Verdicts["pass"]; got != 12 {
		t.Errorf("pool wrote through into its input: pass = %d, want 12", got)
	}
}

// A run measured before the probe tallied classes cannot contribute one,
// so pooling it with a run that can must drop the tally rather than
// report a partial one.
//
// A tally summing to 12 next to trials=24 does not read as "one run
// predates the counter"; it reads as "12 trials produced no verdict",
// which is not a thing that can happen. Unknown is the honest answer, and
// it is the same answer Trials == 0 gives for the ratio.
func TestPool_RefusesToTallyAcrossAPreCounterRun(t *testing.T) {
	old := reportWith(t, agentgrade.TransportUnary, caseResult{
		Case: "read-file", Verdict: "pass", Trials: 12,
	})
	fresh := reportWith(t, agentgrade.TransportStream, caseResult{
		Case: "read-file", Verdict: "pass", Trials: 12,
		Verdicts: map[string]int{"pass": 12},
	})

	for _, tc := range []struct {
		name string
		reps []probeReport
	}{
		{"old first", []probeReport{old, fresh}},
		{"new first", []probeReport{fresh, old}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pool(tc.reps)
			if err != nil {
				t.Fatalf("pool: %v", err)
			}
			if got.Results[0].Verdicts != nil {
				t.Errorf("verdicts = %v, want none — one run cannot report classes",
					got.Results[0].Verdicts)
			}
			if got.Results[0].Trials != 24 {
				t.Errorf("trials = %d, want 24 — the ratio still pools", got.Results[0].Trials)
			}
		})
	}
}

// The rate table's class column, which is the whole reason the counts are
// stored where a reader can see them.
func TestDescribeClasses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		counts map[string]int
		want   string
	}{
		{
			// Not "" and not "pass×0": a record that predates the counter
			// knows nothing about classes, which is a different statement
			// from "only one class occurred".
			name: "a record with no tally says so",
			want: "(not counted)",
		},
		{
			name:   "worst class first, not alphabetical",
			counts: map[string]int{"pass": 20, "fail_no_tool_call": 1, "fail_invalid_tool_arguments": 3},
			want:   "fail_no_tool_call×1 fail_invalid_tool_arguments×3 pass×20",
		},
		{
			// A store that has not been through --recompute still holds
			// the pre-#483 spelling. Ranked as the class it names, it
			// prints among the failures; ranked as the unknown string it
			// literally is, Severity returns the pass rank and it would
			// print last, beside the clean trials.
			name:   "a pre-rename spelling still ranks as the failure it is",
			counts: map[string]int{"pass": 20, "warn_invalid_tool_arguments": 4},
			want:   "warn_invalid_tool_arguments×4 pass×20",
		},
		{
			name:   "one class",
			counts: map[string]int{"pass": 24},
			want:   "pass×24",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := describeClasses(tc.counts); got != tc.want {
				t.Errorf("describeClasses(%v) = %q, want %q", tc.counts, got, tc.want)
			}
		})
	}
}

// --recompute is the operation that makes the stored tally load-bearing:
// a grading-policy change reaches the catalog through it instead of
// through a GPU sweep.
func TestRecomputeAgentGrades(t *testing.T) {
	// A record written before #483: the class is spelled warn_, and
	// `failed` counts only the class that was failing at the time.
	set := catalog.AgentGradeSet{Models: map[string]catalog.ModelAgentGrade{
		"subject": {Variants: map[string]catalog.VariantAgentGrade{"q4": {
			Verdict: catalog.AgentGradePass,
			Cases: map[string]catalog.CaseOutcome{
				"greeting": {Verdict: "pass", Trials: 24,
					Verdicts: map[string]int{"pass": 24}},
				"read-file": {Verdict: "warn_invalid_tool_arguments", Trials: 24, Failed: 0,
					Verdicts: map[string]int{"pass": 21, "warn_invalid_tool_arguments": 3}},
			},
		}}},
	}}

	changed, skipped := recomputeAgentGrades(set)
	if changed != 1 || skipped != 0 {
		t.Fatalf("changed=%d skipped=%d, want 1/0", changed, skipped)
	}
	got := set.Models["subject"].Variants["q4"]
	if got.Verdict != catalog.AgentGradeFail {
		t.Errorf("grade = %q, want %q — the case now holds a failing class",
			got.Verdict, catalog.AgentGradeFail)
	}
	rf := got.Cases["read-file"]
	if rf.Verdict != string(agentgrade.VerdictInvalidToolArguments) {
		t.Errorf("case verdict = %q, want the renamed class %q",
			rf.Verdict, agentgrade.VerdictInvalidToolArguments)
	}
	if rf.Failed != 3 {
		t.Errorf("failed = %d, want 3 — recounted under the current rule", rf.Failed)
	}
	if n := rf.Verdicts["warn_invalid_tool_arguments"]; n != 0 {
		t.Errorf("the old spelling survived in the tally: %v", rf.Verdicts)
	}
	if n := rf.Verdicts[string(agentgrade.VerdictInvalidToolArguments)]; n != 3 {
		t.Errorf("tally = %v, want the 3 trials under the canonical name", rf.Verdicts)
	}
	// Trials is the one thing recompute must not touch: it is a
	// measurement, not a derivation.
	if rf.Trials != 24 {
		t.Errorf("trials = %d, want 24 untouched", rf.Trials)
	}
	// A clean case stays clean and is not rewritten into existence.
	if g := set.Models["subject"].Variants["q4"].Cases["greeting"]; g.Failed != 0 || g.Verdict != "pass" {
		t.Errorf("greeting = %+v, want an untouched pass", g)
	}

	// Idempotent, and it has to be: the command rewrites a tracked file,
	// so a second run reporting work would leave a reviewer unable to tell
	// "the policy moved" from "the tool is unstable".
	if again, _ := recomputeAgentGrades(set); again != 0 {
		t.Errorf("a second recompute rewrote %d variant(s); it must be a no-op", again)
	}
}

// A case measured before the counter existed has no tally to re-read, so
// its stored numbers are the only evidence it has. Zeroing them would
// silently re-grade it as clean; recompute leaves it alone and says how
// many it left.
func TestRecomputeLeavesUncountedCasesAlone(t *testing.T) {
	set := catalog.AgentGradeSet{Models: map[string]catalog.ModelAgentGrade{
		"subject": {Variants: map[string]catalog.VariantAgentGrade{"q4": {
			Verdict: catalog.AgentGradeFail,
			Cases: map[string]catalog.CaseOutcome{
				"read-file": {Verdict: "fail_no_tool_call", Trials: 24, Failed: 9},
			},
		}}},
	}}

	_, skipped := recomputeAgentGrades(set)
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1", skipped)
	}
	got := set.Models["subject"].Variants["q4"]
	if c := got.Cases["read-file"]; c.Failed != 9 || c.Verdict != "fail_no_tool_call" {
		t.Errorf("an uncounted case was rewritten: %+v", c)
	}
	if got.Verdict != catalog.AgentGradeFail {
		t.Errorf("grade = %q, want fail — an uncounted failing case still fails", got.Verdict)
	}
}
