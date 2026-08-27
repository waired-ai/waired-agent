package intercept

import (
	"bytes"
	"encoding/json"
	"strings"
)

// wairedModelPrefix marks model ids that only waired's local gateway
// understands. Managed settings pin Claude Code subagents to
// "waired/subagent" (#646, claudemanaged.SubagentModelID — literal
// duplicated to keep this fail-open package stdlib-only; keep in sync),
// so on every passthrough leg to the real Anthropic API the model must
// be rewritten to a real Anthropic id or the request is rejected with
// an unknown-model error — which would break the route=anthropic escape
// hatch and the post-dispatch fallback for every subagent turn.
const wairedModelPrefix = "waired/"

// defaultPassthroughModel is the replacement used before any main-loop
// model has been observed this process lifetime. An alias id (not a
// dated snapshot) so it tracks Anthropic-side upgrades; once a main
// request passes through, the observed id takes over (Claude Code's
// own "subagents inherit the main model when unset" semantics are not
// recoverable per request — the env var wins at resolution position 1
// — so the last-observed main model is the closest approximation).
const defaultPassthroughModel = "claude-sonnet-5"

// wairedLocalModel / wairedAutoModel / wairedCloudModel are the reserved /model
// route-directive ids (#52). Selected in Claude Code's /model picker they force
// this request's route regardless of the operator's /waired-route policy: local
// pins to the device (route=waired), auto is Waired-first with Anthropic
// fallback (route=auto), cloud pins to the real Anthropic API (route=anthropic).
// The gateway advertises them in /v1/models discovery
// (gateway.ModelWaired{Local,Auto,Cloud}); the literals are duplicated here to
// keep this fail-open package stdlib-only — keep both sides in sync.
const (
	wairedLocalModel = "anthropic-waired-local"
	wairedAutoModel  = "claude-waired-auto"
	// wairedAuto1MModel is the 1M tier of the same auto route. The route
	// it forces is identical; what differs is the window Claude Code
	// sized the session to from the "[1m]" suffix, and the window the
	// gateway therefore demands of a serving endpoint. When none declares
	// it, selection fails and the auto fallback carries the turn to the
	// real Anthropic API — which is the tier's contract, not a fault.
	wairedAuto1MModel = "claude-waired-auto[1m]"
	// wairedAutoLegacyModel is the pre-waired#1031 spelling of the auto
	// id. It is no longer advertised and is still routed: a Claude Code
	// that selected it keeps it in its own settings across an upgrade,
	// and the picker cache has no TTL, so a whole session can arrive
	// under the old name.
	wairedAutoLegacyModel = "anthropic-waired-auto"
	// wairedCloudModel keeps the "[1m]" spelling because that spelling is
	// what sizes the session: Claude Code reads the tier off the id string
	// it holds. It is no longer advertised (the picker offers the real
	// Anthropic models instead, which say WHICH model answers) and is still
	// routed, for the same reason wairedAutoLegacyModel is: the picker cache
	// has no TTL, so a client can carry the id for a whole session.
	wairedCloudModel = "claude-waired-cloud[1m]"
	// wairedCloudBareModel is wairedCloudModel as it actually arrives.
	// Claude Code strips "[1m]" before the request leaves it (measured on
	// 2.1.229 / 2.1.241 / 2.1.245, waired-agent#1036), so the spelled form
	// never reaches the table — matching it exactly is how this id ended up
	// routed as an unknown model and served locally.
	wairedCloudBareModel = "claude-waired-cloud"
	// wairedPeerModel restricts the turn to another computer on the mesh.
	// Same route as the local pin — a peer is a Waired node, so the turn
	// never leaves for Anthropic — and which node is decided a layer
	// down, where the mesh is in hand. See gateway.ModelWairedPeer.
	wairedPeerModel = "claude-waired-peer"
	// wairedPeerPinPrefix heads the per-peer ids generated from the live mesh
	// (waired-agent#830). This package never sees a mesh — it is stdlib-only
	// and fail-open by contract — so it recognises the family by prefix rather
	// than by lookup. The route is the same as the bare peer id's; which peer
	// is resolved a layer down, against the snapshot that produced the id.
	wairedPeerPinPrefix = wairedPeerModel + "-"
	// wairedPublicModel restricts the turn to a Public Share machine —
	// someone else's computer (waired-agent#901). Same route as the peer
	// ids: it is still a Waired node, so the turn never leaves for
	// Anthropic. Which machine, and whether the consumer's posture admits
	// one at all, is decided a layer down.
	wairedPublicModel = "claude-waired-public"
)

// directiveRoute maps a reserved directive model id to the route it forces,
// or ("", false) for any other id (which follows the /waired-route policy).
// Consulted only when Config.ModelRouteDirectives is set.
//
// The three route values are unchanged by the peer directive, deliberately.
// This package answers one question — does the turn leave this device? — and
// "which Waired node serves it" is a different axis, resolved in
// cmd/waired-agent where the mesh snapshot is in hand. A peer is a Waired
// node, so the answer here is routeWaired, and routeAuto would be wrong twice
// over: peer-only is fail-closed by ratified decision
// (docs/decisions/20260801/1840-tray-routing-split-and-peer-only.md §3), and a
// silent Anthropic fallback is the defect waired-agent#325 removed.
//
// A model id the real Anthropic API serves takes routeAnthropic too. Naming a
// model in /model is naming where it runs: waired does not answer as Opus.
// This overrides the per-class policy the same way the reserved ids do —
// including route=waired, because that setting is a standing preference for
// traffic nobody directed, not an enforcement boundary (owner ruling
// 2026-08-28: `/waired-route` and the CLI are global user settings, `/model`
// is a setting inside one session, and a narrower scope may win).
func directiveRoute(model string) (route string, ok bool) {
	bare := normalizeModelID(model)
	switch bare {
	case wairedLocalModel, wairedPeerModel, wairedPublicModel:
		return routeWaired, true
	// wairedAuto1MModel normalises onto wairedAutoModel: the tier travels in
	// the context-1m beta header, which the gateway reads (waired-agent#1036).
	case wairedAutoModel, wairedAutoLegacyModel:
		return routeAuto, true
	case wairedCloudBareModel:
		return routeAnthropic, true
	}
	if strings.HasPrefix(bare, wairedPeerPinPrefix) && len(bare) > len(wairedPeerPinPrefix) {
		return routeWaired, true
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
	bare := normalizeModelID(model)
	return strings.HasPrefix(bare, wairedModelPrefix) || strings.Contains(bare, wairedIDMarker)
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

// rewritePassthroughModel returns (newBody, true) when body is a JSON
// object whose "model" is a waired/-prefixed string OR any reserved directive
// id (#52); otherwise (nil, false) and the caller passes the original bytes
// through verbatim. None is a real Anthropic model, so any that reaches a
// passthrough leg must be rewritten or the API rejects it. In practice the auto
// directive's Anthropic-fallback leg and the cloud directive both hit this; the
// local directive never passes through (route=waired has no fallback), but it is
// covered defensively. The mutation is lossless for every other field: the
// object is decoded as map[string]json.RawMessage so numbers, unicode, and
// unknown fields are re-emitted byte-exact — only the "model" value is re-encoded.
func rewritePassthroughModel(body []byte, replacement string) ([]byte, bool) {
	// Cheap pre-filter: only subagent-labelled bodies (waired/ prefix) and the
	// reserved directive ids carry the substring "waired"; everything else skips
	// the parse.
	if !bytes.Contains(body, []byte("waired")) {
		return nil, false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, false
	}
	raw, present := obj["model"]
	if !present {
		return nil, false
	}
	var model string
	if err := json.Unmarshal(raw, &model); err != nil {
		return nil, false
	}
	// Only waired's own ids are meaningless upstream. A real Anthropic id
	// reaching this leg is the model the user picked, and it travels verbatim.
	if !isWairedOwnedID(model) {
		return nil, false
	}
	enc, err := json.Marshal(replacement)
	if err != nil {
		return nil, false
	}
	obj["model"] = enc
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, false
	}
	return out, true
}

// observeMainModel remembers the most recent real (non-waired) model id
// seen on the message paths, per process. It feeds
// passthroughReplacement so subagent rewrites follow whatever model the
// operator's Claude Code main loop is actually using.
func (s *Server) observeMainModel(model string) {
	// Skip every waired-owned id (#52): none is a real Anthropic model, so
	// letting one become the passthrough replacement target would rewrite a
	// fake id to itself and still be rejected upstream. Asked by family rather
	// than by table membership, because waired-agent#1036 got in through a
	// spelling the table did not hold: Claude Code strips "[1m]", so
	// `claude-waired-cloud` arrived, missed the exact-match table, and was
	// stored here as the "last real main model" — after which every fallback
	// replay on the host 404'd.
	if model == "" || isWairedOwnedID(model) {
		return
	}
	s.lastMainModel.Store(model)
}

// passthroughReplacement resolves what a waired/* model id becomes on a
// real-Anthropic leg: the config override when set, else the
// last-observed main-loop model, else the default alias.
func (s *Server) passthroughReplacement() string {
	if s.cfg.PassthroughModelOverride != "" {
		return s.cfg.PassthroughModelOverride
	}
	if v, ok := s.lastMainModel.Load().(string); ok && v != "" {
		return v
	}
	return defaultPassthroughModel
}

// forgetObservedMainModel drops the observed replacement when it is the one
// upstream just rejected, so the next replay falls back to
// defaultPassthroughModel instead of repeating a 404 for the rest of the
// process lifetime.
//
// An observed id can go stale honestly — Anthropic retires a dated snapshot —
// and a rewrite is the one place waired puts a model id on the wire that the
// user never typed. Both make the failure ours to recover from rather than to
// re-run (waired-agent#1036).
func (s *Server) forgetObservedMainModel(model string) {
	if model == "" || s.cfg.PassthroughModelOverride != "" {
		return
	}
	if v, ok := s.lastMainModel.Load().(string); ok && v == model {
		s.lastMainModel.Store("")
	}
}

// preparePassthroughBody observes the main model and rewrites a
// waired/* model id in a buffered message body bound for the real
// Anthropic API. Returns the (possibly rewritten) bytes.
func (s *Server) preparePassthroughBody(body []byte, path string) []byte {
	if model, ok := bodyModel(body); ok {
		s.observeMainModel(model)
	}
	rewritten, ok := rewritePassthroughModel(body, s.passthroughReplacement())
	if !ok {
		return body
	}
	s.log.Info("intercept: rewrote waired model id for upstream passthrough",
		"path", path, "to", s.passthroughReplacement())
	return rewritten
}
