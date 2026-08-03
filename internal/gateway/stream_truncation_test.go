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

// parseFailingEngine answers with the 500 ollama returns when its own
// tool parser rejects what the model emitted, for the first `failures`
// attempts.
func parseFailingEngine(t *testing.T, failures int, reply string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var attempts atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if int(attempts.Add(1)) <= failures {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"XML syntax error on line 8: ` +
				`element <function> closed by </parameter>","type":"api_error"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		msg, _ := json.Marshal(map[string]any{
			"id": "chatcmpl-442",
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": reply},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"prompt_tokens": 7, "completion_tokens": 11},
		})
		_, _ = w.Write(msg)
	})
	return httptest.NewServer(mux), &attempts
}

// The non-streaming path retries the same condition as the streaming
// one. It is not merely symmetry for its own sake: the probe drives both
// transports and #440 pools them as two samples of one thing, so a retry
// on only one would quietly make that untrue.
func TestAnthropicNonStream_RetriesAnEngineParseFailure(t *testing.T) {
	upstream, attempts := parseFailingEngine(t, 1, xmlFunctionTranscript)
	defer upstream.Close()
	rec := &captureRecorder{}
	gw := recoveryGateway(t, upstream.URL, rec)

	body := serveRecovery(t, gw, recoveryRequestBody(t, false)).Body.String()

	if got := attempts.Load(); got != 2 {
		t.Errorf("engine saw %d attempts, want 2", got)
	}
	if !strings.Contains(body, `"type":"tool_use"`) {
		t.Errorf("the retry's tool call never reached the client:\n%s", body)
	}
	if events := rec.requestsSnapshot(); len(events) != 1 || events[0].ErrorReason != "" {
		t.Error("a turn that succeeded on retry was recorded as a failure")
	}
}

// Retries stop at the bound, and the client still gets the engine's own
// message — the non-streaming path never hid anything, and this must not
// start hiding it.
func TestAnthropicNonStream_GivesUpAndStillReportsTheError(t *testing.T) {
	upstream, attempts := parseFailingEngine(t, 99, "")
	defer upstream.Close()
	gw := recoveryGateway(t, upstream.URL, &captureRecorder{})

	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages",
		strings.NewReader(recoveryRequestBody(t, false)))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if got, want := attempts.Load(), int32(maxStreamRetries+1); got != want {
		t.Errorf("engine saw %d attempts, want %d", got, want)
	}
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want the engine's own 500", w.Code)
	}
	if !strings.Contains(w.Body.String(), "XML syntax error") {
		t.Errorf("the engine's message was swallowed:\n%s", w.Body.String())
	}
}

// An engine that is simply down must not be hammered: the retry exists
// for a bad draw from the model, and an outage is not one.
func TestAnthropicNonStream_DoesNotRetryAnOutage(t *testing.T) {
	var attempts atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"model requires more system memory than is available"}}`))
	})
	upstream := httptest.NewServer(mux)
	defer upstream.Close()
	gw := recoveryGateway(t, upstream.URL, &captureRecorder{})

	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages",
		strings.NewReader(recoveryRequestBody(t, false)))
	r.RemoteAddr = "127.0.0.1:1"
	gw.Handler().ServeHTTP(httptest.NewRecorder(), r)

	if got := attempts.Load(); got != 1 {
		t.Errorf("engine saw %d attempts, want 1 — an outage is not a bad draw", got)
	}
}

// silentEngine reports a clean finish and delivers reasoning only. Not a
// truncation — the engine says the turn ended normally — but from the
// client's seat it is the same stall, so it is the same failed draw.
//
// Measured: 24 engine-direct draws of qwen3.5:9b on the fixture produced
// 17 clean tool calls and 7 truncations and NOT ONE turn where the model
// reasoned and then chose silence. This shape is not the model declining
// to answer; it is another lost call wearing a clean finish.
func silentEngine(t *testing.T, failures int, reply, finish string) (*httptest.Server, *atomic.Int32) {
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
		for _, chunk := range chopForStreaming("The user wants /etc/hostname, so I should call Read.") {
			send(map[string]any{"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"reasoning": chunk},
			}}})
		}
		if int(n) > failures {
			for _, chunk := range chopForStreaming(reply) {
				send(map[string]any{"choices": []any{map[string]any{
					"index": 0, "delta": map[string]any{"content": chunk},
				}}})
			}
		}
		send(map[string]any{"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{}, "finish_reason": finish,
		}}})
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if f != nil {
			f.Flush()
		}
	})
	return httptest.NewServer(mux), &attempts
}

// A turn carrying only reasoning is re-drawn even though the engine
// declared a normal end. The retry condition is "produced no answer",
// not "the stream broke" — the first version keyed on the break and this
// case walked straight through it.
func TestAnthropicStream_RetriesAnAnswerlessCleanFinish(t *testing.T) {
	upstream, attempts := silentEngine(t, 1, xmlFunctionTranscript, "stop")
	defer upstream.Close()
	gw := recoveryGateway(t, upstream.URL, &captureRecorder{})

	sse := serveRecovery(t, gw, recoveryRequestBody(t, true)).Body.String()

	if got := attempts.Load(); got != 2 {
		t.Errorf("engine saw %d attempts, want 2", got)
	}
	if !strings.Contains(sse, `"type":"tool_use"`) {
		t.Errorf("the retry's tool call never reached the client:\n%s", sse)
	}
}

func TestAnthropicStream_NotesAnAnswerlessCleanFinish(t *testing.T) {
	upstream, attempts := silentEngine(t, 99, "", "stop")
	defer upstream.Close()
	rec := &captureRecorder{}
	gw := recoveryGateway(t, upstream.URL, rec)

	sse := serveRecovery(t, gw, recoveryRequestBody(t, true)).Body.String()

	if got, want := attempts.Load(), int32(maxStreamRetries+1); got != want {
		t.Errorf("engine saw %d attempts, want %d", got, want)
	}
	if !strings.Contains(collectTextDeltas(t, sse), "could not deliver a usable reply") {
		t.Errorf("a thinking-only turn was passed off as an answer:\n%s", sse)
	}
	if events := rec.requestsSnapshot(); len(events) != 1 || events[0].ErrorReason != "engine_truncated_stream" {
		t.Error("a thinking-only turn was metered as a success")
	}
}

// max_tokens is the one answerless turn that is nobody's failure: the
// engine did what the request asked. It must not be re-drawn — that
// would spend the same budget again — and it keeps its own note, which
// unlike the other one names something the reader can change.
//
// The old guard also required NO thinking block, so a model that spent
// its whole budget reasoning fell through it. Same bug class as #442:
// a guard keyed on emptiness that a thinking block defeats.
func TestAnthropicStream_MaxTokensIsNotRetriedAndKeepsItsOwnNote(t *testing.T) {
	upstream, attempts := silentEngine(t, 99, "", "length")
	defer upstream.Close()
	rec := &captureRecorder{}
	gw := recoveryGateway(t, upstream.URL, rec)

	sse := serveRecovery(t, gw, recoveryRequestBody(t, true)).Body.String()

	if got := attempts.Load(); got != 1 {
		t.Errorf("engine saw %d attempts, want 1 — a spent budget is not a bad draw", got)
	}
	text := collectTextDeltas(t, sse)
	if !strings.Contains(text, "max_tokens") {
		t.Errorf("a budget exhaustion was not reported as one:\n%q", text)
	}
	if strings.Contains(text, "Switching to a different model") {
		t.Errorf("the wrong note: this one is fixed by raising max_tokens:\n%q", text)
	}
	if events := rec.requestsSnapshot(); len(events) != 1 || events[0].ErrorReason != "" {
		t.Error("max_tokens was recorded as a gateway failure")
	}
}
