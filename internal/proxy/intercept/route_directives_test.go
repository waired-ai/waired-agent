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

// TestDirectiveLocalForcesWaired: the reserved local id runs the turn on this
// device. There is no policy left to override — the id is the whole decision
// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md).
func TestDirectiveLocalForcesWaired(t *testing.T) {
	var gotPath string
	s := newDirectiveServer(t, Deps{
		LocalInference:       recordingHandler(&gotPath),
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

// TestDirectiveCloudForcesAnthropic: the reserved cloud id pins the request to
// the real Anthropic API even though the per-class policy says waired, and the
// fake id is rewritten to a real model on passthrough (upstream would reject
// "claude-waired-cloud[1m]").
func TestDirectiveCloudForcesAnthropic(t *testing.T) {
	var bodies []string
	var localHit bool
	s := newDirectiveServer(t, Deps{
		LocalInference:       recordingHandler2(&localHit),
		PassthroughTransport: bodyCapturingUpstream(&bodies),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	postJSON(t, srv.URL+"/v1/messages", `{"model":"`+legacyCloudModel+`","max_tokens":16}`)
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

// TestDirectiveAutoServesOnWaired: the any-node id runs the turn on Waired.
// It is spelled "auto" for the route it used to force; the route is gone and
// the id is not, because sessions carry it (waired-agent#1185 renames the row).
func TestDirectiveAutoServesOnWaired(t *testing.T) {
	var gotPath string
	s := newDirectiveServer(t, Deps{
		LocalInference:       recordingHandler(&gotPath),
		PassthroughTransport: fakeUpstream(nil),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"`+legacyAutoModel+`","max_tokens":16}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if gotPath != "/anthropic/v1/messages" {
		t.Errorf("the any-node id did not serve on Waired (gotPath=%q)", gotPath)
	}
	if resp.Header.Get("X-Fake-Upstream") == "1" {
		t.Error("a Waired id must not pass through to the real Anthropic API")
	}
}

// TestDirectiveHonouredEvenWhenFlagOff: the flag gates the ADVERTISEMENT, not
// the routing. Claude Code's picker cache has no TTL, so a session can carry
// an id this build no longer offers, and the alternative — relaying a Waired
// id upstream — is a 404 there.
func TestDirectiveHonouredEvenWhenFlagOff(t *testing.T) {
	var gotPath string
	s := newServer(t, Deps{ // newServer => ModelRouteDirectives off
		LocalInference:       recordingHandler(&gotPath),
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
	if resp.Header.Get("X-Fake-Upstream") == "1" {
		t.Error("with the feature off, a Waired id was relayed upstream, where it is a 404")
	}
	if gotPath != "/anthropic/v1/messages" {
		t.Errorf("feature off: the Waired id must still be served here (gotPath=%q)", gotPath)
	}
}

// TestAnthropicModelIdGoesToAnthropic: picking a model the real Anthropic API
// serves says where the turn runs (owner ruling 2026-08-28, waired-agent#1037),
// and it is now the ONLY way a turn leaves this machine
// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md).
// The subtest names are what the id used to have to beat.
func TestAnthropicModelIdGoesToAnthropic(t *testing.T) {
	for _, policy := range []string{"a machine with a local engine", "and one without a preference"} {
		t.Run(policy, func(t *testing.T) {
			var localHit bool
			var bodies []string
			s := newDirectiveServer(t, Deps{
				LocalInference:       recordingHandler2(&localHit),
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
		policy string // unused since the routes went; kept so the rows read as prose
		body   string
		path   string
		want   *record
	}{
		{"a named Anthropic model", "",
			`{"model":"claude-opus-5","max_tokens":16}`, "/v1/messages",
			&record{"claude-opus-5", routeAnthropic, classMain}},
		{"a Waired row", "",
			`{"model":"` + legacyAutoModel + `","max_tokens":16}`, "/v1/messages",
			&record{legacyAutoModel, routeWaired, classMain}},
		{"an id that names neither side stays here", "",
			`{"model":"waired/subagent","max_tokens":16}`, "/v1/messages",
			&record{"waired/subagent", routeWaired, classMain}},
		{"the token-counting probe is not a turn", "",
			`{"model":"claude-opus-5"}`, "/v1/messages/count_tokens", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []record
			s := newDirectiveServer(t, Deps{
				LocalInference:       recordingHandler(new(string)),
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
		t.Errorf("an id naming neither side must be served here (gotPath=%q)", gotPath)
	}
	if resp.Header.Get("X-Fake-Upstream") == "1" {
		t.Error("an id the real Anthropic API does not serve must not be relayed there")
	}
}

// TestDirectiveRouteMapping is a pure unit check of the id→route table.
func TestDirectiveRouteMapping(t *testing.T) {
	cases := map[string]struct {
		wantRoute string
		wantOK    bool
	}{
		wairedLocalModel: {routeWaired, true},
		legacyAutoModel:  {routeWaired, true},
		legacyCloudModel: {routeAnthropic, true},
		// Claude Code strips "[1m]" before sending, so the bare spellings are
		// what actually arrive — and they must map the same way
		// (waired-agent#1036: the bare cloud id missed the table, was served
		// locally, and then poisoned every fallback replay).
		legacyCloudBareModel:     {routeAnthropic, true},
		"claude-waired-auto[1m]": {routeWaired, true},
		"CLAUDE-WAIRED-AUTO":     {routeWaired, true},
		// A model the real Anthropic API serves names where it runs
		// (waired-agent#1037), and is the only id that leaves this machine.
		"claude-opus-4-8[1m]": {routeAnthropic, true},
		"claude-fable-5":      {routeAnthropic, true},
		// Names neither side. routeForModel serves these here, which is what
		// keeps the sentinel's claim true: nothing but a turn carrying a real
		// Anthropic id leaves the machine.
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
