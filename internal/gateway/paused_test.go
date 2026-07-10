package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/runtime"
)

func newPausedGateway(t *testing.T, isPaused func() bool) *Server {
	t.Helper()
	reg := runtime.NewRegistry()
	return NewServer(ServerConfig{}, Deps{
		Selector:       &fakeSelector{},
		Runtimes:       reg,
		ListManifests:  func() []catalog.Manifest { return nil },
		HTTPClient:     http.DefaultClient,
		AllowOpenAI:    true,
		AllowAnthropic: true,
		IsPaused:       isPaused,
	})
}

func TestPausedGate_Returns503ForAnthropic(t *testing.T) {
	gw := newPausedGateway(t, func() bool { return true })
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("paused agent should return 503, got %d (body=%s)", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not valid JSON: %v\n%s", err, w.Body.String())
	}
	if body.Type != "error" || body.Error.Type != "waired_paused" {
		t.Errorf("body shape: %+v", body)
	}
	if body.Error.Message == "" {
		t.Error("error message must be non-empty so users learn how to recover")
	}
}

func TestPausedGate_Returns503ForOpenAI(t *testing.T) {
	gw := newPausedGateway(t, func() bool { return true })
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("paused agent should return 503 for OpenAI route too, got %d", w.Code)
	}
}

func TestPausedGate_PassesThroughWhenNotPaused(t *testing.T) {
	gw := newPausedGateway(t, func() bool { return false })
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	// Whatever the result, it must not be 503 from the gate.
	if w.Code == http.StatusServiceUnavailable {
		t.Fatalf("active agent must not return 503, got body=%s", w.Body.String())
	}
}

func TestPausedGate_NilGateIsTransparent(t *testing.T) {
	gw := newPausedGateway(t, nil)
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	if w.Code == http.StatusServiceUnavailable {
		t.Fatalf("nil IsPaused must not gate, got 503 body=%s", w.Body.String())
	}
}

func TestPausedGate_FlipReflectedLive(t *testing.T) {
	var paused atomic.Bool
	gw := newPausedGateway(t, paused.Load)

	// Active first.
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	if w.Code == http.StatusServiceUnavailable {
		t.Fatalf("not paused yet, expected non-503")
	}

	// Now pause and re-fire.
	paused.Store(true)
	w = httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("after pause, expected 503, got %d", w.Code)
	}
}
