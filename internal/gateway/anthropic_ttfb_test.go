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
		[]byte(ttfbStreamBody), "waired/default", w, 50*time.Millisecond)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get(HeaderLocalError); got != LocalErrorPeerTTFBTimeout {
		t.Errorf("HeaderLocalError = %q, want %q", got, LocalErrorPeerTTFBTimeout)
	}
	if w.Header().Get(HeaderTTFBBudgetMs) == "" {
		t.Errorf("HeaderTTFBBudgetMs not staged for the notice")
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
		[]byte(ttfbStreamBody), "waired/default", w, 100*time.Millisecond)

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
		[]byte(ttfbStreamBody), "waired/default", w, 30*time.Millisecond)

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
		[]byte(ttfbStreamBody), "waired/default", w, 0) // disabled

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (ttfb=0 disables the deadline)", w.Code)
	}
}

// TestTTFBBudgetFor is the R1 arming gate: armed only for a peer leg that the
// intercept authorized via HeaderFallbackAllowed (auto mode).
func TestTTFBBudgetFor(t *testing.T) {
	budget := func(class string) time.Duration {
		if class == "sub" {
			return 20 * time.Millisecond
		}
		return 60 * time.Millisecond
	}
	peer := router.Selection{Runtime: remoteRuntimePrefix + "peerX"}
	local := router.Selection{Runtime: "ollama"}
	withHdr := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", nil)
		r.Header.Set(HeaderFallbackAllowed, "1")
		return r
	}
	noHdr := func() *http.Request {
		return httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", nil)
	}

	cases := []struct {
		name  string
		deps  Deps
		sel   router.Selection
		r     *http.Request
		class string
		want  time.Duration
	}{
		{"peer + header + main", Deps{TTFBBudget: budget}, peer, withHdr(), "main", 60 * time.Millisecond},
		{"peer + header + sub", Deps{TTFBBudget: budget}, peer, withHdr(), "sub", 20 * time.Millisecond},
		{"peer, no header (pinned route)", Deps{TTFBBudget: budget}, peer, noHdr(), "main", 0},
		{"local runtime + header", Deps{TTFBBudget: budget}, local, withHdr(), "main", 0},
		{"nil TTFBBudget", Deps{}, peer, withHdr(), "main", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ttfbBudgetFor(tc.deps, tc.sel, tc.r, tc.class); got != tc.want {
				t.Errorf("ttfbBudgetFor = %v, want %v", got, tc.want)
			}
		})
	}
}
