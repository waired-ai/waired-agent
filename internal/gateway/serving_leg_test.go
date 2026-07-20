package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/internal/runtime"
)

// The serving leg of a peer hop had no test coverage at all: every
// two-node test in the tree stubs the receiving side (peerBEcho in
// internal/runtime/peer, fakeSelector here), so nothing ever drove a
// real HandlerSet + real router.Selector with the model string a
// consumer actually puts on the wire. That gap is why #107 shipped —
// every ollama peer hop 404'd. These tests close it.
//
// Everything below deliberately uses the REAL Selector. Swapping in
// fakeSelector would make them pass against the bug.

// newServingLegGateway builds a HandlerSet in the same posture as
// cmd/waired-agent's overlayDeps: a local-only Selector (nil
// MeshSnapshotFn and AllowExternal=false, so a peer request can never
// recurse to a third node) over the real catalog and a ready local
// model.
func newServingLegGateway(t *testing.T, upstreamURL string) *HandlerSet {
	t.Helper()

	reg := runtime.NewRegistry()
	reg.Register(fakeAdapter{baseURL: upstreamURL})

	manifests := []catalog.Manifest{qwenManifest()}
	sel := router.NewSelector(router.Inputs{
		Manifests: manifests,
		LocalState: catalog.State{
			Version: catalog.StateVersion,
			Models: map[string]catalog.ModelState{
				"qwen3-8b-instruct": {
					VariantID: "q4-gguf",
					OllamaTag: "qwen3:8b-q4_K_M",
					State:     catalog.ModelStateReady,
					PulledAt:  time.Now(),
				},
			},
			Endpoints: map[string]catalog.EndpointState{},
		},
		Hardware: hardware.Profile{
			OS: "linux", Arch: "x86_64",
			CPU:        hardware.CPUInfo{Cores: 16},
			RAMTotalGB: 64, RAMAvailableGB: 48,
			Engines: hardware.InstalledEngines{
				Ollama: hardware.EngineInfo{Installed: true, Version: "0.22.1"},
			},
		},
		Runtimes: reg,
		// Overlay posture — loop prevention.
		MeshSnapshotFn: nil,
		AllowExternal:  false,
	})

	return NewHandlerSet(Deps{
		Selector:      sel,
		Runtimes:      reg,
		ListManifests: asManifestList(manifests),
		HTTPClient:    http.DefaultClient,
		AllowOpenAI:   true,
	})
}

// consumerModelString is the exact value a consuming agent puts in the
// proxied body's `model` field: buildMeshCandidates matches a peer's
// advertised InferenceState.Models against Variant.Source.Tag,
// makeMeshCandidate carries the match as Selection.EngineModel, and
// openai.go rewrites the body to it. Derived from the fixture rather
// than hardcoded so a catalog change cannot silently decouple the two.
func consumerModelString(t *testing.T) string {
	t.Helper()
	tag := qwenManifest().Variants[0].Source.Tag
	if tag == "" {
		t.Fatal("fixture has no engine tag")
	}
	return tag
}

func TestServingLeg_AcceptsEngineNativeModel(t *testing.T) {
	var captured string
	upstream := fakeOllama(t, &captured)
	defer upstream.Close()

	h := newServingLegGateway(t, upstream.URL)
	model := consumerModelString(t)

	body, _ := json.Marshal(map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("serving leg returned %d for the consumer's own model string %q; want 200.\nbody: %s",
			w.Code, model, w.Body.String())
	}
	if captured == "" {
		t.Fatal("upstream engine was never reached")
	}
	var got struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(captured), &got); err != nil {
		t.Fatalf("decode proxied body: %v", err)
	}
	if got.Model != model {
		t.Fatalf("proxied model = %q, want %q", got.Model, model)
	}
}

func TestServingLeg_AcceptsCatalogAlias(t *testing.T) {
	// The loopback surface keeps working unchanged — alias resolution
	// still has priority over the engine-native fallback.
	var captured string
	upstream := fakeOllama(t, &captured)
	defer upstream.Close()

	h := newServingLegGateway(t, upstream.URL)
	body, _ := json.Marshal(map[string]any{
		"model":    "waired/default",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
}

func TestServingLeg_UnknownModelStill404s(t *testing.T) {
	// The fallback must not turn the 404 into a wildcard accept.
	upstream := fakeOllama(t, nil)
	defer upstream.Close()

	h := newServingLegGateway(t, upstream.URL)
	body, _ := json.Marshal(map[string]any{
		"model":    "not-in-any-catalog:latest",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", w.Code, w.Body.String())
	}
}
