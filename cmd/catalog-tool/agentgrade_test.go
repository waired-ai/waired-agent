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
	return importAgentGrade(writeReport(t, rep), o)
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

// --transport exists so the operator can record that BOTH paths were
// driven and agreed — a fact the probe cannot know. It must not become a
// free-text field that lets the store claim a path nobody drove.
func TestResolveTransport(t *testing.T) {
	both := transportBoth
	for _, tc := range []struct {
		name     string
		reported string
		override string
		want     string
		wantErr  bool
	}{
		{"no override keeps what ran", agentgrade.TransportStream, "", agentgrade.TransportStream, false},
		{"override may restate it", agentgrade.TransportUnary, agentgrade.TransportUnary, agentgrade.TransportUnary, false},
		{"both, measured on stream", agentgrade.TransportStream, both, both, false},
		{"both, measured on unary", agentgrade.TransportUnary, both, both, false},
		{"cannot claim the other path", agentgrade.TransportUnary, agentgrade.TransportStream, "", true},
		{"cannot invent a transport", agentgrade.TransportStream, "grpc", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveTransport(tc.reported, tc.override)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveTransport(%q, %q) = %q, want error", tc.reported, tc.override, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveTransport(%q, %q): %v", tc.reported, tc.override, err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
