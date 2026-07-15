package intercept

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fallbackHeaderRecorder commits a 200 (no fallback) and records the
// X-Waired-Fallback-Allowed header the gateway leg received.
func fallbackHeaderRecorder(got *string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = r.Header.Get(fallbackAllowedHeader)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"type":"message"}`)
	})
}

// ttfbTimeoutLocalHandler emulates the gateway's pre-commit TTFB abort: it
// stages the peer/budget/reason on the (uncommitted) recorder then 502s, so
// dispatchAuto treats it as fallback-eligible.
func ttfbTimeoutLocalHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(localErrorHeader, localErrPeerTTFBTimeout)
		w.Header().Set(inferencePeerHeader, "peerX")
		w.Header().Set(ttfbBudgetHeader, "20000")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"type":"error"}`)
	})
}

func sseUpstream(sse string) http.RoundTripper {
	return rtFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sse)),
			Request:    r,
		}, nil
	})
}

func postMessages(t *testing.T, url string, spoofFallbackAllowed bool) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+"/v1/messages", strings.NewReader(`{"model":"claude-test","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if spoofFallbackAllowed {
		req.Header.Set(fallbackAllowedHeader, "1") // a client trying to force the abort
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// --- R1: fallback-allowed header is set ONLY on the auto leg -----------------

func TestFallbackAllowedHeader_AutoSets(t *testing.T) {
	var got string
	s := newServer(t, Deps{
		LocalInference:       fallbackHeaderRecorder(&got),
		Degraded:             func() bool { return false },
		ClassRoute:           classRouteFunc(routeAuto),
		PassthroughTransport: fakeUpstream(nil),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp := postMessages(t, srv.URL, false)
	_ = resp.Body.Close()
	if got != "1" {
		t.Errorf("auto leg: gateway saw fallback-allowed = %q, want \"1\"", got)
	}
}

func TestFallbackAllowedHeader_WairedDoesNotSet(t *testing.T) {
	var got string
	s := newServer(t, Deps{
		LocalInference:       fallbackHeaderRecorder(&got),
		Degraded:             func() bool { return false },
		ClassRoute:           classRouteFunc(routeWaired),
		PassthroughTransport: fakeUpstream(nil),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp := postMessages(t, srv.URL, false)
	_ = resp.Body.Close()
	if got != "" {
		t.Errorf("waired leg: gateway saw fallback-allowed = %q, want empty", got)
	}
}

func TestFallbackAllowedHeader_SpoofStrippedOnPinnedLeg(t *testing.T) {
	var got string
	s := newServer(t, Deps{
		LocalInference:       fallbackHeaderRecorder(&got),
		Degraded:             func() bool { return false },
		ClassRoute:           classRouteFunc(routeWaired),
		PassthroughTransport: fakeUpstream(nil),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp := postMessages(t, srv.URL, true /* client spoofs the header */)
	_ = resp.Body.Close()
	if got != "" {
		t.Errorf("spoofed header reached the gateway on a pinned leg = %q, want empty", got)
	}
}

// --- R2: reroute notice injected into the fallback response ------------------

func newServerAnnotate(t *testing.T, annotate bool, deps Deps) *Server {
	t.Helper()
	s, err := NewServer(Config{Addr: "127.0.0.1:0", AnnotateReroute: annotate}, deps)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s
}

func TestRerouteNotice_InjectedOnAutoFallback(t *testing.T) {
	sse := sseMessageStart + textBlock(0, "answer") + sseMessageTail
	s := newServerAnnotate(t, true, Deps{
		LocalInference:       ttfbTimeoutLocalHandler(),
		Degraded:             func() bool { return false },
		ClassRoute:           classRouteFunc(routeAuto),
		PassthroughTransport: sseUpstream(sse),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp := postMessages(t, srv.URL, false)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	out := string(body)

	if !strings.Contains(out, "mesh peer (peerX)") {
		t.Errorf("reroute notice (with peer) not injected:\n%s", out)
	}
	if strings.Index(out, "mesh peer (peerX)") > strings.Index(out, "event: message_delta") {
		t.Errorf("notice injected after message_delta:\n%s", out)
	}
	if !strings.Contains(out, "event: message_stop") {
		t.Errorf("terminal events lost:\n%s", out)
	}
}

func TestRerouteNotice_SuppressedWhenAnnotateOff(t *testing.T) {
	sse := sseMessageStart + textBlock(0, "answer") + sseMessageTail
	s := newServerAnnotate(t, false, Deps{
		LocalInference:       ttfbTimeoutLocalHandler(),
		Degraded:             func() bool { return false },
		ClassRoute:           classRouteFunc(routeAuto),
		PassthroughTransport: sseUpstream(sse),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp := postMessages(t, srv.URL, false)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if strings.Contains(string(body), "mesh peer") {
		t.Errorf("notice injected despite AnnotateReroute=false:\n%s", string(body))
	}
}

func TestRerouteNotice_ToolUseResponseUntouched(t *testing.T) {
	sse := sseMessageStart + textBlock(0, "let me look") + toolUseBlock(1) + sseMessageTail
	s := newServerAnnotate(t, true, Deps{
		LocalInference:       ttfbTimeoutLocalHandler(),
		Degraded:             func() bool { return false },
		ClassRoute:           classRouteFunc(routeAuto),
		PassthroughTransport: sseUpstream(sse),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp := postMessages(t, srv.URL, false)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if strings.Contains(string(body), "mesh peer") {
		t.Errorf("notice injected into a tool_use response:\n%s", string(body))
	}
}
