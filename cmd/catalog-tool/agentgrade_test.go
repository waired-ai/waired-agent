package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/agentgrade"
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
		out = append(out, map[string]any{
			"case": c.Case, "verdict": c.Verdict,
			"trials": c.Trials, "failed_trials": c.FailedTrials,
		})
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
		caseResult{"greeting", "fail_malformed_tool_call", 12, 1},
		caseResult{"read-file", "pass", 12, 0})
	s := reportWith(t, agentgrade.TransportStream,
		caseResult{"greeting", "warn_unprompted_tool_call", 12, 0},
		caseResult{"read-file", "pass", 12, 0})

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
	c := caseResult{"read-file", "pass", 12, 0}
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
	base := reportWith(t, agentgrade.TransportUnary, caseResult{"read-file", "pass", 12, 0})
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
			other := reportWith(t, agentgrade.TransportStream, caseResult{"read-file", "pass", 12, 0})
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
	r := reportWith(t, agentgrade.TransportStream, caseResult{"read-file", "pass", 12, 0})
	got, err := pool([]probeReport{r})
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if got.Transport != agentgrade.TransportStream || got.Trials != 12 {
		t.Errorf("transport=%q trials=%d", got.Transport, got.Trials)
	}
}
