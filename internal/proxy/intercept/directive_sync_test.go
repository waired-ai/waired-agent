package intercept

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/gateway"
	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
)

// TestDirectiveIdsInSyncWithGateway guards the hand-duplicated directive id
// literals: the intercept honours (model_rewrite.go) and advertises-on-passthrough
// (models_directives.go) exactly the ids the gateway advertises on the
// local-serving path (internal/gateway/anthropic_models.go). They are duplicated
// — not shared — to keep this fail-open package stdlib-only, so nothing but this
// test stops them silently drifting. Drift would make the picker show an id the
// intercept can no longer force a route for (local) or rewrite for upstream
// (cloud), with no other test catching it.
// Driven from the two tables rather than from a hand-written id list. The
// old form named three ids explicitly, so wairedAuto1MModel drifted out of
// this package's splice entirely — the 1M tier was unreachable from /model
// on the anthropic route — while this test stayed green (waired-agent#830).
// A list that has to be edited to cover a new entry does not guard the entry
// it was not edited for.
func TestDirectiveIdsInSyncWithGateway(t *testing.T) {
	ours, theirs := directiveModels(), gateway.DirectiveModels()
	if len(ours) != len(theirs) {
		t.Fatalf("directive count drift: intercept advertises %d, gateway advertises %d\nintercept=%v\ngateway=%v",
			len(ours), len(theirs), ours, theirs)
	}
	// Order too: it is the order the entries appear in the /model picker,
	// and the two surfaces must not present the same set differently.
	for i := range ours {
		if ours[i].id != theirs[i].ID {
			t.Errorf("directive id drift at %d: intercept %q != gateway %q", i, ours[i].id, theirs[i].ID)
			continue
		}
		// Display names are user-visible copy that docs-site quotes
		// verbatim, not decoration — the auto label had already lost its
		// "— 200k" tier here while the ids still matched.
		if ours[i].display != theirs[i].DisplayName {
			t.Errorf("display name drift for %q: intercept %q != gateway %q",
				ours[i].id, ours[i].display, theirs[i].DisplayName)
		}
	}
}

// The splice and the single-object synthesiser must cover the same table:
// directiveDisplayName returning "" is how GET /v1/models/<id> ends up
// synthesising an object with an empty display_name.
func TestDirectiveDisplayNameCoversEveryAdvertisedId(t *testing.T) {
	for _, e := range directiveModelEntries() {
		if got := directiveDisplayName(e.id); got == "" {
			t.Errorf("advertised id %q has no display name for the single-object path", e.id)
		} else if got != e.obj["display_name"] {
			t.Errorf("display name drift for %q: splice %q != single-object %q",
				e.id, e.obj["display_name"], got)
		}
	}
}

// Every advertised id must survive Claude Code's ^(claude|anthropic) filter
// on the discovery fetch, and must be one the intercept can actually route.
func TestAdvertisedDirectivesArePickerSafeAndRoutable(t *testing.T) {
	for _, m := range directiveModels() {
		if !strings.HasPrefix(m.id, "claude") && !strings.HasPrefix(m.id, "anthropic") {
			t.Errorf("advertised id %q cannot survive Claude Code's picker filter", m.id)
		}
		if _, ok := directiveRoute(m.id); !ok {
			t.Errorf("advertised id %q has no route — the picker would offer an entry the intercept ignores", m.id)
		}
	}
}

// TestRouteDecisionMatchesTheCLICopy pins the third hand-copy of the routing
// rule. internal/integration/claudecode carries it so the `waired` CLI can
// DESCRIBE, in the status line, the route a session's /model pick forces —
// without linking this package or the gateway. The decision itself is made
// here; a footer that disagrees with the router is exactly the surface defect
// this lane removes, so the two are compared over the ids that matter,
// including the bare spellings Claude Code actually sends.
func TestRouteDecisionMatchesTheCLICopy(t *testing.T) {
	ids := []string{
		wairedAutoModel, wairedAuto1MModel, wairedAutoLegacyModel,
		wairedLocalModel, wairedPeerModel, wairedPublicModel,
		wairedCloudModel, wairedCloudBareModel,
		"claude-waired-peer-linux-gpu", "CLAUDE-WAIRED-AUTO",
		"claude-opus-5", "claude-opus-4-8[1m]", "claude-fable-5", "claude-haiku-4-5-20251001",
		"waired/subagent", "waired/default", "gpt-4o", "",
	}
	for _, id := range ids {
		wantRoute, wantOK := directiveRoute(id)
		gotRoute, gotOK := claudecode.RouteForModelID(id)
		if gotRoute != wantRoute || gotOK != wantOK {
			t.Errorf("claudecode.RouteForModelID(%q) = (%q,%v), intercept says (%q,%v)",
				id, gotRoute, gotOK, wantRoute, wantOK)
		}
		if got, want := claudecode.IsWairedModelID(id), isWairedOwnedID(id); got != want {
			t.Errorf("claudecode.IsWairedModelID(%q) = %v, intercept says %v", id, got, want)
		}
	}
}
