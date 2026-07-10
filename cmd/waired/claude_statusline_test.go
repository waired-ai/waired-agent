package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// errDetectFailed stands in for any DetectEffectiveStatusLine failure.
var errDetectFailed = errors.New("detect failed")

// plainStatusline forces deterministic ASCII (no color, no emoji) so the
// rendered segment can be asserted exactly.
func plainStatusline(t *testing.T) {
	t.Helper()
	t.Setenv("NO_COLOR", "1")
	t.Setenv("WAIRED_NO_EMOJI", "1")
}

func fallbackAt(when time.Time, count int64, reason, direction string) *management.ClaudeRoutingFallbackEvent {
	return &management.ClaudeRoutingFallbackEvent{When: when, Reason: reason, Count: count, Direction: direction}
}

func routing(main state.ClaudeRouteClass, opts ...func(*management.ClaudeRoutingState)) management.ClaudeRoutingState {
	st := management.ClaudeRoutingState{Policy: state.ClaudeRoutingPolicy{Main: main, Sub: state.ClaudeRouteSame}}
	for _, o := range opts {
		o(&st)
	}
	return st
}

func withModel(m string) func(*management.ClaudeRoutingState) {
	return func(st *management.ClaudeRoutingState) { st.LastLocalModel = m }
}

func withFallback(e *management.ClaudeRoutingFallbackEvent) func(*management.ClaudeRoutingState) {
	return func(st *management.ClaudeRoutingState) { st.LastFallback = e }
}

func TestRenderStatusline(t *testing.T) {
	plainStatusline(t)
	now := time.Now()
	cases := []struct {
		name   string
		route  management.ClaudeRoutingState
		health string
		want   string
	}{
		{"auto-waired", routing(state.ClaudeRouteAuto), "ready", "waired: on Waired"},
		{"auto-degraded", routing(state.ClaudeRouteAuto), "degraded", "waired: fallback -> Anthropic (local degraded)"},
		{"auto-recent-fallback", routing(state.ClaudeRouteAuto, withFallback(fallbackAt(now.Add(-2*time.Second), 1, "local_status_503", "anthropic"))), "ready", "waired: fell back -> Anthropic"},
		{"waired-ready", routing(state.ClaudeRouteWaired), "ready", "waired: Waired-only"},
		{"waired-down", routing(state.ClaudeRouteWaired), "no_engine", "! waired: Waired-only (down)"},
		{"anthropic", routing(state.ClaudeRouteAnthropic), "ready", "-> waired: Anthropic"},
		{"empty-mode-defaults-auto", management.ClaudeRoutingState{}, "ready", "waired: on Waired"},
		// #602: the last locally-served model id is appended while serving on
		// Waired, and hidden on every non-Waired-serving branch.
		{"auto-waired-model", routing(state.ClaudeRouteAuto, withModel("qwen3-8b-instruct")), "ready", "waired: on Waired (qwen3-8b-instruct)"},
		{"auto-degraded-hides-model", routing(state.ClaudeRouteAuto, withModel("qwen3-8b-instruct")), "degraded", "waired: fallback -> Anthropic (local degraded)"},
		{"auto-recent-fallback-hides-model", routing(state.ClaudeRouteAuto, withModel("qwen3-8b-instruct"), withFallback(fallbackAt(now.Add(-2*time.Second), 1, "local_status_503", "anthropic"))), "ready", "waired: fell back -> Anthropic"},
		{"waired-ready-model", routing(state.ClaudeRouteWaired, withModel("qwen3-8b-instruct")), "ready", "waired: Waired-only (qwen3-8b-instruct)"},
		{"waired-down-hides-model", routing(state.ClaudeRouteWaired, withModel("qwen3-8b-instruct")), "no_engine", "! waired: Waired-only (down)"},
		// A local-degrade fallback (anthropic route → local) must NOT read as a
		// "fell back to Anthropic" segment.
		{"local-degrade-ignored-in-auto", routing(state.ClaudeRouteAuto, withFallback(fallbackAt(now.Add(-2*time.Second), 1, "anthropic_unreachable", "local"))), "ready", "waired: on Waired"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderStatusline(tc.route, tc.health); got != tc.want {
				t.Errorf("renderStatusline = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStatuslineDownPlain(t *testing.T) {
	plainStatusline(t)
	if got := statuslineDown(); got != "x waired: agent down" {
		t.Errorf("statuslineDown = %q", got)
	}
}

func TestRenderStatuslineColorized(t *testing.T) {
	t.Setenv("WAIRED_NO_EMOJI", "1") // drop glyphs, keep color
	got := renderStatusline(routing(state.ClaudeRouteAuto), "ready")
	if !strings.HasPrefix(got, ansiGreen) || !strings.HasSuffix(got, ansiReset) {
		t.Errorf("expected green-wrapped segment, got %q", got)
	}
}

// routeStub serves the given routing state on the Claude route endpoint and an
// inference status carrying subsystemState.
func routeStub(t *testing.T, st management.ClaudeRoutingState, subsystemState string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/waired/v1/integration/claude/route":
			_ = json.NewEncoder(w).Encode(st)
		case inferenceStatusPath:
			_ = json.NewEncoder(w).Encode(map[string]any{"subsystem_state": subsystemState})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchRouteAndHealth(t *testing.T) {
	srv := routeStub(t, routing(state.ClaudeRouteWaired), "degraded")
	route, health, ok := fetchRouteAndHealth(srv.URL)
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if route.Policy.Main != state.ClaudeRouteWaired {
		t.Errorf("main = %q", route.Policy.Main)
	}
	if health != "degraded" {
		t.Errorf("health = %q", health)
	}
}

func TestFetchRouteAndHealthUnreachable(t *testing.T) {
	srv := routeStub(t, management.ClaudeRoutingState{}, "ready")
	url := srv.URL
	srv.Close() // now unreachable
	if _, _, ok := fetchRouteAndHealth(url); ok {
		t.Error("ok = true against a closed server")
	}
}

func TestRunFallbackHookEmitsOnNewFallback(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	st := routing(state.ClaudeRouteAuto, withFallback(fallbackAt(time.Now(), 1, "local_status_503", "anthropic")))
	srv := routeStub(t, st, "ready")
	stdin := strings.NewReader(`{"session_id":"sess-A","hook_event_name":"Stop"}`)

	var out bytes.Buffer
	if err := runFallbackHook(srv.URL, stdin, &out); err != nil {
		t.Fatalf("hook: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("expected a systemMessage JSON, got %q (%v)", out.String(), err)
	}
	if !strings.Contains(got["systemMessage"], "real Anthropic API") || !strings.Contains(got["systemMessage"], "local_status_503") {
		t.Errorf("systemMessage = %q", got["systemMessage"])
	}

	// Same count again for the same session ⇒ no repeat.
	out.Reset()
	if err := runFallbackHook(srv.URL, strings.NewReader(`{"session_id":"sess-A"}`), &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("hook repeated on unchanged count: %q", out.String())
	}
}

func TestRunFallbackHookSuppressesStale(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	st := routing(state.ClaudeRouteAuto, withFallback(fallbackAt(time.Now().Add(-10*time.Minute), 5, "local_status_400", "anthropic")))
	srv := routeStub(t, st, "ready")
	var out bytes.Buffer
	if err := runFallbackHook(srv.URL, strings.NewReader(`{"session_id":"sess-B"}`), &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("hook emitted for a stale fallback: %q", out.String())
	}
}

func TestRunFallbackHookSuppressesLocalDirection(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	// A local-degrade (anthropic route → local) is not a "reply came from
	// Anthropic" notice.
	st := routing(state.ClaudeRouteAnthropic, withFallback(fallbackAt(time.Now(), 1, "anthropic_unreachable", "local")))
	srv := routeStub(t, st, "ready")
	var out bytes.Buffer
	if err := runFallbackHook(srv.URL, strings.NewReader(`{"session_id":"sess-L"}`), &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("hook emitted for a local-direction fallback: %q", out.String())
	}
}

func TestRunFallbackHookSilentWhenNoFallback(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	srv := routeStub(t, routing(state.ClaudeRouteAuto), "ready")
	var out bytes.Buffer
	if err := runFallbackHook(srv.URL, strings.NewReader(`{"session_id":"sess-C"}`), &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("hook emitted with no LastFallback: %q", out.String())
	}
}

func TestRunFallbackHookSilentWhenUnreachable(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	srv := routeStub(t, management.ClaudeRoutingState{}, "ready")
	url := srv.URL
	srv.Close()
	var out bytes.Buffer
	if err := runFallbackHook(url, strings.NewReader(`{"session_id":"sess-D"}`), &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("hook emitted against a closed agent: %q", out.String())
	}
}

func TestMgmtURL(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:9476":        "http://127.0.0.1:9476" + inferenceStatusPath,
		"http://127.0.0.1:9476": "http://127.0.0.1:9476" + inferenceStatusPath,
		"127.0.0.1:9476/":       "http://127.0.0.1:9476" + inferenceStatusPath,
	}
	for in, want := range cases {
		if got := mgmtURL(in, inferenceStatusPath); got != want {
			t.Errorf("mgmtURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStatuslineShadowNotice(t *testing.T) {
	eff := claudecode.EffectiveStatusLine{
		Scope:   claudecode.ScopeProjectLocal,
		Path:    "/repo/.claude/settings.local.json",
		Kind:    claudecode.StatusLineForeign,
		Command: "bash ~/.claude/statusline.sh",
	}
	got := statuslineShadowNotice(eff, nil)
	for _, want := range []string{"/repo/.claude/settings.local.json", "project-local", statuslineSnippet} {
		if !strings.Contains(got, want) {
			t.Errorf("notice missing %q:\n%s", want, got)
		}
	}

	if got := statuslineShadowNotice(claudecode.EffectiveStatusLine{Scope: claudecode.ScopeUser}, nil); got != "" {
		t.Errorf("user scope must not be reported as shadowed: %q", got)
	}
	if got := statuslineShadowNotice(claudecode.EffectiveStatusLine{}, nil); got != "" {
		t.Errorf("no statusline must not be reported as shadowed: %q", got)
	}
	if got := statuslineShadowNotice(eff, errDetectFailed); got != "" {
		t.Errorf("detection errors must be silent (best-effort): %q", got)
	}
}
