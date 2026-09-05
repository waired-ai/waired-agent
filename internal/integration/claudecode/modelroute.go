package claudecode

import "strings"

// The two sides a turn can run on, spelled the way the daemon publishes them
// as last_request_route. They are literals here for the same reason the
// directive ids are: the `waired` CLI's display path should not pull in the
// runtime state package's dependencies for two strings.
const (
	RouteWaired    = "waired"
	RouteAnthropic = "anthropic"
)

// tierMarker1M is the suffix Claude Code sizes a session from, and strips from
// the id before sending. It is matched case-insensitively and anywhere in the
// id, which is how Claude Code itself reads it.
const tierMarker1M = TierMarker1M

// wairedIDMarker is the substring every waired-owned model id carries.
const wairedIDMarker = "waired"

// NormalizeModelID reduces a model id to the form the tables are keyed by:
// lower-cased, with every tier marker removed. Advertised ids keep their
// spelling — Claude Code needs "[1m]" in the id to size the session — so only
// the lookup is on the bare form (waired-agent#1036).
func NormalizeModelID(id string) string {
	bare := strings.ToLower(strings.TrimSpace(id))
	for {
		i := strings.Index(bare, tierMarker1M)
		if i < 0 {
			return bare
		}
		bare = bare[:i] + bare[i+len(tierMarker1M):]
	}
}

// RouteForModelID reports the side a model id names, and whether it names one
// at all. It mirrors the intercept's directiveRoute
// (internal/proxy/intercept/model_rewrite.go) — the decision itself is made
// there; this is the copy the CLI reads to DESCRIBE it, which is why
// modelroute_test.go pins the two together.
//
// It exists because the status line answers a per-session question, and the id
// is the only per-session input it gets: Claude Code hands it in on stdin, per
// render, so two sessions on one computer can honestly say different things
// (waired-agent#1037). ok=false means the id names neither side, which the
// intercept serves on Waired — the caller decides how to say that.
func RouteForModelID(id string) (route string, forced bool) {
	bare := NormalizeModelID(id)
	switch bare {
	case DirectiveModelAny, DirectiveModelLocal, DirectiveModelPeer, DirectiveModelPublic:
		return RouteWaired, true
	// The pre-#1185 spellings. NormalizeModelID has already dropped the tier
	// marker, so one case covers the bare and "[1m]" forms of each.
	case LegacyModelAuto, LegacyModelAutoLegacy, LegacyModelLocal,
		LegacyModelPeer, LegacyModelPublic:
		return RouteWaired, true
	}
	for _, prefix := range []string{PeerDirectivePrefix, LegacyPeerDirectivePrefix} {
		if strings.HasPrefix(bare, prefix) && len(bare) > len(prefix) {
			return RouteWaired, true
		}
	}
	// The retired cloud id (LegacyModelCloud) deliberately falls through to
	// "names neither side": waired-agent#1186 stopped relaying it, because
	// relaying meant rewriting the body's model to some other id. It is
	// answered on this machine now, and the answer names the fix.
	//
	// A model the real Anthropic API serves names where it runs.
	if strings.HasPrefix(bare, "claude-") && !strings.Contains(bare, wairedIDMarker) {
		return RouteAnthropic, true
	}
	return "", false
}

// IsWairedModelID reports whether the id is one of waired's own — the
// "waired/" subagent label or any spelling of a directive id. A real Anthropic
// model will not contain the marker; a future waired id will.
func IsWairedModelID(id string) bool {
	bare := NormalizeModelID(id)
	return strings.HasPrefix(bare, "waired/") || strings.Contains(bare, wairedIDMarker)
}
