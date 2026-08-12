package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/runtime"
)

// newHardenedGateway is the coding-agent data plane's shape: no token, browser
// hardening on (cmd/waired-agent/inference.go).
func newHardenedGateway(t *testing.T) *Server {
	t.Helper()
	reg := runtime.NewRegistry()
	return NewServer(ServerConfig{BrowserHardening: true}, Deps{
		Selector:      &fakeSelector{},
		Runtimes:      reg,
		ListManifests: func() []catalog.Manifest { return nil },
		HTTPClient:    http.DefaultClient,
		AllowOpenAI:   true,
	})
}

// TestBrowserHardening_OffByDefault pins the property the rest of this
// package's tests rest on: ServerConfig{} keeps the guard off, so a request
// built by httptest.NewRequest (Host: example.com, no Origin) still reaches
// the routes. Product contract — the config-gate shape is the ruling of
// waired-ai/waired#836, extended to this listener by waired-ai/waired#1195.
func TestBrowserHardening_OffByDefault(t *testing.T) {
	gw := newTokenedGateway(t, "")
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.RemoteAddr = "127.0.0.1:1"
	r.Host = "evil.com"
	r.Header.Set("Origin", "http://evil.com")
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	if w.Code == http.StatusForbidden {
		t.Fatalf("guard should be off without BrowserHardening; got 403 body=%s", w.Body.String())
	}
}

// TestBrowserHardening_RejectsRebindingHost is the defence itself: a page the
// user visits cannot reach the no-token data plane, because the name it
// rebound is what lands in Host (waired-ai/waired#1195).
func TestBrowserHardening_RejectsRebindingHost(t *testing.T) {
	gw := newHardenedGateway(t)
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.RemoteAddr = "127.0.0.1:1"
	r.Host = "evil.com"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("rebound Host → status = %d, want 403", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "permission_error") {
		t.Errorf("error body missing type: %s", body)
	}
	if !strings.Contains(body, "invalid Host header") {
		t.Errorf("error body missing message: %s", body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

func TestBrowserHardening_RejectsCrossOrigin(t *testing.T) {
	gw := newHardenedGateway(t)
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.RemoteAddr = "127.0.0.1:1"
	r.Host = "127.0.0.1:9479"
	r.Header.Set("Origin", "http://evil.com")
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin → status = %d, want 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "cross-origin request rejected") {
		t.Errorf("error body missing message: %s", w.Body.String())
	}
}

// TestBrowserHardening_AllowsLocalClients checks the cost to everyone who is
// supposed to be here: a loopback Host, and no Origin at all (curl, the CLI,
// editor extensions, the waired-authored coding-agent plugins).
func TestBrowserHardening_AllowsLocalClients(t *testing.T) {
	for _, host := range []string{"127.0.0.1:9479", "localhost:9479", "[::1]:9479"} {
		t.Run(host, func(t *testing.T) {
			gw := newHardenedGateway(t)
			r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			r.RemoteAddr = "127.0.0.1:1"
			r.Host = host
			w := httptest.NewRecorder()
			gw.Handler().ServeHTTP(w, r)
			if w.Code == http.StatusForbidden {
				t.Fatalf("Host %q was rejected: %s", host, w.Body.String())
			}
		})
	}
}

// TestBrowserHardening_PostNeedsNoJSONContentType records today's behaviour:
// this listener does not carry the management API's Content-Type requirement.
// The Origin check already rejects the cross-site simple-request POST that
// requirement defends against.
func TestBrowserHardening_PostNeedsNoJSONContentType(t *testing.T) {
	gw := newHardenedGateway(t)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[]}`))
	r.RemoteAddr = "127.0.0.1:1"
	r.Host = "127.0.0.1:9479"
	r.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code == http.StatusUnsupportedMediaType {
		t.Fatalf("gateway should not require a JSON Content-Type; got 415")
	}
}

// TestBrowserHardening_LoopbackEnforcedFirst pins the order: an off-host peer
// is answered by loopbackOnly, not by the Host check, so the two cannot be
// confused in a log or a bug report.
func TestBrowserHardening_LoopbackEnforcedFirst(t *testing.T) {
	gw := newHardenedGateway(t)
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.RemoteAddr = "203.0.113.7:1234"
	r.Host = "evil.com"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("off-host peer → status = %d, want 403", w.Code)
	}
	if body := w.Body.String(); !strings.Contains(body, "loopback only") {
		t.Errorf("expected loopbackOnly to answer first, got %q", body)
	}
}
