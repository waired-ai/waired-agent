package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
)

// PIN: record of today's diagnostic wording, not a product contract — no
// ruling fixes these strings. What IS load-bearing is which three states are
// distinguished: absent, baseUrl-mismatch, and healthy. The middle one is
// invisible from every other surface, because Claude Code compares the cached
// baseUrl to the live one as an exact string and then silently ignores the
// whole file (internal/integration/claudecode/gatewaycache.go).
func TestClaudePickerStatusRow(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	const live = "http://127.0.0.1:9472"
	path := "/home/u/.claude/cache/gateway-models.json"

	present := func(baseURL string, n int, at time.Time) claudecode.GatewayCacheState {
		models := make([]claudecode.GatewayCacheModel, n)
		return claudecode.GatewayCacheState{Path: path, Present: true, BaseURL: baseURL, FetchedAt: at, Models: models}
	}

	for _, tc := range []struct {
		name     string
		facts    claudePickerFacts
		want     []string
		wantNone bool
	}{{
		name:     "nothing to say when this host is not routed at all",
		facts:    claudePickerFacts{liveBaseURL: "", now: now},
		wantNone: true,
	}, {
		name: "absent points at the file and at the user who has to own it",
		facts: claudePickerFacts{
			state:       claudecode.GatewayCacheState{Path: path},
			liveBaseURL: live, now: now,
		},
		want: []string{"not written", path, "waired claude enable"},
	}, {
		name: "under sudo it names whose home was inspected",
		facts: claudePickerFacts{
			state:       claudecode.GatewayCacheState{Path: path},
			liveBaseURL: live, viaSudo: true, sudoUser: "u", now: now,
		},
		want: []string{"not written", "(user u)"},
	}, {
		name: "a base URL mismatch is reported as the client ignoring the file",
		facts: claudePickerFacts{
			state:       present(live+"/", 4, now.Add(-time.Hour)),
			liveBaseURL: live, now: now,
		},
		want: []string{"IGNORED BY CLAUDE CODE", live + "/", live},
	}, {
		name: "healthy says how many entries and how old",
		facts: claudePickerFacts{
			state:       present(live, 5, now.Add(-90*time.Minute)),
			liveBaseURL: live, now: now,
		},
		want: []string{"5 entries", "1 hours ago", path},
	}, {
		name: "an unreadable file is not reported as a missing one",
		facts: claudePickerFacts{
			state:       claudecode.GatewayCacheState{Path: path},
			readErr:     errors.New("parse: unexpected end of JSON input"),
			liveBaseURL: live, now: now,
		},
		want: []string{"UNREADABLE", "unexpected end of JSON"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got := claudePickerStatusRow(tc.facts)
			if tc.wantNone {
				if got != "" {
					t.Fatalf("want no row, got %q", got)
				}
				return
			}
			if got == "" {
				t.Fatal("want a row, got none")
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("row missing %q:\n%s", w, got)
				}
			}
		})
	}
}

func TestHumanAge(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, want string
		then       time.Time
	}{
		{"unwritten", "at an unknown time", time.Time{}},
		{"clock skew reads as unknown, not negative", "at an unknown time", now.Add(time.Hour)},
		{"seconds", "just now", now.Add(-30 * time.Second)},
		{"minutes", "5 minutes ago", now.Add(-5 * time.Minute)},
		{"hours", "3 hours ago", now.Add(-3 * time.Hour)},
		{"days", "3 days ago", now.Add(-72 * time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := humanAge(now, tc.then); got != tc.want {
				t.Errorf("humanAge = %q, want %q", got, tc.want)
			}
		})
	}
}
