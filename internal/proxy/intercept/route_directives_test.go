package intercept

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newDirectiveServer builds a Server with the #52 model-route-directives
// feature enabled.
func newDirectiveServer(t *testing.T, deps Deps) *Server {
	t.Helper()
	s, err := NewServer(Config{Addr: "127.0.0.1:0", ModelRouteDirectives: true}, deps)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s
}

// TestDirectiveLocalForcesWaired: the reserved local id pins the request to
// LOCAL inference even though the per-class policy says anthropic — the
// directive overrides /waired-route.
func TestDirectiveLocalForcesWaired(t *testing.T) {
	var gotPath string
	s := newDirectiveServer(t, Deps{
		LocalInference:       recordingHandler(&gotPath),
		Degraded:             func() bool { return false },
		ClassRoute:           classRouteFunc(routeAnthropic), // opposite of the directive
		PassthroughTransport: fakeUpstream(nil),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"`+wairedLocalModel+`","max_tokens":16}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if gotPath != "/anthropic/v1/messages" {
		t.Errorf("local directive did not serve locally (gotPath=%q)", gotPath)
	}
	if resp.Header.Get("X-Fake-Upstream") == "1" {
		t.Error("local directive must not pass through to the real Anthropic API")
	}
}

// TestDirectiveLocalServesEvenWhenDegraded: the local directive is strict
// (route=waired semantics) — it serves locally and surfaces the local error
// rather than failing open, so a degraded engine still exercises local.
func TestDirectiveLocalServesEvenWhenDegraded(t *testing.T) {
	var gotPath string
	s := newDirectiveServer(t, Deps{
		LocalInference:       recordingHandler(&gotPath),
		Degraded:             func() bool { return true }, // degraded
		ClassRoute:           classRouteFunc(routeAuto),
		PassthroughTransport: fakeUpstream(nil),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"`+wairedLocalModel+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if gotPath != "/anthropic/v1/messages" {
		t.Errorf("local directive must serve locally even when degraded (gotPath=%q)", gotPath)
	}
	if resp.Header.Get("X-Fake-Upstream") == "1" {
		t.Error("local directive must never leak upstream")
	}
}

// TestDirectiveCloudForcesAnthropic: the reserved cloud id pins the request to
// the real Anthropic API even though the per-class policy says waired, and the
// fake id is rewritten to a real model on passthrough (upstream would reject
// "claude-waired-cloud[1m]").
func TestDirectiveCloudForcesAnthropic(t *testing.T) {
	var bodies []string
	var localHit bool
	s := newDirectiveServer(t, Deps{
		LocalInference:       recordingHandler2(&localHit),
		Degraded:             func() bool { return false },
		ClassRoute:           classRouteFunc(routeWaired), // opposite of the directive
		PassthroughTransport: bodyCapturingUpstream(&bodies),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	postJSON(t, srv.URL+"/v1/messages", `{"model":"`+wairedCloudModel+`","max_tokens":16}`)
	if localHit {
		t.Error("cloud directive must not serve locally")
	}
	if len(bodies) != 1 {
		t.Fatalf("upstream saw %d bodies, want 1", len(bodies))
	}
	if got := upstreamModel(t, bodies[0]); got != defaultPassthroughModel {
		t.Errorf("cloud directive upstream model = %q, want rewritten %q (never the fake id)", got, defaultPassthroughModel)
	}
}

// TestDirectiveAutoForcesAuto: the reserved auto id forces route=auto even
// though the per-class policy says anthropic — so a healthy local engine serves
// the turn instead of the real Anthropic API.
func TestDirectiveAutoForcesAuto(t *testing.T) {
	var gotPath string
	s := newDirectiveServer(t, Deps{
		LocalInference:       recordingHandler(&gotPath),
		Degraded:             func() bool { return false },   // healthy
		ClassRoute:           classRouteFunc(routeAnthropic), // opposite of the directive
		PassthroughTransport: fakeUpstream(nil),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"`+wairedAutoModel+`","max_tokens":16}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if gotPath != "/anthropic/v1/messages" {
		t.Errorf("auto directive did not serve locally when healthy (gotPath=%q)", gotPath)
	}
	if resp.Header.Get("X-Fake-Upstream") == "1" {
		t.Error("auto directive with healthy local must not pass through")
	}
}

// TestDirectiveAutoFallsBackRewritten: the auto id is Waired-first with Anthropic
// fallback — when local is degraded the turn fails open to the real API, and the
// synthetic id MUST be rewritten to a real model (the auto id would otherwise be
// rejected upstream). Guards the passthrough-rewrite generalization to all
// directive ids.
func TestDirectiveAutoFallsBackRewritten(t *testing.T) {
	var bodies []string
	var localHit bool
	s := newDirectiveServer(t, Deps{
		LocalInference:       recordingHandler2(&localHit),
		Degraded:             func() bool { return true }, // degraded → fail open
		ClassRoute:           classRouteFunc(routeWaired),
		PassthroughTransport: bodyCapturingUpstream(&bodies),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	postJSON(t, srv.URL+"/v1/messages", `{"model":"`+wairedAutoModel+`","max_tokens":16}`)
	if len(bodies) != 1 {
		t.Fatalf("auto+degraded upstream saw %d bodies, want 1 (fail open)", len(bodies))
	}
	if got := upstreamModel(t, bodies[0]); got != defaultPassthroughModel {
		t.Errorf("auto fallback upstream model = %q, want rewritten %q (never the fake id)", got, defaultPassthroughModel)
	}
}

// TestDirectiveIgnoredWhenFlagOff: with the feature off, the reserved id is a
// plain unknown model and rides the per-class policy — proving the override is
// strictly opt-in and does not perturb the default fast path.
func TestDirectiveIgnoredWhenFlagOff(t *testing.T) {
	var gotPath string
	s := newServer(t, Deps{ // newServer => ModelRouteDirectives off
		LocalInference:       recordingHandler(&gotPath),
		Degraded:             func() bool { return false },
		ClassRoute:           classRouteFunc(routeAnthropic),
		PassthroughTransport: fakeUpstream(nil),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"`+wairedLocalModel+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("X-Fake-Upstream") != "1" {
		t.Error("with the feature off, the reserved id must follow the anthropic policy (passthrough)")
	}
	if gotPath != "" {
		t.Errorf("feature off: must not force local (gotPath=%q)", gotPath)
	}
}

// TestNonDirectiveFollowsPolicyWhenFlagOn: a normal model id is unaffected by
// the feature and still follows the per-class /waired-route policy — the two
// mechanisms run in parallel.
func TestNonDirectiveFollowsPolicyWhenFlagOn(t *testing.T) {
	var localHit bool
	s := newDirectiveServer(t, Deps{
		LocalInference:       recordingHandler2(&localHit),
		Degraded:             func() bool { return false },
		ClassRoute:           classRouteFunc(routeAnthropic),
		PassthroughTransport: fakeUpstream(nil),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-4-8[1m]","max_tokens":16}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("X-Fake-Upstream") != "1" {
		t.Error("a normal id under route=anthropic must pass through, feature flag notwithstanding")
	}
	if localHit {
		t.Error("a normal id under route=anthropic must not serve locally")
	}
}

// TestDirectiveRouteMapping is a pure unit check of the id→route table.
func TestDirectiveRouteMapping(t *testing.T) {
	cases := map[string]struct {
		wantRoute string
		wantOK    bool
	}{
		wairedLocalModel:      {routeWaired, true},
		wairedAutoModel:       {routeAuto, true},
		wairedCloudModel:      {routeAnthropic, true},
		"claude-opus-4-8[1m]": {"", false},
		"waired/subagent":     {"", false},
		"":                    {"", false},
	}
	for model, want := range cases {
		gotRoute, gotOK := directiveRoute(model)
		if gotRoute != want.wantRoute || gotOK != want.wantOK {
			t.Errorf("directiveRoute(%q) = (%q,%v), want (%q,%v)", model, gotRoute, gotOK, want.wantRoute, want.wantOK)
		}
	}
}
