package main

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
)

func TestFormatUpdateSummary(t *testing.T) {
	t.Run("available", func(t *testing.T) {
		s := formatUpdateSummary(&management.UpdateStatus{
			Phase:          management.UpdatePhaseAvailable,
			CurrentVersion: "1.2.3",
			LatestVersion:  "1.4.0",
			Available:      true,
		})
		if !strings.Contains(s, "1.2.3") || !strings.Contains(s, "1.4.0") {
			t.Errorf("summary missing versions: %q", s)
		}
	})
	t.Run("error phase shows reason and no latest line", func(t *testing.T) {
		s := formatUpdateSummary(&management.UpdateStatus{
			Phase:          management.UpdatePhaseError,
			CurrentVersion: "1.2.3",
			Error:          "github unreachable",
		})
		if !strings.Contains(s, "github unreachable") {
			t.Errorf("summary missing error: %q", s)
		}
		if strings.Contains(s, "Latest version") {
			t.Errorf("error summary should not print latest line: %q", s)
		}
	})
	t.Run("unknown versions render a placeholder", func(t *testing.T) {
		s := formatUpdateSummary(&management.UpdateStatus{Phase: management.UpdatePhaseIdle})
		if !strings.Contains(s, "(unknown)") {
			t.Errorf("expected placeholder for empty versions: %q", s)
		}
	})
}

func TestRequestedChannel(t *testing.T) {
	for _, tc := range []struct {
		edge, stable bool
		want         string
	}{
		{false, false, ""},
		{true, false, "edge"},
		{false, true, "stable"},
	} {
		if got := requestedChannel(tc.edge, tc.stable); got != tc.want {
			t.Errorf("requestedChannel(%v,%v) = %q, want %q", tc.edge, tc.stable, got, tc.want)
		}
	}
}

func TestShouldStopUpToDate(t *testing.T) {
	upToDate := &management.UpdateStatus{Phase: management.UpdatePhaseIdle, Available: false}
	available := &management.UpdateStatus{Phase: management.UpdatePhaseAvailable, Available: true}
	errored := &management.UpdateStatus{Phase: management.UpdatePhaseError, Available: false}

	for _, tc := range []struct {
		name      string
		st        *management.UpdateStatus
		requested string
		host      string
		force     bool
		want      bool
	}{
		// Stable host the daemon confirms current: the only case we stop.
		{"stable host up-to-date stops", upToDate, "", "stable", false, true},
		{"unknown host up-to-date stops", upToDate, "", "", false, true},
		// Edge host always proceeds — the daemon can't rank edge builds.
		{"edge host proceeds even if daemon says up-to-date", upToDate, "", "edge", false, false},
		// Explicit channel request always proceeds (switch/refresh).
		{"explicit stable proceeds", upToDate, "stable", "stable", false, false},
		{"explicit edge proceeds", upToDate, "edge", "stable", false, false},
		// --force always proceeds.
		{"force proceeds", upToDate, "", "stable", true, false},
		// Available or unusable daemon answers proceed.
		{"available proceeds", available, "", "stable", false, false},
		{"daemon error proceeds", errored, "", "stable", false, false},
		{"nil status proceeds", nil, "", "stable", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldStopUpToDate(tc.st, tc.requested, tc.host, tc.force); got != tc.want {
				t.Errorf("shouldStopUpToDate = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseNotifyArg(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    bool
		wantErr bool
	}{
		{"on", true, false},
		{"off", false, false},
		{"ON", true, false},
		{" Off ", false, false},
		{"true", true, false},
		{"disable", false, false},
		{"enabled", true, false},
		{"", false, true},
		{"maybe", false, true},
	} {
		got, err := parseNotifyArg(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseNotifyArg(%q): expected error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseNotifyArg(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseNotifyArg(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
