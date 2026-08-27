package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/waired-ai/waired-agent/internal/router"
)

// The shapes in this file were measured against ollama 0.32.13 serving
// qwen3.8:27b-mtp-q4_K_M-wb2048 on 2026-08-27 (waired-agent#1035,
// docs/knowledges/20260827/1330-non-leading-system-turns-are-rejected.md):
// a system turn anywhere but first is answered with HTTP 500 "system
// message must be at the beginning".
//
// PRODUCT CONTRACT (waired-agent#1035): whatever the client sends, the
// body this gateway hands an engine carries at most one instruction turn
// and it is first.

func msg(role, content string) AnthropicMessage {
	return AnthropicMessage{Role: role, Content: json.RawMessage(`"` + content + `"`)}
}

func convertOK(t *testing.T, req AnthropicRequest) OpenAIRequest {
	t.Helper()
	if req.MaxTokens == 0 {
		req.MaxTokens = 64
	}
	out, err := AnthropicToOpenAI(req)
	if err != nil {
		t.Fatalf("AnthropicToOpenAI: %v", err)
	}
	return out
}

func roles(msgs []OpenAIMessage) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Role)
	}
	return out
}

func TestAnthropicToOpenAI_MidConversationSystemFolded(t *testing.T) {
	// The shape Claude Code 2.1.229/241 sends under the
	// mid-conversation-system-2026-04-07 beta.
	out := convertOK(t, AnthropicRequest{
		System:   json.RawMessage(`[{"type":"text","text":"You are Claude Code."}]`),
		Messages: []AnthropicMessage{msg("user", "hello"), msg("system", "Available agent types")},
	})

	if got := roles(out.Messages); !reflect.DeepEqual(got, []string{"system", "user"}) {
		t.Fatalf("roles = %v, want [system user]", got)
	}
	lead := out.Messages[0].Content
	top, mid := strings.Index(lead, "You are Claude Code."), strings.Index(lead, "Available agent types")
	if top < 0 || mid < 0 {
		t.Fatalf("leading system lost content: %q", lead)
	}
	if top >= mid {
		t.Errorf("folded out of order: %q", lead)
	}
}

func TestAnthropicToOpenAI_LeadingDeveloperBecomesSystem(t *testing.T) {
	out := convertOK(t, AnthropicRequest{
		Messages: []AnthropicMessage{msg("developer", "be terse"), msg("user", "hi")},
	})
	if got := roles(out.Messages); !reflect.DeepEqual(got, []string{"system", "user"}) {
		t.Fatalf("roles = %v, want [system user]", got)
	}
	if out.Messages[0].Content != "be terse" {
		t.Errorf("content = %q, want %q", out.Messages[0].Content, "be terse")
	}
}

func TestAnthropicToOpenAI_EmptyInstructionTurnDropped(t *testing.T) {
	t.Run("no top-level system", func(t *testing.T) {
		out := convertOK(t, AnthropicRequest{
			Messages: []AnthropicMessage{msg("user", "hi"), msg("system", "")},
		})
		// A contentless system message is worse than none: it marshals as
		// a bare {"role":"system"} the engine has to render.
		if got := roles(out.Messages); !reflect.DeepEqual(got, []string{"user"}) {
			t.Fatalf("roles = %v, want [user]", got)
		}
	})
	t.Run("with top-level system", func(t *testing.T) {
		out := convertOK(t, AnthropicRequest{
			System:   json.RawMessage(`"lead"`),
			Messages: []AnthropicMessage{msg("user", "hi"), msg("system", "")},
		})
		if out.Messages[0].Content != "lead" {
			t.Errorf("content = %q, want %q (no trailing separator)", out.Messages[0].Content, "lead")
		}
	})
}

func TestAnthropicToOpenAI_FoldKeepsToolMessagesInPlace(t *testing.T) {
	out := convertOK(t, AnthropicRequest{
		System: json.RawMessage(`"lead"`),
		Messages: []AnthropicMessage{
			msg("user", "call echo"),
			{Role: "assistant", Content: json.RawMessage(
				`[{"type":"tool_use","id":"toolu_1","name":"echo","input":{"s":"x"}}]`)},
			{Role: "user", Content: json.RawMessage(
				`[{"type":"tool_result","tool_use_id":"toolu_1","content":"x"}]`)},
			msg("system", "reminder"),
			msg("user", "go on"),
		},
	})
	want := []string{"system", "user", "assistant", "tool", "user"}
	if got := roles(out.Messages); !reflect.DeepEqual(got, want) {
		t.Fatalf("roles = %v, want %v", got, want)
	}
	if out.Messages[3].ToolCallID != "toolu_1" {
		t.Errorf("tool_call_id = %q, want toolu_1", out.Messages[3].ToolCallID)
	}
	if len(out.Messages[2].ToolCalls) != 1 {
		t.Fatalf("assistant lost its tool_calls")
	}
	if !strings.Contains(out.Messages[0].Content, "reminder") {
		t.Errorf("instruction turn not folded: %q", out.Messages[0].Content)
	}
}

func TestAnthropicToOpenAI_SystemBlockArrayWithToolResultFansOut(t *testing.T) {
	// A tool_result inside an instruction turn already fans out into its
	// own role:"tool" message; only the instruction half may move.
	out := convertOK(t, AnthropicRequest{
		Messages: []AnthropicMessage{
			msg("user", "call echo"),
			{Role: "assistant", Content: json.RawMessage(
				`[{"type":"tool_use","id":"toolu_1","name":"echo","input":{}}]`)},
			{Role: "system", Content: json.RawMessage(
				`[{"type":"text","text":"note"},{"type":"tool_result","tool_use_id":"toolu_1","content":"x"}]`)},
		},
	})
	want := []string{"system", "user", "assistant", "tool"}
	if got := roles(out.Messages); !reflect.DeepEqual(got, want) {
		t.Fatalf("roles = %v, want %v", got, want)
	}
	if out.Messages[0].Content != "note" {
		t.Errorf("folded content = %q, want %q", out.Messages[0].Content, "note")
	}
	if out.Messages[3].Content != "x" {
		t.Errorf("tool content = %q, want x", out.Messages[3].Content)
	}
}

func TestAnthropicToOpenAI_SystemAfterLastUserTurn(t *testing.T) {
	// The turn now ends on the assistant, which some templates continue
	// rather than restart. Pathological input, and a fixed ollama 0.32.15
	// produces the same thing — pinned as a record of today's behaviour,
	// not as a promise.
	out := convertOK(t, AnthropicRequest{
		Messages: []AnthropicMessage{msg("user", "hi"), msg("assistant", "hello"), msg("system", "note")},
	})
	want := []string{"system", "user", "assistant"}
	if got := roles(out.Messages); !reflect.DeepEqual(got, want) {
		t.Fatalf("roles = %v, want %v", got, want)
	}
}

func TestAnthropicToOpenAI_MultipleTextBlocksInInstructionTurn(t *testing.T) {
	// convertAnthropicMessage joins a message's text blocks with "" while
	// anthropicSystemToString joins system BLOCKS with "\n". The
	// normalizer must consume the already-converted strings and not
	// re-derive them, or every multi-block message changes bytes.
	out := convertOK(t, AnthropicRequest{
		Messages: []AnthropicMessage{
			msg("user", "hi"),
			{Role: "system", Content: json.RawMessage(`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`)},
		},
	})
	if out.Messages[0].Content != "ab" {
		t.Errorf("content = %q, want %q", out.Messages[0].Content, "ab")
	}
}

func TestNormalizeInstructionTurns_ByteLevelNoOpWhenAlreadyLegal(t *testing.T) {
	// The serialised body is the engine's prompt-cache key: a request
	// that was already legal must marshal to the bytes it did before.
	t.Run("nil stays nil", func(t *testing.T) {
		if got := normalizeInstructionTurns(nil); got != nil {
			t.Fatalf("got %#v, want nil — an empty slice turns %q into %q",
				got, `"messages":null`, `"messages":[]`)
		}
	})
	t.Run("legal conversation is untouched", func(t *testing.T) {
		in := []OpenAIMessage{{Role: "system", Content: "s"}, {Role: "user", Content: "u"}}
		if got := normalizeInstructionTurns(in); !reflect.DeepEqual(got, in) {
			t.Fatalf("got %#v, want %#v", got, in)
		}
	})
	t.Run("marshalled body is byte-identical", func(t *testing.T) {
		out := convertOK(t, AnthropicRequest{
			Model:    "waired/default",
			System:   json.RawMessage(`"You are Claude Code."`),
			Messages: []AnthropicMessage{msg("user", "hello")},
		})
		got, err := json.Marshal(out)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		const want = `{"model":"waired/default","max_tokens":64,"messages":[` +
			`{"role":"system","content":"You are Claude Code."},{"role":"user","content":"hello"}]}`
		if string(got) != want {
			t.Errorf("body changed:\n got %s\nwant %s", got, want)
		}
	})
}

func TestCountTokensApprox_MatchesTheConvertedBodyAfterFold(t *testing.T) {
	// #436: the requester and the serving peer must count the same
	// conversation identically. This goes red the moment the fold moves
	// out of AnthropicToOpenAI and into the handler.
	req := AnthropicRequest{
		MaxTokens: 64,
		System:    json.RawMessage(`[{"type":"text","text":"You are Claude Code."}]`),
		Messages:  []AnthropicMessage{msg("user", "hello"), msg("system", "Available agent types")},
	}
	got := CountTokensApprox(req)
	body, err := json.Marshal(convertOK(t, req))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if want := CountOpenAIPromptTokensApprox(body); got != want {
		t.Errorf("CountTokensApprox = %d, converted body counts %d", got, want)
	}
}

func TestIsEngineRequestShapeRejection(t *testing.T) {
	const measured = `{"error":{"message":"system message must be at the beginning","type":"api_error"}}`
	if !IsEngineRequestShapeRejection(measured) {
		t.Errorf("measured ollama 0.32.13 body not recognised")
	}
	for _, body := range []string{
		`{"error":"something went wrong"}`,
		`{"error":"model runner has unexpectedly stopped"}`,
		`{"error":"an error was encountered while running the model: CUDA error\nCUDA error: out of memory"}`,
	} {
		if IsEngineRequestShapeRejection(body) {
			t.Errorf("false positive on %q", body)
		}
	}
	// The two classifiers must stay disjoint: engineParseFailureMarkers
	// drives retries and a model's agentgrade verdict; a shape rejection
	// is neither.
	for _, m := range engineParseFailureMarkers {
		if IsEngineRequestShapeRejection(m) {
			t.Errorf("parse-failure marker %q also reads as a shape rejection", m)
		}
	}
	for _, m := range engineRequestShapeMarkers {
		if IsEngineParseFailure(m) {
			t.Errorf("shape marker %q also reads as a parse failure", m)
		}
	}
}

// fakeEngineStatus answers every chat completion with a fixed status and
// body, counting attempts. No existing fake returns a non-2xx on the
// Anthropic surface.
func fakeEngineStatus(t *testing.T, status int, body string, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	return httptest.NewServer(mux)
}

const shapeRejection500 = `{"error":{"message":"system message must be at the beginning","type":"api_error"}}`

func postAnthropicBody(t *testing.T, gw *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	return w
}

func anthropicGatewayFor(t *testing.T, url string) *Server {
	t.Helper()
	return anthropicGatewayUnderTest(t, &fakeSelector{sel: router.Selection{
		Runtime: "ollama", EngineModel: "qwen3:8b-q4_K_M", ModelID: "qwen3-8b-instruct",
	}}, url)
}

func TestAnthropicMessages_MidConversationSystemShape(t *testing.T) {
	var captured string
	upstream := fakeOllamaForAnthropic(t, &captured)
	defer upstream.Close()

	w := postAnthropicBody(t, anthropicGatewayFor(t, upstream.URL), `{
		"model":"waired/default","max_tokens":64,
		"system":[
			{"type":"text","text":"You are Claude Code."},
			{"type":"text","text":"<env>","cache_control":{"type":"ephemeral"}},
			{"type":"text","text":"<policy>"}
		],
		"tools":[{"name":"Read","description":"read a file","input_schema":{"type":"object"}}],
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"a system reminder"},
				{"type":"text","text":"hello"}]},
			{"role":"system","content":"Available agent types for the Agent tool: general-purpose"}
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
		t.Fatalf("engine-bound roles = %v, want a leading system turn", sent.Messages)
	}
	for i, m := range sent.Messages[1:] {
		if m.Role == "system" || m.Role == "developer" {
			t.Errorf("messages[%d] is still an instruction turn (%s)", i+1, m.Role)
		}
	}
	for _, want := range []string{"You are Claude Code.", "<policy>", "Available agent types"} {
		if !strings.Contains(sent.Messages[0].Content, want) {
			t.Errorf("leading system lost %q: %q", want, sent.Messages[0].Content)
		}
	}
}

func TestAnthropicMessages_EngineShapeRejectionBecomes400(t *testing.T) {
	var hits atomic.Int32
	upstream := fakeEngineStatus(t, http.StatusInternalServerError, shapeRejection500, &hits)
	defer upstream.Close()

	w := postAnthropicBody(t, anthropicGatewayFor(t, upstream.URL),
		`{"model":"waired/default","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
	}
	var env anthropicErrorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.Error.Type != "invalid_request_error" {
		t.Errorf("error.type = %q, want invalid_request_error", env.Error.Type)
	}
	if got := w.Header().Get(HeaderLocalError); got != LocalErrorEngineRequestShape {
		t.Errorf("%s = %q, want %q", HeaderLocalError, got, LocalErrorEngineRequestShape)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("engine attempts = %d, want 1 — a deterministic rejection must not be retried", got)
	}
}

func TestAnthropicMessages_PlainEngine500StaysA500(t *testing.T) {
	var hits atomic.Int32
	upstream := fakeEngineStatus(t, http.StatusInternalServerError, `{"error":"something went wrong"}`, &hits)
	defer upstream.Close()

	w := postAnthropicBody(t, anthropicGatewayFor(t, upstream.URL),
		`{"model":"waired/default","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if got := w.Header().Get(HeaderLocalError); got != "" {
		t.Errorf("%s = %q, want empty", HeaderLocalError, got)
	}
}

func TestAnthropicMessagesStream_EngineShapeRejectionBecomes400(t *testing.T) {
	var hits atomic.Int32
	upstream := fakeEngineStatus(t, http.StatusInternalServerError, shapeRejection500, &hits)
	defer upstream.Close()

	w := postAnthropicBody(t, anthropicGatewayFor(t, upstream.URL),
		`{"model":"waired/default","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get(HeaderLocalError); got != LocalErrorEngineRequestShape {
		t.Errorf("%s = %q, want %q", HeaderLocalError, got, LocalErrorEngineRequestShape)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("engine attempts = %d, want 1", got)
	}
}
