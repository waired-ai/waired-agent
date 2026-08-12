package management

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// fullyWiredServer mounts every route group the TCP read allow-list names,
// so a test can tell "this route rejects the method" from "this route was
// never registered".
func fullyWiredServer(t *testing.T) *Server {
	t.Helper()
	cfg := &CatalogConfig{
		PreferencePath: filepath.Join(t.TempDir(), "preferred-model.json"),
		ManifestsFn:    func() ([]catalog.Manifest, error) { return catalogFixture(), nil },
	}
	return New(stubStatus{}, stubPinger{}).
		WithInference(&fakeInference{}).
		WithCatalog(cfg).
		WithSetupExecutor(&fakeSetupExecutor{})
}

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
		{"enforced-socket-up-head-passes", true, true, http.MethodHead, "/waired/v1/status", http.StatusOK},
		{"enforced-socket-up-put-blocked", true, true, http.MethodPut, "/waired/v1/worker", http.StatusForbidden},
		// An extension verb is a write: isMutating is an allow-list, so an
		// unknown method never falls through as a read (waired#836 audit).
		{"enforced-socket-up-unknown-verb-blocked", true, true, "PROPFIND", "/waired/v1/status", http.StatusForbidden},
		{"enforced-socket-up-ping-passes", true, true, http.MethodPost, "/waired/v1/ping", http.StatusOK},
		// The exemption is POST /ping, not "anything addressed at /ping".
		{"enforced-socket-up-ping-delete-blocked", true, true, http.MethodDelete, "/waired/v1/ping", http.StatusForbidden},
		// The guard must match what ServeMux routes on: a target that only
		// looks like /ping once decoded does not get the exemption.
		{"enforced-socket-up-ping-escaped-blocked", true, true, http.MethodPost, "/waired%2Fv1/ping", http.StatusForbidden},
		{"enforced-socket-up-ping-trailing-slash-blocked", true, true, http.MethodPost, "/waired/v1/ping/", http.StatusForbidden},
		{"enforced-socket-up-ping-dotdot-blocked", true, true, http.MethodPost, "/waired/v1/models/../ping", http.StatusForbidden},
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

// TestReadGuard covers the loopback-TCP read guard (waired#836): once the
// socket is up AND enforcement is on, TCP serves only the tcpReadRoutes
// allow-list and every other read must use the socket. Mutating verbs are
// writeGuard's business and must pass through untouched, so that turning
// --mgmt-socket-writes-only off is not silently undone here.
func TestReadGuard(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	cases := []struct {
		name     string
		enforce  bool
		socketUp bool
		method   string
		path     string
		want     int
	}{
		{"not-enforced-identity-passes", false, true, http.MethodGet, "/waired/v1/identity", http.StatusOK},
		{"enforced-socket-down-identity-passes", true, false, http.MethodGet, "/waired/v1/identity", http.StatusOK},
		{"enforced-socket-up-identity-blocked", true, true, http.MethodGet, "/waired/v1/identity", http.StatusForbidden},
		{"enforced-socket-up-metrics-blocked", true, true, http.MethodGet, "/waired/v1/metrics", http.StatusForbidden},
		{"enforced-socket-up-events-blocked", true, true, http.MethodGet, "/waired/v1/observability/events", http.StatusForbidden},
		{"enforced-socket-up-head-identity-blocked", true, true, http.MethodHead, "/waired/v1/identity", http.StatusForbidden},
		// The allow-list: the routes non-Go consumers actually call.
		{"enforced-socket-up-status-passes", true, true, http.MethodGet, "/waired/v1/status", http.StatusOK},
		{"enforced-socket-up-inference-status-passes", true, true, http.MethodGet, "/waired/v1/inference/status", http.StatusOK},
		{"enforced-socket-up-runtimes-passes", true, true, http.MethodGet, "/waired/v1/inference/runtimes", http.StatusOK},
		{"enforced-socket-up-catalog-passes", true, true, http.MethodGet, "/waired/v1/inference/catalog", http.StatusOK},
		{"enforced-socket-up-setup-state-passes", true, true, http.MethodGet, "/waired/v1/setup/state", http.StatusOK},
		{"enforced-socket-up-status-query-passes", true, true, http.MethodGet, "/waired/v1/status?x=1", http.StatusOK},
		// An allow-listed route must be addressed as itself, not smuggled
		// through a form that decodes or cleans into it.
		{"enforced-socket-up-status-escaped-blocked", true, true, http.MethodGet, "/waired/v1/%73tatus", http.StatusForbidden},
		{"enforced-socket-up-status-trailing-slash-blocked", true, true, http.MethodGet, "/waired/v1/status/", http.StatusForbidden},
		{"enforced-socket-up-status-dotdot-blocked", true, true, http.MethodGet, "/waired/v1/identity/../status", http.StatusForbidden},
		// Writes belong to writeGuard: readGuard must not answer for them.
		{"enforced-socket-up-post-passes-to-writeguard", true, true, http.MethodPost, "/waired/v1/pause", http.StatusOK},
		{"enforced-socket-up-delete-passes-to-writeguard", true, true, http.MethodDelete, "/waired/v1/models/foo", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var up atomic.Bool
			up.Store(tc.socketUp)
			h := readGuard(ok, tc.enforce, &up)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.path, nil)
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("%s: got %d want %d (body=%s)", tc.name, rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// TestTCPReadRoutesAreReads pins that nothing mutating ever enters the TCP
// read allow-list. A write on that list would be reachable from a browser
// (readGuard would pass it, and writeGuard only stops it while the socket
// is up), which is exactly what waired#836 closed.
func TestTCPReadRoutesAreReads(t *testing.T) {
	srv := fullyWiredServer(t)
	for route := range tcpReadRoutes {
		t.Run(route, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, route, strings.NewReader("{}"))
			req.RemoteAddr = "127.0.0.1:1"
			req.Header.Set("Content-Type", "application/json")
			srv.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("POST %s: got %d, want 405 — an allow-listed TCP read route must not accept writes", route, rec.Code)
			}
		})
	}
}

// TestServeAppliesTransportGuards pins the composition in Serve(). The
// existing coverage only asserts the negative (Handler() carries no write
// guard), so deleting the wrappers from Serve() would leave the suite
// green while removing both #836 and #838 from the running daemon.
func TestServeAppliesTransportGuards(t *testing.T) {
	srv := newServer(Status{DeviceName: "alice"}, fakePinger{}).
		WithSocketWritesOnly(true).
		WithSocketReadsOnly(true)
	srv.socketUp.Store(true)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, addr) }()

	base := "http://" + addr
	client := &http.Client{Timeout: 5 * time.Second}
	waitForServer(t, client, base+"/waired/v1/status")

	// writeGuard: a mutating verb over TCP is refused.
	resp, err := client.Post(base+"/waired/v1/pause", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /pause: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST /pause over TCP: got %d want 403 (writeGuard not composed into Serve)", resp.StatusCode)
	}

	// readGuard: a read outside the allow-list is refused.
	resp, err = client.Get(base + "/waired/v1/identity")
	if err != nil {
		t.Fatalf("GET /identity: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("GET /identity over TCP: got %d want 403 (readGuard not composed into Serve)", resp.StatusCode)
	}

	// The allow-list still answers.
	resp, err = client.Get(base + "/waired/v1/status")
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /status over TCP: got %d want 200", resp.StatusCode)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve returned %v", err)
	}
}

// waitForServer polls url until it answers or the deadline passes. Serve()
// binds asynchronously, so the first request can race the listener.
func waitForServer(t *testing.T, client *http.Client, url string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server at %s never answered", url)
}

// TestServeDefaultDoesNotEnforceReads mirrors the write-side pin: a server
// built without WithSocketReadsOnly leaves TCP reads flowing, which is how
// every other test in this package drives Handler().
func TestServeDefaultDoesNotEnforceReads(t *testing.T) {
	srv := newServer(Status{DeviceName: "alice"}, fakePinger{})
	if srv.enforceSocketReads {
		t.Fatal("enforceSocketReads should default to false")
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/identity", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Fatalf("Handler() applied a read guard (got 403); it must serve the raw mux")
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
