package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/internal/runtime"
)

// anthropicGatewayWithWindow builds a gateway wired with a
// Deps.ContextWindowFor so the #623 advertisement + overflow guard are
// exercised. window(id) returns the effective window for model id.
func anthropicGatewayWithWindow(t *testing.T, sel SelectorIface, adapterURL string, manifests []catalog.Manifest, window func(string) int) *Server {
	t.Helper()
	reg := runtime.NewRegistry()
	reg.Register(fakeAdapter{baseURL: adapterURL})
	return NewServer(ServerConfig{Addr: "127.0.0.1:0"}, Deps{
		Selector:         sel,
		Runtimes:         reg,
		ListManifests:    asManifestList(manifests),
		HTTPClient:       http.DefaultClient,
		AllowOpenAI:      true,
		AllowAnthropic:   true,
		ContextWindowFor: window,
	})
}

// TestAnthropicMessages_ContextOverflow: a prompt that exceeds the served
// model's effective window is rejected with the exact Anthropic 400 that
// makes Claude Code auto-compact, and carries the no-fallback marker.
func TestAnthropicMessages_ContextOverflow(t *testing.T) {
	sel := &fakeSelector{sel: router.Selection{
		Runtime: "ollama", EngineModel: "qwen3:8b-q4_K_M", ModelID: "qwen3-8b-instruct",
	}}
	// Tiny window forces overflow; no upstream is needed — the guard
	// rejects before the engine is looked up.
	gw := anthropicGatewayWithWindow(t, sel, "", nil, func(string) int { return 100 })

	long := strings.Repeat("word ", 200) // 1000 bytes ≈ 250 approx tokens > 100
	body := `{"model":"claude-sonnet-4","max_tokens":64,"messages":[{"role":"user","content":"` + long + `"}]}`
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", w.Code, w.Body.String())
	}
	if got := w.Header().Get(HeaderLocalError); got != LocalErrorContextOverflow {
		t.Errorf("%s = %q, want %q (no-fallback marker)", HeaderLocalError, got, LocalErrorContextOverflow)
	}
	var env anthropicErrorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Type != "error" || env.Error.Type != "invalid_request_error" {
		t.Errorf("envelope = %+v, want error/invalid_request_error", env)
	}
	// The exact wording is load-bearing: Claude Code only auto-compacts on
	// "prompt is too long: <N> tokens > <M> maximum".
	if !strings.HasPrefix(env.Error.Message, "prompt is too long: ") ||
		!strings.HasSuffix(env.Error.Message, " tokens > 100 maximum") {
		t.Errorf("message = %q, want 'prompt is too long: N tokens > 100 maximum'", env.Error.Message)
	}
}

// TestAnthropicMessages_UnderWindowProceeds: a prompt within the window is
// served normally (no 400, no marker).
func TestAnthropicMessages_UnderWindowProceeds(t *testing.T) {
	upstream := fakeOllamaForAnthropic(t, nil)
	defer upstream.Close()
	sel := &fakeSelector{sel: router.Selection{
		Runtime: "ollama", EngineModel: "qwen3:8b-q4_K_M", ModelID: "qwen3-8b-instruct",
	}}
	gw := anthropicGatewayWithWindow(t, sel, upstream.URL, nil, func(string) int { return 1_000_000 })

	body := `{"model":"claude-sonnet-4","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200 (under window)", w.Code, w.Body.String())
	}
	if got := w.Header().Get(HeaderLocalError); got != "" {
		t.Errorf("%s = %q, want empty under window", HeaderLocalError, got)
	}
}

// TestAnthropicMessages_UnknownWindowFailsOpen: a 0 window ("unknown") skips
// the guard so an over-window prompt is still served (never spuriously 400s).
func TestAnthropicMessages_UnknownWindowFailsOpen(t *testing.T) {
	upstream := fakeOllamaForAnthropic(t, nil)
	defer upstream.Close()
	sel := &fakeSelector{sel: router.Selection{
		Runtime: "ollama", EngineModel: "qwen3:8b-q4_K_M", ModelID: "qwen3-8b-instruct",
	}}
	gw := anthropicGatewayWithWindow(t, sel, upstream.URL, nil, func(string) int { return 0 })

	long := strings.Repeat("word ", 200)
	body := `{"model":"claude-sonnet-4","max_tokens":64,"messages":[{"role":"user","content":"` + long + `"}]}`
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200 (unknown window fails open)", w.Code, w.Body.String())
	}
}

// TestAnthropicModelsList: /anthropic/v1/models returns the local catalog in
// Anthropic shape, each entry carrying max_input_tokens from ContextWindowFor.
func TestAnthropicModelsList(t *testing.T) {
	gw := anthropicGatewayWithWindow(t, &fakeSelector{}, "", []catalog.Manifest{qwenManifest()},
		func(string) int { return 131072 })

	r := httptest.NewRequest(http.MethodGet, "/anthropic/v1/models", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Data    []anthropicModel `json:"data"`
		HasMore bool             `json:"has_more"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.HasMore {
		t.Errorf("has_more = true, want false")
	}
	byID := map[string]anthropicModel{}
	for _, m := range got.Data {
		byID[m.ID] = m
	}
	for _, id := range []string{"qwen3-8b-instruct", "waired/default", "waired/coding"} {
		m, ok := byID[id]
		if !ok {
			t.Errorf("model id %q missing from /anthropic/v1/models", id)
			continue
		}
		if m.Type != "model" {
			t.Errorf("%s type = %q, want model", id, m.Type)
		}
		if m.MaxInputTokens != 131072 {
			t.Errorf("%s max_input_tokens = %d, want 131072", id, m.MaxInputTokens)
		}
	}
}

// TestAnthropicModelsSingle: /anthropic/v1/models/{id} returns one object;
// an unknown id 404s.
func TestAnthropicModelsSingle(t *testing.T) {
	gw := anthropicGatewayWithWindow(t, &fakeSelector{}, "", []catalog.Manifest{qwenManifest()},
		func(string) int { return 131072 })

	r := httptest.NewRequest(http.MethodGet, "/anthropic/v1/models/qwen3-8b-instruct", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var m anthropicModel
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m.ID != "qwen3-8b-instruct" || m.Type != "model" || m.MaxInputTokens != 131072 {
		t.Errorf("single model = %+v", m)
	}

	r2 := httptest.NewRequest(http.MethodGet, "/anthropic/v1/models/does-not-exist", nil)
	r2.RemoteAddr = "127.0.0.1:1"
	w2 := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w2, r2)
	if w2.Code != http.StatusNotFound {
		t.Errorf("unknown id status = %d, want 404", w2.Code)
	}
}
