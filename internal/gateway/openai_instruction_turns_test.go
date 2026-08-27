package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/waired-ai/waired-agent/internal/router"
)

// TestNormalizeOpenAIBodyInstructionTurns covers the fold itself.
//
// A record of today's behaviour. The shapes it exercises are the ones
// measured against a live engine on sv-mag, 2026-08-27 (ollama 0.32.13,
// qwen3.8:27b-mtp-q4_K_M): [user, system], [system, system, user] and a
// tool-call conversation with a system turn in the middle all answered
// 500 "system message must be at the beginning", while [system, user]
// and [system, user, developer, user] answered 200.
func TestNormalizeOpenAIBodyInstructionTurns(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		changed bool
	}{
		{
			name:    "mid-conversation system folds into the leading one",
			in:      `{"model":"m","messages":[{"role":"system","content":"You are Claude Code."},{"role":"user","content":"hi"},{"role":"system","content":"Available agent types"}]}`,
			want:    `{"messages":[{"role":"system","content":"You are Claude Code.\n\nAvailable agent types"},{"role":"user","content":"hi"}],"model":"m"}`,
			changed: true,
		},
		{
			name:    "no leading system creates one",
			in:      `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"system","content":"rules"}]}`,
			want:    `{"messages":[{"role":"system","content":"rules"},{"role":"user","content":"hi"}],"model":"m"}`,
			changed: true,
		},
		{
			name:    "a second leading system folds too",
			in:      `{"model":"m","messages":[{"role":"system","content":"a"},{"role":"system","content":"b"},{"role":"user","content":"hi"}]}`,
			want:    `{"messages":[{"role":"system","content":"a\n\nb"},{"role":"user","content":"hi"}],"model":"m"}`,
			changed: true,
		},
		{
			// developer is an instruction turn wherever it sits — the
			// same rule normalizeInstructionTurns applies. That the
			// measured renderer happened to accept it mid-conversation
			// says the template did not raise, not that it rendered.
			name:    "a leading developer turn becomes the system message",
			in:      `{"model":"m","messages":[{"role":"developer","content":"rules"},{"role":"user","content":"hi"}]}`,
			want:    `{"messages":[{"role":"system","content":"rules"},{"role":"user","content":"hi"}],"model":"m"}`,
			changed: true,
		},
		{
			// Everything the fold does not understand on a kept message
			// survives, because kept messages are never re-encoded.
			name:    "tool messages keep their place and their fields",
			in:      `{"model":"m","messages":[{"role":"system","content":"s"},{"role":"user","content":"hi"},{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"Read","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c1","content":"file"},{"role":"system","content":"more"},{"role":"user","content":"go on"}]}`,
			want:    `{"messages":[{"role":"system","content":"s\n\nmore"},{"role":"user","content":"hi"},{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"Read","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c1","content":"file"},{"role":"user","content":"go on"}],"model":"m"}`,
			changed: true,
		},
		{
			name:    "an array of text parts is merged",
			in:      `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}]}`,
			want:    `{"messages":[{"role":"system","content":"ab"},{"role":"user","content":"hi"}],"model":"m"}`,
			changed: true,
		},
		{
			// A contentless instruction turn carries no instructions,
			// and a contentless system message is worse than none.
			name:    "an empty instruction turn leaves no system message behind",
			in:      `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"system","content":""}]}`,
			want:    `{"messages":[{"role":"user","content":"hi"}],"model":"m"}`,
			changed: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, changed := normalizeOpenAIBodyInstructionTurns([]byte(c.in))
			if changed != c.changed {
				t.Fatalf("changed = %v, want %v", changed, c.changed)
			}
			if string(got) != c.want {
				t.Errorf("body:\n got %s\nwant %s", got, c.want)
			}
		})
	}
}

// TestNormalizeOpenAIBodyInstructionTurns_LeavesTheBodyAlone is the
// half that matters most: this surface forwards a third-party client's
// own bytes, so anything the fold does not fully understand must come
// back byte-identical rather than partially rewritten.
func TestNormalizeOpenAIBodyInstructionTurns_LeavesTheBodyAlone(t *testing.T) {
	cases := map[string]string{
		"already legal":     `{"model":"m","messages":[{"role":"system","content":"s"},{"role":"user","content":"hi"}]}`,
		"no system at all":  `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		"no messages key":   `{"model":"m"}`,
		"messages is null":  `{"model":"m","messages":null}`,
		"not json":          `not json at all`,
		"messages not list": `{"model":"m","messages":"hello"}`,
		// An image part cannot be merged into a string without
		// inventing something, so the whole request is left as sent.
		"non-text content part": `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"image_url","image_url":{"url":"data:x"}}]}]}`,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			got, changed := normalizeOpenAIBodyInstructionTurns([]byte(in))
			if changed {
				t.Errorf("changed = true, want false")
			}
			if string(got) != in {
				t.Errorf("body was rewritten:\n got %s\nwant %s", got, in)
			}
		})
	}
}

// TestNormalizeOpenAIBodyInstructionTurns_MatchesTheAnthropicFold pins
// the two surfaces to the same prompt. A conversation that arrives as
// Anthropic shapes and the same conversation posted directly as OpenAI
// shapes must reach the engine with identical instruction text —
// otherwise the same client switching surfaces gets a different prompt,
// and the prompt cache sees two conversations where there is one.
func TestNormalizeOpenAIBodyInstructionTurns_MatchesTheAnthropicFold(t *testing.T) {
	viaAnthropic := convertOK(t, AnthropicRequest{
		Model:    "waired/default",
		System:   json.RawMessage(`"You are Claude Code."`),
		Messages: []AnthropicMessage{msg("user", "hello"), msg("system", "Available agent types")},
	})

	native := `{"model":"waired/default","max_tokens":64,"messages":[` +
		`{"role":"system","content":"You are Claude Code."},` +
		`{"role":"user","content":"hello"},` +
		`{"role":"system","content":"Available agent types"}]}`
	folded, changed := normalizeOpenAIBodyInstructionTurns([]byte(native))
	if !changed {
		t.Fatal("the native body was not folded")
	}
	var got struct {
		Messages []struct{ Role, Content string } `json:"messages"`
	}
	if err := json.Unmarshal(folded, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Messages) != len(viaAnthropic.Messages) {
		t.Fatalf("message count = %d, want %d", len(got.Messages), len(viaAnthropic.Messages))
	}
	for i := range got.Messages {
		if got.Messages[i].Role != viaAnthropic.Messages[i].Role ||
			got.Messages[i].Content != viaAnthropic.Messages[i].Content {
			t.Errorf("messages[%d] = %+v, want %+v", i, got.Messages[i], viaAnthropic.Messages[i])
		}
	}
}

func openAIGatewayFor(t *testing.T, url string) *Server {
	t.Helper()
	return newGatewayUnderTest(t, &fakeSelector{sel: router.Selection{
		Runtime: "ollama", EngineModel: "qwen3:8b-q4_K_M", ModelID: "qwen3-8b-instruct",
		ExecutionMode: "local",
	}}, url)
}

func postOpenAIBody(t *testing.T, gw *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	return w
}

// TestOpenAIChatCompletions_MidConversationSystemShape is the twin of
// TestAnthropicMessages_MidConversationSystemShape, driven end to end so
// the assertion is about what the ENGINE received rather than what a
// helper returned.
func TestOpenAIChatCompletions_MidConversationSystemShape(t *testing.T) {
	var captured string
	upstream := fakeOllama(t, &captured)
	defer upstream.Close()

	w := postOpenAIBody(t, openAIGatewayFor(t, upstream.URL), `{
		"model":"waired/default",
		"messages":[
			{"role":"system","content":"You are Claude Code."},
			{"role":"user","content":"hello"},
			{"role":"system","content":"Available agent types for the Agent tool"}
		]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	var sent struct {
		Messages []struct{ Role, Content string } `json:"messages"`
	}
	if err := json.Unmarshal([]byte(captured), &sent); err != nil {
		t.Fatalf("decode engine-bound body: %v (%s)", err, captured)
	}
	if len(sent.Messages) == 0 || sent.Messages[0].Role != "system" {
		t.Fatalf("engine-bound roles = %+v, want a leading system turn", sent.Messages)
	}
	for i, m := range sent.Messages[1:] {
		if isInstructionRole(m.Role) {
			t.Errorf("messages[%d] is still an instruction turn (%s)", i+1, m.Role)
		}
	}
	for _, want := range []string{"You are Claude Code.", "Available agent types"} {
		if !strings.Contains(sent.Messages[0].Content, want) {
			t.Errorf("leading system lost %q: %q", want, sent.Messages[0].Content)
		}
	}
	// The model rewrite still happens on the folded body.
	if !strings.Contains(captured, `"qwen3:8b-q4_K_M"`) {
		t.Errorf("engine did not see the rewritten model field: %s", captured)
	}
}

// TestOpenAIChatCompletions_EngineShapeRejectionBecomes400 pins the
// sibling half of waired-agent#1035 on this surface: a deterministic
// rejection reaches the client as 400, so a well-behaved client stops
// instead of retrying a request that will fail identically.
func TestOpenAIChatCompletions_EngineShapeRejectionBecomes400(t *testing.T) {
	var hits atomic.Int32
	upstream := fakeEngineStatus(t, http.StatusInternalServerError, shapeRejection500, &hits)
	defer upstream.Close()

	w := postOpenAIBody(t, openAIGatewayFor(t, upstream.URL),
		`{"model":"waired/default","messages":[{"role":"user","content":"hi"}]}`)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
	}
	var env openAIErrorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, w.Body.String())
	}
	if env.Error.Type != "invalid_request_error" {
		t.Errorf("error type = %q, want invalid_request_error", env.Error.Type)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("engine saw %d attempts, want 1 — the gateway must not retry a shape rejection", got)
	}
	if w.Header().Get("Content-Length") != "" {
		t.Error("the engine's Content-Length survived onto a rewritten body")
	}
}

// TestOpenAIChatCompletions_OtherEngineErrorsPassThrough keeps the
// remap narrow: only a request-shape rejection changes status.
func TestOpenAIChatCompletions_OtherEngineErrorsPassThrough(t *testing.T) {
	var hits atomic.Int32
	upstream := fakeEngineStatus(t, http.StatusInternalServerError,
		`{"error":"an error was encountered while running the model"}`, &hits)
	defer upstream.Close()

	w := postOpenAIBody(t, openAIGatewayFor(t, upstream.URL),
		`{"model":"waired/default","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want the engine's own 500", w.Code)
	}
}
