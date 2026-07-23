package gateway

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/internal/runtime"
)

// namedFakeAdapter is fakeAdapter with a caller-chosen registry name,
// so a test can register an external openai-compat endpoint alongside
// the local engine.
type namedFakeAdapter struct {
	name    string
	baseURL string
}

func (f namedFakeAdapter) Name() string                          { return f.name }
func (f namedFakeAdapter) EnsureRunning(_ context.Context) error { return nil }
func (f namedFakeAdapter) Health(_ context.Context) runtime.Health {
	return runtime.Health{State: runtime.StateReady}
}
func (f namedFakeAdapter) Stop(_ context.Context) error { return nil }
func (f namedFakeAdapter) BaseURL() string              { return f.baseURL }

// admissionSpy records the calls a listener makes into the shared
// admission counter, plus whether a slot was held at the moment the
// engine was actually reached.
type admissionSpy struct {
	admits   atomic.Int32
	releases atomic.Int32
}

func (a *admissionSpy) hook(_ context.Context) func() {
	a.admits.Add(1)
	return func() { a.releases.Add(1) }
}

func (a *admissionSpy) held() int32 { return a.admits.Load() - a.releases.Load() }

// newAdmissionGateway builds a loopback-style listener (local engine +
// one external endpoint + peer routing enabled) wired to spy.
func newAdmissionGateway(t *testing.T, sel SelectorIface, spy *admissionSpy, engineURL string) *Server {
	t.Helper()
	reg := runtime.NewRegistry()
	reg.Register(fakeAdapter{baseURL: engineURL})
	reg.Register(namedFakeAdapter{name: "openai-compat:cloud", baseURL: engineURL})
	return NewServer(ServerConfig{Addr: "127.0.0.1:0"}, Deps{
		Selector:       sel,
		Runtimes:       reg,
		ListManifests:  asManifestList([]catalog.Manifest{qwenManifest()}),
		HTTPClient:     http.DefaultClient,
		AllowOpenAI:    true,
		AllowAnthropic: true,
		PeerAdapterFactory: func(string) (runtime.Adapter, error) {
			return fakeAdapter{baseURL: engineURL}, nil
		},
		LocalAdmission: spy.hook,
	})
}

func postChat(t *testing.T, gw *Server) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"waired/default","messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	return w
}

// TestLocalAdmission_LocalDispatchHoldsSlotUntilTheEngineIsDone: the
// owner's own request occupies a slot on this machine's shared
// admission counter for as long as it occupies the engine — that is
// what makes Config.Capacity mean "concurrent requests on this
// machine" and what lets the owner-priority latch fire (waired#899).
func TestLocalAdmission_LocalDispatchHoldsSlotUntilTheEngineIsDone(t *testing.T) {
	spy := &admissionSpy{}
	var heldAtEngine int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.StoreInt32(&heldAtEngine, spy.held())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()

	sel := &fakeSelector{sel: router.Selection{Runtime: "ollama", EngineModel: "qwen3:8b-q4_K_M"}}
	gw := newAdmissionGateway(t, sel, spy, upstream.URL)

	if code := postChat(t, gw).Code; code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if got := spy.admits.Load(); got != 1 {
		t.Fatalf("admits = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&heldAtEngine); got != 1 {
		t.Fatalf("slots held while the engine was serving = %d, want 1", got)
	}
	if got := spy.releases.Load(); got != 1 {
		t.Fatalf("releases = %d, want 1 (the slot must be freed when the request ends)", got)
	}
}

// TestLocalAdmission_RemoteDispatchIsNotCounted: a local request the
// router sent to a mesh peer does not occupy this machine's engine, so
// counting it here would starve peers and latch public admission for
// work that never ran locally.
func TestLocalAdmission_RemoteDispatchIsNotCounted(t *testing.T) {
	spy := &admissionSpy{}
	upstream := fakeOllama(t, nil)
	defer upstream.Close()

	sel := &fakeSelector{sel: router.Selection{Runtime: "remote:dev-peer-b", EngineModel: "qwen3:8b-q4_K_M"}}
	gw := newAdmissionGateway(t, sel, spy, upstream.URL)

	if code := postChat(t, gw).Code; code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if got := spy.admits.Load(); got != 0 {
		t.Fatalf("admits = %d, want 0 for a peer-served request", got)
	}
}

// TestLocalAdmission_ExternalEndpointIsNotCounted: same reasoning for
// an external openai-compat endpoint — the upstream provider runs the
// model, not this machine.
func TestLocalAdmission_ExternalEndpointIsNotCounted(t *testing.T) {
	spy := &admissionSpy{}
	upstream := fakeOllama(t, nil)
	defer upstream.Close()

	sel := &fakeSelector{sel: router.Selection{Runtime: "openai-compat:cloud", EngineModel: "gpt-x"}}
	gw := newAdmissionGateway(t, sel, spy, upstream.URL)

	if code := postChat(t, gw).Code; code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if got := spy.admits.Load(); got != 0 {
		t.Fatalf("admits = %d, want 0 for an external endpoint", got)
	}
}

// TestLocalAdmission_AnthropicSurfaceCountsToo: the Claude intercept is
// the busiest local surface, so its handler must take a slot as well —
// the two dispatch sites are separate code paths.
func TestLocalAdmission_AnthropicSurfaceCountsToo(t *testing.T) {
	spy := &admissionSpy{}
	upstream := fakeOllama(t, nil)
	defer upstream.Close()

	sel := &fakeSelector{sel: router.Selection{Runtime: "ollama", EngineModel: "qwen3:8b-q4_K_M"}}
	gw := newAdmissionGateway(t, sel, spy, upstream.URL)

	body := `{"model":"waired/default","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if got := spy.admits.Load(); got != 1 {
		t.Fatalf("admits = %d, want 1", got)
	}
	if got := spy.releases.Load(); got != 1 {
		t.Fatalf("releases = %d, want 1", got)
	}
}

// TestLocalAdmission_UnwiredListenerIsUnaffected: the overlay listener
// leaves Deps.LocalAdmission nil (its requests are counted by the
// inference server's capacityGate), so the hook must be entirely
// optional.
func TestLocalAdmission_UnwiredListenerIsUnaffected(t *testing.T) {
	upstream := fakeOllama(t, nil)
	defer upstream.Close()

	sel := &fakeSelector{sel: router.Selection{Runtime: "ollama", EngineModel: "qwen3:8b-q4_K_M"}}
	gw := newGatewayUnderTest(t, sel, upstream.URL)
	if code := postChat(t, gw).Code; code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
}
