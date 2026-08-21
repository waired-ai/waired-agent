package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/internal/runtime"
)

// waired-agent#947 has two halves, and they live in two packages.
//
// The gateway's half is that it hands EnsureRunning the REQUEST's context,
// which is correct and is pinned here: a client that disconnects must stop
// waiting, and must stop holding the admission slot it takes while the engine
// comes up. What was wrong was the consequence — internal/runtime passed that
// same context to exec.CommandContext, so a request that won the
// single-flight gate owned the engine it started and SIGKILLed it (the leader
// only, orphaning the workers still holding VRAM) when ServeHTTP returned.
//
// The lifetime half is therefore pinned where it is decided: internal/runtime's
// TestOllamaEnsureRunning_CallerCancellationDoesNotOwnTheEngine asserts the
// spawn gets a context that can never be cancelled.
//
// This test exists because the fake here USED TO DISCARD the context, which is
// exactly why the defect was unwritable in this package — the same shape
// inference_pull_engine_test.go blames for letting the pull-path version
// through.
func TestGatewayPassesTheRequestContextToEnsureRunning(t *testing.T) {
	upstream := fakeOllama(t, nil)
	defer upstream.Close()

	ensured := &ensuredContexts{}
	reg := runtime.NewRegistry()
	reg.Register(fakeAdapter{baseURL: upstream.URL, ensured: ensured})
	gw := NewServer(ServerConfig{Addr: "127.0.0.1:0"}, Deps{
		Selector: &fakeSelector{sel: router.Selection{
			EndpointID:    "ep_local_ollama_qwen3",
			ModelID:       "qwen3-8b-instruct",
			VariantID:     "q4-gguf",
			Runtime:       "ollama",
			EngineModel:   "qwen3:8b-q4_K_M",
			ExecutionMode: "local",
		}},
		Runtimes:      reg,
		ListManifests: asManifestList([]catalog.Manifest{qwenManifest()}),
		HTTPClient:    http.DefaultClient,
		AllowOpenAI:   true,
	})

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"qwen3-8b-instruct","messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Content-Type", "application/json")
	r.RemoteAddr = "127.0.0.1:1"
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	gw.Handler().ServeHTTP(httptest.NewRecorder(), r.WithContext(ctx))

	got := ensured.last()
	if got == nil {
		t.Fatal("EnsureRunning was never called")
	}
	// Live, i.e. the caller can still walk away. Not an uncancellable one:
	// a disconnected client would then keep an admission slot for the whole
	// of a cold start.
	if got.Done() == nil {
		t.Error("EnsureRunning got a context that can never be cancelled; " +
			"a client that disconnects would go on waiting")
	}
	if got.Err() != nil {
		t.Errorf("EnsureRunning got an already-cancelled context: %v", got.Err())
	}
}
