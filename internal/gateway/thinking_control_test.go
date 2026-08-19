package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/internal/runtime"
)

// PRODUCT CONTRACT (waired-agent#856): a request that does not ask for
// extended thinking must not get a reasoning trace, and a request that
// asks for a JSON shape must have that shape enforced on the engine.
//
// The measurement behind it: replaying Claude Code's own session-title
// request — which sends no `thinking` at all — cost 8.03 s on a 24 GB
// discrete GPU and 60.19 s on an 8 GB laptop GPU, against 0.32 s and
// 3.09 s once the engine was told not to think. On a single-slot engine
// that wait is in front of the user's first turn.

func TestThinkingDisabled(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		// Anthropic's default is off, and this is what Claude Code's
		// background calls send.
		{"absent", ``, true},
		{"null", `null`, true},
		{"whitespace", `   `, true},
		{"explicitly disabled", `{"type":"disabled"}`, true},
		{"disabled, odd case", `{"TYPE":"Disabled"}`, true},

		// Ordinary coding turns. These must keep today's behaviour.
		{"enabled", `{"type":"enabled","budget_tokens":4096}`, false},
		{"adaptive", `{"type":"adaptive","display":"omitted"}`, false},

		// A value we could not parse is not a licence to change what
		// the model does.
		{"malformed", `{"type":`, false},
		{"wrong shape", `"disabled"`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ThinkingDisabled(json.RawMessage(tc.raw))
			if got != tc.want {
				t.Errorf("ThinkingDisabled(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestApplyThinkingControl(t *testing.T) {
	cases := []struct {
		name               string
		runtime            string
		wantEffort         string
		wantTemplateKwargs string
	}{
		{"ollama takes reasoning_effort", "ollama", "none", ""},
		{"vllm takes a chat-template argument", "vllm", "", `{"enable_thinking":false}`},
		// A peer's engine kind never reaches the router, so there is
		// nothing to key on and we must not guess.
		{"peer is left alone", "remote:dev_abc", "", ""},
		{"unknown runtime is left alone", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req OpenAIRequest
			ApplyThinkingControl(&req, tc.runtime)
			if req.ReasoningEffort != tc.wantEffort {
				t.Errorf("reasoning_effort = %q, want %q", req.ReasoningEffort, tc.wantEffort)
			}
			if string(req.ChatTemplateKwargs) != tc.wantTemplateKwargs {
				t.Errorf("chat_template_kwargs = %q, want %q",
					req.ChatTemplateKwargs, tc.wantTemplateKwargs)
			}
		})
	}
}

func TestApplyThinkingControl_NilRequest(t *testing.T) {
	ApplyThinkingControl(nil, "ollama") // must not panic
}

func TestAnthropicToOpenAI_OutputConfigFormat(t *testing.T) {
	cases := []struct {
		name         string
		outputConfig string
		want         string
	}{
		{
			name:         "json_schema is enforced on the engine",
			outputConfig: `{"effort":"high","format":{"type":"json_schema","schema":{"type":"object","properties":{"title":{"type":"string"}}}}}`,
			want:         `{"type":"json_schema","json_schema":{"name":"response","schema":{"type":"object","properties":{"title":{"type":"string"}}}}}`,
		},
		{
			name:         "a supplied name is carried through",
			outputConfig: `{"format":{"type":"json_schema","name":"session_title","schema":{"type":"object"}}}`,
			want:         `{"type":"json_schema","json_schema":{"name":"session_title","schema":{"type":"object"}}}`,
		},
		// effort is a hint about how hard to work, not a switch for the
		// reasoning trace; translating it would conflate the two.
		{"effort alone asks for no shape", `{"effort":"xhigh"}`, ""},
		{"absent", ``, ""},
		{"null", `null`, ""},
		{"a format we do not translate", `{"format":{"type":"text"}}`, ""},
		{"json_schema without a schema", `{"format":{"type":"json_schema"}}`, ""},
		{"malformed", `{"format":`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := AnthropicRequest{
				Model:        "waired/default",
				MaxTokens:    64,
				Messages:     []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
				OutputConfig: json.RawMessage(tc.outputConfig),
			}
			out, err := AnthropicToOpenAI(req)
			if err != nil {
				t.Fatalf("convert: %v", err)
			}
			if string(out.ResponseFormat) != tc.want {
				t.Errorf("response_format = %q, want %q", out.ResponseFormat, tc.want)
			}
		})
	}
}

// The serialised body is what the engine's prompt cache keys on: one
// changed byte near the front forfeits the whole prefix and costs a full
// re-prefill of the conversation (measured at 10 s on a 24 GB discrete
// GPU, 41 s on an Apple-silicon laptop, for a 30k-token prompt). A
// request that asks for none of the new fields must therefore marshal
// exactly as it did before they existed.
func TestAnthropicToOpenAI_BodyUnchangedWhenNothingIsAsked(t *testing.T) {
	req := AnthropicRequest{
		Model:     "waired/default",
		MaxTokens: 64,
		Messages:  []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	out, err := AnthropicToOpenAI(req)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"model":"waired/default","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	if string(encoded) != want {
		t.Errorf("body drifted.\n got: %s\nwant: %s", encoded, want)
	}
}

// namedFakeAdapter is fakeAdapter with the engine name under test, so a
// vLLM selection can reach a fake engine the same way an ollama one
// does.
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

func gatewayWithRuntimes(t *testing.T, sel SelectorIface, adapterURL string, names ...string) *Server {
	t.Helper()
	reg := runtime.NewRegistry()
	for _, n := range names {
		reg.Register(namedFakeAdapter{name: n, baseURL: adapterURL})
	}
	return NewServer(ServerConfig{}, Deps{
		Selector:       sel,
		Runtimes:       reg,
		ListManifests:  asManifestList(nil),
		HTTPClient:     http.DefaultClient,
		AllowOpenAI:    true,
		AllowAnthropic: true,
	})
}

func TestAnthropicMessages_ThinkingControlReachesTheEngine(t *testing.T) {
	cases := []struct {
		name     string
		runtime  string
		thinking string
		wantIn   []string
		wantOut  []string
	}{
		{
			// Claude Code's session-title call: no thinking config at all.
			name:    "no thinking config, ollama",
			runtime: "ollama",
			wantIn:  []string{`"reasoning_effort":"none"`},
			wantOut: []string{`chat_template_kwargs`},
		},
		{
			name:     "explicitly disabled, vllm",
			runtime:  "vllm",
			thinking: `,"thinking":{"type":"disabled"}`,
			wantIn:   []string{`"chat_template_kwargs":{"enable_thinking":false}`},
			wantOut:  []string{`reasoning_effort`},
		},
		{
			// An ordinary coding turn. Nothing may change for it.
			name:     "adaptive keeps today's behaviour",
			runtime:  "ollama",
			thinking: `,"thinking":{"type":"adaptive","display":"omitted"}`,
			wantOut:  []string{`reasoning_effort`, `chat_template_kwargs`},
		},
		// The peer case is unit-tested in TestApplyThinkingControl
		// instead: a remote selection needs a peer adapter factory this
		// listener deliberately does not have, so it never reaches an
		// engine here.
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var captured string
			upstream := fakeOllamaForAnthropic(t, &captured)
			defer upstream.Close()

			sel := &fakeSelector{sel: router.Selection{
				Runtime:     tc.runtime,
				EngineModel: "qwen3:8b-q4_K_M",
				ModelID:     "qwen3-8b-instruct",
			}}
			gw := gatewayWithRuntimes(t, sel, upstream.URL, tc.runtime)

			body := `{"model":"waired/default","max_tokens":64,` +
				`"messages":[{"role":"user","content":"hi"}]` + tc.thinking + `}`
			r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages",
				bytes.NewBufferString(body))
			r.RemoteAddr = "127.0.0.1:1"
			w := httptest.NewRecorder()
			gw.Handler().ServeHTTP(w, r)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
			}
			for _, want := range tc.wantIn {
				if !strings.Contains(captured, want) {
					t.Errorf("engine did not see %s; captured = %s", want, captured)
				}
			}
			for _, unwanted := range tc.wantOut {
				if strings.Contains(captured, unwanted) {
					t.Errorf("engine unexpectedly saw %s; captured = %s", unwanted, captured)
				}
			}
		})
	}
}

func TestAnthropicMessages_OutputConfigReachesTheEngine(t *testing.T) {
	var captured string
	upstream := fakeOllamaForAnthropic(t, &captured)
	defer upstream.Close()

	sel := &fakeSelector{sel: router.Selection{
		Runtime: "ollama", EngineModel: "qwen3:8b-q4_K_M", ModelID: "qwen3-8b-instruct",
	}}
	gw := anthropicGatewayUnderTest(t, sel, upstream.URL)

	body := `{"model":"waired/default","max_tokens":64,` +
		`"messages":[{"role":"user","content":"name this"}],` +
		`"output_config":{"effort":"high","format":{"type":"json_schema",` +
		`"schema":{"type":"object","properties":{"title":{"type":"string"}}}}}}`
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages",
		bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(captured, `"response_format":{"type":"json_schema"`) {
		t.Errorf("engine was not told the output shape; captured = %s", captured)
	}
}
