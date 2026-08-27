package claudecode

import "strings"

// Route values, spelled the way state.ClaudeRoutingPolicy spells them. They are
// literals here for the same reason the directive ids are: the `waired` CLI's
// display path should not pull in the runtime state package's dependencies for
// three strings. modelroute_test.go asserts they equal the state constants.
const (
	RouteAuto      = "auto"
	RouteWaired    = "waired"
	RouteAnthropic = "anthropic"
)

// tierMarker1M is the suffix Claude Code sizes a session from, and strips from
// the id before sending. It is matched case-insensitively and anywhere in the
// id, which is how Claude Code itself reads it.
const tierMarker1M = "[1m]"

// wairedIDMarker is the substring every waired-owned model id carries.
const wairedIDMarker = "waired"

// directiveModelAutoLegacy is the pre-waired#1031 spelling of the auto id. No
// surface offers it; the intercept still routes it, so the status line still
// has to be able to describe a session that carries it.
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

// RouteForModelID reports the route a model id forces on the session that
// carries it, and whether it forces one at all. It mirrors the intercept's
// directiveRoute (internal/proxy/intercept/model_rewrite.go) — the routing
// decision itself is made there; this is the copy the CLI reads to DESCRIBE
// that decision, which is why modelroute_test.go pins the two together.
//
// It exists because the status line answers a per-session question. The route
// waired persists is machine-wide, but a /model pick lives inside one Claude
// Code session, and the two can honestly disagree: the footer of a session that
// picked Opus has to say Anthropic even while the machine's setting says
// otherwise, and a second session running alongside it has to say something
// else. Reading the id Claude Code hands the status line on stdin is what keeps
// each footer about its own session (waired-agent#1037).
func RouteForModelID(id string) (route string, forced bool) {
	bare := NormalizeModelID(id)
	switch bare {
	case DirectiveModelLocal, DirectiveModelPeer, DirectiveModelPublic:
		return RouteWaired, true
	case DirectiveModelAuto, directiveModelAutoLegacy:
		return RouteAuto, true
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
