package main

import (
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// TestClaudeSubDisplay locks the subagent display line, in particular the
// readable framing when Sub == "same" (it spells out the effective route
// without the old nested-paren "auto(prefer…" wart).
func TestClaudeSubDisplay(t *testing.T) {
	cases := []struct {
		name string
		pol  state.ClaudeRoutingPolicy
		want string
	}{
		{
			name: "same follows auto main",
			pol:  state.ClaudeRoutingPolicy{Main: state.ClaudeRouteAuto, Sub: state.ClaudeRouteSame},
			want: "same as main  (auto — prefer Waired; visible fallback to Anthropic on failure)",
		},
		{
			name: "empty sub is treated as same",
			pol:  state.ClaudeRoutingPolicy{Main: state.ClaudeRouteWaired, Sub: ""},
			want: "same as main  (waired — Waired only; never contacts Anthropic)",
		},
		{
			name: "explicit anthropic sub",
			pol:  state.ClaudeRoutingPolicy{Main: state.ClaudeRouteAuto, Sub: state.ClaudeRouteAnthropic},
			want: "anthropic  (always the real Anthropic API)",
		},
		{
			name: "explicit waired sub",
			pol:  state.ClaudeRoutingPolicy{Main: state.ClaudeRouteAnthropic, Sub: state.ClaudeRouteWaired},
			want: "waired  (Waired only; never contacts Anthropic)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := claudeSubDisplay(tc.pol)
			if got != tc.want {
				t.Fatalf("claudeSubDisplay(%+v) = %q, want %q", tc.pol, got, tc.want)
			}
			if strings.Contains(got, "((") || strings.Contains(got, ")(") {
				t.Fatalf("nested/garbled parens in %q", got)
			}
		})
	}
}

// TestClaudeRouteHint keeps the parenthesized main-line hint and the bare-text
// form in sync (the former just wraps the latter).
func TestClaudeRouteHint(t *testing.T) {
	for _, r := range []state.ClaudeRouteClass{state.ClaudeRouteAuto, state.ClaudeRouteWaired, state.ClaudeRouteAnthropic} {
		hint := claudeRouteHint(r)
		text := claudeRouteHintText(r)
		if want := "  (" + text + ")"; hint != want {
			t.Fatalf("claudeRouteHint(%q) = %q, want %q", r, hint, want)
		}
		if strings.HasPrefix(text, "(") || strings.HasSuffix(text, ")") {
			t.Fatalf("claudeRouteHintText(%q) should be bare, got %q", r, text)
		}
	}
}

// TestClaudeServedDisplay locks the `last served:` value for each combination
// of the fields behind it. The peer branch carries the weight: until #755 the
// gateway only reported the serving model for a remapped id, so a mesh-served
// turn left both fields empty and this rendering had never run against a real
// state. Record of today's behaviour.
func TestClaudeServedDisplay(t *testing.T) {
	served := time.Date(2026, 8, 13, 10, 4, 11, 0, time.UTC)
	stamp := served.Local().Format(time.RFC3339)
	cases := []struct {
		name string
		st   management.ClaudeRoutingState
		want string
	}{
		{
			name: "this device",
			st:   management.ClaudeRoutingState{LastLocalModel: "qwen3.5-2b", LastServedAt: served},
			want: stamp + " — qwen3.5-2b (this device)",
		},
		{
			name: "peer",
			st:   management.ClaudeRoutingState{LastLocalModel: "qwen3.6-27b", LastServedBy: "shared-7", LastServedAt: served},
			want: stamp + " — qwen3.6-27b (peer shared-7)",
		},
		{
			name: "peer without a model",
			st:   management.ClaudeRoutingState{LastServedBy: "shared-7", LastServedAt: served},
			want: stamp + " — peer shared-7",
		},
		{
			// An agent predating LastServedAt reports no time; the line
			// falls back to the form it had before #755 rather than
			// printing a zero date.
			name: "no time reported",
			st:   management.ClaudeRoutingState{LastLocalModel: "qwen3.5-2b"},
			want: "qwen3.5-2b (this device)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := claudeServedDisplay(tc.st); got != tc.want {
				t.Fatalf("claudeServedDisplay(%+v) = %q, want %q", tc.st, got, tc.want)
			}
		})
	}
}
