package intercept

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAutoEngineRequestShapeFallsBack: the gateway maps an engine's
// deterministic request-shape rejection to a 400 so the client stops
// retrying (waired-agent#1035). Unlike localErrContextOverflow that 400
// IS a normal fallback reason — nothing was committed and the turn is
// not the client's fault — so auto mode must still reroute it, and name
// the cause instead of a bare local_status_400.
func TestAutoEngineRequestShapeFallsBack(t *testing.T) {
	var last http.Request
	var fbReason string
	s := newServer(t, Deps{
		LocalInference: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set(localErrorHeader, "engine_request_shape")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error",`+
				`"message":"system message must be at the beginning"}}`)
		}),
		Degraded:             func() bool { return false },
		ClassRoute:           classRouteFunc(routeAuto),
		OnFallback:           func(reason string) { fbReason = reason },
		PassthroughTransport: fakeUpstream(&last),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.Header.Get("X-Fake-Upstream") != "1" {
		t.Error("a shape rejection must reroute to the real Anthropic API")
	}
	if fbReason != "local_engine_request_shape" {
		t.Errorf("OnFallback reason = %q, want local_engine_request_shape", fbReason)
	}
}
