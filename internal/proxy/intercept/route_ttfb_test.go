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

// pinnedUnreachableLocalHandler emulates the gateway's strict-pin 503: the
// pin cannot serve, nothing was committed, so dispatchAuto treats it as
// fallback-eligible. Since waired-agent#325 this is the shape a down pin
// actually produces — before it, the Claude selector swallowed the error and
// served locally, so this leg never ran.
func pinnedUnreachableLocalHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(localErrorHeader, localErrPinnedPeerUnreachable)
		w.Header().Set(inferencePeerHeader, "linux-gpu")
		w.WriteHeader(http.StatusServiceUnavailable)
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

// TestRerouteNotice_PinnedPeerUnreachableNamesTheSetting pins the
// user-visible half of waired-agent#325: on the auto route a down pin no
// longer quietly runs the turn on this machine, it leaves for the Anthropic
// API — so the transcript has to say WHY, and point at the setting that
// caused it. The peer is deliberately not named: a device identifier means
// nothing to the person reading the conversation.
func TestRerouteNotice_PinnedPeerUnreachableNamesTheSetting(t *testing.T) {
	sse := sseMessageStart + textBlock(0, "answer") + sseMessageTail
	s := newServerAnnotate(t, true, Deps{
		LocalInference:       pinnedUnreachableLocalHandler(),
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

	if !strings.Contains(out, "the worker you pinned is unavailable") {
		t.Errorf("pinned reroute notice not injected:\n%s", out)
	}
	if !strings.Contains(out, "`waired worker`") {
		t.Errorf("notice must point at the setting that caused it:\n%s", out)
	}
	if strings.Contains(out, "linux-gpu") {
		t.Errorf("notice must not name the peer:\n%s", out)
	}
	if strings.Index(out, "the worker you pinned") > strings.Index(out, "event: message_delta") {
		t.Errorf("notice injected after message_delta:\n%s", out)
	}
}

// TestPinnedPeerUnreachable_WairedRouteFailsTheTurn is the other half of the
// #325 contract: on the "waired" route there is no Anthropic escape hatch, so
// the 503 must reach Claude Code and the turn visibly fails. Silently serving
// it from this machine's engine is what the issue removed.
func TestPinnedPeerUnreachable_WairedRouteFailsTheTurn(t *testing.T) {
	s := newServer(t, Deps{
		LocalInference:       pinnedUnreachableLocalHandler(),
		Degraded:             func() bool { return false },
		ClassRoute:           classRouteFunc(routeWaired),
		PassthroughTransport: fakeUpstream(nil),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp := postMessages(t, srv.URL, false)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", resp.StatusCode, string(body))
	}
	if h := resp.Header.Get(fallbackHeader); h != "" {
		t.Errorf("%s = %q, want no fallback on the waired route", fallbackHeader, h)
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
