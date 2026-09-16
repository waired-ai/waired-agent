package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// Every Waired row is one of two sessions whatever computer answers it: 200704
// tokens, or 1048576 for a "[1m]" row. The floor makes the answering computer
// hold at least the row's window; the guard holds the turn to it.
//
// PRODUCT CONTRACT, ratifying source: owner decision 2026-09-16 on
// waired-agent#1396.

func TestGuardedWindow(t *testing.T) {
	for _, tc := range []struct {
		name     string
		declared int // the answering computer's window; 0 = declares none
		local    int // this device's own computation
		row      int
		want     int
	}{
		{"a 200k row on a computer that holds 1M", hostfit.ServingWindow1M, 0, hostfit.ServingWindow200k, hostfit.ServingWindow200k},
		{"a 1M row on the same computer", hostfit.ServingWindow1M, 0, hostfit.ServingWindow1M, hostfit.ServingWindow1M},
		{"a 200k row on a 200k computer", hostfit.ServingWindow200k, 0, hostfit.ServingWindow200k, hostfit.ServingWindow200k},
		{"a 200k row answered by this device holding 1M", 0, hostfit.ServingWindow1M, hostfit.ServingWindow200k, hostfit.ServingWindow200k},
		{"a row with nothing known about the engine takes its own window", 0, 0, hostfit.ServingWindow200k, hostfit.ServingWindow200k},
		// A request that is not a Waired row — a catalog id, or the
		// waired/default a chat app sends — is held to the engine alone.
		{"not a row: the engine's window", hostfit.ServingWindow1M, 0, 0, hostfit.ServingWindow1M},
		{"not a row, nothing known: fails open", 0, 0, 0, 0},
		// The floor keeps this from arising; if it does, the smaller wins.
		{"an engine under the row's window", 131072, 0, hostfit.ServingWindow200k, 131072},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps := Deps{ContextWindowFor: func(string) int { return tc.local }}
			sel := router.Selection{ModelID: "m", ContextWindow: tc.declared}
			if got := guardedWindow(deps, sel, tc.row); got != tc.want {
				t.Errorf("guardedWindow = %d, want %d", got, tc.want)
			}
		})
	}
}

// rowSessionGateway is a gateway that routes the Waired rows on both listeners,
// with every turn answered by a computer that declares 1M.
func rowSessionGateway(t *testing.T, upstreamURL string) *Server {
	t.Helper()
	reg := runtime.NewRegistry()
	reg.Register(fakeAdapter{baseURL: upstreamURL})
	return NewServer(ServerConfig{Addr: "127.0.0.1:0"}, Deps{
		Selector: &fakeSelector{sel: router.Selection{
			Runtime: "ollama", EngineModel: "qwen3:8b-q4_K_M", ModelID: "qwen3-8b-instruct",
			ContextWindow: hostfit.ServingWindow1M,
		}},
		Runtimes:              reg,
		ListManifests:         asManifestList(nil),
		HTTPClient:            http.DefaultClient,
		AllowOpenAI:           true,
		AllowAnthropic:        true,
		ClaudeModelDirectives: true,
		RouteDirectives:       true,
		ContextWindowFor:      func(string) int { return hostfit.ServingWindow1M },
	})
}

// overOneRowSession is a user turn the approximate counter puts a little past
// 200704 tokens and far under 1M.
func overOneRowSession() string {
	return strings.Repeat("word ", 170000)
}

func TestAnthropicMessages_ABaseRowIsHeldToItsSession(t *testing.T) {
	upstream := fakeOllamaForAnthropic(t, nil)
	defer upstream.Close()
	gw := rowSessionGateway(t, upstream.URL)
	body := `{"model":"waired","max_tokens":64,"messages":[{"role":"user","content":"` + overOneRowSession() + `"}]}`

	send := func(beta string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewBufferString(body))
		r.RemoteAddr = "127.0.0.1:1"
		if beta != "" {
			r.Header.Set("Anthropic-Beta", beta)
		}
		w := httptest.NewRecorder()
		gw.Handler().ServeHTTP(w, r)
		return w
	}

	t.Run("the 200k row refuses past 200704 on a 1M computer", func(t *testing.T) {
		w := send("")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want the 400 Claude Code compacts on", w.Code)
		}
		if got := w.Header().Get(HeaderLocalError); got != LocalErrorContextOverflow {
			t.Errorf("%s = %q, want %q", HeaderLocalError, got, LocalErrorContextOverflow)
		}
		if got := w.Header().Get(HeaderContextWindow); got != strconv.Itoa(hostfit.ServingWindow200k) {
			t.Errorf("%s = %q, want the row's 200704, not the computer's 1M", HeaderContextWindow, got)
		}
	})

	t.Run("the same turn on the 1M row goes through", func(t *testing.T) {
		if w := send("context-1m-2025-08-07"); w.Code != http.StatusOK {
			t.Fatalf("status = %d body=%.200s, want 200 — the 1M row is a 1M session", w.Code, w.Body.String())
		}
	})
}

func TestOpenAIChatCompletions_ABaseRowIsHeldToItsSession(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","model":"m",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()
	gw := rowSessionGateway(t, upstream.URL)

	send := func(model string) *httptest.ResponseRecorder {
		body := `{"model":"` + model + `","messages":[{"role":"user","content":"` + overOneRowSession() + `"}]}`
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
		r.RemoteAddr = "127.0.0.1:1"
		w := httptest.NewRecorder()
		gw.Handler().ServeHTTP(w, r)
		return w
	}

	// What OpenCode and OpenClaw send for waired/default and for a computer by
	// name: both are 200k sessions, and the plugins told the client so.
	for _, model := range []string{"waired", "waired/local", "waired/peer-linux-gpu"} {
		t.Run(model+" refuses past 200704", func(t *testing.T) {
			w := send(model)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", w.Code)
			}
			var env openAIErrorEnvelope
			if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
				t.Fatalf("body is not an OpenAI error envelope: %.200s", w.Body.String())
			}
			if !strings.HasSuffix(env.Error.Message, "> 200704 maximum") {
				t.Errorf("message = %q, want it sized by the row's 200704", env.Error.Message)
			}
		})
	}

	for _, model := range []string{"waired[1m]", "qwen3-8b-instruct"} {
		t.Run(model+" is held to the computer's window", func(t *testing.T) {
			// The twin is a 1M session; a catalog id is not a Waired row at all.
			if w := send(model); w.Code != http.StatusOK {
				t.Fatalf("status = %d body=%.200s, want 200", w.Code, w.Body.String())
			}
		})
	}
}

// The Claude listener's own listing states the session each Waired row is,
// not this computer's window — the same number CLAUDE_CODE_MAX_CONTEXT_TOKENS
// gives Claude Code.
func TestAnthropicModelList_StatesEachRowsSession(t *testing.T) {
	h := &HandlerSet{deps: Deps{
		ClaudeModelDirectives: true,
		ListManifests:         asManifestList(nil),
		ContextWindowFor:      func(string) int { return 131072 },
	}}
	byID := map[string]int{}
	for _, m := range h.anthropicModelList() {
		byID[m.ID] = m.MaxInputTokens
	}
	for _, d := range DirectiveModels() {
		if got := byID[d.ID]; got != hostfit.ServingWindow200k {
			t.Errorf("%s max_input_tokens = %d, want 200704", d.ID, got)
		}
	}
	// The "caller named no model" alias is not a row: it still states this
	// computer's window, which `waired claude disable` reads to recognise a
	// value an older build wrote.
	if got := byID[router.DefaultModelAlias]; got != 131072 {
		t.Errorf("%s max_input_tokens = %d, want this computer's 131072", router.DefaultModelAlias, got)
	}
}
