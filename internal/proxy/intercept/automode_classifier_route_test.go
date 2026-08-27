package intercept

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// classifierBody is a stage-1 auto-mode classifier request, shaped from real
// captures against Claude Code 2.1.247 (waired-agent#1041): no tools, a tiny
// budget, and the stop sequence that closes the verdict tag.
const classifierBody = `{"model":"claude-sonnet-5","max_tokens":64,` +
	`"stop_sequences":["</severity>"],` +
	`"messages":[{"role":"user","content":"<transcript>"}]}`

// TestAutoModeClassifierGoesToAnthropicOnEveryRoute is the ratified rule:
// PRODUCT CONTRACT (owner ruling, waired-agent#1041) — the auto-mode safety
// classifier is answered by the real Anthropic API whatever the per-class
// route says, including "waired". The waired row is the whole point: that
// route otherwise never leaves the device.
func TestAutoModeClassifierGoesToAnthropicOnEveryRoute(t *testing.T) {
	for _, route := range []string{routeAuto, routeWaired, routeAnthropic} {
		t.Run(route, func(t *testing.T) {
			var localHit bool
			s := newServer(t, Deps{
				LocalInference:       recordingHandler2(&localHit),
				Degraded:             func() bool { return false },
				ClassRoute:           classRouteFunc(route),
				PassthroughTransport: fakeUpstream(nil),
			})
			srv := httptest.NewServer(s.Handler())
			defer srv.Close()

			resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
				strings.NewReader(classifierBody))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.Header.Get("X-Fake-Upstream") != "1" {
				t.Errorf("route=%s: the classifier did not reach the Anthropic API", route)
			}
			if localHit {
				t.Errorf("route=%s: the classifier was served by local inference", route)
			}
		})
	}
}

// TestAutoModeClassifierBeatsTheDirectiveID pins the ORDER. Once a classifier
// request has failed, Claude Code re-sends the classifier under the session's
// own model, which on this surface can be a reserved directive id
// (waired-agent#1039 observed `claude-waired-auto`). Were the directive id
// consulted first, the strict-local id would pull the permission decision back
// onto this device — the exact thing the ruling forbids.
func TestAutoModeClassifierBeatsTheDirectiveID(t *testing.T) {
	var localHit bool
	s := newDirectiveServer(t, Deps{
		LocalInference:       recordingHandler2(&localHit),
		Degraded:             func() bool { return false },
		ClassRoute:           classRouteFunc(routeAuto),
		PassthroughTransport: fakeUpstream(nil),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	body := `{"model":"` + wairedLocalModel + `","max_tokens":64,` +
		`"stop_sequences":["</severity>"],` +
		`"messages":[{"role":"user","content":"<transcript>"}]}`
	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("X-Fake-Upstream") != "1" {
		t.Error("the strict-local directive id kept the classifier on this device")
	}
	if localHit {
		t.Error("the classifier was served by local inference under a directive id")
	}
}

// TestNonClassifierBodyStillHonoursRouteWaired is the non-regression half: the
// check above must not widen into "anything small goes upstream". A normal
// turn on route=waired stays on this device exactly as before.
func TestNonClassifierBodyStillHonoursRouteWaired(t *testing.T) {
	var localHit bool
	s := newServer(t, Deps{
		LocalInference:       recordingHandler2(&localHit),
		Degraded:             func() bool { return false },
		ClassRoute:           classRouteFunc(routeWaired),
		PassthroughTransport: fakeUpstream(nil),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	body := `{"model":"claude-opus-5","max_tokens":64000,"stream":true,` +
		`"tools":[{"name":"Bash"}],"messages":[{"role":"user","content":"hello"}]}`
	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !localHit {
		t.Error("route=waired stopped serving an ordinary turn locally")
	}
	if resp.Header.Get("X-Fake-Upstream") == "1" {
		t.Error("route=waired sent an ordinary turn upstream")
	}
}

// TestAutoModeClassifierDegradesToLocalWhenAnthropicIsUnreachable: sending the
// classifier upstream must not make auto mode fail closed when the machine is
// offline. An unreachable upstream (transport-level, nothing committed) falls
// back to local serving — a slow verdict beats every tool call being denied.
func TestAutoModeClassifierDegradesToLocalWhenAnthropicIsUnreachable(t *testing.T) {
	var localHit bool
	s := newServer(t, Deps{
		LocalInference:       recordingHandler2(&localHit),
		Degraded:             func() bool { return false },
		ClassRoute:           classRouteFunc(routeAuto),
		PassthroughTransport: unreachableUpstream(),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(classifierBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !localHit {
		t.Error("an unreachable Anthropic API must degrade the classifier to local serving")
	}
}
