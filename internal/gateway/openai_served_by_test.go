package gateway

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/waired-ai/waired-agent/internal/router"
)

// TestOpenAIChatCompletions_NamesTheComputerThatAnswered is
// waired-agent#1176. Measured on sv-mag before the fix: a peer-served reply
// on :9473 carried `X-Waired-Inference-Peer: dev_0a0d…` and a locally served
// one carried no X-Waired header at all — only Content-Length, Content-Type,
// Date and the engine's own `Server: uvicorn`. The body's `model` field is
// the engine tag (`openai/gpt-oss-20b`), which names neither Waired nor the
// computer. One listener over, the Anthropic surface answers the same
// question for the same case with HeaderLocalModel (anthropic.go), so an
// OpenAI-dialect client — OpenCode, a chat app, the verification harness —
// was the only caller that could not tell a Waired answer from an upstream
// one (#1073: every surface says which computer answered).
//
// PIN: product contract — waired-ai/waired-agent#1176, and the pair's
// meaning is stated by HeaderLocalModel's own doc comment (probe.go): set on
// a local leg and a mesh leg alike, with the serving peer, when there was
// one, on HeaderInferencePeer. So "model header, no peer header" is "this
// device", exactly as on :9472.
func TestOpenAIChatCompletions_NamesTheComputerThatAnswered(t *testing.T) {
	t.Run("a locally served reply names the model that answered", func(t *testing.T) {
		upstream := fakeOllama(t, nil)
		defer upstream.Close()

		sel := &fakeSelector{sel: router.Selection{
			EndpointID: "ep_local_ollama_qwen3", ModelID: "qwen3-8b-instruct",
			Runtime: "ollama", EngineModel: "qwen3:8b-q4_K_M", ExecutionMode: "local",
		}}
		gw := newGatewayUnderTest(t, sel, upstream.URL)

		w := postChatCompletion(t, gw, `{"model":"waired/default","messages":[{"role":"user","content":"hi"}]}`)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
		}
		if got := w.Header().Get(HeaderLocalModel); got != "qwen3-8b-instruct" {
			t.Errorf("%s = %q, want the catalog id that answered", HeaderLocalModel, got)
		}
		// The other half of the pair: no peer header is what makes the
		// answer this device's.
		if got := w.Header().Get(HeaderInferencePeer); got != "" {
			t.Errorf("%s = %q on a local answer, want it absent", HeaderInferencePeer, got)
		}
	})

	t.Run("a streamed local reply names it too", func(t *testing.T) {
		// The header must be staged before the first byte or SSE loses it.
		// Streaming and non-streaming share one path here, and this is the
		// row that says so.
		upstream := fakeOllama(t, nil)
		defer upstream.Close()

		sel := &fakeSelector{sel: router.Selection{
			ModelID: "qwen3-8b-instruct", Runtime: "ollama", EngineModel: "qwen3:8b-q4_K_M",
		}}
		gw := newGatewayUnderTest(t, sel, upstream.URL)

		w := postChatCompletion(t, gw,
			`{"model":"waired/default","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

		if got := w.Header().Get(HeaderLocalModel); got != "qwen3-8b-instruct" {
			t.Errorf("%s = %q on the streamed reply", HeaderLocalModel, got)
		}
	})

	t.Run("a peer's own header does not double the answer", func(t *testing.T) {
		// A mesh leg's upstream is another node running THIS handler, so it
		// stamps the same headers for its own client. proxyToEngine copies
		// upstream headers with Add, so without the guard the client is
		// handed two values for one question — and the second one is a
		// different computer's view of the turn.
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set(HeaderLocalModel, "a-model-the-peer-named")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
		}))
		defer upstream.Close()

		// ExecutionMode stays "local" so the fake adapter serves the leg —
		// the probe fan-out needs a live peer lookup this suite does not
		// have. PeerDisplayID being set is what makes this the peer arm, the
		// same shape peer_context_window_test.go uses.
		sel := &fakeSelector{sel: router.Selection{
			ModelID: "qwen3-8b-instruct", Runtime: "ollama", EngineModel: "qwen3:8b-q4_K_M",
			PeerDisplayID: "peer-a",
		}}
		gw := newGatewayUnderTest(t, sel, upstream.URL)

		w := postChatCompletion(t, gw, `{"model":"waired/default","messages":[{"role":"user","content":"hi"}]}`)

		if got := w.Header().Values(HeaderLocalModel); len(got) != 1 || got[0] != "qwen3-8b-instruct" {
			t.Errorf("%s = %v, want exactly this gateway's one answer", HeaderLocalModel, got)
		}
		if got := w.Header().Get(HeaderInferencePeer); got != "peer-a" {
			t.Errorf("%s = %q, want the peer that answered", HeaderInferencePeer, got)
		}
		// Everything the gateway did not stage still comes through.
		if got := w.Header().Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want the upstream's", got)
		}
	})
}

func postChatCompletion(t *testing.T, gw *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	return w
}
