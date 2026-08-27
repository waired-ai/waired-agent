package intercept

import "encoding/json"

// bodyIsAutoModeClassifier reports whether a buffered message-path body is one
// of Claude Code's auto-permission-mode safety-classifier requests.
//
// The classifier is the second model Claude Code runs in auto mode to decide
// whether a tool call may proceed. Claude Code picks that model itself and
// does not let anyone configure it ("Claude Code selects the classifier model,
// so which reason you see isn't something you configure" —
// code.claude.com/docs/en/auto-mode-config), so a Waired node standing in for
// it substitutes a different model for a permission decision. waired-agent#1041
// measured what that costs: on a host serving qwen3.6-35b-a3b the same request
// scored 0 and 50 on two consecutive calls where the real classifier scored 5,
// and Claude Code compares those numbers against thresholds it has pinned per
// classifier model. The owner's ruling (waired-agent#1041) is that the
// classifier goes to the real Anthropic API on EVERY route, `waired` included.
//
// Identification is by SHAPE, not by model id. The id is not usable: it is
// `claude-sonnet-5` by default, but Claude Code latches onto the session's own
// model for the rest of the session once a classifier request fails — which on
// a Waired host means it can arrive carrying a Waired directive id
// (waired-agent#1039 observed `claude-waired-auto`, #1041 observed
// `claude-sonnet-5[1m]`; both were correct).
//
// Two facts separate it from every other body Claude Code sends on this
// surface, measured against 2.1.247 with the real client:
//
//   - no `tools` key at all (the main conversation carries dozens)
//   - a non-empty `stop_sequences` (`["</severity>"]` or `["</block>"]`)
//
// Nothing else Claude Code sends sets `stop_sequences`: not the main
// conversation, not session-title generation, not the startup quota probe, not
// the post-model-switch `Hi`. Deliberately NOT matched on the classifier's
// system prompt text — that is prose which changes between releases, and
// pinning routing to it would break silently on an upgrade.
//
// Anything unreadable, non-object, or ambiguous returns false: the request then
// takes the route it would have taken before this check existed, which is the
// same fail-open posture the model-directive and class-classification paths
// already take on an unparseable body.
//
// This surface is Claude Code's alone (Inference.ClaudeGatewayPort; other
// coding agents reach the general gateway on LocalGatewayPort), so the
// predicate never sees another client's local-inference request.
func bodyIsAutoModeClassifier(body []byte) bool {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return false
	}
	if _, present := obj["tools"]; present {
		return false
	}
	raw, present := obj["stop_sequences"]
	if !present {
		return false
	}
	var stops []json.RawMessage
	if err := json.Unmarshal(raw, &stops); err != nil {
		return false
	}
	return len(stops) > 0
}
