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

// TestAnthropicModelIdGoesToAnthropicWhateverThePolicy: picking a model the
// real Anthropic API serves says where the turn runs, and outranks the
// per-class /waired-route policy — in BOTH directions. The route=waired half is
// the interesting one: that setting is a standing preference for traffic nobody
// directed, not an enforcement boundary, so a narrower scope (one session's
// /model pick) may win over it (owner ruling 2026-08-28, waired-agent#1037).
//
// This replaces TestNonDirectiveFollowsPolicyWhenFlagOn, which pinned the
// opposite contract: a real Anthropic id used to be read as "no model named"
// and served locally under the default auto policy, so /model said Fable 5
// while a local model answered.
func TestAnthropicModelIdGoesToAnthropicWhateverThePolicy(t *testing.T) {
	for _, policy := range []string{routeAnthropic, routeWaired, routeAuto} {
		t.Run(policy, func(t *testing.T) {
			var localHit bool
			var bodies []string
			s := newDirectiveServer(t, Deps{
				LocalInference:       recordingHandler2(&localHit),
				Degraded:             func() bool { return false },
				ClassRoute:           classRouteFunc(policy),
				PassthroughTransport: bodyCapturingUpstream(&bodies),
			})
			srv := httptest.NewServer(s.Handler())
			defer srv.Close()

			postJSON(t, srv.URL+"/v1/messages", `{"model":"claude-opus-4-8[1m]","max_tokens":16}`)
			if localHit {
				t.Errorf("policy %q: a model the user named must not be answered by a local model", policy)
			}
			if len(bodies) != 1 {
				t.Fatalf("policy %q: upstream saw %d bodies, want 1", policy, len(bodies))
			}
			// It travels verbatim — marker and all. Rewriting it would answer
			// as a model the user did not pick, which is the defect this
			// replaces, and the upstream understands its own spelling.
			if got := upstreamModel(t, bodies[0]); got != "claude-opus-4-8[1m]" {
				t.Errorf("policy %q: upstream model = %q, want the picked id unchanged", policy, got)
			}
		})
	}
}

// TestOnRequestReportsWhatTheTurnAskedFor: the diagnostic surfaces need the
// model a turn CARRIED and where that id sent it. A turn answered by the real
// Anthropic API never reaches OnServed, so without this the host can describe
// only the traffic it served itself (waired-agent#1036).
func TestOnRequestReportsWhatTheTurnAskedFor(t *testing.T) {
	type record struct{ model, route, class string }
	for _, tc := range []struct {
		name   string
		policy string
		body   string
		path   string
		want   *record
	}{
		{"a named model, whatever the policy", routeWaired,
			`{"model":"claude-opus-5","max_tokens":16}`, "/v1/messages",
			&record{"claude-opus-5", routeAnthropic, classMain}},
		{"a Waired row", routeAnthropic,
			`{"model":"` + wairedAutoModel + `","max_tokens":16}`, "/v1/messages",
			&record{wairedAutoModel, routeAuto, classMain}},
		{"an id that decides nothing rides the policy", routeAnthropic,
			`{"model":"waired/subagent","max_tokens":16}`, "/v1/messages",
			&record{"waired/subagent", routeAnthropic, classMain}},
		{"the token-counting probe is not a turn", routeAnthropic,
			`{"model":"claude-opus-5"}`, "/v1/messages/count_tokens", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []record
			s := newDirectiveServer(t, Deps{
				LocalInference:       recordingHandler(new(string)),
				Degraded:             func() bool { return false },
				ClassRoute:           classRouteFunc(tc.policy),
				PassthroughTransport: fakeUpstream(nil),
				OnRequest: func(model, route, class string) {
					got = append(got, record{model, route, class})
				},
			})
			srv := httptest.NewServer(s.Handler())
			defer srv.Close()

			postJSON(t, srv.URL+tc.path, tc.body)
			if tc.want == nil {
				if len(got) != 0 {
					t.Fatalf("recorded %v, want nothing", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("recorded %v, want exactly one", got)
			}
			if got[0] != *tc.want {
				t.Errorf("recorded %+v, want %+v", got[0], *tc.want)
			}
		})
	}
}

// TestUnknownNonAnthropicIdStillFollowsPolicy: an id from some other vendor is
// not a Claude Code /model pick, so it keeps riding the per-class policy rather
// than being sent to an API that would reject it.
func TestUnknownNonAnthropicIdStillFollowsPolicy(t *testing.T) {
	var gotPath string
	s := newDirectiveServer(t, Deps{
		LocalInference:       recordingHandler(&gotPath),
		Degraded:             func() bool { return false },
		ClassRoute:           classRouteFunc(routeWaired),
		PassthroughTransport: fakeUpstream(nil),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"gpt-4o","max_tokens":16}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if gotPath != "/anthropic/v1/messages" {
		t.Errorf("a non-Anthropic id under route=waired must follow the policy (gotPath=%q)", gotPath)
	}
	if resp.Header.Get("X-Fake-Upstream") == "1" {
		t.Error("a non-Anthropic id under route=waired must not pass through")
	}
}

// TestDirectiveRouteMapping is a pure unit check of the id→route table.
func TestDirectiveRouteMapping(t *testing.T) {
	cases := map[string]struct {
		wantRoute string
		wantOK    bool
	}{
		wairedLocalModel: {routeWaired, true},
		wairedAutoModel:  {routeAuto, true},
		wairedCloudModel: {routeAnthropic, true},
		// Claude Code strips "[1m]" before sending, so the bare spellings are
		// what actually arrive — and they must map the same way
		// (waired-agent#1036: the bare cloud id missed the table, was served
		// locally, and then poisoned every fallback replay).
		wairedCloudBareModel:     {routeAnthropic, true},
		"claude-waired-auto[1m]": {routeAuto, true},
		"CLAUDE-WAIRED-AUTO":     {routeAuto, true},
		// A model the real Anthropic API serves names where it runs
		// (waired-agent#1037). This outranks the per-class policy, the same
		// way the reserved ids do.
		"claude-opus-4-8[1m]": {routeAnthropic, true},
		"claude-fable-5":      {routeAnthropic, true},
		// Not a Claude Code /model pick: no route is forced, so it keeps
		// following the policy.
		"gpt-4o":          {"", false},
		"waired/subagent": {"", false},
		"waired/default":  {"", false},
		"":                {"", false},
	}
	for model, want := range cases {
		gotRoute, gotOK := directiveRoute(model)
		if gotRoute != want.wantRoute || gotOK != want.wantOK {
			t.Errorf("directiveRoute(%q) = (%q,%v), want (%q,%v)", model, gotRoute, gotOK, want.wantRoute, want.wantOK)
		}
	}
}
