package intercept

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// overflowHandler writes the #623 context-window 400 with the no-fallback
// marker the gateway stages — the intercept must surface it, never fall back.
func overflowHandler(hit *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hit != nil {
			*hit = true
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(localErrorHeader, localErrContextOverflow)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 300 tokens > 100 maximum"}}`)
	})
}

func TestModelsAutoServesLocallyWithRewrite(t *testing.T) {
	var gotPath string
	var last http.Request
	s := newServer(t, Deps{
		LocalInference:       recordingHandler(&gotPath),
		Degraded:             func() bool { return false }, // healthy
		ClassRoute:           classRouteFunc(routeAuto),
		PassthroughTransport: fakeUpstream(&last),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotPath != "/anthropic/v1/models" {
		t.Errorf("local dispatch path = %q, want /anthropic/v1/models", gotPath)
	}
	if resp.Header.Get("X-Fake-Upstream") == "1" {
		t.Error("auto+healthy must serve /v1/models locally, not pass through")
	}
}

func TestModelsSubpathIdRewrite(t *testing.T) {
	var gotPath string
	s := newServer(t, Deps{
		LocalInference:       recordingHandler(&gotPath),
		Degraded:             func() bool { return false },
		ClassRoute:           classRouteFunc(routeAuto),
		PassthroughTransport: fakeUpstream(nil),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/models/claude-sonnet-4")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotPath != "/anthropic/v1/models/claude-sonnet-4" {
		t.Errorf("single-model dispatch path = %q, want /anthropic/v1/models/claude-sonnet-4", gotPath)
	}
}

func TestModelsAutoDegradedPassesThrough(t *testing.T) {
	var localHit string
	var last http.Request
	s := newServer(t, Deps{
		LocalInference:       recordingHandler(&localHit),
		Degraded:             func() bool { return true }, // degraded → fail open
		ClassRoute:           classRouteFunc(routeAuto),
		PassthroughTransport: fakeUpstream(&last),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("X-Fake-Upstream") != "1" {
		t.Error("auto+degraded must pass /v1/models through to upstream")
	}
	if localHit != "" {
		t.Errorf("auto+degraded must not hit local inference, got path %q", localHit)
	}
}

func TestModelsLocalModeServesLocallyEvenDegraded(t *testing.T) {
	var gotPath string
	var last http.Request
	s := newServer(t, Deps{
		LocalInference:       recordingHandler(&gotPath),
		Degraded:             func() bool { return true }, // degraded — local mode still serves the list
		ClassRoute:           classRouteFunc(routeWaired),
		PassthroughTransport: fakeUpstream(&last),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotPath != "/anthropic/v1/models" {
		t.Errorf("route=local dispatch path = %q, want /anthropic/v1/models", gotPath)
	}
	if resp.Header.Get("X-Fake-Upstream") == "1" {
		t.Error("route=local must never pass /v1/models through")
	}
}

func TestModelsAnthropicModePassesThrough(t *testing.T) {
	var localHit string
	var last http.Request
	s := newServer(t, Deps{
		LocalInference:       recordingHandler(&localHit),
		Degraded:             func() bool { return false }, // healthy
		ClassRoute:           classRouteFunc(routeAnthropic),
		PassthroughTransport: fakeUpstream(&last),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("X-Fake-Upstream") != "1" {
		t.Error("route=anthropic must pass /v1/models through even when local is healthy")
	}
	if localHit != "" {
		t.Errorf("route=anthropic must not hit local inference, got path %q", localHit)
	}
}

// TestAutoContextOverflowSurfacedNoFallback: an overflow 400 with the
// no-fallback marker must reach the client (to trigger Claude Code
// auto-compaction), NOT be retried against the real Anthropic API.
func TestAutoContextOverflowSurfacedNoFallback(t *testing.T) {
	var localHit bool
	var last http.Request
	var fbFired bool
	s := newServer(t, Deps{
		LocalInference:       overflowHandler(&localHit),
		Degraded:             func() bool { return false },
		ClassRoute:           classRouteFunc(routeAuto),
		OnFallback:           func(string) { fbFired = true },
		PassthroughTransport: fakeUpstream(&last),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if !localHit {
		t.Error("local inference was not invoked")
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 surfaced to client", resp.StatusCode)
	}
	if resp.Header.Get("X-Fake-Upstream") == "1" {
		t.Error("context_overflow 400 must NOT fall back to upstream")
	}
	if fbFired {
		t.Error("OnFallback must not fire for a context_overflow 400")
	}
	if !strings.Contains(string(body), "prompt is too long") {
		t.Errorf("surfaced body = %q, want the overflow 400", body)
	}
	if got := resp.Header.Get(localErrorHeader); got != localErrContextOverflow {
		t.Errorf("%s = %q, want %q committed to client", localErrorHeader, got, localErrContextOverflow)
	}
}

// TestAutoNonOverflowErrorStillFallsBack: a plain uncommitted 4xx WITHOUT the
// marker keeps the historical auto-mode fallback (regression guard).
func TestAutoNonOverflowErrorStillFallsBack(t *testing.T) {
	var last http.Request
	var fbFired bool
	s := newServer(t, Deps{
		LocalInference:       errorHandler(http.StatusBadRequest, nil),
		Degraded:             func() bool { return false },
		ClassRoute:           classRouteFunc(routeAuto),
		OnFallback:           func(string) { fbFired = true },
		PassthroughTransport: fakeUpstream(&last),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("X-Fake-Upstream") != "1" {
		t.Error("plain pre-first-byte 4xx (no marker) must still fall back to upstream")
	}
	if !fbFired {
		t.Error("OnFallback should fire for a non-overflow fallback")
	}
}
