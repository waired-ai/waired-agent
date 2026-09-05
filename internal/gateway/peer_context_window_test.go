package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/router"
)

// TestEffectiveContextWindow_PrefersTheEndpointThatAnswers is
// waired-agent#436 at the smallest scale it can be stated.
//
// Deps.ContextWindowFor describes THIS device. On a mesh leg the request
// is answered somewhere else, and guarding it against the local number
// let an over-window prompt through to be truncated at the head. The
// peer's own declaration wins whenever it made one.
func TestEffectiveContextWindow_PrefersTheEndpointThatAnswers(t *testing.T) {
	local := Deps{ContextWindowFor: func(string) int { return 200704 }}

	t.Run("a declaring peer wins", func(t *testing.T) {
		sel := router.Selection{ModelID: "m", ExecutionMode: "remote", ContextWindow: 98304}
		if got := effectiveContextWindow(local, sel); got != 98304 {
			t.Errorf("got %d, want the peer's 98304 and not this device's 200704", got)
		}
	})

	t.Run("a peer declaring nothing falls back", func(t *testing.T) {
		// Every agent predating the field sends 0. Treating that as a
		// zero-token window would 400 every request to it; treating it as
		// unknown reproduces the behaviour that shipped, which is what
		// lets a fleet upgrade in any order.
		sel := router.Selection{ModelID: "m", ExecutionMode: "remote"}
		if got := effectiveContextWindow(local, sel); got != 200704 {
			t.Errorf("got %d, want the pre-#1031 fallback 200704", got)
		}
	})

	t.Run("a local selection uses the local computation", func(t *testing.T) {
		sel := router.Selection{ModelID: "m", ExecutionMode: "local"}
		if got := effectiveContextWindow(local, sel); got != 200704 {
			t.Errorf("got %d, want 200704", got)
		}
	})

	t.Run("both unknown fails open", func(t *testing.T) {
		if got := effectiveContextWindow(Deps{}, router.Selection{ModelID: "m"}); got != 0 {
			t.Errorf("got %d, want 0 so the guard does not fire on a guess", got)
		}
	})
}

// TestAnthropicMessages_PeerWindowGuardsTheRequest is the same bug at
// the handler: a prompt this device would happily serve is refused
// because the PEER that would answer it cannot.
func TestAnthropicMessages_PeerWindowGuardsTheRequest(t *testing.T) {
	// ExecutionMode stays "local" so the fake adapter serves the leg: the
	// probe fan-out needs a live peer lookup this suite does not have, and
	// the guard does not branch on the mode — Selection.ContextWindow being
	// set is what makes this a peer's window rather than this device's.
	sel := &fakeSelector{sel: router.Selection{
		Runtime: "ollama", EngineModel: "qwen3:8b-q4_K_M", ModelID: "qwen3-8b-instruct",
		PeerDisplayID: "peer-a", ContextWindow: 100,
	}}
	// This device would allow 1,000,000 tokens. The peer said 100.
	gw := anthropicGatewayWithWindow(t, sel, "", nil, func(string) int { return 1000000 })

	long := strings.Repeat("word ", 200) // ≈ 250 approx tokens
	body := `{"model":"claude-sonnet-4","max_tokens":64,"messages":[{"role":"user","content":"` + long + `"}]}`
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400 sized by the peer's window", w.Code, w.Body.String())
	}
	if got := w.Header().Get(HeaderLocalError); got != LocalErrorContextOverflow {
		t.Errorf("%s = %q, want %q", HeaderLocalError, got, LocalErrorContextOverflow)
	}
	var env anthropicErrorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("body is not an Anthropic error envelope: %s", w.Body.String())
	}
	if env.Error.Message != contextOverflowToken {
		t.Errorf("message = %q, want %q", env.Error.Message, contextOverflowToken)
	}
	// The point of the test: the window the 400 was sized by is the peer's
	// 100, not this device's 1,000,000.
	if got := w.Header().Get(HeaderContextWindow); got != "100" {
		t.Errorf("%s = %q, want the peer's 100 and not this device's window",
			HeaderContextWindow, got)
	}
}

// TestOpenAIChatCompletions_PeerWindowGuardsTheRequest is
// waired-agent#436 on the OpenAI surface. It matters because the
// loopback (:9473) and data-plane (:9479) listeners carry a
// PeerAdapterFactory — they dispatch to peers — so sizing their guard
// from this device's own window is wrong in BOTH directions.
func TestOpenAIChatCompletions_PeerWindowGuardsTheRequest(t *testing.T) {
	long := strings.Repeat("word ", 200) // ≈ 250 approx tokens

	t.Run("a smaller peer refuses what this device would serve", func(t *testing.T) {
		sel := &fakeSelector{sel: router.Selection{
			Runtime: "ollama", EngineModel: "qwen3:8b-q4_K_M", ModelID: "qwen3-8b-instruct",
			PeerDisplayID: "peer-a", ContextWindow: 100,
		}}
		gw := anthropicGatewayWithWindow(t, sel, "", nil, func(string) int { return 1000000 })

		body := `{"model":"qwen3-8b-instruct","messages":[{"role":"user","content":"` + long + `"}]}`
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
		r.RemoteAddr = "127.0.0.1:1"
		w := httptest.NewRecorder()
		gw.Handler().ServeHTTP(w, r)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d body=%s, want 400 sized by the peer's window", w.Code, w.Body.String())
		}
		var env openAIErrorEnvelope
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("body is not an OpenAI error envelope: %s", w.Body.String())
		}
		if !strings.Contains(env.Error.Message, "> 100 maximum") {
			t.Errorf("the 400 names %q, not the peer's 100-token window", env.Error.Message)
		}
	})

	t.Run("a larger peer is not refused by this device's window", func(t *testing.T) {
		// The direction that only appears once the guard is wired on a
		// surface that can dispatch: this device holds a small model, the
		// peer that would answer holds a big one, and refusing here would
		// invent a limit neither endpoint has.
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","model":"m",` +
				`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
				`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
		}))
		defer upstream.Close()

		sel := &fakeSelector{sel: router.Selection{
			Runtime: "ollama", EngineModel: "qwen3:8b-q4_K_M", ModelID: "qwen3-8b-instruct",
			PeerDisplayID: "peer-a", ContextWindow: 1000000,
		}}
		gw := anthropicGatewayWithWindow(t, sel, upstream.URL, nil, func(string) int { return 100 })

		body := `{"model":"qwen3-8b-instruct","messages":[{"role":"user","content":"` + long + `"}]}`
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
		r.RemoteAddr = "127.0.0.1:1"
		w := httptest.NewRecorder()
		gw.Handler().ServeHTTP(w, r)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s, want 200 — the peer serves a 1M window",
				w.Code, w.Body.String())
		}
	})
}

// TestOpenAIChatCompletions_ServingSideWindowGuard is the other half of
// waired-agent#436: the overlay listener is where peer traffic lands,
// and it is the only place that knows what this engine is loaded with
// RIGHT NOW rather than what it advertised at its last push.
func TestOpenAIChatCompletions_ServingSideWindowGuard(t *testing.T) {
	sel := &fakeSelector{sel: router.Selection{
		Runtime: "ollama", EngineModel: "qwen3:8b-q4_K_M", ModelID: "qwen3-8b-instruct",
	}}
	gw := anthropicGatewayWithWindow(t, sel, "", nil, func(string) int { return 100 })

	long := strings.Repeat("word ", 200)
	body := `{"model":"qwen3-8b-instruct","messages":[{"role":"user","content":"` + long + `"}]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", w.Code, w.Body.String())
	}
	if got := w.Header().Get(HeaderLocalError); got != LocalErrorContextOverflow {
		t.Errorf("%s = %q, want %q — without it the requester cannot tell this "+
			"refusal from an engine fault", HeaderLocalError, got, LocalErrorContextOverflow)
	}
	if !strings.Contains(w.Body.String(), "prompt is too long") {
		t.Errorf("body does not name the cause: %s", w.Body.String())
	}
}

// TestOpenAIChatCompletions_WindowGuardFailsOpen keeps the guard from
// becoming a new way to refuse traffic. Peer requests are the one
// surface whose client is not this machine's owner, so a guess here
// would be a black hole nobody local can see.
func TestOpenAIChatCompletions_WindowGuardFailsOpen(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","model":"m",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	sel := &fakeSelector{sel: router.Selection{
		Runtime: "ollama", EngineModel: "qwen3:8b-q4_K_M", ModelID: "qwen3-8b-instruct",
	}}
	gw := anthropicGatewayWithWindow(t, sel, upstream.URL, nil, func(string) int { return 0 })

	long := strings.Repeat("word ", 200)
	body := `{"model":"qwen3-8b-instruct","messages":[{"role":"user","content":"` + long + `"}]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200 (unknown window must not refuse)", w.Code, w.Body.String())
	}
}

// TestAnthropicMessages_RelaysPeerContextOverflow closes the loop: the
// serving peer's OpenAI-shaped refusal has to reach the local client as
// the Anthropic 400 it compacts on. Passed through as an upstream_error
// it reads as a fault, and the turn fails instead of shrinking.
func TestAnthropicMessages_RelaysPeerContextOverflow(t *testing.T) {
	// Staged the way the serving half stages it (internal/gateway/openai.go):
	// the reason header, the two numbers, and the OpenAI-shaped body an
	// OpenAI client would read. A fake that stages less than the real peer
	// would let the relay drop the numbers and still pass.
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		stageContextOverflow(w, 300, 128)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error",` +
			`"code":"context_length_exceeded","message":"prompt is too long: 300 tokens > 128 maximum"}}`))
	}))
	defer peer.Close()

	for _, stream := range []bool{false, true} {
		name := "non-stream"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			// The upstream here IS the serving peer — the relay logic under
			// test is in the proxy legs, which do not branch on the mode.
			sel := &fakeSelector{sel: router.Selection{
				Runtime: "ollama", EngineModel: "qwen3:8b-q4_K_M",
				ModelID: "qwen3-8b-instruct", PeerDisplayID: "peer-a",
			}}
			// No local window: the ONLY thing that can raise the 400 here is
			// relaying what the peer said.
			gw := anthropicGatewayWithWindow(t, sel, peer.URL, nil, func(string) int { return 0 })

			body := `{"model":"claude-sonnet-4","max_tokens":64,"stream":` +
				map[bool]string{true: "true", false: "false"}[stream] +
				`,"messages":[{"role":"user","content":"hi"}]}`
			r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewBufferString(body))
			r.RemoteAddr = "127.0.0.1:1"
			w := httptest.NewRecorder()
			gw.Handler().ServeHTTP(w, r)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s, want 400", w.Code, w.Body.String())
			}
			if got := w.Header().Get(HeaderLocalError); got != LocalErrorContextOverflow {
				t.Errorf("%s = %q, want %q (surface, don't fall back)",
					HeaderLocalError, got, LocalErrorContextOverflow)
			}
			var env anthropicErrorEnvelope
			if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
				t.Fatalf("body is not an Anthropic error envelope: %s", w.Body.String())
			}
			if env.Error.Type != "invalid_request_error" {
				t.Errorf("error type = %q, want invalid_request_error — an upstream_error "+
					"does not trigger compaction", env.Error.Type)
			}
			if env.Error.Message != contextOverflowToken {
				t.Errorf("message = %q, want %q", env.Error.Message, contextOverflowToken)
			}
			// The peer counted against ITS window, so its numbers — not this
			// device's — are what reach the client.
			if got := w.Header().Get(HeaderPromptTokens); got != "300" {
				t.Errorf("%s = %q, want the peer's 300", HeaderPromptTokens, got)
			}
			if got := w.Header().Get(HeaderContextWindow); got != "128" {
				t.Errorf("%s = %q, want the peer's 128", HeaderContextWindow, got)
			}
		})
	}
}

// TestAnthropicMessages_RelaysAnOlderPeersContextOverflow: a peer running a
// build from before waired-agent#1187 stages the reason header and nothing
// else. The relay still has to produce the token — the numbers are a
// courtesy, the recovery is not.
func TestAnthropicMessages_RelaysAnOlderPeersContextOverflow(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(HeaderLocalError, LocalErrorContextOverflow)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error",` +
			`"code":"context_length_exceeded","message":"prompt is too long: 300 tokens > 128 maximum"}}`))
	}))
	defer peer.Close()

	sel := &fakeSelector{sel: router.Selection{
		Runtime: "ollama", EngineModel: "qwen3:8b-q4_K_M",
		ModelID: "qwen3-8b-instruct", PeerDisplayID: "peer-a",
	}}
	gw := anthropicGatewayWithWindow(t, sel, peer.URL, nil, func(string) int { return 0 })

	body := `{"model":"claude-sonnet-4","max_tokens":64,"stream":false,` +
		`"messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", w.Code, w.Body.String())
	}
	var env anthropicErrorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("body is not an Anthropic error envelope: %s", w.Body.String())
	}
	if env.Error.Message != contextOverflowToken {
		t.Errorf("message = %q, want %q", env.Error.Message, contextOverflowToken)
	}
	if got := w.Header().Get(HeaderPromptTokens); got != "" {
		t.Errorf("%s = %q, want it absent — the peer sent none", HeaderPromptTokens, got)
	}
}

// The "the two guards must agree" contract moved to
// context_overflow_count_test.go, which asserts it over a fixture that
// carries tool schemas, a tool call and a tool result — the parts a
// coding-agent conversation is actually made of. This file's version
// used two plain-text messages, the one shape the old pair of
// hand-written walks could not disagree about.
