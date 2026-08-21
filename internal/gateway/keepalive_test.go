package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// flushRecorder is httptest.ResponseRecorder plus http.Flusher, so the
// streaming path takes the same branch it takes in production. Writes are
// mutex-guarded because the keepalive goroutine and the stream loop are two
// writers whose handoff is exactly what these tests exercise.
type flushRecorder struct {
	mu      sync.Mutex
	rec     *httptest.ResponseRecorder
	flushes int
}

func newFlushRecorder() *flushRecorder {
	return &flushRecorder{rec: httptest.NewRecorder()}
}

func (f *flushRecorder) Header() http.Header { return f.rec.Header() }

func (f *flushRecorder) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rec.Write(p)
}

func (f *flushRecorder) WriteHeader(code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec.WriteHeader(code)
}

func (f *flushRecorder) Flush() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flushes++
	f.rec.Flush()
}

func (f *flushRecorder) body() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rec.Body.String()
}

func (f *flushRecorder) code() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rec.Code
}

// TestKeepalive_HoldsAPinnedLocalLegOpen is the wire half of
// waired-agent#837: a leg with nowhere else to send the turn waits, but it
// does not wait silently. The engine here withholds its headers the way
// ollama does while it loads weights.
func TestKeepalive_HoldsAPinnedLocalLegOpen(t *testing.T) {
	engine := slowFirstByteEngine(200 * time.Millisecond)
	defer engine.Close()
	h := NewHandlerSet(Deps{HTTPClient: http.DefaultClient})
	w := newFlushRecorder()

	h.proxyAnthropicStream(context.Background(), http.DefaultClient, engine.URL,
		[]byte(ttfbStreamBody), "waired/default", nil, w,
		waitPolicy{Keepalive: 20 * time.Millisecond}, localSel, nil, nil)

	body := w.body()
	if w.code() != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.code(), body)
	}
	if !strings.Contains(body, keepaliveFrame) {
		t.Fatalf("no keepalive reached the client during a 200ms header wait; body=%q", body)
	}
	// The order is the point: the client learns the connection is alive
	// BEFORE the engine has produced anything.
	if i, j := strings.Index(body, keepaliveFrame), strings.Index(body, "message_start"); j >= 0 && i > j {
		t.Errorf("keepalive arrived after message_start (i=%d j=%d); it holds nothing open there", i, j)
	}
	// And the turn still completes normally afterwards.
	for _, want := range []string{"message_start", "Hi", "message_stop"} {
		if !strings.Contains(body, want) {
			t.Errorf("held stream lost %q from the completed turn; body=%q", want, body)
		}
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
}

// TestKeepalive_QuietTurnIsByteIdenticalToAnUnheldOne pins the property that
// makes this safe to arm by default on a whole listener: a turn the engine
// answers inside one interval writes no extra bytes at all
// (docs/decisions/20260727/1500-vllm-install-progress-from-uv-lines.md — a
// signal that degrades quietly rather than breaking).
func TestKeepalive_QuietTurnIsByteIdenticalToAnUnheldOne(t *testing.T) {
	engine := slowFirstByteEngine(0)
	defer engine.Close()
	h := NewHandlerSet(Deps{HTTPClient: http.DefaultClient})

	run := func(wait waitPolicy) string {
		w := newFlushRecorder()
		h.proxyAnthropicStream(context.Background(), http.DefaultClient, engine.URL,
			[]byte(ttfbStreamBody), "waired/default", nil, w, wait, localSel, nil, nil)
		return w.body()
	}
	held := run(waitPolicy{Keepalive: time.Hour})
	plain := run(waitPolicy{})

	// msg_<unix nanos> differs per run; everything else must match.
	if normalizeMsgID(held) != normalizeMsgID(plain) {
		t.Errorf("an interval that never elapsed still changed the stream:\n held=%q\nplain=%q", held, plain)
	}
	if strings.Contains(held, keepaliveFrame) {
		t.Errorf("a frame was written at t=0; the keepalive must fire on the first TICK: %q", held)
	}
}

func normalizeMsgID(s string) string {
	i := strings.Index(s, `"id":"msg_`)
	if i < 0 {
		return s
	}
	j := strings.Index(s[i:], `"`+`,`)
	if j < 0 {
		return s
	}
	return s[:i] + `"id":"msg_X"` + s[i+j+1:]
}

// TestKeepalive_CommittedEngineErrorBecomesAnSSEError covers the cost of
// committing early: the status is spent, so a later engine failure has to
// reach the client in band. What must NOT change is the record — rr keeps the
// real status and reason (waired-agent#538).
func TestKeepalive_CommittedEngineErrorBecomesAnSSEError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(120 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		http.Error(w, "runner died", http.StatusInternalServerError)
	})
	engine := httptest.NewServer(mux)
	defer engine.Close()

	h := NewHandlerSet(Deps{HTTPClient: http.DefaultClient})
	w := newFlushRecorder()
	rr := &requestRec{}
	rr.succeed()

	h.proxyAnthropicStream(context.Background(), http.DefaultClient, engine.URL,
		[]byte(ttfbStreamBody), "waired/default", nil, w,
		waitPolicy{Keepalive: 20 * time.Millisecond}, localSel, rr, nil)

	body := w.body()
	if w.code() != http.StatusOK {
		t.Fatalf("status = %d; the stream committed at the first keepalive, so 200 is what the client got", w.code())
	}
	if !strings.Contains(body, "event: error") {
		t.Errorf("a committed stream that failed said nothing to the client: %q", body)
	}
	if !strings.Contains(body, "runner died") {
		t.Errorf("the engine's own message did not reach the client: %q", body)
	}
	if rr.ev.Status != http.StatusInternalServerError {
		t.Errorf("recorded status = %d, want 500 — the record must keep the real failure", rr.ev.Status)
	}
	if rr.ev.ErrorReason != "upstream_error" {
		t.Errorf("recorded error_reason = %q, want upstream_error", rr.ev.ErrorReason)
	}
}

// TestKeepalive_TransportErrorBeforeAnyFrameStaysA502: nothing was written
// yet, so the pre-commit path is untouched and the intercept can still see a
// 502. This is the case a naive "always commit" implementation would break.
func TestKeepalive_TransportErrorBeforeAnyFrameStaysA502(t *testing.T) {
	dead := httptest.NewServer(http.NewServeMux())
	deadURL := dead.URL
	dead.Close()

	h := NewHandlerSet(Deps{HTTPClient: http.DefaultClient})
	w := newFlushRecorder()
	rr := &requestRec{}
	rr.succeed()

	h.proxyAnthropicStream(context.Background(), http.DefaultClient, deadURL,
		[]byte(ttfbStreamBody), "waired/default", nil, w,
		waitPolicy{Keepalive: time.Hour}, localSel, rr, nil)

	if w.code() != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.code())
	}
	if strings.Contains(w.body(), "event: error") {
		t.Errorf("an uncommitted failure was written in band: %q", w.body())
	}
}

// TestKeepalive_StopsWhenTheClientIsGone: the hold must not outlive the
// request. Run under -race, this also covers the writer handoff.
func TestKeepalive_StopsWhenTheClientIsGone(t *testing.T) {
	engine := slowFirstByteEngine(2 * time.Second)
	defer engine.Close()
	h := NewHandlerSet(Deps{HTTPClient: http.DefaultClient})
	w := newFlushRecorder()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.proxyAnthropicStream(ctx, http.DefaultClient, engine.URL,
			[]byte(ttfbStreamBody), "waired/default", nil, w,
			waitPolicy{Keepalive: time.Millisecond}, localSel, nil, nil)
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the handler did not return after the client went away")
	}
	before := w.body()
	time.Sleep(20 * time.Millisecond)
	if w.body() != before {
		t.Error("frames were still being written after the handler returned")
	}
}

func TestHoldStopReason(t *testing.T) {
	cases := []struct {
		name string
		resp *http.Response
		err  error
		want string
	}{
		{"transport error", nil, context.Canceled, "engine_request_failed"},
		{"no response", nil, nil, "no_response"},
		{"engine error", &http.Response{StatusCode: 500}, nil, "engine_error"},
		{"first byte", &http.Response{StatusCode: 200}, nil, "first_byte"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := holdStopReason(tc.resp, tc.err); got != tc.want {
				t.Errorf("holdStopReason = %q, want %q", got, tc.want)
			}
		})
	}
}
