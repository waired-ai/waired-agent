package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// openAIStreamBody and openAIPlainBody differ in one member, which is the
// whole of what decides whether a keepalive may be armed.
const (
	openAIStreamBody = `{"model":"waired/default","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	openAIPlainBody  = `{"model":"waired/default","messages":[{"role":"user","content":"hi"}]}`
)

// TestOpenAIKeepalive_HoldsAStreamingRequestOpen is waired-agent#952: a
// streaming request against an engine that is still loading used to receive
// nothing at all until the answer arrived complete — 116 s of it on real
// hardware, twice diagnosed as a hang and killed.
//
// Product contract (waired-agent#837's rule, extended to this leg): a local
// leg with nowhere else to send the turn waits, but does not go silent.
func TestOpenAIKeepalive_HoldsAStreamingRequestOpen(t *testing.T) {
	engine := slowFirstByteEngine(200 * time.Millisecond)
	defer engine.Close()

	w := newFlushRecorder()
	rr := &requestRec{start: time.Now()}
	rr.succeed()
	started, err := proxyToEngine(context.Background(), engine.Client(), engine.URL,
		"/v1/chat/completions", http.Header{}, []byte(openAIStreamBody), w, localSel, rr, nil,
		proxyOpts{Streaming: true, Keepalive: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("proxyToEngine: %v", err)
	}
	if !started {
		t.Error("responseStarted = false after a committed stream")
	}
	if !strings.Contains(w.body(), ": waired keepalive") {
		t.Errorf("no keepalive reached the client: %q", w.body())
	}
	if got := w.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if got := w.code(); got != http.StatusOK {
		t.Errorf("status = %d, want 200", got)
	}
	// The engine's own stream still arrives, whole, after the frames.
	if !strings.Contains(w.body(), `"content":"Hi"`) || !strings.Contains(w.body(), "[DONE]") {
		t.Errorf("the engine's stream did not follow the keepalives: %q", w.body())
	}
	// And the frames came first, which is the point.
	if strings.Index(w.body(), ": waired keepalive") > strings.Index(w.body(), `"content":"Hi"`) {
		t.Errorf("the hold wrote after the engine answered: %q", w.body())
	}
}

// TestOpenAIKeepalive_NeverOnANonStreamingRequest is the first of #952's four
// questions. An SSE frame in a non-streamed response is a protocol error, not
// a courtesy: the client asked for one JSON object.
//
// Product contract. This is also the assertion that makes the `stream` decode
// worth its cost — without it the cheap implementation (arm it always) would
// pass every other test here.
func TestOpenAIKeepalive_NeverOnANonStreamingRequest(t *testing.T) {
	engine := slowFirstByteEngine(200 * time.Millisecond)
	defer engine.Close()

	w := newFlushRecorder()
	rr := &requestRec{start: time.Now()}
	rr.succeed()
	if _, err := proxyToEngine(context.Background(), engine.Client(), engine.URL,
		"/v1/chat/completions", http.Header{}, []byte(openAIPlainBody), w, localSel, rr, nil,
		proxyOpts{Streaming: jsonBoolMemberOf(t, openAIPlainBody, "stream"), Keepalive: 10 * time.Millisecond}); err != nil {
		t.Fatalf("proxyToEngine: %v", err)
	}
	if strings.Contains(w.body(), "waired keepalive") {
		t.Errorf("a non-streaming request was answered with SSE frames: %q", w.body())
	}
}

// TestOpenAIKeepalive_UnwiredIntervalIsTodaysBytes pins the overlay
// listener's position (#952's third question) and the general off switch:
// Deps.StreamKeepalive is 0 there and must stay 0, because a first byte on
// the serving side of a mesh leg is one the requesting peer's engine never
// produced — it disarms that peer's #757 budget and makes every peer on the
// mesh look responsive.
func TestOpenAIKeepalive_UnwiredIntervalIsTodaysBytes(t *testing.T) {
	engine := slowFirstByteEngine(100 * time.Millisecond)
	defer engine.Close()

	w := newFlushRecorder()
	rr := &requestRec{start: time.Now()}
	rr.succeed()
	if _, err := proxyToEngine(context.Background(), engine.Client(), engine.URL,
		"/v1/chat/completions", http.Header{}, []byte(openAIStreamBody), w, localSel, rr, nil,
		proxyOpts{Streaming: true, Keepalive: 0}); err != nil {
		t.Fatalf("proxyToEngine: %v", err)
	}
	if strings.Contains(w.body(), "waired keepalive") {
		t.Errorf("a listener with no interval wired wrote a keepalive: %q", w.body())
	}
}

// TestOpenAIKeepalive_QuietTurnIsByteIdenticalToAnUnheldOne: a turn whose
// engine answers inside one interval must be indistinguishable from the same
// turn before this existed — the "breaks nothing, falls quietly back" rule
// (docs/decisions/20260727/1500-vllm-install-progress-from-uv-lines.md) the
// Anthropic leg was held to.
func TestOpenAIKeepalive_QuietTurnIsByteIdenticalToAnUnheldOne(t *testing.T) {
	engine := slowFirstByteEngine(0)
	defer engine.Close()

	run := func(keepalive time.Duration) (string, string) {
		t.Helper()
		w := newFlushRecorder()
		rr := &requestRec{start: time.Now()}
		rr.succeed()
		if _, err := proxyToEngine(context.Background(), engine.Client(), engine.URL,
			"/v1/chat/completions", http.Header{}, []byte(openAIStreamBody), w, localSel, rr, nil,
			proxyOpts{Streaming: true, Keepalive: keepalive}); err != nil {
			t.Fatalf("proxyToEngine: %v", err)
		}
		return w.body(), w.Header().Get("Content-Type")
	}
	heldBody, heldType := run(time.Hour)
	plainBody, plainType := run(0)
	if heldBody != plainBody {
		t.Errorf("a quiet turn differed:\n held = %q\nplain = %q", heldBody, plainBody)
	}
	if heldType != plainType {
		t.Errorf("Content-Type differed: held = %q, plain = %q", heldType, plainType)
	}
}

// TestOpenAIKeepalive_CommittedEngineErrorBecomesADataFrame is #952's fourth
// question. Pre-commit this leg forwards the engine's status and body
// verbatim, which is what it exists to do; post-commit the status is spent,
// so the same envelope goes out as one `data:` frame and the real status is
// recorded on the request instead (waired-agent#538).
func TestOpenAIKeepalive_CommittedEngineErrorBecomesADataFrame(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(60 * time.Millisecond)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"ENGINEMARKER out of memory"}}`))
	}))
	defer engine.Close()

	w := newFlushRecorder()
	rr := &requestRec{start: time.Now()}
	rr.succeed()
	started, err := proxyToEngine(context.Background(), engine.Client(), engine.URL,
		"/v1/chat/completions", http.Header{}, []byte(openAIStreamBody), w, localSel, rr, nil,
		proxyOpts{Streaming: true, Keepalive: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("proxyToEngine: %v", err)
	}
	if !started {
		t.Error("responseStarted = false after a committed stream")
	}
	if got := w.code(); got != http.StatusOK {
		t.Errorf("status = %d; the hold already spent it, so it cannot become 500", got)
	}
	if got := rr.ev.Status; got != http.StatusInternalServerError {
		t.Errorf("recorded status = %d, want 500 — the ring has to keep the real one", got)
	}
	body := w.body()
	if !strings.Contains(body, "data: ") || !strings.Contains(body, "ENGINEMARKER") {
		t.Errorf("the engine's message did not reach the client as a frame: %q", body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Error("a failed stream was closed with the sentinel that says it finished")
	}
}

// TestOpenAIKeepalive_CommittedTransportErrorIsReportedAsStarted: the hold
// spends the status, so a transport failure after it is no longer the
// "never reached the engine" case. Reporting it as not-started would make
// the caller log "failed before the engine answered" about a client that is
// already mid-stream (waired-agent#538's distinction).
func TestOpenAIKeepalive_CommittedTransportErrorIsReportedAsStarted(t *testing.T) {
	// An engine that accepts the connection, waits, then hangs up without
	// ever writing a response — the shape `ollama serve` being killed has.
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(60 * time.Millisecond)
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, err := hj.Hijack()
			if err == nil {
				_ = conn.Close()
				return
			}
		}
		panic(http.ErrAbortHandler)
	}))
	defer engine.Close()

	w := newFlushRecorder()
	rr := &requestRec{start: time.Now()}
	rr.succeed()
	started, err := proxyToEngine(context.Background(), engine.Client(), engine.URL,
		"/v1/chat/completions", http.Header{}, []byte(openAIStreamBody), w, localSel, rr, nil,
		proxyOpts{Streaming: true, Keepalive: 10 * time.Millisecond})
	if err == nil {
		t.Fatal("proxyToEngine returned nil for an engine that hung up without answering")
	}
	if !started {
		t.Error("responseStarted = false although the hold had already committed the status")
	}
	if got := rr.ev.ErrorReason; got != "engine_request_failed" {
		t.Errorf("ErrorReason = %q, want engine_request_failed", got)
	}
	if !strings.Contains(w.body(), "data: ") {
		t.Errorf("a committed failure was not written in band: %q", w.body())
	}
}

// TestJSONBoolMember: absent, null and a non-bool all read as "no claim".
func TestJSONBoolMember(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{
		{`{"stream":true}`, true},
		{`{"stream":false}`, false},
		{`{}`, false},
		{`{"stream":null}`, false},
		{`{"stream":"true"}`, false},
		{`{"stream":1}`, false},
	} {
		if got := jsonBoolMemberOf(t, tc.body, "stream"); got != tc.want {
			t.Errorf("jsonBoolMember(%s) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

func jsonBoolMemberOf(t *testing.T, body, name string) bool {
	t.Helper()
	raw, err := decodeJSONObject([]byte(body))
	if err != nil {
		t.Fatalf("decodeJSONObject(%s): %v", body, err)
	}
	return jsonBoolMember(raw, name)
}
