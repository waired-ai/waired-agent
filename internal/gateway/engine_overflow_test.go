package gateway

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
)

// vLLM v0.29.0's refusal of a prompt longer than its max_model_len
// (vllm/renderers/params.py): a 400 whose message the gateway's approximate
// count let through. Said as the engine says it, Claude Code shows a raw API
// error and never compacts; said as the gateway's own over-window 400, it
// compacts and sends the turn again (waired-ai/waired#1481 item 4, measured
// on Claude Code 2.1.278 for #1482). Record of today's behaviour.
const vllmOverflow400 = `{"object":"error","message":"This model's maximum context length is 40960 tokens. However, you requested 8192 output tokens and your prompt contains 39000 input tokens, for a total of 47192 tokens. Please reduce the length of the input prompt or the number of requested output tokens.","type":"BadRequestError","param":"input_tokens","code":400}`

func TestAnthropicMessages_EngineOverflowIsTheOverflow400(t *testing.T) {
	for _, stream := range []bool{false, true} {
		var hits atomic.Int32
		upstream := fakeEngineStatus(t, http.StatusBadRequest, vllmOverflow400, &hits)
		body := `{"model":"waired/default","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
		if stream {
			body = `{"model":"waired/default","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
		}
		w := postAnthropicBody(t, anthropicGatewayFor(t, upstream.URL), body)
		upstream.Close()
		if w.Code != http.StatusBadRequest {
			t.Fatalf("stream=%v: status %d, want 400; body %s", stream, w.Code, w.Body.String())
		}
		var env anthropicErrorEnvelope
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("stream=%v: decode: %v", stream, err)
		}
		if env.Error.Type != "invalid_request_error" || env.Error.Message != contextOverflowToken {
			t.Errorf("stream=%v: error %+v, want invalid_request_error / %q", stream, env.Error, contextOverflowToken)
		}
		if got := w.Header().Get(HeaderLocalError); got != LocalErrorContextOverflow {
			t.Errorf("stream=%v: %s = %q, want %q", stream, HeaderLocalError, got, LocalErrorContextOverflow)
		}
		if got := hits.Load(); got != 1 {
			t.Errorf("stream=%v: engine attempts %d, want 1", stream, got)
		}
	}
}

func TestIsEngineContextOverflow(t *testing.T) {
	if !IsEngineContextOverflow(vllmOverflow400) {
		t.Error("vLLM's refusal not recognised")
	}
	for _, other := range []string{shapeRejection500, `{"error":"something went wrong"}`, ""} {
		if IsEngineContextOverflow(other) {
			t.Errorf("%q read as an over-window refusal", other)
		}
	}
}

// The OpenAI surface answers the same refusal as its own over-window 400
// (code context_length_exceeded, the engine's words kept), and stages the
// header a relaying waired node turns into the Anthropic token.
func TestOpenAIChatCompletions_EngineOverflowIsTheOverflow400(t *testing.T) {
	var hits atomic.Int32
	upstream := fakeEngineStatus(t, http.StatusBadRequest, vllmOverflow400, &hits)
	defer upstream.Close()
	w := postOpenAIBody(t, openAIGatewayFor(t, upstream.URL),
		`{"model":"waired/default","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400; body %s", w.Code, w.Body.String())
	}
	var env openAIErrorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if env.Error.Code != "context_length_exceeded" || env.Error.Type != "invalid_request_error" {
		t.Errorf("error %+v, want invalid_request_error / context_length_exceeded", env.Error)
	}
	if got := w.Header().Get(HeaderLocalError); got != LocalErrorContextOverflow {
		t.Errorf("%s = %q, want %q", HeaderLocalError, got, LocalErrorContextOverflow)
	}
}
