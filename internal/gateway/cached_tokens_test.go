package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// Cached prompt tokens (waired-agent#885).

// retryingUsageEngine truncates its first attempt and completes the
// second, reporting DIFFERENT usage for each — so a test can tell
// "attempt 2's numbers" from "both attempts summed".
func retryingUsageEngine(t *testing.T) *httptest.Server {
	t.Helper()
	var attempts atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f, _ := w.(http.Flusher)
		send := func(payload map[string]any) {
			b, _ := json.Marshal(payload)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
			if f != nil {
				f.Flush()
			}
		}
		usage := func(prompt, completion, cached int) {
			send(map[string]any{"choices": []any{}, "usage": map[string]any{
				"prompt_tokens": prompt, "completion_tokens": completion,
				"prompt_tokens_details": map[string]any{"cached_tokens": cached},
			}})
		}
		if n == 1 {
			// Abandoned: reasoning only, no content, no finish_reason —
			// the shape that makes proxyAnthropicStream retry.
			send(map[string]any{"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"reasoning": "thinking"},
			}}})
			usage(1000, 40, 100)
			return
		}
		send(map[string]any{"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"role": "assistant", "content": "hi"},
		}}})
		send(map[string]any{"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{}, "finish_reason": "stop",
		}}})
		usage(1000, 60, 900)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if f != nil {
			f.Flush()
		}
	})
	return httptest.NewServer(mux)
}

// PRODUCT CONTRACT (waired-agent#885, and waired#829 for the convention
// it follows): an abandoned attempt's cached tokens are folded in, like
// its prompt and completion tokens beside them.
//
// This is the single line in the streaming leg a later edit is most
// likely to drop, because it sits in a retry branch that most changes
// never execute. Summing is also what keeps the number readable:
// prompt_tokens is summed too, so cached <= input survives and their
// ratio stays "the fraction of prompt tokens the engine did not have to
// prefill" rather than a figure that can exceed 1.
func TestAnthropicStream_CachedTokensFoldInAnAbandonedAttempt(t *testing.T) {
	engine := retryingUsageEngine(t)
	defer engine.Close()

	rec := &captureRecorder{}
	h := NewHandlerSet(Deps{HTTPClient: http.DefaultClient, Recorder: rec})
	rr := h.startRequest(nil, "anthropic")
	rr.ev.Model = "qwen3-8b-instruct"
	w := httptest.NewRecorder()
	h.proxyAnthropicStream(context.Background(), http.DefaultClient, engine.URL,
		[]byte(ttfbStreamBody), "waired/default", nil, w, waitPolicy{}, localSel, rr, nil)
	rr.finish()

	evs := rec.requestsSnapshot()
	if len(evs) != 1 {
		t.Fatalf("recorded %d events, want 1", len(evs))
	}
	ev := evs[0]
	if ev.InputTokens != 2000 {
		t.Errorf("InputTokens = %d, want 2000 (both attempts)", ev.InputTokens)
	}
	if ev.CachedInputTokens != 1000 {
		t.Errorf("CachedInputTokens = %d, want 1000 (100 from the abandoned attempt + 900 from the surviving one) — "+
			"900 means the retry fold is missing the cached counter", ev.CachedInputTokens)
	}
	if ev.CachedInputTokens > ev.InputTokens {
		t.Errorf("cached %d > input %d; the ratio is no longer a fraction", ev.CachedInputTokens, ev.InputTokens)
	}
}

// All three decoders share no code, so each is asserted separately:
// Anthropic non-streaming decodes OpenAIResponse, Anthropic streaming
// decodes per-chunk, and the OpenAI passthrough sniffs bytes it forwards
// without parsing.
func TestGateway_CachedTokensOnEveryLeg(t *testing.T) {
	for _, tc := range []struct {
		name    string
		path    string
		payload map[string]any
	}{
		{"openai non-stream", "/v1/chat/completions", map[string]any{
			"model": "waired/default", "messages": []map[string]string{{"role": "user", "content": "hi"}}}},
		{"openai stream", "/v1/chat/completions", map[string]any{
			"model": "waired/default", "stream": true,
			"messages": []map[string]string{{"role": "user", "content": "hi"}}}},
		{"anthropic non-stream", "/anthropic/v1/messages", map[string]any{
			"model": "waired/default", "max_tokens": 64,
			"messages": []map[string]string{{"role": "user", "content": "hi"}}}},
		{"anthropic stream", "/anthropic/v1/messages", map[string]any{
			"model": "waired/default", "max_tokens": 64, "stream": true,
			"messages": []map[string]string{{"role": "user", "content": "hi"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := cachedTokensEngine(t)
			defer engine.Close()
			rec := &captureRecorder{}
			gw := newMeteringGateway(t, engine.URL, rec, nil)

			if w := postJSON(t, gw, tc.path, tc.payload); w.Code != http.StatusOK {
				t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
			}
			evs := rec.requestsSnapshot()
			if len(evs) != 1 {
				t.Fatalf("recorded %d events, want 1", len(evs))
			}
			if evs[0].InputTokens != 11 {
				t.Errorf("InputTokens = %d, want 11", evs[0].InputTokens)
			}
			if evs[0].CachedInputTokens != 9 {
				t.Errorf("CachedInputTokens = %d, want 9 — this leg drops the engine's breakdown", evs[0].CachedInputTokens)
			}
		})
	}
}

// RECORD OF TODAY'S BEHAVIOUR: an engine that reports usage without a
// breakdown — every ollama host — leaves the field unobserved rather
// than recording a zero that would read as "nothing was cached".
func TestGateway_NoBreakdownLeavesCachedUnobserved(t *testing.T) {
	engine := meteringEngine(t, nil)
	defer engine.Close()
	rec := &captureRecorder{}
	gw := newMeteringGateway(t, engine.URL, rec, nil)

	if w := postJSON(t, gw, "/anthropic/v1/messages", map[string]any{
		"model": "waired/default", "max_tokens": 64, "stream": true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}); w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	evs := rec.requestsSnapshot()
	if len(evs) != 1 {
		t.Fatalf("recorded %d events, want 1", len(evs))
	}
	if evs[0].InputTokens != 11 {
		t.Errorf("InputTokens = %d, want 11 — the tokens must still be recorded", evs[0].InputTokens)
	}
	if evs[0].CachedInputTokens != 0 {
		t.Errorf("CachedInputTokens = %d, want 0 for an engine that reports no breakdown", evs[0].CachedInputTokens)
	}
}

// cachedTokensEngine mirrors meteringEngine but reports the vLLM
// prompt-token breakdown alongside the same 11/7 counts, so no existing
// assertion on those numbers moves.
func cachedTokensEngine(t *testing.T) *httptest.Server {
	t.Helper()
	const usage = `"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,` +
		`"prompt_tokens_details":{"cached_tokens":9}}`
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var probe struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&probe)
		if probe.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			f, _ := w.(http.Flusher)
			_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n")
			_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
			_, _ = fmt.Fprintf(w, "data: {\"choices\":[],%s}\n\n", usage)
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			if f != nil {
				f.Flush()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"chatcmpl-1","object":"chat.completion","model":"m",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],%s}`, usage)
	})
	return httptest.NewServer(mux)
}
