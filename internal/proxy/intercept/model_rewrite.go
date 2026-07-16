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

// wairedLocalModel / wairedCloudModel are the reserved /model route-directive
// ids (#52). Selected in Claude Code's /model picker they force this request's
// route regardless of the operator's /waired-route policy: local pins to the
// device (route=waired), cloud pins to the real Anthropic API
// (route=anthropic). The gateway advertises them in /v1/models discovery
// (gateway.ModelWaired{Local,Cloud}); the literals are duplicated here to keep
// this fail-open package stdlib-only — keep both sides in sync.
const (
	wairedLocalModel = "anthropic-waired-local"
	wairedCloudModel = "claude-waired-cloud[1m]"
)

// directiveRoute maps a reserved directive model id to the route it forces,
// or ("", false) for any other id (which follows the /waired-route policy).
// Consulted only when Config.ModelRouteDirectives is set.
func directiveRoute(model string) (route string, ok bool) {
	switch model {
	case wairedLocalModel:
		return routeWaired, true
	case wairedCloudModel:
		return routeAnthropic, true
	default:
		return "", false
	}
}

// isDirectiveModel reports whether model is one of the reserved directive ids.
func isDirectiveModel(model string) bool {
	_, ok := directiveRoute(model)
	return ok
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
// object whose "model" is a waired/-prefixed string OR the reserved cloud
// directive id (#52); otherwise (nil, false) and the caller passes the
// original bytes through verbatim. Both are ids the real Anthropic API would
// reject, so they must be rewritten to a real model on any passthrough leg.
// The mutation is lossless for every other field: the object is decoded as
// map[string]json.RawMessage so numbers, unicode, and unknown fields are
// re-emitted byte-exact — only the "model" value is re-encoded.
func rewritePassthroughModel(body []byte, replacement string) ([]byte, bool) {
	// Cheap pre-filter: only subagent-labelled bodies carry the waired/
	// prefix and only a cloud-directive selection carries that id;
	// everything else skips the parse.
	if !bytes.Contains(body, []byte(`"`+wairedModelPrefix)) && !bytes.Contains(body, []byte(wairedCloudModel)) {
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
	if !strings.HasPrefix(model, wairedModelPrefix) && model != wairedCloudModel {
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
	// Skip waired/ ids and the reserved directive ids (#52): none is a real
	// Anthropic model, so letting one become the passthrough replacement
	// target would rewrite a fake id to itself and still be rejected upstream.
	if model == "" || strings.HasPrefix(model, wairedModelPrefix) || isDirectiveModel(model) {
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
