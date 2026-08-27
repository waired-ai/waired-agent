package claudecode

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// TestRouteSpellingsMatchTheState pins the three literals against the values
// the routing policy is actually persisted and compared as. They are literals
// here so the CLI's display path does not link the runtime state package's
// dependencies; that trade is only safe while this test holds.
func TestRouteSpellingsMatchTheState(t *testing.T) {
	for _, tc := range []struct{ ours, theirs string }{
		{RouteAuto, string(state.ClaudeRouteAuto)},
		{RouteWaired, string(state.ClaudeRouteWaired)},
		{RouteAnthropic, string(state.ClaudeRouteAnthropic)},
	} {
		if tc.ours != tc.theirs {
			t.Errorf("route spelling drift: %q != %q", tc.ours, tc.theirs)
		}
	}
}

// TestRouteForModelID covers what the status line has to describe. The bare
// spellings are the ones that actually arrive: Claude Code strips "[1m]"
// before sending (waired-agent#1036).
func TestRouteForModelID(t *testing.T) {
	for _, tc := range []struct {
		id    string
		route string
		ok    bool
		why   string
	}{
		{DirectiveModelAuto, RouteAuto, true, "the auto row"},
		{DirectiveModelAuto1M, RouteAuto, true, "same route, different tier"},
		{"claude-waired-auto", RouteAuto, true, "what the 1M row sends once the marker is stripped"},
		{DirectiveModelLocal, RouteWaired, true, "this device only"},
		{DirectiveModelPeer, RouteWaired, true, "another device is still a Waired node"},
		{DirectiveModelPublic, RouteWaired, true, "so is someone else's"},
		{"claude-waired-peer-linux-gpu", RouteWaired, true, "and a named one"},
		{DirectiveModelCloud, RouteAnthropic, true, "the retired cloud row is still routed"},
		{"claude-waired-cloud", RouteAnthropic, true, "and its bare spelling is what arrives"},
		{"claude-opus-5", RouteAnthropic, true, "naming a model names where it runs"},
		{"claude-fable-5", RouteAnthropic, true, "whichever model it is"},
		{"waired/subagent", "", false, "the subagent label forces nothing; the policy decides"},
		{"gpt-4o", "", false, "not a Claude Code pick at all"},
		{"", "", false, "no id, no claim"},
	} {
		gotRoute, gotOK := RouteForModelID(tc.id)
		if gotRoute != tc.route || gotOK != tc.ok {
			t.Errorf("RouteForModelID(%q) = (%q,%v), want (%q,%v) — %s",
				tc.id, gotRoute, gotOK, tc.route, tc.ok, tc.why)
		}
	}
}
