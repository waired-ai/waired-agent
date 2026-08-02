package gateway

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
)

// TestDirectiveModelsMatchPickerCache guards the hand-duplicated directive
// table: claudecode.DirectiveModels is what the agent writes straight into
// Claude Code's /model picker cache (#407), and this — the gateway's own
// /v1/models advertisement — is what it is a copy of. The literals are
// duplicated so the `waired` CLI does not link this package (and the router,
// and the inference stack) for three strings, the same trade
// internal/proxy/intercept makes; nothing but this test stops them drifting.
//
// It asserts against the ADVERTISED response rather than the constants, so it
// also covers the display names, which live inline in anthropicModelList and
// are user-visible copy in the picker. Drift would put a label or an id in the
// picker that the gateway does not serve — and it would be invisible, because
// under subscription OAuth discovery never runs to contradict the cache.
func TestDirectiveModelsMatchPickerCache(t *testing.T) {
	advertised := discoveryModels(t, true)
	want := claudecode.DirectiveCacheModels()
	if len(want) == 0 {
		t.Fatal("claudecode.DirectiveCacheModels is empty — the picker cache would be unwritable")
	}
	for _, w := range want {
		got, ok := advertised[w.ID]
		if !ok {
			t.Errorf("picker cache offers id %q that discovery does not advertise", w.ID)
			continue
		}
		if got.DisplayName != w.DisplayName {
			t.Errorf("display name drift for %q: gateway %q != picker cache %q",
				w.ID, got.DisplayName, w.DisplayName)
		}
	}
	// And the other direction: every directive id the gateway advertises must
	// be offered by the cache, or the picker silently loses one.
	inCache := map[string]bool{}
	for _, w := range want {
		inCache[w.ID] = true
	}
	for _, id := range []string{ModelWairedAuto, ModelWairedLocal, ModelWairedCloud} {
		if !inCache[id] {
			t.Errorf("gateway advertises directive id %q that the picker cache omits", id)
		}
	}
}
