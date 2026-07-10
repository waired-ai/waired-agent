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

func newInferenceDisabledGateway(t *testing.T, isInferenceDisabled func() bool) *Server {
	t.Helper()
	reg := runtime.NewRegistry()
	return NewServer(ServerConfig{}, Deps{
		Selector:            &fakeSelector{},
		Runtimes:            reg,
		ListManifests:       func() []catalog.Manifest { return nil },
		HTTPClient:          http.DefaultClient,
		AllowOpenAI:         true,
		AllowAnthropic:      true,
		IsInferenceDisabled: isInferenceDisabled,
	})
}

func TestInferenceGate_Returns503ForAnthropic(t *testing.T) {
	gw := newInferenceDisabledGateway(t, func() bool { return true })
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("inference-disabled agent should return 503, got %d (body=%s)", w.Code, w.Body.String())
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
	if body.Type != "error" || body.Error.Type != "waired_inference_disabled" {
		t.Errorf("body shape: %+v (want error.type=waired_inference_disabled)", body)
	}
	if body.Error.Message == "" {
		t.Error("error message must be non-empty so users learn how to recover")
	}
}

func TestInferenceGate_Returns503ForOpenAI(t *testing.T) {
	gw := newInferenceDisabledGateway(t, func() bool { return true })
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("inference-disabled agent should return 503 for OpenAI route, got %d", w.Code)
	}
}

func TestInferenceGate_PassesThroughWhenEnabled(t *testing.T) {
	gw := newInferenceDisabledGateway(t, func() bool { return false })
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	if w.Code == http.StatusServiceUnavailable {
		t.Fatalf("enabled inference must not gate, got body=%s", w.Body.String())
	}
}

func TestInferenceGate_NilGateIsTransparent(t *testing.T) {
	gw := newInferenceDisabledGateway(t, nil)
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	if w.Code == http.StatusServiceUnavailable {
		t.Fatalf("nil IsInferenceDisabled must not gate, got 503 body=%s", w.Body.String())
	}
}

func TestInferenceGate_FlipReflectedLive(t *testing.T) {
	var disabled atomic.Bool
	gw := newInferenceDisabledGateway(t, disabled.Load)

	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	if w.Code == http.StatusServiceUnavailable {
		t.Fatalf("not disabled yet, expected non-503")
	}

	disabled.Store(true)
	w = httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("after disable, expected 503, got %d", w.Code)
	}
}
