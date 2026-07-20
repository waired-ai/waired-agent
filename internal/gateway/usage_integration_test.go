package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/internal/runtime"
)

// Handler-level metering: token counts must reach both the local
// telemetry event and the Deps.OnUsage sink, on every surface and both
// stream modes (public share spec §12).

type captureSink struct {
	mu      sync.Mutex
	samples []UsageSample
}

func (c *captureSink) fn() func(context.Context, UsageSample) {
	return func(_ context.Context, s UsageSample) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.samples = append(c.samples, s)
	}
}

func (c *captureSink) snapshot() []UsageSample {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]UsageSample(nil), c.samples...)
}

// meteringEngine mimics an engine that reports usage, in either mode.
func meteringEngine(t *testing.T, capture *string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if capture != nil {
			*capture = string(body)
		}
		var probe struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &probe)
		if probe.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			f, _ := w.(http.Flusher)
			for _, chunk := range []string{
				`data: {"choices":[{"delta":{"content":"hi"}}]}`,
				`data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7}}`,
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
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"hi"}}],` +
			`"usage":{"prompt_tokens":11,"completion_tokens":7}}`))
	})
	return httptest.NewServer(mux)
}

func newMeteringGateway(t *testing.T, upstream string, rec Recorder, sink func(context.Context, UsageSample)) *Server {
	t.Helper()
	reg := runtime.NewRegistry()
	reg.Register(fakeAdapter{baseURL: upstream})
	sel := &fakeSelector{sel: router.Selection{
		EndpointID:    "ep_local_ollama_qwen3-8b-instruct",
		ModelID:       "qwen3-8b-instruct",
		VariantID:     "q4-gguf",
		Runtime:       "ollama",
		EngineModel:   "qwen3:8b-q4_K_M",
		ExecutionMode: "local",
	}}
	return NewServer(ServerConfig{Addr: "127.0.0.1:0"}, Deps{
		Selector:       sel,
		Runtimes:       reg,
		ListManifests:  asManifestList([]catalog.Manifest{qwenManifest()}),
		HTTPClient:     http.DefaultClient,
		AllowOpenAI:    true,
		AllowAnthropic: true,
		Recorder:       rec,
		OnUsage:        sink,
	})
}

func postJSON(t *testing.T, gw *Server, path string, payload map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(payload)
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	return w
}

func TestGateway_RecordsTokens(t *testing.T) {
	for _, tc := range []struct {
		name    string
		path    string
		payload map[string]any
	}{
		{"openai non-stream", "/v1/chat/completions", map[string]any{
			"model": "waired/default", "messages": []map[string]string{{"role": "user", "content": "hi"}}}},
		{"openai stream", "/v1/chat/completions", map[string]any{
			"model": "waired/default", "stream": true,
			"messages": []map[string]string{{"role": "user", "content": "hi"}}}},
		{"anthropic non-stream", "/anthropic/v1/messages", map[string]any{
			"model": "waired/default", "max_tokens": 16,
			"messages": []map[string]string{{"role": "user", "content": "hi"}}}},
		{"anthropic stream", "/anthropic/v1/messages", map[string]any{
			"model": "waired/default", "max_tokens": 16, "stream": true,
			"messages": []map[string]string{{"role": "user", "content": "hi"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := meteringEngine(t, nil)
			defer upstream.Close()
			rec := &captureRecorder{}
			sink := &captureSink{}
			gw := newMeteringGateway(t, upstream.URL, rec, sink.fn())

			if w := postJSON(t, gw, tc.path, tc.payload); w.Code != http.StatusOK {
				t.Fatalf("status = %d; body: %s", w.Code, w.Body.String())
			}

			events := rec.requestsSnapshot()
			if len(events) != 1 {
				t.Fatalf("request events = %d, want 1", len(events))
			}
			if events[0].InputTokens != 11 || events[0].OutputTokens != 7 {
				t.Errorf("local telemetry tokens = %d/%d, want 11/7",
					events[0].InputTokens, events[0].OutputTokens)
			}

			samples := sink.snapshot()
			if len(samples) != 1 {
				t.Fatalf("usage samples = %d, want 1", len(samples))
			}
			s := samples[0]
			if s.InputTokens != 11 || s.OutputTokens != 7 {
				t.Errorf("sample tokens = %d/%d, want 11/7", s.InputTokens, s.OutputTokens)
			}
			// The control plane resolves a quality tier from the ENGINE
			// name, so the sample must carry it, not the catalog id.
			if s.EngineModel != "qwen3:8b-q4_K_M" {
				t.Errorf("EngineModel = %q", s.EngineModel)
			}
			if s.ModelID != "qwen3-8b-instruct" {
				t.Errorf("ModelID = %q", s.ModelID)
			}
			if s.Status != http.StatusOK {
				t.Errorf("Status = %d", s.Status)
			}
		})
	}
}

// Local telemetry is the §12 side benefit and must not depend on any
// Public Share wiring: with no sink at all, the tokens still land in the
// event.
func TestGateway_TokensRecordedWithoutSink(t *testing.T) {
	upstream := meteringEngine(t, nil)
	defer upstream.Close()
	rec := &captureRecorder{}
	gw := newMeteringGateway(t, upstream.URL, rec, nil)

	if w := postJSON(t, gw, "/v1/chat/completions", map[string]any{
		"model": "waired/default", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	}); w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	events := rec.requestsSnapshot()
	if len(events) != 1 || events[0].InputTokens != 11 {
		t.Fatalf("tokens missing from local telemetry: %+v", events)
	}
}

// A request that never reached an engine must not be metered — counting
// it would inflate a ledger the user sees.
func TestGateway_FailedRequestNotMetered(t *testing.T) {
	sink := &captureSink{}
	reg := runtime.NewRegistry() // no adapter registered
	gw := NewServer(ServerConfig{Addr: "127.0.0.1:0"}, Deps{
		Selector: &fakeSelector{sel: router.Selection{
			ModelID: "qwen3-8b-instruct", Runtime: "ollama",
			EngineModel: "qwen3:8b-q4_K_M", ExecutionMode: "local",
		}},
		Runtimes:      reg,
		ListManifests: asManifestList([]catalog.Manifest{qwenManifest()}),
		HTTPClient:    http.DefaultClient,
		AllowOpenAI:   true,
		OnUsage:       sink.fn(),
	})

	w := postJSON(t, gw, "/v1/chat/completions", map[string]any{
		"model": "waired/default", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if w.Code < 400 {
		t.Fatalf("expected a failure status, got %d", w.Code)
	}
	if got := sink.snapshot(); len(got) != 0 {
		t.Fatalf("metered a request that never reached an engine: %+v", got)
	}
}

// §15-10: prompt content must not appear in telemetry, anywhere. The
// canary rides in the message body and in the response; if any capture
// path ever widens to include content, this fails.
func TestNoPromptContentInTelemetry(t *testing.T) {
	const canary = "waired-canary-8e1d4c"

	for _, tc := range []struct {
		name    string
		path    string
		payload map[string]any
	}{
		{"openai", "/v1/chat/completions", map[string]any{
			"model": "waired/default", "messages": []map[string]string{{"role": "user", "content": canary}}}},
		{"openai stream", "/v1/chat/completions", map[string]any{
			"model": "waired/default", "stream": true,
			"messages": []map[string]string{{"role": "user", "content": canary}}}},
		{"anthropic", "/anthropic/v1/messages", map[string]any{
			"model": "waired/default", "max_tokens": 16,
			"messages": []map[string]string{{"role": "user", "content": canary}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := meteringEngine(t, nil)
			defer upstream.Close()
			rec := &captureRecorder{}
			sink := &captureSink{}
			gw := newMeteringGateway(t, upstream.URL, rec, sink.fn())

			if w := postJSON(t, gw, tc.path, tc.payload); w.Code != http.StatusOK {
				t.Fatalf("status = %d; body: %s", w.Code, w.Body.String())
			}

			for _, ev := range rec.requestsSnapshot() {
				blob, _ := json.Marshal(ev)
				if bytes.Contains(blob, []byte(canary)) {
					t.Errorf("RequestEvent carries prompt content: %s", blob)
				}
			}
			for _, s := range sink.snapshot() {
				blob, _ := json.Marshal(s)
				if bytes.Contains(blob, []byte(canary)) {
					t.Errorf("UsageSample carries prompt content: %s", blob)
				}
				if strings.Contains(s.ModelID+s.EngineModel+s.Class, canary) {
					t.Errorf("UsageSample string field carries prompt content: %+v", s)
				}
			}
		})
	}
}
