//go:build integration

package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/router"
)

// TestAnthropicReasoning_RealOllama drives the real Anthropic↔OpenAI
// translation against a live Ollama serving a thinking model, proving
// #644 end-to-end: message.reasoning surfaces as Anthropic thinking
// blocks, reasoning-only truncations never return an empty turn, and
// streamed responses report real token usage.
//
// Env-gated (no live backend in the default CI lane):
//
//	WAIRED_OLLAMA_URL     e.g. http://127.0.0.1:11434
//	WAIRED_THINKING_MODEL e.g. qwen3:1.7b (must emit message.reasoning)
//
// Run: WAIRED_OLLAMA_URL=http://127.0.0.1:11434 WAIRED_THINKING_MODEL=qwen3:1.7b \
//
//	go test -tags integration -run TestAnthropicReasoning_RealOllama ./internal/gateway/ -v
func TestAnthropicReasoning_RealOllama(t *testing.T) {
	ollamaURL := os.Getenv("WAIRED_OLLAMA_URL")
	model := os.Getenv("WAIRED_THINKING_MODEL")
	if ollamaURL == "" || model == "" {
		t.Skip("set WAIRED_OLLAMA_URL and WAIRED_THINKING_MODEL to run")
	}

	newGateway := func() *Server {
		sel := &fakeSelector{sel: router.Selection{Runtime: "ollama", EngineModel: model}}
		return anthropicGatewayUnderTest(t, sel, ollamaURL)
	}
	post := func(t *testing.T, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewBufferString(body))
		r.RemoteAddr = "127.0.0.1:1"
		w := httptest.NewRecorder()
		newGateway().Handler().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
		}
		return w
	}

	// 1. Non-stream, generous budget: reasoning → thinking block, real usage.
	t.Run("nonstream_thinking_and_usage", func(t *testing.T) {
		w := post(t, `{"model":"waired/default","max_tokens":512,`+
			`"messages":[{"role":"user","content":"What is 17*23? Think briefly, then answer."}]}`)
		var resp AnthropicResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		var hasThinking bool
		for _, b := range resp.Content {
			if b.Type == "thinking" && b.Thinking != "" {
				hasThinking = true
			}
		}
		if !hasThinking {
			t.Fatalf("no thinking block in response: %s", w.Body.String())
		}
		if resp.Content[0].Type != "thinking" {
			t.Errorf("first block should be thinking, got %q", resp.Content[0].Type)
		}
		if resp.Usage.OutputTokens <= 0 {
			t.Errorf("output_tokens = %d, want > 0", resp.Usage.OutputTokens)
		}
		t.Logf("non-stream: %d blocks, output_tokens=%d", len(resp.Content), resp.Usage.OutputTokens)
	})

	// 2. Non-stream, tiny budget: reasoning eats the whole budget. The
	// turn must NOT be an empty content:[] (the overnight-stall shape).
	t.Run("nonstream_reasoning_only_not_empty", func(t *testing.T) {
		w := post(t, `{"model":"waired/default","max_tokens":24,`+
			`"messages":[{"role":"user","content":"What is 17*23? Think step by step."}]}`)
		var resp AnthropicResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp.Content) == 0 {
			t.Fatalf("reasoning-only turn returned empty content (stall shape): %s", w.Body.String())
		}
		if resp.StopReason != "max_tokens" {
			t.Errorf("stop_reason = %q, want max_tokens", resp.StopReason)
		}
		t.Logf("reasoning-only: %d blocks, first=%q stop=%q", len(resp.Content), resp.Content[0].Type, resp.StopReason)
	})

	// 3. Stream: thinking_delta events + non-zero output_tokens in the
	// final message_delta (message_start always carries output_tokens:0,
	// so parse the message_delta event specifically).
	t.Run("stream_thinking_and_usage", func(t *testing.T) {
		w := post(t, `{"model":"waired/default","max_tokens":512,"stream":true,`+
			`"messages":[{"role":"user","content":"What is 17*23? Think briefly, then answer."}]}`)
		out := w.Body.String()
		if !strings.Contains(out, `"type":"thinking_delta"`) {
			t.Errorf("stream missing thinking_delta events:\n%s", truncate(out))
		}
		if !strings.Contains(out, `"type":"thinking"`) {
			t.Errorf("stream missing thinking content_block_start:\n%s", truncate(out))
		}
		if got := messageDeltaOutputTokens(t, out); got <= 0 {
			t.Errorf("message_delta output_tokens = %d, want > 0:\n%s", got, truncate(out))
		} else {
			t.Logf("stream produced thinking_delta + message_delta output_tokens=%d (len=%d)", got, len(out))
		}
	})
}

// messageDeltaOutputTokens extracts usage.output_tokens from the
// message_delta event of an Anthropic SSE stream.
func messageDeltaOutputTokens(t *testing.T, sse string) int {
	t.Helper()
	for _, block := range strings.Split(sse, "\n\n") {
		if !strings.Contains(block, "event: message_delta") {
			continue
		}
		for _, line := range strings.Split(block, "\n") {
			data, ok := strings.CutPrefix(line, "data: ")
			if !ok {
				continue
			}
			var ev struct {
				Usage struct {
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				t.Fatalf("parse message_delta: %v", err)
			}
			return ev.Usage.OutputTokens
		}
	}
	t.Fatalf("no message_delta event found in stream")
	return 0
}

func truncate(s string) string {
	if len(s) > 1500 {
		return s[:1500] + "…"
	}
	return s
}
