package intercept

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// waired-agent#788 turned "no host serves this model and none is
// fetching it" from a retryable 503 into a 404. On the waired route that
// is the whole point — the client sees an error instead of backing off
// forever — but the auto route must be unchanged: a turn that can be
// rerouted still is, because fallback eligibility is about the response
// being uncommitted and >= 400, not about which 4xx or 5xx it was.
//
// Without this test the change reads as a gateway-only edit, and the
// route that carries real Claude Code traffic is the one it could break.
func TestAutoFallsBackOnModelNotServed404(t *testing.T) {
	var localHit bool
	var last http.Request
	var fbReason string
	local := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		localHit = true
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(localErrorHeader, "model_not_served")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"not_found_error"}}`)
	})
	s := newServer(t, Deps{
		LocalInference:       local,
		Degraded:             func() bool { return false },
		ClassRoute:           classRouteFunc(routeAuto),
		OnFallback:           func(reason string) { fbReason = reason },
		PassthroughTransport: fakeUpstream(&last),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"claude-x"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if !localHit {
		t.Error("local inference should have been tried first")
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Fake-Upstream") != "1" {
		t.Errorf("a 404 from local inference did not reroute: status=%d upstream=%q",
			resp.StatusCode, resp.Header.Get("X-Fake-Upstream"))
	}
	// The marker is what keeps the journal readable: local_status_404
	// would say only that something 404'd.
	if fbReason != "local_model_not_served" {
		t.Errorf("OnFallback reason=%q, want local_model_not_served", fbReason)
	}
	if resp.Header.Get(localErrorHeader) != "" {
		t.Error("the internal marker leaked to the client")
	}
}

// On the waired route there is no upstream to try, so the 404 is what
// the client gets — the answer waired-agent#788 exists to produce.
func TestWairedRouteSurfacesModelNotServed404(t *testing.T) {
	var last http.Request
	local := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(localErrorHeader, "model_not_served")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"not_found_error"}}`)
	})
	s := newServer(t, Deps{
		LocalInference:       local,
		Degraded:             func() bool { return false },
		ClassRoute:           classRouteFunc(routeWaired),
		PassthroughTransport: fakeUpstream(&last),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"claude-x"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 — the client must see the failure, not wait on it", resp.StatusCode)
	}
	if resp.Header.Get("X-Fake-Upstream") == "1" {
		t.Error("route=waired reached the real Anthropic API")
	}
	if resp.Header.Get("Retry-After") != "" {
		t.Error("a Retry-After on this answer is an invitation to the silent-retry loop this fixed")
	}
}
