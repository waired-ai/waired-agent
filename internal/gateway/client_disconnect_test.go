package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestUnusableTurnReason is the seam under the verdict: the six outcomes
// that shared one reason string before waired-agent#1179, each named.
//
// A record of today's behaviour for the four engine cases; the
// client_disconnected row is a product contract (owner ruling
// 2026-09-03, waired-agent#1179) — a turn the client walked away from is
// not the engine's failure and must not be recorded as one.
func TestUnusableTurnReason(t *testing.T) {
	dead, cancel := context.WithCancel(context.Background())
	cancel()
	markup := newMarkupWatch()
	markup.add("</tool_call>")
	prose := newMarkupWatch()
	prose.add("here is the answer")

	cases := []struct {
		name                   string
		ctx                    context.Context
		usable, truncated      bool
		finishReason           string
		thinkingOpen, textOpen bool
		watch                  *markupWatch
		want                   string
	}{
		{"max_tokens is nobody's failure", context.Background(), false, false, "length", true, false, prose, ""},
		{"max_tokens wins over a dead context", dead, false, false, "length", true, false, prose, ""},
		{"the client hung up", dead, false, true, "", true, false, prose, LocalErrorClientDisconnected},
		{"the stream really did stop early", context.Background(), false, true, "", false, false, prose, reasonEngineTruncatedStream},
		{"text that is only tool-call markup", context.Background(), false, false, "stop", false, true, markup, reasonEngineMarkupOnly},
		{"reasoning and nothing after it", context.Background(), false, false, "stop", true, false, prose, reasonEngineThinkingOnly},
		{"a clean finish carrying nothing", context.Background(), false, false, "stop", false, false, prose, reasonEngineNoUsableTurn},
		{"truncated after a usable answer", context.Background(), true, true, "stop", false, true, prose, reasonEngineTruncatedStream},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := unusableTurnReason(tc.ctx, tc.usable, tc.truncated, tc.finishReason, tc.thinkingOpen, tc.textOpen, tc.watch)
			if got != tc.want {
				t.Errorf("unusableTurnReason = %q, want %q", got, tc.want)
			}
		})
	}
}

// reasoningThenSilence streams a reasoning trace, reports that it has
// done so, and then holds the connection open. It is the rc5 engine as
// measured: qwen3.5-9b wrote 855 characters of reasoning before its
// one-character answer to "what is 2+2", so a turn cut at any point is
// overwhelmingly likely to be carrying reasoning and nothing else.
func reasoningThenSilence(t *testing.T, sent chan<- struct{}) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var attempts atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f, _ := w.(http.Flusher)
		for _, chunk := range []string{"The user wants ", "an answer. Let me ", "think about it."} {
			b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"reasoning": chunk},
			}}})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
			if f != nil {
				f.Flush()
			}
		}
		select {
		case sent <- struct{}{}:
		default:
		}
		holdUntilGone(r)
	})
	return httptest.NewServer(mux), &attempts
}

// holdUntilGone keeps a fake engine's handler open until the caller
// leaves, with a ceiling so a test that fails to disconnect ends as a
// failed assertion rather than a hung httptest.Server.Close. The ceiling
// is absolute slack, not a thing under test: every case here disconnects
// in milliseconds.
func holdUntilGone(r *http.Request) {
	select {
	case <-r.Context().Done():
	case <-time.After(10 * time.Second):
	}
}

// TestAnthropicStream_ClientHangupMidStreamIsNotTheEnginesFailure is the
// rc5 shape, reproduced: the daemon logged truncated=true,
// thinking_only=true and scan_err="context canceled", and recorded it as
// engine_truncated_stream — a name that sent waired-agent#1168 looking
// for a defect in ollama.
//
// PRODUCT CONTRACT (owner ruling 2026-09-03, waired-agent#1179): a turn
// whose client walked away is recorded as the disconnect it was, is not
// re-drawn, and gets no note blaming the model in a transcript nobody
// will open.
func TestAnthropicStream_ClientHangupMidStreamIsNotTheEnginesFailure(t *testing.T) {
	sent := make(chan struct{}, 1)
	upstream, attempts := reasoningThenSilence(t, sent)
	defer upstream.Close()
	rec := &captureRecorder{}
	gw := recoveryGateway(t, upstream.URL, rec)

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		select {
		case <-sent:
		case <-time.After(10 * time.Second):
		}
		cancel()
	}()

	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages",
		strings.NewReader(recoveryRequestBody(t, true))).WithContext(ctx)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if got := attempts.Load(); got != 1 {
		t.Errorf("engine saw %d attempts, want 1: there is nobody left to re-draw for", got)
	}
	events := rec.requestsSnapshot()
	if len(events) != 1 {
		t.Fatalf("recorded %d requests, want 1", len(events))
	}
	if events[0].ErrorReason != LocalErrorClientDisconnected {
		t.Errorf("ErrorReason = %q, want %q — the engine was still working when we hung up",
			events[0].ErrorReason, LocalErrorClientDisconnected)
	}
	if text := w.Body.String(); strings.Contains(text, "could not deliver a usable reply") ||
		strings.Contains(text, "spent the whole reply on reasoning") {
		t.Errorf("a note blaming the model was written into an abandoned turn:\n%s", text)
	}
}

// TestAnthropicStream_PreCommitHangupIsNamed is the other half of
// waired-agent#1168: the client leaves before the engine has produced
// any response headers at all. The status stays 502, but the reason and
// the staged marker say who left, so the intercept's journal reads
// local_client_disconnected instead of local_status_502.
func TestAnthropicStream_PreCommitHangupIsNamed(t *testing.T) {
	mux := http.NewServeMux()
	var reached atomic.Int32
	mux.HandleFunc("/v1/chat/completions", func(_ http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		holdUntilGone(r)
	})
	upstream := httptest.NewServer(mux)
	defer upstream.Close()
	rec := &captureRecorder{}
	gw := recoveryGateway(t, upstream.URL, rec)

	// Cancelled before the handler runs: the leg fails on the dead context
	// inside postToEngine, which is the pre-commit exit this covers, and
	// the fake engine is never reached — so nothing has to be torn down
	// and the case does not wait on a socket to notice.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages",
		strings.NewReader(recoveryRequestBody(t, true))).WithContext(ctx)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	events := rec.requestsSnapshot()
	if len(events) != 1 {
		t.Fatalf("recorded %d requests, want 1", len(events))
	}
	if events[0].ErrorReason != LocalErrorClientDisconnected {
		t.Errorf("ErrorReason = %q, want %q", events[0].ErrorReason, LocalErrorClientDisconnected)
	}
	if got := w.Header().Get(HeaderLocalError); got != LocalErrorClientDisconnected {
		t.Errorf("%s = %q, want %q — the intercept reads this to name the reason",
			HeaderLocalError, got, LocalErrorClientDisconnected)
	}
	if got := reached.Load(); got != 0 {
		t.Errorf("the engine was reached %d times; this case is about the leg that never got there", got)
	}
}

// TestAnthropicStream_ACleanFinishCarryingNothing covers the fourth way
// out of the verdict: the engine ended normally and sent no reasoning,
// no text and no tool call. Filed under truncation before
// waired-agent#1179, though nothing was truncated.
func TestAnthropicStream_ACleanFinishCarryingNothing(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{}, "finish_reason": "stop",
		}}})
		_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
	})
	upstream := httptest.NewServer(mux)
	defer upstream.Close()
	rec := &captureRecorder{}
	gw := recoveryGateway(t, upstream.URL, rec)

	serveRecovery(t, gw, recoveryRequestBody(t, true))

	events := rec.requestsSnapshot()
	if len(events) != 1 {
		t.Fatalf("recorded %d requests, want 1", len(events))
	}
	if events[0].ErrorReason != reasonEngineNoUsableTurn {
		t.Errorf("ErrorReason = %q, want %q — nothing about this stream was truncated",
			events[0].ErrorReason, reasonEngineNoUsableTurn)
	}
}
