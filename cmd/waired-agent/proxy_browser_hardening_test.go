package main

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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

// TestBuildClaudeListenerWiresBrowserHardening pins the wiring, not the guard:
// the agent's --browser-hardening flag has to reach the Claude listener's
// config, or :9472 ships open however well internal/loopbackguard is tested
// (waired-ai/waired#1195).
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
			// A wired, non-degraded handle keeps the request local. Without
			// one the listener fails open to the real api.anthropic.com,
			// which is not something a unit test may reach for.
			ph := &proxyHandle{}
			ph.SetLocalInference(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "LOCAL")
			}))
			ph.SetDegraded(func() bool { return false })

			port := freeLoopbackPort(t)
			srv, ln, err := buildClaudeListener(port, ph, nil, false, tc.hardening,
				slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatalf("buildClaudeListener: %v", err)
			}
			defer ln.Close()

			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
			req.RemoteAddr = "127.0.0.1:1"
			req.Host = "evil.com"
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)

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
