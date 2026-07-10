package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/internal/runtime"
)

func anthropicGatewayUnderTest(t *testing.T, sel SelectorIface, adapterURL string) *Server {
	t.Helper()
	reg := runtime.NewRegistry()
	reg.Register(fakeAdapter{baseURL: adapterURL})
	return NewServer(ServerConfig{}, Deps{
		Selector:       sel,
		Runtimes:       reg,
		ListManifests:  asManifestList(nil),
		HTTPClient:     http.DefaultClient,
		AllowOpenAI:    true,
		AllowAnthropic: true,
	})
}

// fakeOllamaForAnthropic mirrors fakeOllama but serves both
// streaming and non-streaming chat completions, capturing the
// rewritten request body for assertions.
func fakeOllamaForAnthropic(t *testing.T, captured *string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if captured != nil {
			*captured = string(body)
		}
		var probe struct {
			Stream        bool `json:"stream"`
			StreamOptions *struct {
				IncludeUsage bool `json:"include_usage"`
			} `json:"stream_options"`
		}
		_ = json.Unmarshal(body, &probe)
		if probe.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			f, _ := w.(http.Flusher)
			// Real Ollama only emits the trailing usage chunk when the
			// request opted in via stream_options.include_usage; the
			// finish chunk itself carries no usage. Mirror that so the
			// test actually exercises the opt-in (bug #644 part 3).
			chunks := []string{
				`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
				`data: {"choices":[{"index":0,"delta":{"content":"Hello"}}]}`,
				`data: {"choices":[{"index":0,"delta":{"content":"!"},"finish_reason":null}]}`,
				`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			}
			if probe.StreamOptions != nil && probe.StreamOptions.IncludeUsage {
				chunks = append(chunks,
					`data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
			}
			chunks = append(chunks, `data: [DONE]`)
			for _, c := range chunks {
				_, _ = w.Write([]byte(c + "\n\n"))
				if f != nil {
					f.Flush()
				}
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-99",
			"choices":[{"index":0,"message":{"role":"assistant","content":"Hi there"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":5,"completion_tokens":3}
		}`))
	})
	return httptest.NewServer(mux)
}

func TestAnthropicMessages_NonStreamHappyPath(t *testing.T) {
	var captured string
	upstream := fakeOllamaForAnthropic(t, &captured)
	defer upstream.Close()

	sel := &fakeSelector{sel: router.Selection{
		Runtime: "ollama", EngineModel: "qwen3:8b-q4_K_M", ModelID: "qwen3-8b-instruct",
	}}
	gw := anthropicGatewayUnderTest(t, sel, upstream.URL)

	body := `{"model":"waired/default","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp AnthropicResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Type != "message" || resp.Role != "assistant" {
		t.Errorf("envelope = %+v", resp)
	}
	if resp.Model != "waired/default" {
		t.Errorf("model echoed back = %q, want waired/default", resp.Model)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "Hi there" {
		t.Errorf("content = %+v", resp.Content)
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q", resp.StopReason)
	}
	if !strings.Contains(captured, `"qwen3:8b-q4_K_M"`) {
		t.Errorf("upstream did not see engine model; captured = %s", captured)
	}
}

// TestAnthropicMessages_ClaudeCodeShape posts a request shaped the way
// Claude Code sends them — `system` as an array of text blocks with
// cache_control, tools and message content also carrying cache_control.
// This used to 400 with feature "system_blocks"; it must now succeed and
// forward the flattened system prompt to the engine.
func TestAnthropicMessages_ClaudeCodeShape(t *testing.T) {
	var captured string
	upstream := fakeOllamaForAnthropic(t, &captured)
	defer upstream.Close()

	sel := &fakeSelector{sel: router.Selection{
		Runtime: "ollama", EngineModel: "qwen3:8b-q4_K_M", ModelID: "qwen3-8b-instruct",
	}}
	gw := anthropicGatewayUnderTest(t, sel, upstream.URL)

	body := `{
		"model":"waired/default","max_tokens":64,
		"system":[
			{"type":"text","text":"You are Claude Code."},
			{"type":"text","text":"<ctx>","cache_control":{"type":"ephemeral"}}
		],
		"tools":[{"name":"Read","description":"read a file","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}],
		"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]
	}`
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(captured, `"role":"system"`) || !strings.Contains(captured, "You are Claude Code.") {
		t.Errorf("flattened system not forwarded to engine; captured = %s", captured)
	}
}

func TestAnthropicMessages_RequiresMaxTokens(t *testing.T) {
	gw := anthropicGatewayUnderTest(t, &fakeSelector{}, "http://unused")
	body := `{"model":"waired/default","messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "max_tokens") {
		t.Errorf("error body should mention max_tokens: %s", w.Body.String())
	}
}

func TestAnthropicMessages_StreamProducesAnthropicEvents(t *testing.T) {
	upstream := fakeOllamaForAnthropic(t, nil)
	defer upstream.Close()

	sel := &fakeSelector{sel: router.Selection{Runtime: "ollama", EngineModel: "qwen3:8b-q4_K_M"}}
	gw := anthropicGatewayUnderTest(t, sel, upstream.URL)

	body := `{"model":"waired/default","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q", ct)
	}
	body2 := w.Body.String()
	for _, want := range []string{
		"event: message_start",
		"event: content_block_start",
		"event: content_block_delta",
		"event: content_block_stop",
		"event: message_delta",
		"event: message_stop",
		`"text":"Hello"`,
		`"text":"!"`,
		`"stop_reason":"end_turn"`,
		`"output_tokens":2`, // usage propagated from the trailing chunk (#644)
	} {
		if !strings.Contains(body2, want) {
			t.Errorf("stream missing %q\nfull body:\n%s", want, body2)
		}
	}
}

// fakeOllamaReasoning is an upstream that returns a thinking model's
// reasoning: message.reasoning (non-stream) and delta.reasoning before
// the content deltas (stream). It also honours stream_options.include_usage.
func fakeOllamaReasoning(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Stream        bool `json:"stream"`
			StreamOptions *struct {
				IncludeUsage bool `json:"include_usage"`
			} `json:"stream_options"`
		}
		_ = json.Unmarshal(body, &probe)
		if probe.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			f, _ := w.(http.Flusher)
			chunks := []string{
				`data: {"choices":[{"index":0,"delta":{"role":"assistant","reasoning":"Think: "}}]}`,
				`data: {"choices":[{"index":0,"delta":{"reasoning":"17*23=391"}}]}`,
				`data: {"choices":[{"index":0,"delta":{"content":"The answer is "}}]}`,
				`data: {"choices":[{"index":0,"delta":{"content":"391."},"finish_reason":null}]}`,
				`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			}
			if probe.StreamOptions != nil && probe.StreamOptions.IncludeUsage {
				chunks = append(chunks, `data: {"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":7}}`)
			}
			chunks = append(chunks, `data: [DONE]`)
			for _, c := range chunks {
				_, _ = w.Write([]byte(c + "\n\n"))
				if f != nil {
					f.Flush()
				}
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-r",
			"choices":[{"index":0,"message":{"role":"assistant","reasoning":"17*23=391","content":"The answer is 391."},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":9,"completion_tokens":7}
		}`))
	})
	return httptest.NewServer(mux)
}

func TestAnthropicMessages_NonStreamReasoning(t *testing.T) {
	upstream := fakeOllamaReasoning(t)
	defer upstream.Close()

	sel := &fakeSelector{sel: router.Selection{Runtime: "ollama", EngineModel: "qwen3:8b-q4_K_M"}}
	gw := anthropicGatewayUnderTest(t, sel, upstream.URL)

	body := `{"model":"waired/default","max_tokens":64,"messages":[{"role":"user","content":"17*23?"}]}`
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp AnthropicResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Content) != 2 || resp.Content[0].Type != "thinking" || resp.Content[1].Type != "text" {
		t.Fatalf("content = %+v, want [thinking, text]", resp.Content)
	}
	if resp.Content[0].Thinking != "17*23=391" || resp.Content[1].Text != "The answer is 391." {
		t.Errorf("blocks = %+v", resp.Content)
	}
}

func TestAnthropicMessages_StreamReasoningProducesThinking(t *testing.T) {
	var captured string
	upstream := fakeOllamaReasoning(t)
	defer upstream.Close()
	// Capture the outbound request to assert stream_options is sent.
	base := upstream.URL
	capturing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		captured = string(b)
		req, _ := http.NewRequest(http.MethodPost, base+r.URL.Path, bytes.NewReader(b))
		req.Header = r.Header.Clone()
		up, err := http.DefaultClient.Do(req)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer up.Body.Close()
		for k, vs := range up.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(up.StatusCode)
		_, _ = io.Copy(w, up.Body)
	}))
	defer capturing.Close()

	sel := &fakeSelector{sel: router.Selection{Runtime: "ollama", EngineModel: "qwen3:8b-q4_K_M"}}
	gw := anthropicGatewayUnderTest(t, sel, capturing.URL)

	body := `{"model":"waired/default","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"17*23?"}]}`
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	// NB: emitted JSON uses map[string]any, so json.Marshal sorts keys
	// alphabetically — assertions must match that ordering.
	out := w.Body.String()
	for _, want := range []string{
		`"type":"thinking"`,      // thinking content_block_start
		`"thinking":"Think: "`,   // first thinking_delta
		`"thinking":"17*23=391"`, // second thinking_delta
		`"type":"thinking_delta"`,
		`"index":0,"type":"content_block_start"`, // thinking claims index 0
		`"index":1,"type":"content_block_start"`, // text shifted to index 1
		`"text":"The answer is "`,
		`"output_tokens":7`, // usage propagated (#644 part 3)
		`"stop_reason":"end_turn"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stream missing %q\nfull body:\n%s", want, out)
		}
	}
	// Thinking must be closed before text opens (index 0 stop precedes
	// index 1 start).
	if i, j := strings.Index(out, `"index":0,"type":"content_block_stop"`),
		strings.Index(out, `"index":1,"type":"content_block_start"`); i < 0 || j < 0 || i > j {
		t.Errorf("thinking(0) stop must precede text(1) start; got stop@%d start@%d", i, j)
	}
	// The outbound request must opt in to streaming usage.
	if !strings.Contains(captured, `"stream_options":{"include_usage":true}`) {
		t.Errorf("outbound stream request missing stream_options; captured = %s", captured)
	}
}

func TestAnthropicMessages_StreamEmptyLengthGetsNote(t *testing.T) {
	// A max_tokens truncation that streamed no thinking/text/tool must
	// still yield a visible turn, not an empty stream that stalls the loop.
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f, _ := w.(http.Flusher)
		for _, c := range []string{
			`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`,
			`data: [DONE]`,
		} {
			_, _ = w.Write([]byte(c + "\n\n"))
			if f != nil {
				f.Flush()
			}
		}
	})
	upstream := httptest.NewServer(mux)
	defer upstream.Close()

	sel := &fakeSelector{sel: router.Selection{Runtime: "ollama", EngineModel: "qwen3:8b-q4_K_M"}}
	gw := anthropicGatewayUnderTest(t, sel, upstream.URL)

	body := `{"model":"waired/default","max_tokens":1,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	out := w.Body.String()
	if !strings.Contains(out, truncationNote) {
		t.Errorf("stream missing truncation note\nfull body:\n%s", out)
	}
	if !strings.Contains(out, `"stop_reason":"max_tokens"`) {
		t.Errorf("expected stop_reason max_tokens\nfull body:\n%s", out)
	}
}

func TestAnthropicMessages_NonStreamOmitsStreamOptions(t *testing.T) {
	var captured string
	upstream := fakeOllamaForAnthropic(t, &captured)
	defer upstream.Close()

	sel := &fakeSelector{sel: router.Selection{Runtime: "ollama", EngineModel: "qwen3:8b-q4_K_M"}}
	gw := anthropicGatewayUnderTest(t, sel, upstream.URL)

	body := `{"model":"waired/default","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if strings.Contains(captured, "stream_options") {
		t.Errorf("non-stream request must not carry stream_options; captured = %s", captured)
	}
}

func TestAnthropicMessages_UnknownModel404(t *testing.T) {
	sel := &fakeSelector{err: wrap(router.ErrModelNotFound, "alias x not found")}
	gw := anthropicGatewayUnderTest(t, sel, "http://unused")
	body := `{"model":"x","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestAnthropicMessages_ModelNotReadyMaps503Overloaded(t *testing.T) {
	sel := &fakeSelector{err: wrap(router.ErrModelNotReady, "downloading")}
	gw := anthropicGatewayUnderTest(t, sel, "http://unused")
	body := `{"model":"waired/default","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
	var env anthropicErrorEnvelope
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if env.Error.Type != "overloaded_error" {
		t.Errorf("error.type = %q, want overloaded_error", env.Error.Type)
	}
}

func TestAnthropicMessages_VisionRejected400(t *testing.T) {
	gw := anthropicGatewayUnderTest(t, &fakeSelector{}, "http://unused")
	body := `{"model":"waired/default","max_tokens":16,"messages":[
		{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"}}]}
	]}`
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestAnthropicCountTokens(t *testing.T) {
	gw := anthropicGatewayUnderTest(t, &fakeSelector{}, "http://unused")
	body := `{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"a fairly long sentence to count tokens for"}]}`
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages/count_tokens", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("X-Waired-Token-Count") != "approximate" {
		t.Errorf("missing X-Waired-Token-Count header; headers=%v", w.Header())
	}
	var got map[string]int
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["input_tokens"] <= 0 {
		t.Errorf("input_tokens = %d, want > 0", got["input_tokens"])
	}
}

func TestAnthropicDisabled404(t *testing.T) {
	gw := NewServer(ServerConfig{}, Deps{
		Selector:       &fakeSelector{},
		Runtimes:       runtime.NewRegistry(),
		ListManifests:  asManifestList(nil),
		HTTPClient:     http.DefaultClient,
		AllowOpenAI:    true,
		AllowAnthropic: false,
	})
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewBufferString("{}"))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when AllowAnthropic=false", w.Code)
	}
}

// --- Claude-intercept model mapping (#600) ---------------------------------
//
// The Claude surface receives Anthropic model ids (claude-fable-5[1m],
// claude-opus-4-8, ...) that never exist in the catalog. Deps.ResolveUnknownModel
// maps a ErrModelNotFound miss to the device-active model; these tests pin the
// retry, the pass-through of resolvable ids, the branded no-model error, and
// the observability surfaces.

// modelAwareSelector resolves only the ids in known — everything else fails
// with ErrModelNotFound, mirroring the catalog's exact-match alias lookup.
type modelAwareSelector struct {
	known map[string]router.Selection
	got   []router.Request
}

func (m *modelAwareSelector) Select(_ context.Context, req router.Request) (router.Selection, error) {
	m.got = append(m.got, req)
	if sel, ok := m.known[req.Model]; ok {
		return sel, nil
	}
	return router.Selection{}, wrap(router.ErrModelNotFound, "alias "+req.Model+" not found")
}

func (m *modelAwareSelector) SelectK(ctx context.Context, req router.Request, _ int) ([]router.Candidate, error) {
	sel, err := m.Select(ctx, req)
	if err != nil {
		return nil, err
	}
	return []router.Candidate{router.NewLocalCandidate(sel)}, nil
}

func anthropicGatewayWithResolver(t *testing.T, sel SelectorIface, adapterURL string, resolve func(string) (string, bool)) *Server {
	t.Helper()
	reg := runtime.NewRegistry()
	reg.Register(fakeAdapter{baseURL: adapterURL})
	return NewServer(ServerConfig{}, Deps{
		Selector:            sel,
		Runtimes:            reg,
		ListManifests:       asManifestList(nil),
		HTTPClient:          http.DefaultClient,
		AllowOpenAI:         true,
		AllowAnthropic:      true,
		ResolveUnknownModel: func(requested, _ string) (string, bool) { return resolve(requested) },
	})
}

func TestAnthropicMessages_UnknownModelMappedToLocal(t *testing.T) {
	var captured string
	upstream := fakeOllamaForAnthropic(t, &captured)
	defer upstream.Close()

	sel := &modelAwareSelector{known: map[string]router.Selection{
		"qwen3-8b-instruct": {Runtime: "ollama", EngineModel: "qwen3:8b-q4_K_M", ModelID: "qwen3-8b-instruct"},
	}}
	calls := 0
	gw := anthropicGatewayWithResolver(t, sel, upstream.URL, func(requested string) (string, bool) {
		calls++
		if requested != "claude-fable-5[1m]" {
			t.Errorf("resolver got %q, want the original id", requested)
		}
		return "qwen3-8b-instruct", true
	})

	body := `{"model":"claude-fable-5[1m]","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if calls != 1 {
		t.Errorf("resolver calls = %d, want 1", calls)
	}
	if len(sel.got) != 2 || sel.got[0].Model != "claude-fable-5[1m]" || sel.got[1].Model != "qwen3-8b-instruct" {
		t.Errorf("selector saw %+v, want original then mapped", sel.got)
	}
	var resp AnthropicResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Model != "claude-fable-5[1m]" {
		t.Errorf("response model = %q, want the original id echoed", resp.Model)
	}
	if !strings.Contains(captured, `"qwen3:8b-q4_K_M"`) {
		t.Errorf("engine did not receive the mapped EngineModel; captured = %s", captured)
	}
	if got := w.Header().Get(HeaderLocalModel); got != "qwen3-8b-instruct" {
		t.Errorf("%s = %q, want the mapped catalog id", HeaderLocalModel, got)
	}
}

func TestAnthropicMessages_UnknownModelMappedStream(t *testing.T) {
	var captured string
	upstream := fakeOllamaForAnthropic(t, &captured)
	defer upstream.Close()

	sel := &modelAwareSelector{known: map[string]router.Selection{
		"qwen3-8b-instruct": {Runtime: "ollama", EngineModel: "qwen3:8b-q4_K_M", ModelID: "qwen3-8b-instruct"},
	}}
	gw := anthropicGatewayWithResolver(t, sel, upstream.URL, func(string) (string, bool) {
		return "qwen3-8b-instruct", true
	})

	body := `{"model":"claude-fable-5[1m]","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	sse := w.Body.String()
	if !strings.Contains(sse, "message_start") || !strings.Contains(sse, `"claude-fable-5[1m]"`) {
		t.Errorf("message_start must echo the original model id; body:\n%s", sse)
	}
	if got := w.Header().Get(HeaderLocalModel); got != "qwen3-8b-instruct" {
		t.Errorf("%s = %q, want the mapped catalog id", HeaderLocalModel, got)
	}
}

func TestAnthropicMessages_KnownModelBypassesResolver(t *testing.T) {
	var captured string
	upstream := fakeOllamaForAnthropic(t, &captured)
	defer upstream.Close()

	sel := &modelAwareSelector{known: map[string]router.Selection{
		"waired/default": {Runtime: "ollama", EngineModel: "qwen3:8b-q4_K_M", ModelID: "qwen3-8b-instruct"},
	}}
	calls := 0
	gw := anthropicGatewayWithResolver(t, sel, upstream.URL, func(string) (string, bool) {
		calls++
		return "qwen3-8b-instruct", true
	})

	body := `{"model":"waired/default","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if calls != 0 {
		t.Errorf("resolver calls = %d, want 0 for a resolvable id", calls)
	}
	if got := w.Header().Get(HeaderLocalModel); got != "" {
		t.Errorf("%s = %q, want unset when no mapping happened", HeaderLocalModel, got)
	}
}

func TestAnthropicMessages_NoActiveModel503(t *testing.T) {
	sel := &modelAwareSelector{known: nil}
	gw := anthropicGatewayWithResolver(t, sel, "http://unused", func(string) (string, bool) {
		return "", false
	})

	body := `{"model":"claude-fable-5[1m]","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (not a 404 Claude Code renders as model-not-found)", w.Code)
	}
	var env anthropicErrorEnvelope
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if env.Error.Type != "waired_no_local_model" {
		t.Errorf("error.type = %q, want waired_no_local_model", env.Error.Type)
	}
	if !strings.Contains(env.Error.Message, "claude-fable-5[1m]") {
		t.Errorf("error message should name the requested model; got %q", env.Error.Message)
	}
	if got := w.Header().Get(HeaderLocalError); got != "no_model" {
		t.Errorf("%s = %q, want no_model (intercept prefixes local_)", HeaderLocalError, got)
	}
}

func TestAnthropicMessages_MappedModelNotReady503(t *testing.T) {
	// First selection misses (unknown id), the mapped retry hits a model
	// that exists but is still downloading: the precise not-ready error
	// must win over any blanket no-model shape.
	sel := &scriptedSelector{errs: []error{
		wrap(router.ErrModelNotFound, "alias claude-fable-5[1m] not found"),
		wrap(router.ErrModelNotReady, "downloading"),
	}}
	gw := anthropicGatewayWithResolver(t, sel, "http://unused", func(string) (string, bool) {
		return "qwen3-8b-instruct", true
	})

	body := `{"model":"claude-fable-5[1m]","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
	var env anthropicErrorEnvelope
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if env.Error.Type != "overloaded_error" {
		t.Errorf("error.type = %q, want overloaded_error from ErrModelNotReady", env.Error.Type)
	}
}

// scriptedSelector fails each call with the next scripted error.
type scriptedSelector struct {
	errs []error
	call int
}

func (s *scriptedSelector) Select(_ context.Context, _ router.Request) (router.Selection, error) {
	err := s.errs[s.call%len(s.errs)]
	s.call++
	return router.Selection{}, err
}

func (s *scriptedSelector) SelectK(ctx context.Context, req router.Request, _ int) ([]router.Candidate, error) {
	_, err := s.Select(ctx, req)
	return nil, err
}

func TestAnthropicMessages_RecorderSeesMappedModel(t *testing.T) {
	var captured string
	upstream := fakeOllamaForAnthropic(t, &captured)
	defer upstream.Close()

	rec := &captureRecorder{}
	reg := runtime.NewRegistry()
	reg.Register(fakeAdapter{baseURL: upstream.URL})
	sel := &modelAwareSelector{known: map[string]router.Selection{
		"qwen3-8b-instruct": {Runtime: "ollama", EngineModel: "qwen3:8b-q4_K_M", ModelID: "qwen3-8b-instruct"},
	}}
	gw := NewServer(ServerConfig{}, Deps{
		Selector:            sel,
		Runtimes:            reg,
		ListManifests:       asManifestList(nil),
		HTTPClient:          http.DefaultClient,
		AllowAnthropic:      true,
		Recorder:            rec,
		ResolveUnknownModel: func(string, string) (string, bool) { return "qwen3-8b-instruct", true },
	})

	body := `{"model":"claude-fable-5[1m]","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	reqs := rec.requestsSnapshot()
	if len(reqs) != 1 {
		t.Fatalf("recorded requests = %d, want 1", len(reqs))
	}
	if reqs[0].Model != "qwen3-8b-instruct" {
		t.Errorf("recorded model = %q, want the mapped catalog id", reqs[0].Model)
	}
}
