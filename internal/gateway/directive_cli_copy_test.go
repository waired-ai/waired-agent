package gateway

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
)

// TestDirectiveTablesMatchTheCLICopy guards the third hand-duplicated copy of
// the directive table.
//
// This package is the anchor. The intercept's copy
// (internal/proxy/intercept/models_directives.go) has been pinned to it since
// waired-agent#830 by TestDirectiveIdsInSyncWithGateway; the CLI's copy
// (internal/integration/claudecode/directives.go) has not been, even though
// its own doc comment has named this test since #1185 — **the test did not
// exist**. The ids were reachable through TestRouteDecisionMatchesTheCLICopy,
// which asks whether each id still ROUTES; nothing asked whether the two
// tables still say the same thing, and the labels and descriptions were
// covered by neither.
//
// It matters more since waired-agent#1306: the CLI copy is what the Local
// Gateway's /v1/models listing renders through internal/integration/modelrows,
// so a drifted label now reaches OpenCode's and OpenClaw's pickers as well as
// Claude Code's — and docs-site quotes all of them verbatim.
//
// Driven from the two tables rather than a hand-written list, for the reason
// the intercept's twin records: a list that has to be edited to cover a new
// entry does not guard the entry it was not edited for.
func TestDirectiveTablesMatchTheCLICopy(t *testing.T) {
	ours, theirs := DirectiveModels(), claudecode.DirectiveModels()
	if len(ours) != len(theirs) {
		t.Fatalf("directive count drift: gateway advertises %d, the CLI copy has %d\ngateway=%v\ncli=%v",
			len(ours), len(theirs), ours, theirs)
	}
	// Order too: it is the order the rows appear in every picker, and the
	// surfaces must not present the same set differently.
	for i := range ours {
		if ours[i].ID != theirs[i].ID {
			t.Errorf("id drift at %d: gateway %q != CLI %q", i, ours[i].ID, theirs[i].ID)
			continue
		}
		if ours[i].DisplayName != theirs[i].DisplayName {
			t.Errorf("display name drift for %q: gateway %q != CLI %q",
				ours[i].ID, ours[i].DisplayName, theirs[i].DisplayName)
		}
		if ours[i].Description != theirs[i].Description {
			t.Errorf("description drift for %q: gateway %q != CLI %q",
				ours[i].ID, ours[i].Description, theirs[i].Description)
		}
	}
}

// The constants the two copies are built from, and the spelling of a per-peer
// id, have to agree as well — the CLI generates those ids and this package
// decodes them, so a drift here is a row that resolves to nothing.
func TestDirectiveConstantsMatchTheCLICopy(t *testing.T) {
	for _, tc := range []struct{ name, ours, theirs string }{
		{"any", ModelWairedAny, claudecode.DirectiveModelAny},
		{"local", ModelWairedLocal, claudecode.DirectiveModelLocal},
		{"peer", ModelWairedPeer, claudecode.DirectiveModelPeer},
		{"public", ModelWairedPublic, claudecode.DirectiveModelPublic},
		{"per-peer prefix", ModelWairedPeerPrefix, claudecode.PeerDirectivePrefix},
		{"legacy any", ModelWairedAnyLegacy, claudecode.LegacyModelAuto},
		{"legacy any (oldest)", ModelWairedAnyOldest, claudecode.LegacyModelAutoLegacy},
		{"legacy local", ModelWairedLocalLegacy, claudecode.LegacyModelLocal},
		{"legacy peer", ModelWairedPeerLegacy, claudecode.LegacyModelPeer},
		{"legacy public", ModelWairedPublicLegacy, claudecode.LegacyModelPublic},
		{"legacy per-peer prefix", ModelWairedPeerPrefixLegacy, claudecode.LegacyPeerDirectivePrefix},
		{"legacy cloud", ModelWairedCloud, claudecode.LegacyModelCloud},
		{"1M marker", tierMarker1M, claudecode.TierMarker1M},
	} {
		if tc.ours != tc.theirs {
			t.Errorf("%s drift: gateway %q != CLI %q", tc.name, tc.ours, tc.theirs)
		}
	}
	// And a per-peer id the CLI generates has to decode here.
	id := claudecode.PeerDirectiveID("linux-gpu")
	if got := NodeDirectiveFor(id); got != id {
		t.Errorf("NodeDirectiveFor(%q) = %q — a per-peer id the CLI generates does not decode", id, got)
	}
}
