package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// truncatingEngine is an engine that gives up part-way through the
// stream, the way ollama 0.31.1 does when its own tool parser rejects
// what the model emitted (#442).
//
// The shape is measured, not invented: 7 of 12 turns of qwen3.5:9b on
// the agentgrade fixture ended with reasoning delivered in full, no
// content, no error frame, no finish_reason and no [DONE] — the body
// simply closes. Nothing in the stream says anything went wrong, which
// is the entire defect.
//
// failures is how many opening attempts truncate before one succeeds.
func truncatingEngine(t *testing.T, failures int, reply string) (*httptest.Server, *atomic.Int32) {
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
		// Reasoning always arrives in full — that is why the turn is not
		// empty, and why a guard keyed on emptiness would miss it.
		for _, chunk := range chopForStreaming("The user wants /etc/hostname, so I should call Read.") {
			send(map[string]any{"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"reasoning": chunk},
			}}})
		}
		if int(n) <= failures {
			// The measured tail: a final chunk carrying nothing, a null
			// finish_reason, and then silence.
			send(map[string]any{"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"role": "assistant", "content": ""},
				"finish_reason": nil,
			}}})
			return
		}
		for _, chunk := range chopForStreaming(reply) {
			send(map[string]any{"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"content": chunk},
			}}})
		}
		send(map[string]any{"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{}, "finish_reason": "stop",
		}}})
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if f != nil {
			f.Flush()
		}
	})
	return httptest.NewServer(mux), &attempts
}

// Product contract: a turn the engine abandoned before saying how it
// ended is re-drawn, and the client is handed the attempt that worked.
func TestAnthropicStream_RetriesATruncatedTurn(t *testing.T) {
	upstream, attempts := truncatingEngine(t, 1, xmlFunctionTranscript)
	defer upstream.Close()
	rec := &captureRecorder{}
	gw := recoveryGateway(t, upstream.URL, rec)

	sse := serveRecovery(t, gw, recoveryRequestBody(t, true)).Body.String()

	if got := attempts.Load(); got != 2 {
		t.Errorf("engine saw %d attempts, want 2", got)
	}
	if !strings.Contains(sse, `"type":"tool_use"`) {
		t.Errorf("the retry's tool call never reached the client:\n%s", sse)
	}
	if !strings.Contains(sse, `"stop_reason":"tool_use"`) {
		t.Errorf("stop_reason is not tool_use:\n%s", sse)
	}
	if strings.Contains(sse, streamFailureNote("qwen2.5-coder-7b-instruct", 2)) {
		t.Error("the failure note was emitted for a turn that succeeded on retry")
	}
	// The retry's reasoning is suppressed, so the turn carries ONE chain
	// of thought. Counting blocks would not show this — the gateway opens
	// a thinking block once and a second attempt's deltas would land in
	// the same one — so count the trace itself.
	if n := strings.Count(collectThinkingDeltas(t, sse), "The user wants /etc/hostname"); n != 1 {
		t.Errorf("the reasoning trace appears %d times in one turn, want 1:\n%s",
			n, collectThinkingDeltas(t, sse))
	}
	// Both attempts really ran, so both are metered.
	events := rec.requestsSnapshot()
	if len(events) != 1 {
		t.Fatalf("recorded %d requests, want 1", len(events))
	}
	if events[0].ErrorReason != "" {
		t.Errorf("a turn that succeeded on retry was recorded as %q", events[0].ErrorReason)
	}
}

// Product contract: when every attempt is abandoned, the client is TOLD.
// Before #442 this turn arrived as stop_reason end_turn with nothing in
// it — indistinguishable from a model that chose to say nothing, and
// recorded as a success.
func TestAnthropicStream_GivesUpWithAVisibleNote(t *testing.T) {
	upstream, attempts := truncatingEngine(t, 99, xmlFunctionTranscript)
	defer upstream.Close()
	rec := &captureRecorder{}
	gw := recoveryGateway(t, upstream.URL, rec)

	sse := serveRecovery(t, gw, recoveryRequestBody(t, true)).Body.String()

	if got, want := attempts.Load(), int32(maxStreamRetries+1); got != want {
		t.Errorf("engine saw %d attempts, want %d", got, want)
	}
	text := collectTextDeltas(t, sse)
	if !strings.Contains(text, "could not deliver a usable reply") {
		t.Errorf("the client was told nothing:\n%q", text)
	}
	// Named twice over: the reader who never picked a model by name needs
	// "this model", and the reader running several needs the id.
	if !strings.Contains(text, "this model") || !strings.Contains(text, "qwen2.5-coder-7b-instruct") {
		t.Errorf("the note does not identify the model:\n%q", text)
	}
	if !strings.Contains(text, fmt.Sprintf("%d attempts", maxStreamRetries+1)) {
		t.Errorf("the note does not say how many attempts were made:\n%q", text)
	}
	for _, leak := range []string{"engine", "upstream", "parser", "SSE", "finish_reason"} {
		if strings.Contains(text, leak) {
			t.Errorf("the note leaks waired vocabulary %q:\n%q", leak, text)
		}
	}
	// stop_reason stays a value every client already knows.
	if !strings.Contains(sse, `"stop_reason":"end_turn"`) {
		t.Errorf("stop_reason is not end_turn:\n%s", sse)
	}
	events := rec.requestsSnapshot()
	if len(events) != 1 {
		t.Fatalf("recorded %d requests, want 1", len(events))
	}
	if events[0].ErrorReason != "engine_truncated_stream" {
		t.Errorf("ErrorReason = %q, so the failure is metered as a success", events[0].ErrorReason)
	}
}

// A stream that says how it ended is not retried, however it ended. The
// signature is the ABSENCE of a verdict, not an unwelcome one.
func TestAnthropicStream_CompleteTurnIsNotRetried(t *testing.T) {
	upstream, attempts := truncatingEngine(t, 0, "The file holds one line.")
	defer upstream.Close()
	gw := recoveryGateway(t, upstream.URL, &captureRecorder{})

	sse := serveRecovery(t, gw, recoveryRequestBody(t, true)).Body.String()

	if got := attempts.Load(); got != 1 {
		t.Errorf("engine saw %d attempts, want 1", got)
	}
	if got := collectTextDeltas(t, sse); got != "The file holds one line." {
		t.Errorf("streamed text = %q", got)
	}
	if strings.Contains(sse, "could not deliver a usable reply") {
		t.Errorf("a complete turn was reported as a failure:\n%s", sse)
	}
}

// Once text has gone out there is no retrying: no SSE event un-sends a
// text_delta, and re-drawing would splice two different answers into one
// turn. The client keeps what arrived and is told the rest was lost.
func TestAnthropicStream_DoesNotRetryOnceTextIsCommitted(t *testing.T) {
	var attempts atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f, _ := w.(http.Flusher)
		for _, chunk := range chopForStreaming("Checking the file now") {
			b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"content": chunk},
			}}})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
			if f != nil {
				f.Flush()
			}
		}
		// … and then nothing: no finish_reason, no [DONE].
	})
	upstream := httptest.NewServer(mux)
	defer upstream.Close()
	rec := &captureRecorder{}
	gw := recoveryGateway(t, upstream.URL, rec)

	sse := serveRecovery(t, gw, recoveryRequestBody(t, true)).Body.String()

	if got := attempts.Load(); got != 1 {
		t.Errorf("engine saw %d attempts, want 1 — committed text must not be re-drawn", got)
	}
	text := collectTextDeltas(t, sse)
	if !strings.Contains(text, "Checking the file now") {
		t.Errorf("the text that did arrive was dropped:\n%q", text)
	}
	if !strings.Contains(text, "could not deliver a usable reply") {
		t.Errorf("a reply cut off mid-sentence was passed off as complete:\n%q", text)
	}
	if events := rec.requestsSnapshot(); len(events) != 1 || events[0].ErrorReason != "engine_truncated_stream" {
		t.Error("a truncated turn with partial text was metered as a success")
	}
}

func TestStreamFailureNote(t *testing.T) {
	if got := streamFailureNote("", 3); !strings.Contains(got, "this model could not") {
		t.Errorf("with no model id the note should still read naturally: %q", got)
	}
	if got := streamFailureNote("qwen3.5-9b", 1); !strings.Contains(got, "after 1 attempt.") {
		t.Errorf("singular attempt should not read %q", got)
	}
	if got := streamFailureNote("qwen3.5-9b", 3); !strings.Contains(got, "this model (qwen3.5-9b)") {
		t.Errorf("the note should name the model: %q", got)
	}
}

// collectThinkingDeltas concatenates the turn's thinking_delta payloads.
// Blocks cannot answer "was the reasoning duplicated" — the gateway opens
// one thinking block and a retry's deltas would land inside it — so the
// trace has to be read.
func collectThinkingDeltas(t *testing.T, sse string) string {
	t.Helper()
	var out strings.Builder
	for _, line := range strings.Split(sse, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Delta struct {
				Type     string `json:"type"`
				Thinking string `json:"thinking"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) != nil {
			continue
		}
		if ev.Type == "content_block_delta" && ev.Delta.Type == "thinking_delta" {
			out.WriteString(ev.Delta.Thinking)
		}
	}
	return out.String()
}
