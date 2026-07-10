package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func newOpenCodeServer(cfg OpenCodeIntegrationConfig) *Server {
	return New(fakeStatus{}, fakePinger{}).WithOpenCodeIntegration(cfg)
}

// writeOpenCodePlugin writes a waired.js plugin registering provider.waired
// with the given baseURL, mirroring what the opencode adapter emits.
func writeOpenCodePlugin(t *testing.T, home, baseURL string) {
	t.Helper()
	dir := filepath.Join(home, ".config", "opencode", "plugin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `export const WairedPlugin = async () => ({
  config: async (config) => {
    config.provider = config.provider || {};
    config.provider.waired = { options: { baseURL: "` + baseURL + `" } };
  },
});
`
	if err := os.WriteFile(filepath.Join(dir, "waired.js"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func doOpenCodeGet(t *testing.T, srv *Server) (int, OpenCodeIntegrationStatus) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/integration/opencode", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return rec.Code, OpenCodeIntegrationStatus{}
	}
	var got OpenCodeIntegrationStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	return rec.Code, got
}

func doOpenCodeReconfigure(t *testing.T, srv *Server) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/waired/v1/integration/opencode/reconfigure", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	srv.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestOpenCodeIntegration_Disabled404(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/integration/opencode", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestOpenCodeIntegration_NotConfigured(t *testing.T) {
	home := t.TempDir()
	srv := newOpenCodeServer(OpenCodeIntegrationConfig{
		HomeDir: home, ExpectedBaseURL: "http://127.0.0.1:9473/v1",
	})
	code, got := doOpenCodeGet(t, srv)
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	if got.Config.Configured || got.Config.Stale {
		t.Errorf("got %+v, want not configured", got.Config)
	}
}

func TestOpenCodeIntegration_ConfiguredFresh(t *testing.T) {
	home := t.TempDir()
	writeOpenCodePlugin(t, home, "http://127.0.0.1:9479/v1")
	srv := newOpenCodeServer(OpenCodeIntegrationConfig{
		HomeDir: home, ExpectedBaseURL: "http://127.0.0.1:9479/v1",
	})
	code, got := doOpenCodeGet(t, srv)
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	if !got.Config.Configured || got.Config.Stale {
		t.Errorf("got %+v, want fresh configured", got.Config)
	}
}

func TestOpenCodeIntegration_StaleBaseURL(t *testing.T) {
	home := t.TempDir()
	writeOpenCodePlugin(t, home, "http://127.0.0.1:9999/v1")
	srv := newOpenCodeServer(OpenCodeIntegrationConfig{
		HomeDir: home, ExpectedBaseURL: "http://127.0.0.1:9479/v1",
	})
	_, got := doOpenCodeGet(t, srv)
	if !got.Config.Configured || !got.Config.Stale {
		t.Errorf("expected stale, got %+v", got.Config)
	}
	if got.Config.CurrentValue != "http://127.0.0.1:9999/v1" {
		t.Errorf("CurrentValue = %q", got.Config.CurrentValue)
	}
}

func TestOpenCodeIntegration_RejectsNonGET(t *testing.T) {
	srv := newOpenCodeServer(OpenCodeIntegrationConfig{HomeDir: t.TempDir()})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/waired/v1/integration/opencode", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("code=%d, want 405", rec.Code)
	}
}

func TestOpenCodeReconfigure_Success(t *testing.T) {
	called := atomic.Int32{}
	srv := newOpenCodeServer(OpenCodeIntegrationConfig{
		HomeDir: t.TempDir(),
		Reconfigure: func(_ context.Context) error {
			called.Add(1)
			return nil
		},
	})
	code, body := doOpenCodeReconfigure(t, srv)
	if code != http.StatusOK {
		t.Fatalf("code=%d body=%s", code, body)
	}
	if called.Load() != 1 {
		t.Errorf("Reconfigure callback not invoked, count=%d", called.Load())
	}
	var resp map[string]bool
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp["applied"] {
		t.Errorf("applied=false in response: %s", body)
	}
}

func TestOpenCodeReconfigure_CallbackError500(t *testing.T) {
	srv := newOpenCodeServer(OpenCodeIntegrationConfig{
		HomeDir: t.TempDir(),
		Reconfigure: func(_ context.Context) error {
			return errors.New("opencode adapter exploded")
		},
	})
	code, body := doOpenCodeReconfigure(t, srv)
	if code != http.StatusInternalServerError {
		t.Errorf("code=%d, want 500; body=%s", code, body)
	}
	var resp map[string]string
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["error"] == "" {
		t.Errorf("expected error message, got %s", body)
	}
}

func TestOpenCodeReconfigure_NoCallbackRegistered503(t *testing.T) {
	srv := newOpenCodeServer(OpenCodeIntegrationConfig{
		HomeDir:     t.TempDir(),
		Reconfigure: nil,
	})
	code, body := doOpenCodeReconfigure(t, srv)
	if code != http.StatusServiceUnavailable {
		t.Errorf("code=%d, want 503; body=%s", code, body)
	}
}

func TestOpenCodeReconfigure_RejectsNonPOST(t *testing.T) {
	srv := newOpenCodeServer(OpenCodeIntegrationConfig{
		HomeDir:     t.TempDir(),
		Reconfigure: func(_ context.Context) error { return nil },
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/integration/opencode/reconfigure", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("code=%d, want 405", rec.Code)
	}
}
