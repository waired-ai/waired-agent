package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/internal/runtime"
)

// fakeAdapter is a runtime.Adapter that already points at the test's
// fake Ollama (an httptest server). EnsureRunning is a no-op.
type fakeAdapter struct{ baseURL string }

func (f fakeAdapter) Name() string                          { return "ollama" }
func (f fakeAdapter) EnsureRunning(_ context.Context) error { return nil }
func (f fakeAdapter) Health(_ context.Context) runtime.Health {
	return runtime.Health{State: runtime.StateReady}
}
func (f fakeAdapter) Stop(_ context.Context) error { return nil }
func (f fakeAdapter) BaseURL() string              { return f.baseURL }

// fakeSelector returns a canned Selection (or err) regardless of input.
type fakeSelector struct {
	sel router.Selection
	err error
	got router.Request
}

func (f *fakeSelector) Select(_ context.Context, req router.Request) (router.Selection, error) {
	f.got = req
	return f.sel, f.err
}

// SelectK satisfies the Phase 8 SelectorIface contract. Returns a
// single Candidate that commits to the canned Selection. K is
// ignored — fakes don't model the multi-candidate path; the
// internal/gateway/probe_test.go suite exercises that surface
// directly with a stub PeerProbeLookup.
func (f *fakeSelector) SelectK(_ context.Context, req router.Request, _ int) ([]router.Candidate, error) {
	f.got = req
	if f.err != nil {
		return nil, f.err
	}
	return []router.Candidate{router.NewLocalCandidate(f.sel)}, nil
}

func qwenManifest() catalog.Manifest {
	return catalog.Manifest{
		ModelID:       "qwen3-8b-instruct",
		ModelAliases:  []string{"waired/default"},
		ContextLength: 8192,
		Capabilities:  []string{"chat"},
		Variants: []catalog.Variant{{
			VariantID:      "q4-gguf",
			Format:         catalog.FormatOllamaTag,
			RuntimeSupport: []string{"ollama"},
			Source:         catalog.VariantSource{Type: "ollama", Tag: "qwen3:8b-q4_K_M"},
		}},
	}
}

// fakeOllama returns an httptest.Server that mimics Ollama's /v1/...
// surface for the routes the gateway exercises.
func fakeOllama(t *testing.T, capture *string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if capture != nil {
			*capture = string(body)
		}
		// Decide whether to stream based on the body's `stream` field.
		var probe struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &probe)
		if probe.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			f, _ := w.(http.Flusher)
			for _, chunk := range []string{
				`data: {"choices":[{"delta":{"content":"hi"}}]}`,
				`data: [DONE]`,
			} {
				_, _ = w.Write([]byte(chunk + "\n\n"))
				if f != nil {
					f.Flush()
				}
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	})
	return httptest.NewServer(mux)
}

func newGatewayUnderTest(t *testing.T, sel SelectorIface, adapterURL string) *Server {
	t.Helper()
	reg := runtime.NewRegistry()
	reg.Register(fakeAdapter{baseURL: adapterURL})
	return NewServer(ServerConfig{Addr: "127.0.0.1:0"}, Deps{
		Selector:       sel,
		Runtimes:       reg,
		ListManifests:  asManifestList([]catalog.Manifest{qwenManifest()}),
		HTTPClient:     http.DefaultClient,
		AllowOpenAI:    true,
		AllowAnthropic: true,
	})
}

func TestOpenAIModelsList(t *testing.T) {
	gw := newGatewayUnderTest(t, &fakeSelector{}, "")
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var got struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	wantIDs := map[string]bool{"qwen3-8b-instruct": false, "waired/default": false}
	for _, m := range got.Data {
		if _, ok := wantIDs[m.ID]; ok {
			wantIDs[m.ID] = true
		}
	}
	for id, seen := range wantIDs {
		if !seen {
			t.Errorf("model id %q missing from /v1/models response", id)
		}
	}
}

func TestOpenAIChatCompletions_NonStreamProxiesAndRewrites(t *testing.T) {
	var captured string
	upstream := fakeOllama(t, &captured)
	defer upstream.Close()

	sel := &fakeSelector{sel: router.Selection{
		EndpointID:    "ep_local_ollama_qwen3",
		ModelID:       "qwen3-8b-instruct",
		VariantID:     "q4-gguf",
		Runtime:       "ollama",
		EngineModel:   "qwen3:8b-q4_K_M",
		ExecutionMode: "local",
	}}
	gw := newGatewayUnderTest(t, sel, upstream.URL)

	body := `{"model":"waired/default","messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(captured, `"qwen3:8b-q4_K_M"`) {
		t.Errorf("upstream did not see rewritten model field; captured = %s", captured)
	}
	if strings.Contains(captured, `"waired/default"`) {
		t.Errorf("upstream still saw alias; captured = %s", captured)
	}
	if sel.got.Model != "waired/default" {
		t.Errorf("selector saw model %q, want waired/default", sel.got.Model)
	}
}

func TestOpenAIChatCompletions_StreamPassthrough(t *testing.T) {
	upstream := fakeOllama(t, nil)
	defer upstream.Close()

	sel := &fakeSelector{sel: router.Selection{
		Runtime: "ollama", EngineModel: "qwen3:8b-q4_K_M",
	}}
	gw := newGatewayUnderTest(t, sel, upstream.URL)

	body := `{"model":"waired/default","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if !strings.Contains(w.Body.String(), "data: [DONE]") {
		t.Errorf("stream missing [DONE]: %s", w.Body.String())
	}
}

func TestOpenAIChatCompletions_MissingModelField(t *testing.T) {
	gw := newGatewayUnderTest(t, &fakeSelector{}, "http://unused")
	body := `{"messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestOpenAIChatCompletions_UnknownModelMaps404(t *testing.T) {
	sel := &fakeSelector{err: errors.New("alias x: " + router.ErrModelNotFound.Error())}
	// Wrap with %w so errors.Is works:
	sel.err = wrap(router.ErrModelNotFound, "alias x not found")

	gw := newGatewayUnderTest(t, sel, "http://unused")
	body := `{"model":"x","messages":[]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
	var env openAIErrorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode err: %v", err)
	}
	if env.Error.Code != "model_not_found" {
		t.Errorf("error.code = %q", env.Error.Code)
	}
}

func TestOpenAIChatCompletions_ModelNotReady503(t *testing.T) {
	sel := &fakeSelector{err: wrap(router.ErrModelNotReady, "downloading")}
	gw := newGatewayUnderTest(t, sel, "http://unused")
	body := `{"model":"waired/default","messages":[]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Errorf("expected Retry-After header on 503 model_not_ready")
	}
}

func TestOpenAIResponses_NotImplemented(t *testing.T) {
	gw := newGatewayUnderTest(t, &fakeSelector{}, "http://unused")
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString("{}"))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", w.Code)
	}
}

func TestServer_OpenAIDisabled404(t *testing.T) {
	gw := NewServer(ServerConfig{}, Deps{
		Selector:       &fakeSelector{},
		Runtimes:       runtime.NewRegistry(),
		ListManifests:  asManifestList(nil),
		HTTPClient:     http.DefaultClient,
		AllowOpenAI:    false,
		AllowAnthropic: false,
	})
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when AllowOpenAI=false", w.Code)
	}
}

func TestLoopbackOnly_RejectsExternal(t *testing.T) {
	gw := newGatewayUnderTest(t, &fakeSelector{}, "http://unused")
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.RemoteAddr = "203.0.113.42:55555"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for non-loopback caller", w.Code)
	}
}

// wrap is a tiny errors.Wrap polyfill.
func wrap(target error, msg string) error {
	return wrappedErr{target: target, msg: msg}
}

type wrappedErr struct {
	target error
	msg    string
}

func (e wrappedErr) Error() string { return e.msg + ": " + e.target.Error() }
func (e wrappedErr) Unwrap() error { return e.target }
