package intercept

import "testing"

// TestBodyIsAutoModeClassifier pins the shape that identifies Claude Code's
// auto-mode safety classifier on the message path.
//
// PRODUCT CONTRACT, not a record of today's behaviour: the owner ruled
// (waired-agent#1041) that the classifier is served by the real Anthropic API
// on every route. Everything downstream of that ruling depends on this
// predicate telling the classifier apart from the rest of Claude Code's
// traffic, so the rows below are the discrimination itself.
//
// The bodies are synthetic but shaped from real captures against Claude Code
// 2.1.247 (waired-agent#1041); this repository is public, so no captured body,
// identifier or transcript is checked in.
func TestBodyIsAutoModeClassifier(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{
			// Stage 1: severity scoring. No tools, tiny max_tokens, and the
			// stop sequence that closes the verdict tag.
			name: "classifier stage 1",
			body: `{"model":"claude-sonnet-5","max_tokens":64,"thinking":{"type":"disabled"},
			        "stop_sequences":["</severity>"],
			        "system":[{"type":"text","text":"You are a security monitor for autonomous AI coding agents."}],
			        "messages":[{"role":"user","content":[{"type":"text","text":"<transcript>"}]}]}`,
			want: true,
		},
		{
			// The other stage-1 arm scores a block/allow verdict instead of a
			// severity; the closing tag differs, the shape does not.
			name: "classifier stage 1, block arm",
			body: `{"model":"claude-sonnet-5","max_tokens":256,"stop_sequences":["</block>"],
			        "messages":[{"role":"user","content":"<transcript>"}]}`,
			want: true,
		},
		{
			// After a failed classifier request Claude Code latches onto the
			// session's own model for the rest of the session, so on a Waired
			// host the classifier can arrive carrying a directive id
			// (waired-agent#1039 observed exactly this). The id must not be
			// what decides.
			name: "classifier carrying a waired directive id after the demotion latch",
			body: `{"model":"claude-waired-auto","max_tokens":64,"stop_sequences":["</severity>"],
			        "messages":[{"role":"user","content":"<transcript>"}]}`,
			want: true,
		},
		{
			// Stage 2 asks for room to think and a much larger budget.
			name: "classifier stage 2",
			body: `{"model":"claude-sonnet-5","max_tokens":8192,"thinking":{"type":"enabled"},
			        "stop_sequences":["</severity>"],
			        "messages":[{"role":"user","content":"<transcript>"}]}`,
			want: true,
		},
		{
			name: "main conversation carries tools",
			body: `{"model":"claude-opus-5","max_tokens":64000,"stream":true,
			        "tools":[{"name":"Bash","input_schema":{"type":"object"}}],
			        "messages":[{"role":"user","content":"hello"}]}`,
			want: false,
		},
		{
			// Session-title generation: no tools, big budget, no stop sequence.
			name: "session title generation",
			body: `{"model":"claude-opus-5","max_tokens":64000,"stream":true,
			        "system":[{"type":"text","text":"Write a title"}],
			        "messages":[{"role":"user","content":"<session>…</session>"}]}`,
			want: false,
		},
		{
			// The startup quota probe: one token, no tools, no stop sequence.
			name: "quota probe",
			body: `{"model":"claude-opus-5","max_tokens":1,"messages":[{"role":"user","content":"quota"}]}`,
			want: false,
		},
		{
			// A body that declares tools is never the classifier, even if some
			// future client also sets stop_sequences. Erring towards the route
			// the request would have taken keeps this check from hijacking
			// somebody else's local inference.
			name: "tools and stop_sequences together",
			body: `{"model":"claude-opus-5","tools":[{"name":"Bash"}],"stop_sequences":["</severity>"],
			        "messages":[{"role":"user","content":"hi"}]}`,
			want: false,
		},
		{
			name: "empty tools array still counts as declaring tools",
			body: `{"model":"claude-opus-5","tools":[],"stop_sequences":["</severity>"]}`,
			want: false,
		},
		{
			name: "empty stop_sequences is not a verdict request",
			body: `{"model":"claude-opus-5","max_tokens":64,"stop_sequences":[]}`,
			want: false,
		},
		{
			name: "stop_sequences is not an array",
			body: `{"model":"claude-opus-5","max_tokens":64,"stop_sequences":"</severity>"}`,
			want: false,
		},
		{name: "not JSON", body: `not json at all`, want: false},
		{name: "JSON but not an object", body: `["a","b"]`, want: false},
		{name: "empty body", body: ``, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := bodyIsAutoModeClassifier([]byte(tc.body)); got != tc.want {
				t.Errorf("bodyIsAutoModeClassifier() = %v, want %v", got, tc.want)
			}
		})
	}
}
