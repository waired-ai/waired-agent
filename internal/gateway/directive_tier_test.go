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
		{ModelWairedAny, hostfit.ServingWindow200k,
			"the any-node row is the one place waired is choosing, so it " +
				"promises a floor"},
		{Tier1M(ModelWairedAny), hostfit.ServingWindow1M,
			"the [1m] suffix outranks everything, including the env var"},
		{ModelWairedLocal, 0,
			"pinning is how you reach a device that declares no window at all"},
		{ModelWairedPeer, 0,
			"naming a node must not also make demands of it"},
		{Tier1M(ModelWairedLocal), hostfit.ServingWindow1M,
			"a 1M twin IS the demand — waired-agent#1185 gives every row that " +
				"can serve the tier one, and it is only ever offered where a " +
				"node declares the window"},
		{Tier1M(ModelWairedPeer), hostfit.ServingWindow1M,
			"same for the peer row's twin"},
		{ModelWairedCloud, 0,
			"never touches a Waired endpoint"},
		{ModelWairedAnyLegacy, hostfit.ServingWindow200k,
			"the pre-#1185 spelling of the any-node row means the same row"},
		{ModelWairedLocalLegacy, 0,
			"and the pre-#1185 named rows still promise nothing"},
		{"claude-sonnet-5", 0, "an ordinary model id promises nothing"},
		{"", 0, "no id, no promise"},
		{"CLAUDE-WAIRED-AUTO", hostfit.ServingWindow200k,
			"Claude Code reads the id case-insensitively, so this table does too"},
		{"WAIRED/LOCAL[1M]", hostfit.ServingWindow1M,
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
		{"stripped 1M auto", ModelWairedAny, []string{beta}, hostfit.ServingWindow1M,
			"this is what actually arrives when the user picks the 1M row"},
		{"200k auto", ModelWairedAny, nil, hostfit.ServingWindow200k,
			"no header, no widening"},
		{"header among others", ModelWairedAny,
			[]string{"oauth-2025-04-20," + beta}, hostfit.ServingWindow1M,
			"the header is a comma-separated list"},
		{"header split across lines", ModelWairedAny,
			[]string{"oauth-2025-04-20", beta}, hostfit.ServingWindow1M,
			"and it can arrive as repeated header lines"},
		{"unrelated beta", ModelWairedAny, []string{"fine-grained-tool-streaming-2025-05-14"},
			hostfit.ServingWindow200k, "an unrelated flag must not widen the demand"},
		{"id still carries the marker", Tier1M(ModelWairedAny), nil, hostfit.ServingWindow1M,
			"a client that did not strip it is answered from the id"},
		// waired-agent#1185 changed these two. The header is not something a
		// client sends for a BARE row — Claude Code sends it because the id
		// it holds carried "[1m]", and strips the suffix on the way out — so
		// the header arriving on a node-naming id means the operator picked
		// that node's 1M twin. Serving it at 200k instead would be the
		// surprise; the twin is only ever offered where the node declares
		// the window (owner ruling 2026-09-06).
		{"node-naming id with the tier header", ModelWairedPeer, []string{beta},
			hostfit.ServingWindow1M,
			"this is what arrives when the operator picks Waired peer (1M context)"},
		{"local pin with the tier header", ModelWairedLocal, []string{beta},
			hostfit.ServingWindow1M,
			"same, for this computer's own 1M twin"},
		{"node-naming id without it", ModelWairedPeer, nil, 0,
			"the bare row still demands nothing: it is how you reach a device " +
				"that declares no window at all"},
		{"pre-#1185 named row with the tier header", ModelWairedPeerLegacy,
			[]string{beta}, hostfit.ServingWindow1M,
			"a session that picked a twin before the re-spelling carries the " +
				"old id and the same header"},
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
	// Tier1M(ModelWairedAny) is deliberately absent: the 1M row was reachable only
	// because the auto route could carry the turn to the real Anthropic API,
	// and it moved to RoutedDirectiveModels with that crossing
	// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md).
	for _, id := range []string{ModelWairedAny, ModelWairedLocal} {
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
