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

func newOpenClawServer(cfg OpenClawIntegrationConfig) *Server {
	return New(fakeStatus{}, fakePinger{}).WithOpenClawIntegration(cfg)
}

// writeOpenClawPlugin writes an index.mjs registering the waired provider
// with the given BASE_URL, mirroring what the openclaw adapter emits.
func writeOpenClawPlugin(t *testing.T, home, baseURL string) {
	t.Helper()
	dir := filepath.Join(home, ".openclaw", "plugins", "waired")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `const BASE_URL = "` + baseURL + `";
export default { id: "waired", register(api) { api.registerProvider({ id: "waired" }); } };
`
	if err := os.WriteFile(filepath.Join(dir, "index.mjs"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func doOpenClawGet(t *testing.T, srv *Server) (int, OpenClawIntegrationStatus) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/integration/openclaw", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return rec.Code, OpenClawIntegrationStatus{}
	}
	var got OpenClawIntegrationStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	return rec.Code, got
}

func doOpenClawReconfigure(t *testing.T, srv *Server) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/waired/v1/integration/openclaw/reconfigure", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	srv.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestOpenClawIntegration_Disabled404(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/integration/openclaw", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestOpenClawIntegration_NotConfigured(t *testing.T) {
	srv := newOpenClawServer(OpenClawIntegrationConfig{
		HomeDir: t.TempDir(), ExpectedBaseURL: "http://127.0.0.1:9479/v1",
	})
	code, got := doOpenClawGet(t, srv)
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	if got.Config.Configured || got.Config.Stale {
		t.Errorf("got %+v, want not configured", got.Config)
	}
}

func TestOpenClawIntegration_ConfiguredFresh(t *testing.T) {
	home := t.TempDir()
	writeOpenClawPlugin(t, home, "http://127.0.0.1:9479/v1")
	srv := newOpenClawServer(OpenClawIntegrationConfig{
		HomeDir: home, ExpectedBaseURL: "http://127.0.0.1:9479/v1",
	})
	_, got := doOpenClawGet(t, srv)
	if !got.Config.Configured || got.Config.Stale {
		t.Errorf("got %+v, want fresh configured", got.Config)
	}
}

func TestOpenClawIntegration_StaleBaseURL(t *testing.T) {
	home := t.TempDir()
	writeOpenClawPlugin(t, home, "http://127.0.0.1:9999/v1")
	srv := newOpenClawServer(OpenClawIntegrationConfig{
		HomeDir: home, ExpectedBaseURL: "http://127.0.0.1:9479/v1",
	})
	_, got := doOpenClawGet(t, srv)
	if !got.Config.Configured || !got.Config.Stale {
		t.Errorf("expected stale, got %+v", got.Config)
	}
	if got.Config.CurrentValue != "http://127.0.0.1:9999/v1" {
		t.Errorf("CurrentValue = %q", got.Config.CurrentValue)
	}
}

func TestOpenClawReconfigure_Success(t *testing.T) {
	called := atomic.Int32{}
	srv := newOpenClawServer(OpenClawIntegrationConfig{
		HomeDir: t.TempDir(),
		Reconfigure: func(_ context.Context) error {
			called.Add(1)
			return nil
		},
	})
	code, body := doOpenClawReconfigure(t, srv)
	if code != http.StatusOK {
		t.Fatalf("code=%d body=%s", code, body)
	}
	if called.Load() != 1 {
		t.Errorf("Reconfigure callback not invoked, count=%d", called.Load())
	}
}

func TestOpenClawReconfigure_CallbackError500(t *testing.T) {
	srv := newOpenClawServer(OpenClawIntegrationConfig{
		HomeDir: t.TempDir(),
		Reconfigure: func(_ context.Context) error {
			return errors.New("openclaw adapter exploded")
		},
	})
	code, body := doOpenClawReconfigure(t, srv)
	if code != http.StatusInternalServerError {
		t.Errorf("code=%d, want 500; body=%s", code, body)
	}
}

func TestOpenClawReconfigure_NoCallbackRegistered503(t *testing.T) {
	srv := newOpenClawServer(OpenClawIntegrationConfig{HomeDir: t.TempDir()})
	code, body := doOpenClawReconfigure(t, srv)
	if code != http.StatusServiceUnavailable {
		t.Errorf("code=%d, want 503; body=%s", code, body)
	}
}
