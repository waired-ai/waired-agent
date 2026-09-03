package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/internal/runtime"
)

// The inference toggle used to be a middleware wrapped around the whole
// handler set, so it answered 503 before any routing ran — model
// discovery included, and a node with no engine could not reach the mesh
// at all (waired-agent#829). It is now one input to the Selector, which
// means these tests are about the two things that survived the move: the
// wire shape a client sees when the answer really is "nothing can serve
// this", and the routes that must never have been gated in the first
// place.

func newDisabledInferenceGateway(t *testing.T, sel SelectorIface) *Server {
	t.Helper()
	return NewServer(ServerConfig{}, Deps{
		Selector:       sel,
		Runtimes:       runtime.NewRegistry(),
		ListManifests:  func() []catalog.Manifest { return []catalog.Manifest{qwenManifest()} },
		HTTPClient:     http.DefaultClient,
		AllowOpenAI:    true,
		AllowAnthropic: true,
	})
}

// offSelector is a host whose local inference is off with nothing in the
// mesh to take over — the one state the removed gate was right about.
func offSelector() *fakeSelector {
	return &fakeSelector{err: router.ErrLocalInferenceOff}
}

func TestLocalInferenceOff_AnthropicKeepsTheGatesBody(t *testing.T) {
	gw := newDisabledInferenceGateway(t, offSelector())
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages",
		strings.NewReader(`{"model":"waired/default","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	// 400 on the Claude surface: the toggle is off and only a person can
	// turn it back on, so the turn ends now rather than after ten retries
	// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md).
	assertInferenceDisabledBody(t, w, http.StatusBadRequest,
		"Local inference is turned off on this host."+failClosedExits)
	if got := w.Header().Get(HeaderLocalError); got != LocalErrorInferenceDisabled {
		t.Errorf("%s = %q, want %q — the intercept names the toggle from it",
			HeaderLocalError, got, LocalErrorInferenceDisabled)
	}
}

func TestLocalInferenceOff_OpenAIKeepsTheGatesBody(t *testing.T) {
	gw := newDisabledInferenceGateway(t, offSelector())
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"waired/default","messages":[{"role":"user","content":"hi"}]}`))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	assertInferenceDisabledBody(t, w, http.StatusServiceUnavailable, InferenceDisabledMessage)
}

func assertInferenceDisabledBody(t *testing.T, w *httptest.ResponseRecorder, wantStatus int, wantMessage string) {
	t.Helper()
	if w.Code != wantStatus {
		t.Fatalf("want %d, got %d (body=%s)", wantStatus, w.Code, w.Body.String())
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
	// Verbatim, not just non-empty: this is the string the CLI prints and
	// the one users have been told to act on.
	if body.Error.Message != wantMessage {
		t.Errorf("message = %q, want %q", body.Error.Message, wantMessage)
	}
}

// Model discovery is not local execution. The gate blocked it, so a
// request-only node could not even list what it might route to.
func TestLocalInferenceOff_ModelDiscoveryStillAnswers(t *testing.T) {
	for _, path := range []string{"/v1/models", "/anthropic/v1/models"} {
		t.Run(path, func(t *testing.T) {
			gw := newDisabledInferenceGateway(t, offSelector())
			r := httptest.NewRequest(http.MethodGet, path, nil)
			r.RemoteAddr = "127.0.0.1:1"
			w := httptest.NewRecorder()
			gw.Handler().ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("%s = %d, want 200 (body=%s)", path, w.Code, w.Body.String())
			}
		})
	}
}

// The point of the move: with the local engine out, a selection that
// lands somewhere else is served, not refused. Guarding this is what
// stops the gate from creeping back in front of routing.
func TestLocalInferenceOff_RoutableRequestIsServed(t *testing.T) {
	upstream := fakeOllama(t, nil)
	defer upstream.Close()

	gw := newGatewayUnderTest(t, &fakeSelector{sel: router.Selection{
		EndpointID:  "remote-peer",
		ModelID:     "qwen3-8b-instruct",
		VariantID:   "q4",
		Runtime:     "ollama",
		EngineModel: "qwen3:8b-q4_K_M",
	}}, upstream.URL)

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"waired/default","messages":[{"role":"user","content":"hi"}]}`))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("a routable request must be served with local inference off, got %d (body=%s)",
			w.Code, w.Body.String())
	}
}
