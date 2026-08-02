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
	} {
		if got := RequiredWindowFor(tc.id); got != tc.want {
			t.Errorf("RequiredWindowFor(%q) = %d, want %d — %s", tc.id, got, tc.want, tc.why)
		}
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
	for _, id := range []string{ModelWairedAuto, ModelWairedAuto1M, ModelWairedLocal, ModelWairedCloud} {
		if !got[id] {
			t.Errorf("%q is not advertised; the picker will never offer it", id)
		}
	}
	if got[ModelWairedAutoLegacy] {
		t.Errorf("%q is still advertised; it is routed for compatibility, not offered",
			ModelWairedAutoLegacy)
	}
}
