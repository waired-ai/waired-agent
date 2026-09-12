package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/router"
)

// The non-streaming half of the engine wait (waired-agent#1314).
//
// The streaming legs have been held open since #837 and #952. This one could
// not use the same frame — it promised the client ONE JSON object, and an SSE
// comment inside that is a protocol error, not a courtesy — so it stayed
// silent for the whole of a cold load. Measured against the shipping client
// (Claude Code 2.1.269, docs/knowledges/20260912/1500-…): it abandons a
// non-streaming request with no response headers after 300.0 s and retries,
// forever, while the engine is still loading. The fleet has hosts whose first
// byte takes longer than that.
//
// What these tests pin is the shape of the answer, which is not the streaming
// one:
//
//   - The wait is SILENT until HoldAfter, because on this leg the first frame
//     spends the status and there is no in-band way to report a failure
//     afterwards.
//   - After HoldAfter the response is committed and padded with insignificant
//     whitespace, which is the only thing that can go inside one JSON value.
//   - A failure after that closes the connection rather than putting an error
//     envelope under a 200.

// slowNonStreamEngine answers /v1/chat/completions with one JSON object, after
// withholding its response headers for headerDelay — what ollama does while
// it loads weights.
func slowNonStreamEngine(headerDelay time.Duration) *httptest.Server {
	return slowNonStreamEngineWith(headerDelay, http.StatusOK,
		`{"id":"cmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
}

func slowNonStreamEngineWith(headerDelay time.Duration, status int, body string) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(headerDelay):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
	return httptest.NewServer(mux)
}

// nonStreamHeld serves one turn through the non-streaming leg from a REAL
// server, so the response crosses a socket with the chunked framing a
// recorder never produces.
func nonStreamHeld(t *testing.T, engineURL string, wait waitPolicy, rr *requestRec) *httptest.Server {
	t.Helper()
	h := NewHandlerSet(Deps{HTTPClient: http.DefaultClient})
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.proxyAnthropicNonStream(r.Context(), http.DefaultClient, engineURL,
			[]byte(ttfbStreamBody), "waired/default", nil, w, wait, localSel, rr, nil)
	}))
}

// TestNonStreamHold_SaysNothingBeforeItsDelay is the property that makes this
// safe to arm on a whole listener, and the one that differs from the
// streaming legs: a turn answered before HoldAfter writes no extra byte, so
// its failures still carry their real status.
func TestNonStreamHold_SaysNothingBeforeItsDelay(t *testing.T) {
	engine := slowNonStreamEngine(30 * time.Millisecond)
	defer engine.Close()
	h := NewHandlerSet(Deps{HTTPClient: http.DefaultClient})

	run := func(wait waitPolicy) (string, int, string) {
		w := newFlushRecorder()
		h.proxyAnthropicNonStream(context.Background(), http.DefaultClient, engine.URL,
			[]byte(ttfbStreamBody), "waired/default", nil, w, wait, localSel, nil, nil)
		return w.body(), w.code(), w.Header().Get("Content-Type")
	}
	// An hour away, so the engine always wins the race.
	held, heldCode, heldType := run(waitPolicy{Keepalive: time.Millisecond, HoldAfter: time.Hour})
	plain, plainCode, plainType := run(waitPolicy{})

	if normalizeMsgID(held) != normalizeMsgID(plain) {
		t.Errorf("a delay that never elapsed still changed the body:\n held=%q\nplain=%q", held, plain)
	}
	if heldCode != plainCode || heldType != plainType {
		t.Errorf("held response = %d %q, unheld = %d %q", heldCode, heldType, plainCode, plainType)
	}
	if strings.HasPrefix(held, jsonPadFrame) {
		t.Errorf("a pad frame was written before HoldAfter elapsed: %q", held)
	}
}

// TestNonStreamHold_PadsTheBodyOnceTheWaitIsLongEnough is the wire half. The
// padding is insignificant whitespace, so what the client finally reads is
// still exactly one JSON value — the property the whole approach rests on.
func TestNonStreamHold_PadsTheBodyOnceTheWaitIsLongEnough(t *testing.T) {
	engine := slowNonStreamEngine(300 * time.Millisecond)
	defer engine.Close()
	rr := &requestRec{}
	rr.succeed()
	srv := nonStreamHeld(t, engine.URL, waitPolicy{Keepalive: 10 * time.Millisecond, HoldAfter: 20 * time.Millisecond}, rr)
	defer srv.Close()

	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(ttfbStreamBody))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	// No Content-Length: the length is not known at commit time, and its
	// absence is what puts the response on chunked encoding so each pad
	// reaches the client as it is written.
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		t.Errorf("Content-Length = %q on a held response; the pads would be buffered", cl)
	}
	if !strings.HasPrefix(string(raw), jsonPadFrame) {
		t.Fatalf("the client was not spoken to during a 300ms wait; body=%q", raw)
	}
	// The point of the padding, stated as the client reads it.
	var out AnthropicResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("the padded body is not one JSON value: %v; body=%q", err, raw)
	}
	if len(out.Content) == 0 || !strings.Contains(visibleText(out), "Hi") {
		t.Errorf("the completed turn did not survive the padding: %+v", out)
	}
}

// TestNonStreamHold_ADecodingClientNeverSeesThePadding is the same fact from
// the other side: a client that streams the body through a JSON decoder,
// which is what an SDK does, gets the value and nothing else.
func TestNonStreamHold_ADecodingClientNeverSeesThePadding(t *testing.T) {
	engine := slowNonStreamEngine(150 * time.Millisecond)
	defer engine.Close()
	srv := nonStreamHeld(t, engine.URL, waitPolicy{Keepalive: 10 * time.Millisecond, HoldAfter: 20 * time.Millisecond}, nil)
	defer srv.Close()

	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(ttfbStreamBody))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	var out AnthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode straight off the wire: %v", err)
	}
	if out.Type != "message" {
		t.Errorf("Type = %q, want message", out.Type)
	}
}

// TestNonStreamHold_PostCommitFailureClosesRatherThanLies is the cost of the
// commit, and the choice made about it.
//
// Once a pad has gone out the status is spent, and this dialect's
// non-streaming response has no member a failure could go in. Both candidates
// were measured against Claude Code 2.1.269
// (docs/knowledges/20260912/1500-…): an error envelope under the committed
// 200 is read as "API returned an empty or malformed response (HTTP 200) —
// check for a proxy or gateway intercepting the request", and an unfinished
// body is read as "Connection to the API was lost (ECONNRESET). This is
// usually temporary — try again." The second is true and its advice is right,
// so the response is aborted.
//
// What must NOT change is the record: rr keeps the real status and reason
// (waired-agent#538).
func TestNonStreamHold_PostCommitFailureClosesRatherThanLies(t *testing.T) {
	engine := slowNonStreamEngineWith(300*time.Millisecond, http.StatusInternalServerError,
		`{"error":{"message":"the runner is gone"}}`)
	defer engine.Close()
	rr := &requestRec{}
	rr.succeed()
	srv := nonStreamHeld(t, engine.URL, waitPolicy{Keepalive: 10 * time.Millisecond, HoldAfter: 20 * time.Millisecond}, rr)
	defer srv.Close()

	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(ttfbStreamBody))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; the hold had already committed 200", resp.StatusCode)
	}
	raw, readErr := io.ReadAll(resp.Body)
	if readErr == nil {
		t.Fatalf("the body ended cleanly (%q); a client reading that sees a well-formed 200 "+
			"carrying nothing and blames its own proxy", raw)
	}
	if !errors.Is(readErr, io.ErrUnexpectedEOF) && !strings.Contains(readErr.Error(), "unexpected EOF") {
		t.Errorf("read error = %v, want an unexpected EOF from the aborted response", readErr)
	}
	if strings.Contains(string(raw), "the runner is gone") {
		t.Errorf("an error envelope was written under the committed 200: %q", raw)
	}
	if rr.ev.Status != http.StatusInternalServerError {
		t.Errorf("recorded status = %d, want 500: the client cannot be told, so the ring must be", rr.ev.Status)
	}
}

// TestNonStreamHold_IsNeverArmedOnAPeerLeg keeps the restriction the
// streaming keepalive has carried since docs/decisions/20260821/2142, for the
// same reason and not for symmetry: a peer leg's non-2xx can be the
// over-window 400 carrying HeaderLocalError=context_overflow, which
// relayPeerContextOverflow forwards as a STATUS and Claude Code keys
// auto-compaction off. A committed body cannot carry a status.
func TestNonStreamHold_IsNeverArmedOnAPeerLeg(t *testing.T) {
	engine := slowNonStreamEngine(120 * time.Millisecond)
	defer engine.Close()
	h := NewHandlerSet(Deps{HTTPClient: http.DefaultClient})
	w := newFlushRecorder()
	peerSel := router.Selection{Runtime: remoteRuntimePrefix + "peerX"}

	// The policy a peer leg actually gets: a budget, never a hold.
	h.proxyAnthropicNonStream(context.Background(), http.DefaultClient, engine.URL,
		[]byte(ttfbStreamBody), "waired/default", nil, w,
		waitPolicy{Budget: time.Minute, Reason: LocalErrorPeerTTFBTimeout}, peerSel, nil, nil)

	if strings.HasPrefix(w.body(), jsonPadFrame) {
		t.Errorf("a peer leg was held: %q", w.body())
	}
}

// TestOpenAINonStreamHold_PadsTheBodyToo is the same hole in the other
// dialect. #952 closed the streaming half of proxyToEngine and gated it on
// opts.Streaming, which left every stream:false request against this listener
// as silent as the Anthropic one.
func TestOpenAINonStreamHold_PadsTheBodyToo(t *testing.T) {
	engine := slowNonStreamEngine(300 * time.Millisecond)
	defer engine.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = proxyToEngine(r.Context(), http.DefaultClient, engine.URL, "/v1/chat/completions",
			r.Header, []byte(`{"stream":false}`), w, localSel, nil, nil,
			proxyOpts{Streaming: false, Keepalive: 10 * time.Millisecond, HoldAfter: 20 * time.Millisecond})
	}))
	defer srv.Close()

	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(`{"stream":false}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.HasPrefix(string(raw), jsonPadFrame) {
		t.Fatalf("the OpenAI non-streaming leg was silent for 300ms; body=%q", raw)
	}
	var out OpenAIResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("the padded body is not one JSON value: %v; body=%q", err, raw)
	}
	if len(out.Choices) == 0 {
		t.Errorf("the engine's answer did not survive the padding: %q", raw)
	}
}

// TestOpenAIStreamingHoldStillUsesSSE guards the fork itself: the shape is
// chosen by what the client asked for, and a stream must keep getting the
// frame a stream understands.
func TestOpenAIStreamingHoldStillUsesSSE(t *testing.T) {
	engine := slowFirstByteEngine(200 * time.Millisecond)
	defer engine.Close()
	w := newFlushRecorder()

	_, err := proxyToEngine(context.Background(), http.DefaultClient, engine.URL, "/v1/chat/completions",
		http.Header{}, []byte(`{"stream":true}`), w, localSel, nil, nil,
		proxyOpts{Streaming: true, Keepalive: 10 * time.Millisecond, HoldAfter: time.Hour})
	if err != nil {
		t.Fatalf("proxy: %v", err)
	}
	if !strings.Contains(w.body(), keepaliveFrame) {
		t.Errorf("a streaming request got no SSE keepalive: %q", w.body())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
}

// TestEngineHold_NotArmedWithoutBothDurations pins the arming gate: a
// listener with no interval (the overlay, permanently) and a policy with no
// delay both mean "write nothing", and a half-configured hold must not
// quietly become the streaming one.
func TestEngineHold_NotArmedWithoutBothDurations(t *testing.T) {
	shape := holdShape{commit: writeJSONBodyHeaders, frame: jsonPadWhitespace, name: "json-pad"}
	cases := []struct {
		name         string
		first, every time.Duration
		shape        holdShape
	}{
		{"no interval", time.Millisecond, 0, shape},
		{"no delay", 0, time.Millisecond, shape},
		{"neither", 0, 0, shape},
		{"no commit", time.Millisecond, time.Millisecond, holdShape{frame: jsonPadWhitespace}},
		{"no frame", time.Millisecond, time.Millisecond, holdShape{commit: writeJSONBodyHeaders}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newFlushRecorder()
			hold := startEngineHold(context.Background(), w, tc.first, tc.every, tc.shape, nil)
			if hold != nil {
				hold.stop("test")
				t.Fatalf("a hold was armed from %v/%v", tc.first, tc.every)
			}
			// Nil-safe on every method, so one code path serves both.
			if hold.committed() {
				t.Error("an unarmed hold reported itself committed")
			}
			if !hold.canReportInBand() {
				t.Error("an unarmed hold refused an in-band error")
			}
			hold.stop("test")
			if w.body() != "" {
				t.Errorf("an unarmed hold wrote %q", w.body())
			}
		})
	}
}
