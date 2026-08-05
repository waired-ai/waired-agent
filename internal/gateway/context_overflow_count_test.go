package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/router"
)

// The three parts a coding-agent request is actually made of, sized so a
// miss is unmistakable: 4000 bytes ≈ 1000 approximate tokens.
const countProbePayload = 4000

func countProbeText(n int) string { return strings.Repeat("x", n) }

// TestCountOpenAIPromptTokensApprox_CountsWhatACodingSessionIsMadeOf pins
// that the estimate sees the three largest parts of a coding-agent
// request. Agreement between the requester's and the server's counters
// cannot pin this on its own: two counters that both ignore tool schemas
// agree perfectly and are both wrong, and an undercount here is exactly
// the silent head-truncation the guard exists to prevent (#465 item 5).
//
// This is a product contract: waired-ai/waired#1056 (2026-08-03 owner
// decision) makes waired responsible for erroring on an over-window
// request instead of letting the engine truncate, and an estimate blind
// to the bulk of the prompt cannot discharge that.
func TestCountOpenAIPromptTokensApprox_CountsWhatACodingSessionIsMadeOf(t *testing.T) {
	base := OpenAIRequest{
		Model:    "qwen3:8b",
		Messages: []OpenAIMessage{{Role: "user", Content: "hi"}},
	}
	baseline := countOpenAIRequest(t, base)

	payload := countProbeText(countProbePayload)
	const wantAtLeast = countProbePayload / 4 // approxTokenCount's ratio

	cases := []struct {
		name string
		with func(OpenAIRequest) OpenAIRequest
	}{
		{
			// A coding agent declares its whole tool surface on every
			// turn; the schemas alone routinely run to tens of KB.
			name: "tool schemas",
			with: func(r OpenAIRequest) OpenAIRequest {
				r.Tools = []OpenAITool{{
					Type: "function",
					Function: OpenAIToolFunction{
						Name:       "read_file",
						Parameters: json.RawMessage(`{"d":"` + payload + `"}`),
					},
				}}
				return r
			},
		},
		{
			name: "tool call arguments",
			with: func(r OpenAIRequest) OpenAIRequest {
				r.Messages = append(r.Messages, OpenAIMessage{
					Role: "assistant",
					ToolCalls: []OpenAIToolCall{{
						ID: "tc_1", Type: "function",
						Function: OpenAIToolCallFunction{
							Name:      "write_file",
							Arguments: `{"text":"` + payload + `"}`,
						},
					}},
				})
				return r
			},
		},
		{
			// The single largest contributor in a real session: file
			// reads and command output come back as tool results.
			name: "tool result content",
			with: func(r OpenAIRequest) OpenAIRequest {
				r.Messages = append(r.Messages, OpenAIMessage{
					Role: "tool", ToolCallID: "tc_1", Content: payload,
				})
				return r
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := countOpenAIRequest(t, tc.with(base))
			if delta := got - baseline; delta < wantAtLeast {
				t.Errorf("%s added %d tokens to the estimate, want ≥ %d — "+
					"an over-window prompt built from this reaches the engine and loses its head",
					tc.name, delta, wantAtLeast)
			}
		})
	}
}

// TestCountOpenAIPromptTokensApprox_CountsArrayContent: the loopback and
// data-plane listeners serve arbitrary OpenAI clients, and the
// chat-completions schema lets content be an array of parts. A body the
// counter cannot read counts 0 and the guard fails open, so the array
// form has to be counted rather than skipped.
func TestCountOpenAIPromptTokensApprox_CountsArrayContent(t *testing.T) {
	payload := countProbeText(countProbePayload)
	body := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"` + payload + `"}]}]}`)

	if got, want := CountOpenAIPromptTokensApprox(body), countProbePayload/4; got < want {
		t.Errorf("array-form content counted %d tokens, want ≥ %d", got, want)
	}
	if CountOpenAIPromptTokensApprox([]byte("not json")) != 0 {
		t.Error("an unparseable body must count 0 so the guard fails open")
	}
}

// TestCountTokensApprox_CountsTheBytesTheGuardForwards: the Anthropic
// count and the peer's count are the same number because they count the
// same bytes — the converted request this gateway is about to send. The
// old pair of hand-written walks could drift, and a drift means a prompt
// the requester passed is refused by the peer, or the reverse
// (waired-agent#436).
func TestCountTokensApprox_CountsTheBytesTheGuardForwards(t *testing.T) {
	payload := countProbeText(countProbePayload)
	req := AnthropicRequest{
		Model:     "claude-sonnet-4",
		MaxTokens: 64,
		System:    json.RawMessage(`"you are a coding agent"`),
		Tools: []AnthropicTool{{
			Name:        "read_file",
			Description: "read a file",
			InputSchema: json.RawMessage(`{"d":"` + payload + `"}`),
		}},
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"read main.go"`)},
			{Role: "assistant", Content: json.RawMessage(
				`[{"type":"tool_use","id":"tu_1","name":"read_file","input":{"path":"main.go"}}]`)},
			{Role: "user", Content: json.RawMessage(
				`[{"type":"tool_result","tool_use_id":"tu_1","content":"` + payload + `"}]`)},
		},
	}

	oai, err := AnthropicToOpenAI(req)
	if err != nil {
		t.Fatalf("AnthropicToOpenAI: %v", err)
	}
	encoded, err := json.Marshal(oai)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, want := CountTokensApprox(req), CountOpenAIPromptTokensApprox(encoded)
	if got != want {
		t.Errorf("anthropic count %d vs the forwarded body's count %d; the two guards "+
			"would disagree about the same conversation", got, want)
	}
	// And the count is dominated by the two payloads, not by the prose.
	if min := 2 * countProbePayload / 4; got < min {
		t.Errorf("count = %d, want ≥ %d: the tool schema and the tool result "+
			"are most of this request", got, min)
	}
}

// TestAnthropicMessages_OverflowCountsToolSchemasAndResults: the guard
// fires on a conversation whose *messages* are short and whose bulk is
// the tool surface and the tool output. That request used to pass the
// guard and reach the engine.
func TestAnthropicMessages_OverflowCountsToolSchemasAndResults(t *testing.T) {
	sel := &fakeSelector{sel: router.Selection{
		Runtime: "ollama", EngineModel: "qwen3:8b-q4_K_M", ModelID: "qwen3-8b-instruct",
	}}
	gw := anthropicGatewayWithWindow(t, sel, "", nil, func(string) int { return 100 })

	payload := countProbeText(countProbePayload)
	for _, tc := range []struct {
		name string
		body string
	}{
		{
			name: "tool schemas",
			body: `{"model":"claude-sonnet-4","max_tokens":64,` +
				`"tools":[{"name":"read_file","input_schema":{"d":"` + payload + `"}}],` +
				`"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "tool result",
			body: `{"model":"claude-sonnet-4","max_tokens":64,` +
				`"messages":[{"role":"user","content":[{"type":"tool_result",` +
				`"tool_use_id":"tu_1","content":"` + payload + `"}]}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewBufferString(tc.body))
			r.RemoteAddr = "127.0.0.1:1"
			w := httptest.NewRecorder()
			gw.Handler().ServeHTTP(w, r)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s, want 400 — %s is most of this prompt",
					w.Code, w.Body.String(), tc.name)
			}
			if got := w.Header().Get(HeaderLocalError); got != LocalErrorContextOverflow {
				t.Errorf("%s = %q, want %q", HeaderLocalError, got, LocalErrorContextOverflow)
			}
		})
	}
}

func countOpenAIRequest(t *testing.T, req OpenAIRequest) int {
	t.Helper()
	encoded, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return CountOpenAIPromptTokensApprox(encoded)
}
