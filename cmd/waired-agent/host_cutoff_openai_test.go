package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/proto/hostfit"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// fakeOpenAIEngine streams chat completions the way vLLM does, with a
// controllable pause before the first token and between the rest — the two
// intervals the measurement reads as prefill and decode.
type fakeOpenAIEngine struct {
	prefill  time.Duration
	perToken time.Duration
	tokens   int
	// promptTokens is what the usage block reports. Held separately from
	// the prompt actually sent so a test can stage the engine truncating
	// the prompt, which is what Measured() exists to catch.
	promptTokens int
	status       int

	mu       sync.Mutex
	requests []map[string]any
}

func (f *fakeOpenAIEngine) seen() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.requests...)
}

func (f *fakeOpenAIEngine) server(t *testing.T) *httptest.Server {
	t.Helper()
	// Its own mux, never http.DefaultServeMux: the default is process-wide
	// and shared with every other test in this package.
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		f.mu.Lock()
		f.requests = append(f.requests, req)
		f.mu.Unlock()

		if f.status != 0 && f.status != http.StatusOK {
			w.WriteHeader(f.status)
			_, _ = io.WriteString(w, `{"error":"staged"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flush, _ := w.(http.Flusher)

		want := f.tokens
		if maxTok, ok := req["max_tokens"].(float64); ok && int(maxTok) < want {
			want = int(maxTok)
		}
		time.Sleep(f.prefill)
		for i := 0; i < want; i++ {
			if i > 0 {
				time.Sleep(f.perToken)
			}
			_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"tok\"}}]}\n\n")
			if flush != nil {
				flush.Flush()
			}
		}
		_, _ = fmt.Fprintf(w,
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":%d}}\n\n",
			f.promptTokens, want)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flush != nil {
			flush.Flush()
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// PRODUCT CONTRACT (waired-agent#1298): an engine with no
// prompt_eval_*/eval_* counters is measured on the client's clock, at the
// probe's own depth, and the record says which clock produced it.
func TestMeasureHostCutoffOpenAI_SeparatesPrefillFromDecode(t *testing.T) {
	eng := &fakeOpenAIEngine{
		prefill:      120 * time.Millisecond,
		perToken:     2 * time.Millisecond,
		tokens:       hostfit.HostCutoffCompletionSampleTokens,
		promptTokens: hostfit.HostCutoffProbeDepthTokens,
	}
	srv := eng.server(t)

	m, err := measureHostCutoffOpenAI(context.Background(), openAICutoffDeps{
		BaseURL: srv.URL, Model: "probe", Logger: testLogger(),
		HTTPClient: srv.Client(), Nonce: "unit",
	})
	if err != nil {
		t.Fatalf("measureHostCutoffOpenAI: %v", err)
	}
	if m.Method != signer.BenchmarkMethodOpenAIStreamTTFT {
		t.Errorf("Method = %q, want %q", m.Method, signer.BenchmarkMethodOpenAIStreamTTFT)
	}
	if m.Samples != benchSampleCount {
		t.Errorf("Samples = %d, want %d", m.Samples, benchSampleCount)
	}
	if !m.Probe.Measured() {
		t.Fatalf("probe is not Measured(): prompt_tokens=%d", m.Probe.PromptTokens)
	}
	// Absolute slack, not a ratio of the staged times: the figures are
	// wall-clock on a shared CI runner, and the assertion is that the two
	// phases were told APART, not that either is accurate.
	if m.Probe.PrefillTokps <= 0 || m.Probe.DecodeTokps <= 0 {
		t.Fatalf("prefill=%.0f decode=%.1f, want both positive",
			m.Probe.PrefillTokps, m.Probe.DecodeTokps)
	}
	// 21,000 tokens in ~0.12 s is ~175,000 tok/s; 199 tokens over ~0.4 s
	// is ~500 tok/s. A run that folded the two together — timing the whole
	// request and dividing — could not put them three orders of magnitude
	// apart.
	if m.Probe.PrefillTokps <= m.Probe.DecodeTokps*10 {
		t.Errorf("prefill=%.0f decode=%.1f: the two phases were not told apart",
			m.Probe.PrefillTokps, m.Probe.DecodeTokps)
	}

	// The prompts must not share an opening, or vLLM's prefix cache —
	// pinned on with --enable-prefix-caching for coding agents — answers
	// the repeats at a rate no host can achieve.
	seen := eng.seen()
	if len(seen) != benchSampleCount+1 {
		t.Fatalf("requests = %d, want %d samples plus one calibration", len(seen), benchSampleCount+1)
	}
	openings := map[string]bool{}
	for _, req := range seen {
		msgs, _ := req["messages"].([]any)
		if len(msgs) == 0 {
			t.Fatal("a request carried no messages")
		}
		content, _ := msgs[0].(map[string]any)["content"].(string)
		head := content
		if len(head) > 64 {
			head = head[:64]
		}
		if openings[head] {
			t.Errorf("two requests share their opening %q", head)
		}
		openings[head] = true
	}
	// Without stream_options.include_usage vLLM streams no usage block at
	// all, and both token counts would have to be guessed from the text.
	last := seen[len(seen)-1]
	opts, _ := last["stream_options"].(map[string]any)
	if inc, _ := opts["include_usage"].(bool); !inc {
		t.Errorf("stream_options = %v, want include_usage", last["stream_options"])
	}
}

// A prompt the engine truncated is not a measurement of this host, and
// Measured() is what says so. The probe must return the reading rather
// than pretend, so the caller's own guard can refuse to publish it.
func TestMeasureHostCutoffOpenAI_ReportsATruncatedPrompt(t *testing.T) {
	eng := &fakeOpenAIEngine{
		prefill:      10 * time.Millisecond,
		perToken:     time.Millisecond,
		tokens:       hostfit.HostCutoffCompletionSampleTokens,
		promptTokens: hostfit.HostCutoffProbeDepthTokens / 4, // far under the tolerance
	}
	srv := eng.server(t)

	m, err := measureHostCutoffOpenAI(context.Background(), openAICutoffDeps{
		BaseURL: srv.URL, Model: "probe", Logger: testLogger(),
		HTTPClient: srv.Client(), Nonce: "unit",
	})
	if err != nil {
		t.Fatalf("measureHostCutoffOpenAI: %v", err)
	}
	if m.Probe.Measured() {
		t.Fatalf("a prompt of %d tokens read as Measured() at depth %d",
			m.Probe.PromptTokens, hostfit.HostCutoffProbeDepthTokens)
	}
}

// An engine that answers with an error is a failed measurement, not a slow
// host: nothing may be published from it.
func TestMeasureHostCutoffOpenAI_EngineErrorIsNotAMeasurement(t *testing.T) {
	eng := &fakeOpenAIEngine{status: http.StatusServiceUnavailable}
	srv := eng.server(t)

	_, err := measureHostCutoffOpenAI(context.Background(), openAICutoffDeps{
		BaseURL: srv.URL, Model: "probe", Logger: testLogger(),
		HTTPClient: srv.Client(), Nonce: "unit",
	})
	if err == nil {
		t.Fatal("a 503 produced a measurement")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("err = %v, want it to name the status", err)
	}
}

// A stream that produces no tokens cannot yield a decode rate, and a
// decode rate invented from one token would understate every host.
func TestMeasureHostCutoffOpenAI_NoTokensIsNotAMeasurement(t *testing.T) {
	eng := &fakeOpenAIEngine{
		prefill:      5 * time.Millisecond,
		tokens:       1,
		promptTokens: hostfit.HostCutoffProbeDepthTokens,
	}
	srv := eng.server(t)

	_, err := measureHostCutoffOpenAI(context.Background(), openAICutoffDeps{
		BaseURL: srv.URL, Model: "probe", Logger: testLogger(),
		HTTPClient: srv.Client(), Nonce: "unit",
	})
	if err == nil {
		t.Fatal("a one-token stream produced a measurement")
	}
}

// PRODUCT CONTRACT (waired-agent#1298): the probe resolves a variant for
// whichever engine this host serves with, not for ollama only.
//
// The function this replaced answered "serving engine is vllm; the cutoff
// probe reads ollama's counters" and returned before the first setup stage
// was noted — so a vLLM host reported no probe_model_pull row, no
// host_speed row, and the wizard's fallback note for agents that report no
// row at all stayed up for the whole of setup.
func TestHostCutoffProbeVariant_ResolvesPerEngine(t *testing.T) {
	probe := catalog.Manifest{
		ModelID: hostfit.HostCutoffProbeModelID,
		Variants: []catalog.Variant{
			{
				VariantID: "q8-gguf", Format: catalog.FormatOllamaTag,
				RuntimeSupport: []string{catalog.RuntimeOllama},
				Source:         catalog.VariantSource{Type: catalog.SourceOllama, Tag: "qwen3.5:0.8b-q8_0"},
			},
			{
				VariantID: "bf16", Format: catalog.FormatSafetensors,
				RuntimeSupport: []string{catalog.RuntimeVLLM},
				Source:         catalog.VariantSource{Type: catalog.SourceHuggingFace, RepoID: "Qwen/Qwen3.5-0.8B"},
			},
		},
	}
	p := &agentInferenceProvider{manifests: []catalog.Manifest{probe}, logger: testLogger()}

	v, err := p.hostCutoffProbeVariant(context.Background(), catalog.RuntimeOllama)
	if err != nil {
		t.Fatalf("ollama: %v", err)
	}
	if v.Source.Tag != "qwen3.5:0.8b-q8_0" {
		t.Errorf("ollama variant = %+v, want the gguf tag", v.Source)
	}

	v, err = p.hostCutoffProbeVariant(context.Background(), catalog.RuntimeVLLM)
	if err != nil {
		t.Fatalf("vllm: %v", err)
	}
	if v.Source.RepoID != "Qwen/Qwen3.5-0.8B" {
		t.Errorf("vllm variant = %+v, want the safetensors repo", v.Source)
	}
}

// A build whose catalog ships no variant this engine can load has no probe
// to run. It must say so rather than reporting a row that can never
// complete — a host_speed row left running denies setup_complete to a
// computer that is otherwise finished (waired#1143).
func TestHostCutoffProbeVariant_NoVariantForThisEngine(t *testing.T) {
	probe := catalog.Manifest{
		ModelID: hostfit.HostCutoffProbeModelID,
		Variants: []catalog.Variant{{
			VariantID: "q8-gguf", Format: catalog.FormatOllamaTag,
			RuntimeSupport: []string{catalog.RuntimeOllama},
			Source:         catalog.VariantSource{Type: catalog.SourceOllama, Tag: "qwen3.5:0.8b-q8_0"},
		}},
	}
	p := &agentInferenceProvider{manifests: []catalog.Manifest{probe}, logger: testLogger()}

	if _, err := p.hostCutoffProbeVariant(context.Background(), catalog.RuntimeVLLM); err == nil {
		t.Fatal("a gguf-only probe model resolved for vllm")
	}
	if _, err := p.hostCutoffProbeVariant(context.Background(), ""); err == nil {
		t.Fatal("a host with no serving engine resolved a probe")
	}
}

// PRODUCT CONTRACT (waired-agent#1298): on a host whose serving engine is
// not ollama, "the engine is quiet" does not mean "ollama is Ready".
//
// Found on real hardware, not here: with the venv installed and no model
// chosen, the bootstrap correctly declined to start and the measurement
// then did nothing for an hour. p.ollama is non-nil on every host whatever
// engine serves, and on a vLLM host it is never started, so the old
// predicate's last line — ollama.Health == StateReady — was false forever
// and awaitQuietEngine spent the whole settle window polling it.
//
// The vLLM leg is driven here with no ollama adapter at all, which is both
// what a unit fixture has and what makes the regression visible: before
// the fix every row below answered false.
func TestEngineIsQuiet_VLLMLegDoesNotWaitOnOllama(t *testing.T) {
	newProvider := func() *agentInferenceProvider {
		p := &agentInferenceProvider{logger: testLogger()}
		p.setServingEngine(catalog.RuntimeVLLM)
		return p
	}

	t.Run("nothing using the host", func(t *testing.T) {
		if !newProvider().engineIsQuiet(context.Background()) {
			t.Error("quiet = false on an idle vLLM host with no engine up")
		}
	})

	t.Run("the operator stopped the engine", func(t *testing.T) {
		p := newProvider()
		p.vllmParked.Store(true)
		if p.engineIsQuiet(context.Background()) {
			t.Error("quiet = true on a parked host: a measurement would spawn an engine the operator stopped")
		}
	})

	t.Run("a pull is in flight", func(t *testing.T) {
		p := newProvider()
		if _, joined := p.beginPull(&pullJob{modelID: "anything"}); joined {
			t.Fatal("precondition: the first claim must not join")
		}
		if p.engineIsQuiet(context.Background()) {
			t.Error("quiet = true while a pull is in flight")
		}
	})

	t.Run("a reconcile is in flight", func(t *testing.T) {
		p := newProvider()
		p.engineReconcileInFlight.Store(true)
		if p.engineIsQuiet(context.Background()) {
			t.Error("quiet = true while an engine reconcile is in flight")
		}
	})

	// The ollama leg is unchanged, including its answer with no adapter:
	// there, an engine that is not up is exactly what the predicate is
	// waiting for.
	t.Run("the ollama leg still needs an engine", func(t *testing.T) {
		p := &agentInferenceProvider{logger: testLogger()}
		p.setServingEngine(catalog.RuntimeOllama)
		if p.engineIsQuiet(context.Background()) {
			t.Error("quiet = true on an ollama host with no adapter")
		}
	})
}
