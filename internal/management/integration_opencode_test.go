package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The data-plane integration endpoints answer ONE fact each: the URL this
// daemon expects the plugin's provider to point at. Whether the plugin is
// on disk is not asked here any more — that reading belongs to the tray,
// which runs as the desktop user whose home the file lives in
// (waired-agent#986); internal/gui/tray/integration_probe_test.go covers
// it, and internal/integration/detect owns the parsing.

func doExpectationGet(t *testing.T, srv *Server, path string) (int, IntegrationExpectation) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = "127.0.0.1:55555"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return rec.Code, IntegrationExpectation{}
	}
	var got IntegrationExpectation
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	return rec.Code, got
}

func TestIntegrationExpectation_Disabled404(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{})
	for _, path := range []string{
		"/waired/v1/integration/opencode",
		"/waired/v1/integration/openclaw",
	} {
		code, _ := doExpectationGet(t, srv, path)
		if code != http.StatusNotFound {
			t.Errorf("%s: expected 404 when unwired, got %d", path, code)
		}
	}
}

func TestIntegrationExpectation_ReportsTheGatewayURL(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{}).
		WithOpenCodeIntegration(OpenCodeIntegrationConfig{ExpectedBaseURL: "http://127.0.0.1:9479/v1"}).
		WithOpenClawIntegration(OpenClawIntegrationConfig{ExpectedBaseURL: "http://127.0.0.1:9479/v1"})

	for _, path := range []string{
		"/waired/v1/integration/opencode",
		"/waired/v1/integration/openclaw",
	} {
		code, got := doExpectationGet(t, srv, path)
		if code != http.StatusOK {
			t.Fatalf("%s: code=%d", path, code)
		}
		if got.ExpectedBaseURL != "http://127.0.0.1:9479/v1" {
			t.Errorf("%s: ExpectedBaseURL = %q", path, got.ExpectedBaseURL)
		}
	}
}

// TestIntegrationExpectation_SaysNothingAboutAnyHome pins the shape, not
// just a value: a reader that finds a "configured" flag here would be
// reading the SERVICE account's home, which is the defect
// waired-agent#986 closed.
func TestIntegrationExpectation_SaysNothingAboutAnyHome(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{}).
		WithOpenCodeIntegration(OpenCodeIntegrationConfig{ExpectedBaseURL: "http://127.0.0.1:9479/v1"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/integration/opencode", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	srv.Handler().ServeHTTP(rec, req)

	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, banned := range []string{"config", "configured", "path", "home"} {
		if _, ok := raw[banned]; ok {
			t.Errorf("response carries %q: %s", banned, rec.Body.String())
		}
	}
}

func TestIntegrationExpectation_RejectsNonGET(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{}).
		WithOpenCodeIntegration(OpenCodeIntegrationConfig{}).
		WithOpenClawIntegration(OpenClawIntegrationConfig{})
	for _, path := range []string{
		"/waired/v1/integration/opencode",
		"/waired/v1/integration/openclaw",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, path, nil)
		req.RemoteAddr = "127.0.0.1:55555"
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: code=%d, want 405", path, rec.Code)
		}
	}
}

// TestIntegrationReconfigureIsNotServed is the regression guard for the
// half of waired-agent#986 that was a privilege question rather than a
// display one: the daemon used to accept a POST that made it write a
// plugin into a home directory on any local caller's behalf. That is the
// bridge waired#935 keeps it out of, so the route must stay absent.
func TestIntegrationReconfigureIsNotServed(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{}).
		WithOpenCodeIntegration(OpenCodeIntegrationConfig{ExpectedBaseURL: "http://127.0.0.1:9479/v1"}).
		WithOpenClawIntegration(OpenClawIntegrationConfig{ExpectedBaseURL: "http://127.0.0.1:9479/v1"})
	for _, path := range []string{
		"/waired/v1/integration/opencode/reconfigure",
		"/waired/v1/integration/openclaw/reconfigure",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.RemoteAddr = "127.0.0.1:55555"
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: code=%d, want 404 — the daemon must not write integrations", path, rec.Code)
		}
	}
}
