package intercept

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// classRoutePolicy builds a Deps.ClassRoute from a class→route map;
// classes absent from the map default to auto.
func classRoutePolicy(routes map[string]string) func(string) string {
	return func(class string) string {
		if r, ok := routes[class]; ok {
			return r
		}
		return routeAuto
	}
}

// testClassifier mirrors the production classifier: the subagent label
// is sub, everything else is main.
func testClassifier(modelID string) string {
	if modelID == "waired/subagent" {
		return "sub"
	}
	return "main"
}

// unreachableUpstream fails every round trip at the transport level —
// the "api.anthropic.com cannot be reached" shape (no response byte).
func unreachableUpstream() http.RoundTripper {
	return rtFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp: connection refused")
	})
}

// upstreamStatus responds with a fixed HTTP status — an upstream that
// IS reachable but rejects the request (401/429/5xx).
func upstreamStatus(status int, hit *bool) http.RoundTripper {
	return rtFunc(func(r *http.Request) (*http.Response, error) {
		if hit != nil {
			*hit = true
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"X-Fake-Upstream": []string{"1"}},
			Body:       io.NopCloser(strings.NewReader(`{"type":"error"}`)),
			Request:    r,
		}, nil
	})
}

func TestNodeTargetMainAnthropicPassesThrough(t *testing.T) {
	var localHit bool
	var bodies []string
	s := newServer(t, Deps{
		LocalInference:       recordingHandler2(&localHit),
		Degraded:             func() bool { return false },
		PassthroughTransport: bodyCapturingUpstream(&bodies),
		ClassifyModel:        testClassifier,
		ClassRoute:           classRoutePolicy(map[string]string{"main": routeAnthropic}),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-sonnet-5","max_tokens":16}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(bodies) != 1 {
		t.Fatalf("main=anthropic did not pass through despite healthy local (upstream hits=%d)", len(bodies))
	}
	if localHit {
		t.Error("main=anthropic must not hit local inference")
	}
	// A real Anthropic model id passes through byte-identical (no rewrite).
	if got := bodies[0]; got != `{"model":"claude-sonnet-5","max_tokens":16}` {
		t.Errorf("upstream body mutated: %q", got)
	}
}

func TestNodeTargetSubStaysLocalWhenMainAnthropicTargeted(t *testing.T) {
	var localHit bool
	s := newServer(t, Deps{
		LocalInference:       recordingHandler2(&localHit),
		Degraded:             func() bool { return false },
		PassthroughTransport: fakeUpstream(nil),
		ClassifyModel:        testClassifier,
		ClassRoute:           classRoutePolicy(map[string]string{"main": routeAnthropic}),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"waired/subagent","max_tokens":16}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !localHit {
		t.Error("sub-labelled request must serve locally under the hybrid preset")
	}
	if resp.Header.Get("X-Fake-Upstream") == "1" {
		t.Error("sub-labelled request leaked upstream")
	}
}

func TestNodeTargetSubAnthropicRewritesWairedModel(t *testing.T) {
	var localHit bool
	var bodies []string
	s, err := NewServer(Config{
		Addr:                     "127.0.0.1:0",
		PassthroughModelOverride: "claude-test-1",
	}, Deps{
		LocalInference:       recordingHandler2(&localHit),
		Degraded:             func() bool { return false },
		PassthroughTransport: bodyCapturingUpstream(&bodies),
		ClassifyModel:        testClassifier,
		ClassRoute:           classRoutePolicy(map[string]string{"sub": routeAnthropic}),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"waired/subagent","max_tokens":16}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if localHit {
		t.Error("sub=anthropic must not hit local inference")
	}
	if len(bodies) != 1 {
		t.Fatalf("sub=anthropic did not reach upstream (hits=%d)", len(bodies))
	}
	if !strings.Contains(bodies[0], `"claude-test-1"`) {
		t.Errorf("waired/subagent id must be rewritten on the upstream leg, got %q", bodies[0])
	}
}

func TestClassRouteMainWairedServesLocal(t *testing.T) {
	// main=waired serves locally and never leaks upstream, even for a real
	// Anthropic model id — the per-class privacy-strong option.
	var localHit bool
	s := newServer(t, Deps{
		LocalInference:       recordingHandler2(&localHit),
		Degraded:             func() bool { return false },
		PassthroughTransport: fakeUpstream(nil),
		ClassifyModel:        testClassifier,
		ClassRoute:           classRoutePolicy(map[string]string{"main": routeWaired}),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-sonnet-5"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !localHit {
		t.Error("main=waired must serve locally")
	}
	if resp.Header.Get("X-Fake-Upstream") == "1" {
		t.Error("main=waired must never leak upstream")
	}
}

func TestNodeTargetDegradesToLocalWhenUpstreamUnreachable(t *testing.T) {
	var localHit bool
	var gotClass, gotReason string
	s := newServer(t, Deps{
		LocalInference:       recordingHandler2(&localHit),
		Degraded:             func() bool { return false },
		PassthroughTransport: unreachableUpstream(),
		ClassifyModel:        testClassifier,
		ClassRoute:           classRoutePolicy(map[string]string{"main": routeAnthropic}),
		OnNodeFallback: func(class, reason string) {
			gotClass, gotReason = class, reason
		},
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-sonnet-5"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !localHit {
		t.Error("unreachable upstream must degrade to local serving")
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("degraded request status=%d want 200 from local", resp.StatusCode)
	}
	if gotClass != "main" || gotReason != "anthropic_unreachable" {
		t.Errorf("OnNodeFallback = (%q, %q), want (main, anthropic_unreachable)", gotClass, gotReason)
	}
	if got := resp.Header.Get(fallbackHeader); !strings.Contains(got, "local") {
		t.Errorf("degrade must be marked on %s, got %q", fallbackHeader, got)
	}
}

func TestNodeTargetUpstreamHTTPErrorRelayedVerbatim(t *testing.T) {
	// A reachable upstream that rejects (401 without credentials, 429,
	// 5xx) produced a response — relay it; only transport-level
	// unreachability degrades.
	var localHit, upstreamHit bool
	s := newServer(t, Deps{
		LocalInference:       recordingHandler2(&localHit),
		Degraded:             func() bool { return false },
		PassthroughTransport: upstreamStatus(http.StatusUnauthorized, &upstreamHit),
		ClassifyModel:        testClassifier,
		ClassRoute:           classRoutePolicy(map[string]string{"main": routeAnthropic}),
		OnNodeFallback: func(class, reason string) {
			t.Errorf("OnNodeFallback must not fire on an upstream HTTP error (got %s/%s)", class, reason)
		},
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-sonnet-5"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !upstreamHit {
		t.Fatal("request never reached upstream")
	}
	if localHit {
		t.Error("upstream 401 must not degrade to local")
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status=%d want the upstream 401 relayed verbatim", resp.StatusCode)
	}
}

func TestOverCapBodyRidesMainRoute(t *testing.T) {
	// An unclassifiable (over-cap) body cannot be attributed to a class, so it
	// rides the MAIN route — consistent with the classifier's own "unlabeled
	// traffic is main" rule. With main=anthropic that means passthrough.
	old := maxFallbackBodyBytes
	maxFallbackBodyBytes = 32
	defer func() { maxFallbackBodyBytes = old }()

	var localHit bool
	var last http.Request
	s := newServer(t, Deps{
		LocalInference:       recordingHandler2(&localHit),
		Degraded:             func() bool { return false },
		PassthroughTransport: fakeUpstream(&last),
		ClassifyModel:        testClassifier,
		ClassRoute:           classRoutePolicy(map[string]string{"main": routeAnthropic}),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	big := `{"model":"claude-sonnet-5","pad":"` + strings.Repeat("x", 64) + `"}`
	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if localHit {
		t.Error("an over-cap body under main=anthropic must not serve locally")
	}
	if resp.Header.Get("X-Fake-Upstream") != "1" {
		t.Error("over-cap body must ride the main route (anthropic passthrough)")
	}
}

func TestNodeTargetNilPolicyKeepsAutoBehaviour(t *testing.T) {
	var localHit bool
	s := newServer(t, Deps{
		LocalInference:       recordingHandler2(&localHit),
		Degraded:             func() bool { return false },
		PassthroughTransport: fakeUpstream(nil),
		ClassifyModel:        testClassifier,
		// ClassRoute nil: every class is auto.
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-sonnet-5"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !localHit {
		t.Error("nil ClassRoute must keep the default auto flow (local)")
	}
}

func TestNodeTargetCountTokensFollowsClassPolicy(t *testing.T) {
	var localHit bool
	var last http.Request
	s := newServer(t, Deps{
		LocalInference:       recordingHandler2(&localHit),
		Degraded:             func() bool { return false },
		PassthroughTransport: fakeUpstream(&last),
		ClassifyModel:        testClassifier,
		ClassRoute:           classRoutePolicy(map[string]string{"main": routeAnthropic}),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages/count_tokens", "application/json",
		strings.NewReader(`{"model":"claude-sonnet-5"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("X-Fake-Upstream") != "1" {
		t.Error("count_tokens must follow the same per-class policy as messages")
	}
	if localHit {
		t.Error("count_tokens for an anthropic-target class must not hit local")
	}
}
