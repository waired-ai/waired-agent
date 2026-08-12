package intercept

import (
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newHardenedServer is production's shape: browser hardening on
// (cmd/waired-agent/proxy.go passes the agent's --browser-hardening flag).
func newHardenedServer(t *testing.T, deps Deps) *Server {
	t.Helper()
	s, err := NewServer(Config{Addr: "127.0.0.1:0", BrowserHardening: true}, deps)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s
}

// do sends one request through the handler with the given Host/Origin,
// bypassing the client's own Host handling.
func do(t *testing.T, h http.Handler, method, target, host, origin string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, body)
	req.RemoteAddr = "127.0.0.1:1"
	req.Host = host
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestGuardOffByDefault pins the property the rest of this package's tests
// rest on: Config{} leaves the browser checks off, so a request built by
// httptest.NewRequest (Host: example.com) still routes. Product contract —
// the config-gate shape is the ruling of waired-ai/waired#836, extended to
// this listener by waired-ai/waired#1195.
func TestGuardOffByDefault(t *testing.T) {
	var gotPath string
	s := newServer(t, Deps{
		LocalInference:       recordingHandler(&gotPath),
		PassthroughTransport: fakeUpstream(nil),
	})
	rec := do(t, s.Handler(), http.MethodPost, "/v1/messages", "evil.com", "http://evil.com",
		strings.NewReader("{}"))
	if rec.Code == http.StatusForbidden {
		t.Fatalf("guard should be off without BrowserHardening; got 403 body=%s", rec.Body.String())
	}
}

// TestGuardRejectsRebindingHost is the defence itself. Before
// waired-ai/waired#1195 this listener had no Host check, no Origin check and
// no peer check at all, so a page the user visited could reach the routing
// surface and relay through "/" to the real Anthropic API.
func TestGuardRejectsRebindingHost(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/models", "/v1/anything-else"} {
		t.Run(path, func(t *testing.T) {
			s := newHardenedServer(t, Deps{
				LocalInference:       recordingHandler(new(string)),
				PassthroughTransport: fakeUpstream(nil),
			})
			rec := do(t, s.Handler(), http.MethodPost, path, "evil.com", "", strings.NewReader("{}"))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("rebound Host on %s → status = %d, want 403", path, rec.Code)
			}
			assertAnthropicError(t, rec, "permission_error", "invalid Host header")
		})
	}
}

func TestGuardRejectsCrossOrigin(t *testing.T) {
	s := newHardenedServer(t, Deps{
		LocalInference:       recordingHandler(new(string)),
		PassthroughTransport: fakeUpstream(nil),
	})
	rec := do(t, s.Handler(), http.MethodPost, "/v1/messages", "127.0.0.1:9472",
		"http://evil.com", strings.NewReader("{}"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin → status = %d, want 403", rec.Code)
	}
	assertAnthropicError(t, rec, "permission_error", "cross-origin request rejected")
}

// TestGuardRejectsOffHostPeer covers the peer check, which is unconditional:
// this listener had none, unlike every other loopback listener the agent runs.
func TestGuardRejectsOffHostPeer(t *testing.T) {
	s := newServer(t, Deps{ // guard off — the peer check applies anyway
		LocalInference:       recordingHandler(new(string)),
		PassthroughTransport: fakeUpstream(nil),
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	req.RemoteAddr = "203.0.113.7:1234"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("off-host peer → status = %d, want 403", rec.Code)
	}
	assertAnthropicError(t, rec, "permission_error", "loopback only")
}

// TestGuardAllowsClaudeCode is the cost to the client that is supposed to be
// here: Claude Code points ANTHROPIC_BASE_URL at the loopback address and
// sends no Origin.
func TestGuardAllowsClaudeCode(t *testing.T) {
	for _, host := range []string{"127.0.0.1:9472", "localhost:9472", "[::1]:9472"} {
		t.Run(host, func(t *testing.T) {
			var gotPath string
			s := newHardenedServer(t, Deps{
				LocalInference:       recordingHandler(&gotPath),
				PassthroughTransport: fakeUpstream(nil),
			})
			rec := do(t, s.Handler(), http.MethodPost, "/v1/messages", host, "", strings.NewReader("{}"))
			if rec.Code == http.StatusForbidden {
				t.Fatalf("Host %q was rejected: %s", host, rec.Body.String())
			}
			// The intercept rewrites onto the gateway's Anthropic route
			// convention before dispatching locally.
			if gotPath != "/anthropic/v1/messages" {
				t.Fatalf("Host %q did not reach local inference (path=%q)", host, gotPath)
			}
		})
	}
}

// TestGuardPassesMultipartThroughCatchAll is why this listener carries no
// Content-Type requirement, unlike the management API's copy of the same
// guard: "/" reverse-proxies arbitrary Anthropic API calls, and the Files API
// uploads multipart. Requiring application/json here would break them
// (waired-ai/waired#1195).
func TestGuardPassesMultipartThroughCatchAll(t *testing.T) {
	var body strings.Builder
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", "a.txt")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := io.WriteString(part, "hello"); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}

	var seen http.Request
	s := newHardenedServer(t, Deps{
		LocalInference:       recordingHandler(new(string)),
		PassthroughTransport: fakeUpstream(&seen),
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/files", strings.NewReader(body.String()))
	req.RemoteAddr = "127.0.0.1:1"
	req.Host = "127.0.0.1:9472"
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code == http.StatusUnsupportedMediaType || rec.Code == http.StatusForbidden {
		t.Fatalf("multipart upload was rejected: %d %s", rec.Code, rec.Body.String())
	}
	if seen.URL == nil || seen.URL.Path != "/v1/files" {
		t.Fatalf("multipart upload did not reach passthrough (saw %+v)", seen.URL)
	}
}

// assertAnthropicError checks the rejection is shaped like the rest of this
// listener's errors, so a client parses it the same way.
func assertAnthropicError(t *testing.T, rec *httptest.ResponseRecorder, wantType, wantMessage string) {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var got struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not JSON (%v): %s", err, rec.Body.String())
	}
	if got.Type != "error" || got.Error.Type != wantType || got.Error.Message != wantMessage {
		t.Errorf("body = %s, want type=error error.type=%s error.message=%q",
			rec.Body.String(), wantType, wantMessage)
	}
}
