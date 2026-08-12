package intercept

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The loopback guards themselves live in internal/loopbackguard and are
// composed by cmd/waired-agent, which keeps this fail-open package
// stdlib-only. What this package owes them is the seam: Deps.Guard has to wrap
// EVERY route, including the "/" passthrough catch-all — a guard that covers
// only the message paths would leave the open relay open
// (waired-ai/waired#1195).

// countingGuard rejects everything and records how many requests reached it.
func countingGuard(calls *int) func(http.Handler) http.Handler {
	return func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			*calls++
			w.WriteHeader(http.StatusForbidden)
		})
	}
}

func TestGuardWrapsEveryRoute(t *testing.T) {
	// One entry per pattern handler() registers, catch-all included.
	paths := []string{
		"/v1/messages",
		"/v1/messages/count_tokens",
		"/v1/models",
		"/v1/models/claude-opus-4-8",
		"/",
		"/v1/anything-else-entirely",
	}
	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			calls := 0
			var gotPath string
			s := newServerWithDeps(t, Deps{
				LocalInference:       recordingHandler(&gotPath),
				PassthroughTransport: fakeUpstream(nil),
				Guard:                countingGuard(&calls),
			})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, p, strings.NewReader("{}"))
			req.RemoteAddr = "127.0.0.1:1"
			s.Handler().ServeHTTP(rec, req)

			if calls != 1 {
				t.Fatalf("%s: guard ran %d times, want 1", p, calls)
			}
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s: guard did not get to answer (%d)", p, rec.Code)
			}
			if gotPath != "" {
				t.Fatalf("%s: reached local inference despite the guard", p)
			}
		})
	}
}

// TestNilGuardLeavesMuxBare pins the zero value the package's own tests rely
// on: without a Guard the routes are reachable as they always were.
func TestNilGuardLeavesMuxBare(t *testing.T) {
	var gotPath string
	s := newServerWithDeps(t, Deps{
		LocalInference:       recordingHandler(&gotPath),
		PassthroughTransport: fakeUpstream(nil),
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	req.RemoteAddr = "203.0.113.7:1234" // off-host, and still served: no guard
	req.Host = "evil.com"
	s.Handler().ServeHTTP(rec, req)

	if gotPath != "/anthropic/v1/messages" {
		t.Fatalf("bare mux did not route (path=%q, code=%d)", gotPath, rec.Code)
	}
}

// newServerWithDeps mirrors newServer but lets a test set Deps.Guard.
func newServerWithDeps(t *testing.T, deps Deps) *Server {
	t.Helper()
	s, err := NewServer(Config{Addr: "127.0.0.1:0"}, deps)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s
}
