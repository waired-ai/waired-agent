package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/router"
)

// writeOpenAIMiniStream emits a minimal valid OpenAI SSE completion the gateway
// can translate to Anthropic events.
func writeOpenAIMiniStream(w http.ResponseWriter, first, second string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	f, _ := w.(http.Flusher)
	_, _ = w.Write([]byte(`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"` + first + `"}}]}` + "\n\n"))
	if second != "" {
		_, _ = w.Write([]byte(`data: {"choices":[{"index":0,"delta":{"content":"` + second + `"}}]}` + "\n\n"))
	}
	_, _ = w.Write([]byte(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"))
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if f != nil {
		f.Flush()
	}
}

// slowFirstByteEngine withholds the response headers for headerDelay
// (a reachable peer producing no first token), then streams; it returns
// promptly if the caller cancels (the TTFB abort), so it never lingers.
func slowFirstByteEngine(headerDelay time.Duration) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(headerDelay):
		case <-r.Context().Done():
			return
		}
		writeOpenAIMiniStream(w, "Hi", "")
	})
	return httptest.NewServer(mux)
}

// slowBodyEngine commits headers + a first chunk immediately, then delays before
// the remaining chunks — post-commit slowness a disarmed deadline must NOT cut.
func slowBodyEngine(bodyDelay time.Duration) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f, _ := w.(http.Flusher)
		_, _ = w.Write([]byte(`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"Hi"}}]}` + "\n\n"))
		if f != nil {
			f.Flush()
		}
		select {
		case <-time.After(bodyDelay):
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte(`data: {"choices":[{"index":0,"delta":{"content":" there"},"finish_reason":"stop"}]}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if f != nil {
			f.Flush()
		}
	})
	return httptest.NewServer(mux)
}

const ttfbStreamBody = `{"model":"waired/default","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`

// localSel is what these tests dispatch under: the baseURL they pass is a
// local test engine, not a peer. The selection reaches only
// adapterErrorForClient, and a local one keeps the error's detail; the
// public-peer rendering is covered by
// TestDispatchTransportError_DoesNotLeakThePeerAddress.
var localSel = router.Selection{Runtime: "ollama"}

func TestProxyAnthropicStream_TTFBAbortsPreCommit(t *testing.T) {
	// Delay comfortably exceeds the budget so headers never arrive first; the
	// select-on-cancel + short delay keep the lingering handler from stalling
	// engine.Close() long after the 50ms abort.
	engine := slowFirstByteEngine(500 * time.Millisecond)
	defer engine.Close()
	h := NewHandlerSet(Deps{HTTPClient: http.DefaultClient})
	w := httptest.NewRecorder()
	w.Header().Set(HeaderInferencePeer, "peerX") // as setSelectionHeaders stages it

	h.proxyAnthropicStream(context.Background(), http.DefaultClient, engine.URL,
		[]byte(ttfbStreamBody), "waired/default", nil, w, waitPolicy{Budget: 50 * time.Millisecond, Reason: LocalErrorPeerTTFBTimeout}, localSel, nil, nil)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get(HeaderLocalError); got != LocalErrorPeerTTFBTimeout {
		t.Errorf("HeaderLocalError = %q, want %q", got, LocalErrorPeerTTFBTimeout)
	}
	if strings.Contains(w.Body.String(), "message_start") {
		t.Errorf("stream committed before abort: %s", w.Body.String())
	}
}

func TestProxyAnthropicStream_FastPeerNotAborted(t *testing.T) {
	engine := slowFirstByteEngine(0)
	defer engine.Close()
	h := NewHandlerSet(Deps{HTTPClient: http.DefaultClient})
	w := httptest.NewRecorder()

	h.proxyAnthropicStream(context.Background(), http.DefaultClient, engine.URL,
		[]byte(ttfbStreamBody), "waired/default", nil, w, waitPolicy{Budget: 100 * time.Millisecond, Reason: LocalErrorPeerTTFBTimeout}, localSel, nil, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Hi") {
		t.Errorf("expected content in stream: %s", w.Body.String())
	}
	if got := w.Header().Get(HeaderLocalError); got != "" {
		t.Errorf("HeaderLocalError set on success: %q", got)
	}
}

func TestProxyAnthropicStream_PostCommitSlownessNotAborted(t *testing.T) {
	engine := slowBodyEngine(120 * time.Millisecond)
	defer engine.Close()
	h := NewHandlerSet(Deps{HTTPClient: http.DefaultClient})
	w := httptest.NewRecorder()

	// Budget shorter than the mid-stream delay: the deadline must disarm at
	// headers, so the slow SECOND chunk is delivered rather than aborted.
	h.proxyAnthropicStream(context.Background(), http.DefaultClient, engine.URL,
		[]byte(ttfbStreamBody), "waired/default", nil, w, waitPolicy{Budget: 30 * time.Millisecond, Reason: LocalErrorPeerTTFBTimeout}, localSel, nil, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "there") {
		t.Errorf("post-commit content lost (deadline wrongly cut the stream): %s", w.Body.String())
	}
}

func TestProxyAnthropicStream_TTFBZeroDisabled(t *testing.T) {
	engine := slowFirstByteEngine(60 * time.Millisecond)
	defer engine.Close()
	h := NewHandlerSet(Deps{HTTPClient: http.DefaultClient})
	w := httptest.NewRecorder()

	h.proxyAnthropicStream(context.Background(), http.DefaultClient, engine.URL,
		[]byte(ttfbStreamBody), "waired/default", nil, w, waitPolicy{}, localSel, nil, nil) // disabled

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (ttfb=0 disables the deadline)", w.Code)
	}
}

// TestWaitPolicyFor is the arming gate for everything a streaming leg may do
// before the engine's first byte.
//
// PRODUCT CONTRACT, ratified by
// docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md
// (owner ruling waired-ai/waired#1313): the wait policies stay and the branch
// that used to end them — a re-send to the real Anthropic API — becomes an
// error, so the split is by LEG rather than by the intercept's permission
// header. A peer leg keeps the liveness watch and stays uncommitted, which is
// what lets its failure carry a status that names the peer
// (waired-agent#1180). A local leg is held with an SSE keepalive and never
// aborted: this device loading its own weights is not a failure, and there is
// nowhere else to send the turn (docs/decisions/20260821/2142,
// docs/decisions/20260828/0143).
//
// Before that ruling both were conditioned on HeaderFallbackAllowed, which the
// intercept set on the auto leg alone — so a PINNED peer leg got no wait at
// all and hung silently. The "peer, no header" row that pinned exactly that is
// inverted here, and the PR body says so.
func TestWaitPolicyFor(t *testing.T) {
	budget := func(class string) time.Duration {
		if class == "sub" {
			return 20 * time.Millisecond
		}
		return 60 * time.Millisecond
	}
	peer := router.Selection{Runtime: remoteRuntimePrefix + "peerX"}
	local := router.Selection{Runtime: "ollama"}
	// Every dep wired at once — the shape the Claude intercept actually
	// builds, and the one that would break the commit boundary if the two
	// mechanisms could ever arm together.
	all := Deps{TTFBBudget: budget, StreamKeepalive: 5 * time.Millisecond}

	cases := []struct {
		name  string
		deps  Deps
		sel   router.Selection
		class string
		want  waitPolicy
	}{
		{"peer + main", all, peer, "main",
			waitPolicy{Budget: 60 * time.Millisecond, Reason: LocalErrorPeerTTFBTimeout}},
		{"peer + sub", all, peer, "sub",
			waitPolicy{Budget: 20 * time.Millisecond, Reason: LocalErrorPeerTTFBTimeout}},
		{"local: held, never aborted", all, local, "main",
			waitPolicy{Keepalive: 5 * time.Millisecond}},
		{"local, no keepalive wired", Deps{TTFBBudget: budget}, local, "main", waitPolicy{}},
		{"peer, no budget wired", Deps{StreamKeepalive: 5 * time.Millisecond}, peer, "main", waitPolicy{}},
		{"nothing wired (every surface but the intercept)", Deps{}, peer, "main", waitPolicy{}},
		{"nothing wired, local leg", Deps{}, local, "main", waitPolicy{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := waitPolicyFor(tc.deps, tc.sel, tc.class)
			if got != tc.want {
				t.Errorf("waitPolicyFor = %+v, want %+v", got, tc.want)
			}
			if got.Budget > 0 && got.Keepalive > 0 {
				t.Errorf("both armed (%+v): a peer leg must stay uncommitted so its failure can "+
					"still carry a status, and a local leg must not be aborted", got)
			}
			if got.Budget > 0 && got.Reason == "" {
				t.Errorf("budget armed with no reason to stage: %+v", got)
			}
		})
	}
}

// TestProxyAnthropicStream_TTFBTimeoutIsRecorded and its transport-error
// sibling below complement the two tests above, which pass a nil
// requestRec — and that nil is why these two exits recorded nothing.
// handleAnthropicMessagesImpl calls rr.succeed() before dispatch, so an
// exit that writes a 502 without recording leaves 200 in the event ring,
// and the request reads as a finished turn to the tray, the metrics, and
// the per-peer error window (waired-agent#281).
func TestProxyAnthropicStream_TTFBTimeoutIsRecorded(t *testing.T) {
	engine := slowFirstByteEngine(500 * time.Millisecond)
	defer engine.Close()
	h := NewHandlerSet(Deps{HTTPClient: http.DefaultClient})
	w := httptest.NewRecorder()
	rr := &requestRec{}
	rr.succeed() // as the handler does, before it knows anything

	h.proxyAnthropicStream(context.Background(), http.DefaultClient, engine.URL,
		[]byte(ttfbStreamBody), "waired/default", nil, w, waitPolicy{Budget: 50 * time.Millisecond, Reason: LocalErrorPeerTTFBTimeout}, localSel, rr, nil)

	if rr.ev.Status != http.StatusBadRequest {
		t.Errorf("recorded status = %d, want %d (the client was sent 400)", rr.ev.Status, http.StatusBadRequest)
	}
	if rr.ev.ErrorReason != LocalErrorPeerTTFBTimeout {
		t.Errorf("recorded error_reason = %q, want %q", rr.ev.ErrorReason, LocalErrorPeerTTFBTimeout)
	}
}

func TestProxyAnthropicStream_TransportErrorIsRecorded(t *testing.T) {
	dead := httptest.NewServer(http.NewServeMux())
	deadURL := dead.URL
	dead.Close() // nothing is listening on that port now

	h := NewHandlerSet(Deps{HTTPClient: http.DefaultClient})
	w := httptest.NewRecorder()
	rr := &requestRec{}
	rr.succeed()

	h.proxyAnthropicStream(context.Background(), http.DefaultClient, deadURL,
		[]byte(ttfbStreamBody), "waired/default", nil, w, waitPolicy{}, localSel, rr, nil)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if rr.ev.Status != http.StatusBadGateway {
		t.Errorf("recorded status = %d, want %d", rr.ev.Status, http.StatusBadGateway)
	}
	// The reason the non-streaming leg has always used for this: one
	// failure must not be described two ways by two transports.
	if rr.ev.ErrorReason != "engine_request_failed" {
		t.Errorf("recorded error_reason = %q, want %q", rr.ev.ErrorReason, "engine_request_failed")
	}
}
