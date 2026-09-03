package gateway

import (
	"testing"

	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// TestRequiredWindowFor is the contract between a /model id and the
// serving endpoint (waired#1031). Claude Code sized the session from the
// id string before the request existed, so this is what the router has
// to hold an endpoint to.
func TestRequiredWindowFor(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want int
		why  string
	}{
		{ModelWairedAuto, hostfit.ServingWindow200k,
			"the bare claude- id takes Claude Code's 200k default"},
		{ModelWairedAuto1M, hostfit.ServingWindow1M,
			"the [1m] suffix outranks everything, including the env var"},
		{ModelWairedLocal, 0,
			"pinning is how you reach a device that declares no window at all"},
		{ModelWairedCloud, 0,
			"never touches a Waired endpoint"},
		{ModelWairedAutoLegacy, 0,
			"a client still holding the old id is in an env-sized session; " +
				"holding its endpoint to a tier that session was never sized for " +
				"would refuse turns that used to work"},
		{"claude-sonnet-5", 0, "an ordinary model id promises nothing"},
		{"", 0, "no id, no promise"},
		{"CLAUDE-WAIRED-AUTO", hostfit.ServingWindow200k,
			"Claude Code reads the id case-insensitively, so this table does too"},
		{"claude-waired-auto[1M]", hostfit.ServingWindow1M,
			"so is the tier marker"},
		{"claude-waired-cloud", 0,
			"the bare spelling of the retired cloud id promises nothing either"},
	} {
		if got := RequiredWindowFor(tc.id); got != tc.want {
			t.Errorf("RequiredWindowFor(%q) = %d, want %d — %s", tc.id, got, tc.want, tc.why)
		}
	}
}

// TestRequiredWindowForRequest covers the half of the tier that survives the
// trip. Claude Code strips "[1m]" from the model id before sending and keeps
// the tier only in `anthropic-beta` (waired-agent#1036), so a 1M session
// arrives looking exactly like a 200k one unless the header is read.
func TestRequiredWindowForRequest(t *testing.T) {
	const beta = "context-1m-2025-08-07"
	for _, tc := range []struct {
		name string
		id   string
		hdr  []string
		want int
		why  string
	}{
		{"stripped 1M auto", ModelWairedAuto, []string{beta}, hostfit.ServingWindow1M,
			"this is what actually arrives when the user picks the 1M row"},
		{"200k auto", ModelWairedAuto, nil, hostfit.ServingWindow200k,
			"no header, no widening"},
		{"header among others", ModelWairedAuto,
			[]string{"oauth-2025-04-20," + beta}, hostfit.ServingWindow1M,
			"the header is a comma-separated list"},
		{"header split across lines", ModelWairedAuto,
			[]string{"oauth-2025-04-20", beta}, hostfit.ServingWindow1M,
			"and it can arrive as repeated header lines"},
		{"unrelated beta", ModelWairedAuto, []string{"fine-grained-tool-streaming-2025-05-14"},
			hostfit.ServingWindow200k, "an unrelated flag must not widen the demand"},
		{"id still carries the marker", ModelWairedAuto1M, nil, hostfit.ServingWindow1M,
			"a client that did not strip it is answered from the id"},
		{"node-naming id", ModelWairedPeer, []string{beta}, 0,
			"naming a node must not also make demands of it, header or no header"},
		{"local pin", ModelWairedLocal, []string{beta}, 0,
			"same reason: the pin is how you reach a device that declares nothing"},
		{"ordinary id", "claude-sonnet-5", []string{beta}, 0,
			"a real Anthropic model never reaches a Waired endpoint on this path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RequiredWindowForRequest(tc.id, tc.hdr); got != tc.want {
				t.Errorf("RequiredWindowForRequest(%q, %v) = %d, want %d — %s",
					tc.id, tc.hdr, got, tc.want, tc.why)
			}
		})
	}
}

// TestAnthropicModelList_AdvertisesEveryTier keeps the advertisement and
// the routing table from drifting: an id the picker offers but the
// router does not understand is a dead entry in somebody's /model list,
// and an id the router understands but nobody advertises is unreachable.
func TestAnthropicModelList_AdvertisesEveryTier(t *testing.T) {
	h := &HandlerSet{deps: Deps{
		ClaudeModelDirectives: true,
		ListManifests:         asManifestList(nil),
	}}
	got := map[string]bool{}
	for _, m := range h.anthropicModelList() {
		got[m.ID] = true
	}
	// ModelWairedAuto1M is deliberately absent: the 1M row was reachable only
	// because the auto route could carry the turn to the real Anthropic API,
	// and it moved to RoutedDirectiveModels with that crossing
	// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md).
	for _, id := range []string{ModelWairedAuto, ModelWairedLocal} {
		if !got[id] {
			t.Errorf("%q is not advertised; the picker will never offer it", id)
		}
	}
	// Routed, not offered. Every id in this list is one a client can still
	// carry into a session — the picker cache has no TTL — so the router has
	// to understand it while no surface hands it out.
	for _, m := range RoutedDirectiveModels() {
		if got[m.ID] {
			t.Errorf("%q is still advertised; it is routed for compatibility, not offered", m.ID)
		}
	}
}
