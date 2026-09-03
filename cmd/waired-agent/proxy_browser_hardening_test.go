package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The Claude listener's loopback guards are composed here rather than inside
// internal/proxy/intercept, which stays stdlib-only (see claudeListenerGuard).
// So this is where the composed behaviour is pinned — internal/loopbackguard
// owns the checks, intercept owns only the Deps.Guard seam.

// guardedEcho wraps a handler that records whether the request got through.
func guardedEcho(hardening bool, reached *bool) http.Handler {
	return claudeListenerGuard(hardening)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*reached = true
		_, _ = io.WriteString(w, "THROUGH")
	}))
}

func guardRequest(method, target, host, origin, contentType, body string) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1"
	req.Host = host
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req
}

// TestClaudeListenerGuard is the defence itself. Before waired-ai/waired#1195
// this listener had no Host check, no Origin check and no peer check at all, so
// a page the user merely visited could reach the routing surface and relay
// through "/" to the real Anthropic API.
func TestClaudeListenerGuard(t *testing.T) {
	cases := []struct {
		name        string
		host        string
		origin      string
		wantCode    int
		wantMessage string
	}{
		{"loopback-v4", "127.0.0.1:9472", "", http.StatusOK, ""},
		{"loopback-name", "localhost:9472", "", http.StatusOK, ""},
		{"loopback-v6", "[::1]:9472", "", http.StatusOK, ""},
		{"loopback-origin", "127.0.0.1:9472", "http://127.0.0.1:9472", http.StatusOK, ""},
		{"rebound-host", "evil.com", "", http.StatusForbidden, "invalid Host header"},
		{"rebound-host-port", "evil.com:9472", "", http.StatusForbidden, "invalid Host header"},
		{"cross-origin", "127.0.0.1:9472", "http://evil.com", http.StatusForbidden, "cross-origin request rejected"},
		{"empty-host", "", "", http.StatusForbidden, "invalid Host header"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reached bool
			rec := httptest.NewRecorder()
			guardedEcho(true, &reached).ServeHTTP(rec,
				guardRequest(http.MethodPost, "/v1/messages", tc.host, tc.origin, "application/json", "{}"))

			if rec.Code != tc.wantCode {
				t.Fatalf("got %d want %d (body=%s)", rec.Code, tc.wantCode, rec.Body.String())
			}
			if tc.wantCode == http.StatusOK {
				if !reached {
					t.Fatalf("request did not reach the handler")
				}
				return
			}
			if reached {
				t.Fatalf("rejected request still reached the handler")
			}
			assertAnthropicError(t, rec, "permission_error", tc.wantMessage)
		})
	}
}

// TestClaudeListenerGuardRejectsOffHostPeer covers the peer check, which is
// unconditional — every other loopback listener the agent runs has had one
// since it was written; this one simply never did.
func TestClaudeListenerGuardRejectsOffHostPeer(t *testing.T) {
	for _, hardening := range []bool{true, false} {
		var reached bool
		rec := httptest.NewRecorder()
		req := guardRequest(http.MethodPost, "/v1/messages", "127.0.0.1:9472", "", "application/json", "{}")
		req.RemoteAddr = "203.0.113.7:1234"
		guardedEcho(hardening, &reached).ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("hardening=%v: off-host peer got %d, want 403", hardening, rec.Code)
		}
		if reached {
			t.Fatalf("hardening=%v: off-host peer reached the handler", hardening)
		}
		assertAnthropicError(t, rec, "permission_error", "loopback only")
	}
}

// TestClaudeListenerGuardHardeningOff pins the escape hatch: with
// -browser-hardening=false the Host and Origin checks are gone (the peer check
// is not).
func TestClaudeListenerGuardHardeningOff(t *testing.T) {
	var reached bool
	rec := httptest.NewRecorder()
	guardedEcho(false, &reached).ServeHTTP(rec,
		guardRequest(http.MethodPost, "/v1/messages", "evil.com", "http://evil.com", "application/json", "{}"))
	if rec.Code != http.StatusOK || !reached {
		t.Fatalf("hardening off should pass; got %d reached=%v", rec.Code, reached)
	}
}

// TestClaudeListenerGuardPassesMultipart is why this listener carries no
// Content-Type requirement, unlike the management API's use of the same guard:
// the "/" catch-all reverse-proxies arbitrary Anthropic API calls, and the
// Files API uploads multipart. Requiring application/json would break them
// (waired-ai/waired#1195).
func TestClaudeListenerGuardPassesMultipart(t *testing.T) {
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

	for _, ct := range []string{mw.FormDataContentType(), "application/octet-stream", "text/plain", ""} {
		name := ct
		if name == "" {
			name = "absent"
		}
		t.Run(name, func(t *testing.T) {
			var reached bool
			rec := httptest.NewRecorder()
			guardedEcho(true, &reached).ServeHTTP(rec,
				guardRequest(http.MethodPost, "/v1/files", "127.0.0.1:9472", "", ct, body.String()))
			if rec.Code != http.StatusOK || !reached {
				t.Fatalf("Content-Type %q was rejected: %d %s", ct, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestBrowserHardeningEnabled pins the deprecated alias: -mgmt-hardening=false
// must still turn the guards off, or an operator's existing local-debug
// invocation silently stops working after the rename (waired-ai/waired#1195).
func TestBrowserHardeningEnabled(t *testing.T) {
	cases := []struct {
		browser, mgmt, want bool
	}{
		{true, true, true},    // both defaults
		{false, true, false},  // -browser-hardening=false
		{true, false, false},  // -mgmt-hardening=false (the old name)
		{false, false, false}, // both
	}
	for _, tc := range cases {
		got := browserHardeningEnabled(tc.browser, tc.mgmt)
		if got != tc.want {
			t.Errorf("browserHardeningEnabled(%v, %v) = %v, want %v",
				tc.browser, tc.mgmt, got, tc.want)
		}
	}
}

// freeLoopbackPort returns a port nothing is listening on. buildClaudeListener
// binds a concrete 127.0.0.1:<port> (the address Claude Code's
// ANTHROPIC_BASE_URL names), so the test cannot hand it :0.
func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("probe close: %v", err)
	}
	return port
}

// TestBuildClaudeListenerWiresBrowserHardening pins the wiring, not the guard:
// the flag has to reach intercept.Deps.Guard, or :9472 ships open however well
// internal/loopbackguard is tested.
func TestBuildClaudeListenerWiresBrowserHardening(t *testing.T) {
	for _, tc := range []struct {
		name      string
		hardening bool
		wantCode  int
	}{
		{"on", true, http.StatusForbidden},
		{"off", false, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A wired handle keeps the request local. Without one the
			// listener answers with the fail-closed reason instead, which
			// would test the wrong thing here.
			ph := &proxyHandle{}
			ph.SetLocalInference(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "LOCAL")
			}))

			port := freeLoopbackPort(t)
			srv, ln, err := buildClaudeListener(port, ph, nil, false, tc.hardening,
				slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatalf("buildClaudeListener: %v", err)
			}
			defer ln.Close()

			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec,
				guardRequest(http.MethodPost, "/v1/messages", "evil.com", "", "application/json", "{}"))

			if rec.Code != tc.wantCode {
				t.Fatalf("hardening=%v: rebound Host → %d, want %d (body=%s)",
					tc.hardening, rec.Code, tc.wantCode, rec.Body.String())
			}
			if tc.hardening && !strings.Contains(rec.Body.String(), "invalid Host header") {
				t.Errorf("hardening=on: body %s does not name the reason", rec.Body.String())
			}
			if !tc.hardening && rec.Body.String() != "LOCAL" {
				t.Errorf("hardening=off: body %q, want the local handler's answer", rec.Body.String())
			}
		})
	}
}

// TestBuildClaudeListenerStillServesClaudeCode is the other direction, and the
// one that matters more day to day. This listener is fail-open by design —
// while it is alive Claude Code must never be worse off than talking to
// Anthropic directly — and waired-ai/waired#1195 gives it its first reject
// path. A guard that is too strict does not show up as a security hole; it
// shows up as coding stopping. So: Claude Code's own request (loopback Host,
// no Origin, hardening ON) must reach local inference.
func TestBuildClaudeListenerStillServesClaudeCode(t *testing.T) {
	for _, host := range []string{"127.0.0.1:9472", "localhost:9472", "[::1]:9472"} {
		t.Run(host, func(t *testing.T) {
			served := false
			ph := &proxyHandle{}
			ph.SetLocalInference(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				served = true
				_, _ = io.WriteString(w, "LOCAL")
			}))

			port := freeLoopbackPort(t)
			srv, ln, err := buildClaudeListener(port, ph, nil, false, true,
				slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatalf("buildClaudeListener: %v", err)
			}
			defer ln.Close()

			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec,
				guardRequest(http.MethodPost, "/v1/messages", host, "", "application/json", "{}"))

			if !served {
				t.Fatalf("Host %q: Claude Code's own request was blocked (%d %s)",
					host, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestClaudeListenerOverARealSocket drives the listener production actually
// serves. Every other test here builds requests with httptest.NewRequest, which
// sets Host and RemoteAddr by hand — so none of them exercises net/http parsing
// a Host off the wire, or the peer address coming from the kernel. A guard that
// only works against synthesised requests is not a guard.
//
// Both legs stay off the network: the rebinding leg is answered by the guard,
// and the legitimate leg is served by the wired local-inference handler.
func TestClaudeListenerOverARealSocket(t *testing.T) {
	served := 0
	ph := &proxyHandle{}
	ph.SetLocalInference(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served++
		_, _ = io.WriteString(w, "LOCAL")
	}))

	port := freeLoopbackPort(t)
	srv, ln, err := buildClaudeListener(port, ph, nil, false, true,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("buildClaudeListener: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ctx, ln)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	base := "http://" + ln.Addr().String()
	post := func(t *testing.T, host string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, base+"/v1/messages", strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		if host != "" {
			// net/http sends this as the Host header while still dialling the
			// URL's address — exactly the shape of a rebound request.
			req.Host = host
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	t.Run("rebound-host-rejected", func(t *testing.T) {
		code, body := post(t, "evil.com")
		if code != http.StatusForbidden {
			t.Fatalf("got %d want 403 (body=%s)", code, body)
		}
		if !strings.Contains(body, "invalid Host header") {
			t.Errorf("body %s does not name the reason", body)
		}
	})

	t.Run("claude-code-served", func(t *testing.T) {
		before := served
		code, body := post(t, "") // the client's own Host: 127.0.0.1:<port>
		if code != http.StatusOK || body != "LOCAL" {
			t.Fatalf("legitimate request got %d %q, want 200 LOCAL", code, body)
		}
		if served != before+1 {
			t.Fatalf("local inference was not reached")
		}
	})
}

// assertAnthropicError checks the rejection is shaped like the rest of the
// Claude listener's errors, so a client parses it the same way.
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
