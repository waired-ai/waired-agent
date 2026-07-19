package management

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestWriteGuard covers the loopback-TCP write guard (waired#838): once
// the local IPC socket is up AND enforcement is on, mutating verbs over
// TCP are refused so writes can only arrive over the socket — except the
// /ping liveness probe. When enforcement is off, or the socket is down
// (fail-open), everything passes so a bind failure never bricks control.
func TestWriteGuard(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	cases := []struct {
		name     string
		enforce  bool
		socketUp bool
		method   string
		path     string
		want     int
	}{
		{"not-enforced-post-passes", false, true, http.MethodPost, "/waired/v1/pause", http.StatusOK},
		{"enforced-socket-down-post-passes", true, false, http.MethodPost, "/waired/v1/pause", http.StatusOK},
		{"enforced-socket-up-post-blocked", true, true, http.MethodPost, "/waired/v1/pause", http.StatusForbidden},
		{"enforced-socket-up-delete-blocked", true, true, http.MethodDelete, "/waired/v1/models/foo", http.StatusForbidden},
		{"enforced-socket-up-patch-blocked", true, true, http.MethodPatch, "/waired/v1/worker", http.StatusForbidden},
		{"enforced-socket-up-get-passes", true, true, http.MethodGet, "/waired/v1/status", http.StatusOK},
		{"enforced-socket-up-ping-passes", true, true, http.MethodPost, "/waired/v1/ping", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var up atomic.Bool
			up.Store(tc.socketUp)
			h := writeGuard(ok, tc.enforce, &up)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.path, nil)
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("%s: got %d want %d (body=%s)", tc.name, rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// TestServeDefaultDoesNotEnforceWrites documents that a server built
// without WithSocketWritesOnly (the default, and how every unit test
// drives it) leaves TCP writes flowing — Handler() carries no write guard.
func TestServeDefaultDoesNotEnforceWrites(t *testing.T) {
	srv := newServer(Status{DeviceName: "alice"}, fakePinger{})
	if srv.enforceSocketWrites {
		t.Fatal("enforceSocketWrites should default to false")
	}
	// Handler() (used by the 18 existing tests) must never apply writeGuard:
	// a POST write reaches the mux (here 404 "not configured", never 403).
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/waired/v1/pause", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Fatalf("Handler() applied a write guard (got 403); it must serve the raw mux")
	}
}
