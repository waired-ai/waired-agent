package tray

import (
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
)

// waired#809: a degraded *connected* state collapses to a single
// "⚠ <cause>" top-level header so the status row stays an at-a-glance
// health summary while per-subsystem detail moves into submenus.

func TestSummariseAggregateHeader(t *testing.T) {
	tests := []struct {
		name       string
		in         MenuModel
		wantHeader string
		wantReason string
	}{
		{
			name:       "healthy connected is left untouched",
			in:         MenuModel{Kind: MenuConnected, Icon: IconConnected, HeaderTitle: "● Connected"},
			wantHeader: "● Connected",
			wantReason: "",
		},
		{
			name:       "claude inactive names the cause",
			in:         MenuModel{Kind: MenuConnected, Icon: IconDegraded, HeaderTitle: "● Connected", ClaudeHeader: "Claude integration: ○ inactive (agent-stopped)"},
			wantHeader: "⚠ Claude Code routing inactive",
			wantReason: "Claude Code routing inactive",
		},
		{
			name:       "opencode stale names the cause",
			in:         MenuModel{Kind: MenuConnected, Icon: IconDegraded, HeaderTitle: "● Connected", OpenCodeHeader: "OpenCode integration: ⚠ stale (/x)"},
			wantHeader: "⚠ OpenCode integration needs attention",
			wantReason: "OpenCode integration needs attention",
		},
		{
			name:       "recent fallback names the cause",
			in:         MenuModel{Kind: MenuConnected, Icon: IconDegraded, HeaderTitle: "● Connected", HasRecentFallbackBadge: true},
			wantHeader: "⚠ Inference fell back recently",
			wantReason: "Inference fell back recently",
		},
		{
			name:       "claude wins over a coincident fallback (precedence)",
			in:         MenuModel{Kind: MenuConnected, Icon: IconDegraded, HeaderTitle: "● Connected", ClaudeHeader: "Claude integration: ○ inactive (x)", HasRecentFallbackBadge: true},
			wantHeader: "⚠ Claude Code routing inactive",
			wantReason: "Claude Code routing inactive",
		},
		{
			name:       "degraded but not connected (disconnected) is left untouched",
			in:         MenuModel{Kind: MenuDisconnected, Icon: IconDegraded, HeaderTitle: "○ Disconnected", HasRecentFallbackBadge: true},
			wantHeader: "○ Disconnected",
			wantReason: "",
		},
		{
			name:       "error header is left untouched",
			in:         MenuModel{Kind: MenuError, Icon: IconError, HeaderTitle: "⚠ Tunnel error"},
			wantHeader: "⚠ Tunnel error",
			wantReason: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.in
			summariseAggregateHeader(&m)
			if m.HeaderTitle != tc.wantHeader {
				t.Errorf("HeaderTitle=%q, want %q", m.HeaderTitle, tc.wantHeader)
			}
			if m.DegradedReason != tc.wantReason {
				t.Errorf("DegradedReason=%q, want %q", m.DegradedReason, tc.wantReason)
			}
		})
	}
}

// End-to-end through Update: a recent fallback on an otherwise-connected
// menu must both promote the icon (existing behaviour) AND rewrite the
// header to name the cause (waired#809).
func TestUpdate_AggregateHeader_FallbackRewritesHeader(t *testing.T) {
	now := time.Date(2026, 5, 16, 10, 0, 0, 0, time.UTC)
	fbs := []FallbackEntry{{
		TS: now.Add(-1 * time.Minute), From: "a", To: "b", Reason: "engine_not_ready",
	}}
	got := Update(connectedSnapshot(now, fbs, &management.ObservabilityState{}))
	if got.Icon != IconDegraded {
		t.Fatalf("Icon=%d, want IconDegraded", got.Icon)
	}
	if got.HeaderTitle != "⚠ Inference fell back recently" {
		t.Errorf("HeaderTitle=%q, want the aggregated degraded cause", got.HeaderTitle)
	}
}

// A plain healthy connected menu keeps the "● Connected" header (no
// spurious degraded summary).
func TestUpdate_AggregateHeader_HealthyUnchanged(t *testing.T) {
	now := time.Date(2026, 5, 16, 10, 0, 0, 0, time.UTC)
	got := Update(connectedSnapshot(now, nil, &management.ObservabilityState{}))
	if got.HeaderTitle != "● Connected" {
		t.Errorf("HeaderTitle=%q, want ● Connected", got.HeaderTitle)
	}
	if got.DegradedReason != "" {
		t.Errorf("DegradedReason=%q, want empty on a healthy menu", got.DegradedReason)
	}
}
