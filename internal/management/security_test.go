package management

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// hardenedServer is the minimal always-mounted server (status + ping) with
// the browser-hardening guard turned on.
func hardenedServer() *Server {
	return newServer(Status{DeviceName: "alice"}, fakePinger{}).WithBrowserHardening()
}

// TestBrowserGuardOffByDefault documents the property that keeps the rest of
// the package's tests green: without WithBrowserHardening, a request a browser
// could forge (attacker Host + Origin, POST without a JSON Content-Type) still
// reaches the handler.
func TestBrowserGuardOffByDefault(t *testing.T) {
	srv := newServer(Status{DeviceName: "alice"}, fakePinger{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/status", nil)
	req.RemoteAddr = "127.0.0.1:1"
	req.Host = "evil.com:9476"
	req.Header.Set("Origin", "http://evil.com")
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("guard should be off by default; got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestBrowserGuardHost(t *testing.T) {
	cases := []struct {
		host string
		want int
	}{
		{"127.0.0.1:9476", http.StatusOK},
		{"127.0.0.1", http.StatusOK},
		{"localhost:9476", http.StatusOK},
		{"localhost", http.StatusOK},
		{"LOCALHOST:9476", http.StatusOK}, // case-insensitive
		{"[::1]:9476", http.StatusOK},
		{"[::1]", http.StatusOK}, // IPv6 literal, no port
		{"evil.com:9476", http.StatusForbidden},
		{"evil.com", http.StatusForbidden},
		{"169.254.169.254", http.StatusForbidden}, // link-local metadata IP, not loopback
		{"", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.host, func(t *testing.T) {
			srv := hardenedServer()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/waired/v1/status", nil)
			req.RemoteAddr = "127.0.0.1:1"
			req.Host = tc.host
			srv.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("Host %q: got %d want %d (body=%s)", tc.host, rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestBrowserGuardOrigin(t *testing.T) {
	cases := []struct {
		origin string
		want   int
	}{
		{"", http.StatusOK}, // absent → allowed (CLI/curl/testnet scripts)
		{"http://127.0.0.1:9476", http.StatusOK},
		{"http://localhost:9476", http.StatusOK},
		{"https://127.0.0.1", http.StatusOK},
		{"http://evil.com", http.StatusForbidden},
		{"http://evil.com:9476", http.StatusForbidden},
		{"null", http.StatusForbidden},      // opaque origin
		{"file:///x", http.StatusForbidden}, // non-http scheme
	}
	for _, tc := range cases {
		name := tc.origin
		if name == "" {
			name = "absent"
		}
		t.Run(name, func(t *testing.T) {
			srv := hardenedServer()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/waired/v1/status", nil)
			req.RemoteAddr = "127.0.0.1:1"
			req.Host = "127.0.0.1:9476"
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			srv.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("Origin %q: got %d want %d (body=%s)", tc.origin, rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestBrowserGuardContentType(t *testing.T) {
	cases := []struct {
		name string
		ct   string
		want int
	}{
		{"json", "application/json", http.StatusOK},
		{"json-charset", "application/json; charset=utf-8", http.StatusOK},
		{"text-plain", "text/plain", http.StatusUnsupportedMediaType},
		{"form", "application/x-www-form-urlencoded", http.StatusUnsupportedMediaType},
		{"absent", "", http.StatusUnsupportedMediaType},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := hardenedServer()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/waired/v1/ping", strings.NewReader(`{"peer":"bob"}`))
			req.RemoteAddr = "127.0.0.1:1"
			req.Host = "127.0.0.1:9476"
			if tc.ct != "" {
				req.Header.Set("Content-Type", tc.ct)
			}
			srv.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("Content-Type %q: got %d want %d (body=%s)", tc.ct, rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// TestBrowserGuardContentTypeMethodScope confirms the Content-Type check only
// applies to bodied write verbs: GET is never checked, and DELETE (the bodyless
// model-delete) is exempt and relies on the Host/Origin checks instead.
func TestBrowserGuardContentTypeMethodScope(t *testing.T) {
	srv := hardenedServer()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/status", nil)
	req.RemoteAddr = "127.0.0.1:1"
	req.Host = "127.0.0.1:9476"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET should bypass the Content-Type check; got %d", rec.Code)
	}

	// Inference is not wired here, so the DELETE falls through to a 404 at the
	// mux — the point is only that the guard did not 415 it for a missing CT.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/waired/v1/models/foo", nil)
	req.RemoteAddr = "127.0.0.1:1"
	req.Host = "127.0.0.1:9476"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusUnsupportedMediaType {
		t.Fatalf("DELETE should be exempt from the Content-Type check; got 415")
	}
}
