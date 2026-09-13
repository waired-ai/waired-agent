package main

import (
	"net/http"
	"testing"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// TestClassifyOpenAIListenerClass pins what the OpenAI-compatible listener
// reads a request's class from (waired-agent#1366).
//
// The OpenCode rows are a record of OpenCode 1.18.30's headers, captured
// with a pass-through proxy. The unclassified row is a product contract,
// ratifying source: owner decision 2026-09-14 that the Serve main
// conversation / Serve subagents switches apply to OpenCode
// (docs/decisions/20260914/0300-opencode-subagents-get-the-subagent-class.md),
// which does not extend them to clients that have no main conversation.
func TestClassifyOpenAIListenerClass(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{"OpenCode subagent turn", map[string]string{"x-session-id": "ses_b", "x-parent-session-id": "ses_a", "x-session-affinity": "ses_b"}, state.ClaudeClassSub},
		{"OpenCode main turn", map[string]string{"x-session-id": "ses_a", "x-session-affinity": "ses_a"}, state.ClaudeClassMain},
		{"OpenCode title request", map[string]string{"x-session-id": "ses_a"}, state.ClaudeClassMain},
		{"Claude Code subagent on this listener", map[string]string{"X-Claude-Code-Agent-Id": "a1"}, state.ClaudeClassSub},
		{"waired infer, a chat client", nil, ""},
		{"Claude Code main turn on this listener", map[string]string{"User-Agent": "claude-cli/2.1.251"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tc.headers {
				h.Set(k, v)
			}
			if got := classifyOpenAIListenerClass(h); got != tc.want {
				t.Errorf("classifyOpenAIListenerClass = %q, want %q", got, tc.want)
			}
		})
	}
}
