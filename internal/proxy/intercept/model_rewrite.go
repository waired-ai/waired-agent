package intercept

import (
	"encoding/json"
	"strings"
)

// The reserved /model ids (#52). Selected in Claude Code's /model picker they
// say where the turn runs: `waired` names any Waired node the mesh offers,
// and the rest name one. The literals are duplicated here to keep this
// package stdlib-only — keep them in sync with
// gateway.ModelWaired* (internal/gateway/anthropic_models.go), which carries
// the reasoning; directive_sync_test.go asserts the two agree.
const (
	wairedAnyModel    = "waired"
	wairedLocalModel  = "waired/local"
	wairedPeerModel   = "waired/peer"
	wairedPublicModel = "waired/public"
	// wairedPeerPinPrefix heads the per-peer ids generated from the live mesh
	// (waired-agent#830). This package never sees a mesh — it is stdlib-only
	// and fail-open by contract — so it recognises the family by prefix
	// rather than by lookup. The route is the same as the bare peer id's;
	// which peer is resolved a layer down, against the snapshot that produced
	// the id.
	wairedPeerPinPrefix = wairedPeerModel + "-"
)

// The pre-waired-agent#1185 spellings. No surface offers them; they are still
// routed, because a Claude Code that selected one keeps the id in its own
// settings until the operator picks again, and `~/.claude/settings.json` can
// carry one as a default model an older waired wrote there.
const (
	// legacyAutoModel was the any-node row. It is spelled "auto" for a route
	// that no longer exists — Waired first, then the real Anthropic API —
	// which waired-agent#1184 removed. What it means now is what `waired`
	// means: Waired, fail-closed.
	legacyAutoModel = "claude-waired-auto"
	// legacyAutoOldestModel is the pre-waired#1031 spelling of the same id.
	legacyAutoOldestModel = "anthropic-waired-auto"
	// legacyAuto1MModel normalises onto legacyAutoModel, so routing already
	// answers for it; this spelling exists so /v1/models/{id} can name it.
	legacyAuto1MModel = "claude-waired-auto[1m]"
	legacyLocalModel  = "anthropic-waired-local"
	legacyPeerModel   = "claude-waired-peer"
	legacyPeerPinPre  = legacyPeerModel + "-"
	legacyPublicModel = "claude-waired-public"
	// legacyCloudModel pinned the turn to the real Anthropic API. It keeps
	// the "[1m]" spelling because that spelling is what sized the session:
	// Claude Code reads the tier off the id string it holds.
	//
	// It stopped being offered when a real Anthropic id in /model started
	// meaning the real Anthropic API on its own (waired-agent#1037), and it
	// stopped being ROUTED there in waired-agent#1186: relaying it meant
	// rewriting the body's model to some other id, which is the one place
	// waired put a model on the wire that the user never typed. A session
	// still carrying it now gets a 4xx that names the fix.
	legacyCloudModel = "claude-waired-cloud[1m]"
	// legacyCloudBareModel is legacyCloudModel as it actually arrives. Claude
	// Code strips "[1m]" before the request leaves it (measured on 2.1.229 /
	// 2.1.241 / 2.1.245, waired-agent#1036), so the spelled form never
	// reaches the table — matching it exactly is how this id ended up routed
	// as an unknown model and served locally.
	legacyCloudBareModel = "claude-waired-cloud"
)

// directiveRoute maps a model id to the side it names, or ("", false) for an
// id neither side owns — which routeForModel then serves here.
//
// Every waired id answers routeWaired, the peer and public ones included.
// This package answers one question — does the turn leave this device? — and
// "which Waired node serves it" is a different axis, resolved in
// cmd/waired-agent where the mesh snapshot is in hand. A peer is a Waired
// node, so the answer here is routeWaired
// (docs/decisions/20260801/1840-tray-routing-split-and-peer-only.md §3).
//
// A model id the real Anthropic API serves answers routeAnthropic. Naming a
// model in /model is naming where it runs: waired does not answer as Opus
// (waired-agent#1091).
func directiveRoute(model string) (route string, ok bool) {
	bare := normalizeModelID(model)
	switch bare {
	case wairedAnyModel, wairedLocalModel, wairedPeerModel, wairedPublicModel:
		return routeWaired, true
	// The pre-#1185 spellings. normalizeModelID has already dropped the tier
	// marker, so one case covers the bare and "[1m]" forms of each.
	case legacyAutoModel, legacyAutoOldestModel, legacyLocalModel,
		legacyPeerModel, legacyPublicModel:
		return routeWaired, true
	}
	for _, prefix := range []string{wairedPeerPinPrefix, legacyPeerPinPre} {
		if strings.HasPrefix(bare, prefix) && len(bare) > len(prefix) {
			return routeWaired, true
		}
	}
	if isAnthropicOwnedID(bare) {
		return routeAnthropic, true
	}
	return "", false
}

// tierMarker1M is the suffix Claude Code sizes a session from — and strips
// before sending. It is matched case-insensitively and anywhere in the id,
// which is how Claude Code itself reads it.
const tierMarker1M = "[1m]"

// normalizeModelID reduces a client-sent id to the form the tables are keyed
// by: lower-cased, with every tier marker removed. Advertised ids keep their
// spelling — the client needs it to size the session — so only the LOOKUP is
// on the bare form.
func normalizeModelID(model string) string {
	bare := strings.ToLower(strings.TrimSpace(model))
	for {
		i := strings.Index(bare, tierMarker1M)
		if i < 0 {
			return bare
		}
		bare = bare[:i] + bare[i+len(tierMarker1M):]
	}
}

// isWairedOwnedID reports whether the id belongs to waired rather than to the
// real Anthropic API: the "waired/" subagent label, or any spelling of a
// directive id — current, legacy, per-peer, or one this build has not heard
// of yet.
//
// It is deliberately NOT directiveRoute's bool: that answers "which route does
// this id force", which now includes real Anthropic ids. This answers "may this
// id be sent upstream as-is", and the two must not be confused. Treating a real
// model id as waired-owned would rewrite it to something else on a passthrough
// leg and keep it out of the passthrough replacement — answering as a model the
// user did not pick, which is the whole defect this lane removes.
func isWairedOwnedID(model string) bool {
	return strings.Contains(normalizeModelID(model), wairedIDMarker)
}

// wairedIDMarker is the substring every waired-owned model id carries. A real
// Anthropic model will not contain it, and a future waired id will.
const wairedIDMarker = "waired"

// isAnthropicOwnedID reports whether the id names a model the real Anthropic
// API serves. Only "claude-" ids qualify: an id from some other vendor reaching
// this endpoint is not a Claude Code /model pick, so it keeps following the
// policy rather than being sent to an API that would reject it.
func isAnthropicOwnedID(bare string) bool {
	return strings.HasPrefix(bare, "claude-") && !strings.Contains(bare, wairedIDMarker)
}

// isDirectiveModel reports whether model is one of waired's own reserved
// directive ids — a route-forcing id this build would synthesise a /v1/models
// entry for. A real Anthropic id also forces a route now, so directiveRoute's
// bool alone no longer answers this question.
func isDirectiveModel(model string) bool {
	_, ok := directiveRoute(model)
	return ok && isWairedOwnedID(model)
}

// bodyModel extracts the top-level "model" string from a JSON request
// body. ok=false when the body is not a JSON object or model is not a
// string — callers treat that as "leave the body alone" (fail-open).
func bodyModel(body []byte) (string, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return "", false
	}
	raw, present := obj["model"]
	if !present {
		return "", false
	}
	var model string
	if err := json.Unmarshal(raw, &model); err != nil {
		return "", false
	}
	return model, true
}
