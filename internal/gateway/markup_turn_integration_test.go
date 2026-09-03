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

// markupOnlyEngine streams `reply` as ordinary content and then ends the
// turn cleanly: a real finish_reason and a [DONE]. Nothing about the
// stream is broken — the transport did its job and the model's answer is
// the problem, which is why every earlier guard passes it.
//
// This is the measured shape behind waired-agent#786's content failure:
// two 200 responses in the journal, no retry, exit 0, and
// `<response>` / `</function>` / `</tool_call>` on the user's screen.
func markupOnlyEngine(t *testing.T, reply string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var attempts atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
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
		for _, chunk := range chopForStreaming(reply) {
			send(map[string]any{"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"content": chunk},
			}}})
		}
		send(map[string]any{"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{}, "finish_reason": "stop",
		}}})
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		if f != nil {
			f.Flush()
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &attempts
}

// TestAnthropicStream_MarkupOnlyTurnIsNotUsable is the waired-agent#786
// content regression: a turn made entirely of leaked tool-call markup
// used to be metered and returned as a served turn, and the CLI exited 0
// with the tags on screen.
//
// The tags themselves still reach the client — an Anthropic text_delta
// cannot be un-sent — but the turn is now named for what it is, in the
// note the user reads and in the record an operator can search.
func TestAnthropicStream_MarkupOnlyTurnIsNotUsable(t *testing.T) {
	upstream, attempts := markupOnlyEngine(t, "<response>\n</function>\n</tool_call>\n")
	rec := &captureRecorder{}
	gw := recoveryGateway(t, upstream.URL, rec)

	sse := serveRecovery(t, gw, recoveryRequestBody(t, true)).Body.String()

	// Committed text is never re-drawn (the #442 rule), so the engine is
	// asked exactly once even though the turn is unusable.
	if got := attempts.Load(); got != 1 {
		t.Errorf("engine saw %d attempts, want 1 — committed text must not be re-drawn", got)
	}
	text := collectTextDeltas(t, sse)
	if !strings.Contains(text, "could not deliver a usable reply") {
		t.Errorf("a markup-only turn was passed off as an answer:\n%q", text)
	}
	events := rec.requestsSnapshot()
	if len(events) != 1 {
		t.Fatalf("recorded %d requests, want 1", len(events))
	}
	if events[0].ErrorReason != reasonEngineMarkupOnly {
		t.Errorf("ErrorReason = %q — a markup-only turn is recorded as a success", events[0].ErrorReason)
	}
}

// TestAnthropicStream_AnAnswerWithAStrayTagIsStillAnAnswer is the guard
// on the other side. The verdict is subtractive, so any prose at all
// keeps the turn usable; without this the fix would start appending a
// failure note to replies that were fine.
func TestAnthropicStream_AnAnswerWithAStrayTagIsStillAnAnswer(t *testing.T) {
	upstream, _ := markupOnlyEngine(t, "The file holds one line.\n</tool_call>")
	rec := &captureRecorder{}
	gw := recoveryGateway(t, upstream.URL, rec)

	sse := serveRecovery(t, gw, recoveryRequestBody(t, true)).Body.String()

	text := collectTextDeltas(t, sse)
	if !strings.Contains(text, "The file holds one line.") {
		t.Errorf("the answer was dropped:\n%q", text)
	}
	if strings.Contains(text, "could not deliver a usable reply") {
		t.Errorf("an answer carrying one stray tag was reported as a failure:\n%q", text)
	}
	if events := rec.requestsSnapshot(); len(events) != 1 || events[0].ErrorReason != "" {
		t.Errorf("a served turn was metered as a failure: %+v", events)
	}
}
