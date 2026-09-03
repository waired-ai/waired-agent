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
const tierMarker1M = "[1m]"

// wairedIDMarker is the substring every waired-owned model id carries.
const wairedIDMarker = "waired"

// directiveModelAutoLegacy is the pre-waired#1031 spelling of the any-node id.
// No surface offers it; the intercept still routes it, so the status line
// still has to be able to describe a session that carries it.
const directiveModelAutoLegacy = "anthropic-waired-auto"

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
	case DirectiveModelLocal, DirectiveModelPeer, DirectiveModelPublic:
		return RouteWaired, true
	case DirectiveModelAuto, directiveModelAutoLegacy:
		return RouteWaired, true
	case NormalizeModelID(DirectiveModelCloud):
		return RouteAnthropic, true
	}
	if strings.HasPrefix(bare, PeerDirectivePrefix) && len(bare) > len(PeerDirectivePrefix) {
		return RouteWaired, true
	}
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
