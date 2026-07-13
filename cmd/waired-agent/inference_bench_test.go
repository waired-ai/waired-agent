package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// fakeNow returns a closure that advances by step on every call.
// Lets the benchmark measure a deterministic elapsed duration
// without relying on the real clock.
func fakeNow(start time.Time, step time.Duration) func() time.Time {
	cur := start
	return func() time.Time {
		out := cur
		cur = cur.Add(step)
		return out
	}
}

// fakeNowScript returns a clock whose i-th call advances by steps[i]
// (the last step repeats when exhausted). The slope method needs
// per-request elapsed control — a fixed-step clock gives every request
// the same elapsed, which makes every slope denominator zero.
func fakeNowScript(start time.Time, steps ...time.Duration) func() time.Time {
	cur, i := start, 0
	return func() time.Time {
		out := cur
		step := steps[len(steps)-1]
		if i < len(steps) {
			step = steps[i]
		}
		cur = cur.Add(step)
		i++
		return out
	}
}

// fakeOllamaEngine serves both surfaces a real ollama exposes: the
// OpenAI-compat /v1/chat/completions (used by the warm-up) and the
// native /api/generate the #764 measurement reads eval counters from.
// evalDurationsNS are handed out per generate call in order (the last
// repeats); generatePaths/chatMaxTokens record what the engine saw.
type fakeOllamaEngine struct {
	evalCount       int
	evalDurationsNS []int64
	generateCalls   atomic.Int64
	chatMaxTokens   []int
}

func (f *fakeOllamaEngine) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/generate":
			n := int(f.generateCalls.Add(1)) - 1
			dur := f.evalDurationsNS[len(f.evalDurationsNS)-1]
			if n < len(f.evalDurationsNS) {
				dur = f.evalDurationsNS[n]
			}
			fmt.Fprintf(w, `{"eval_count":%d,"eval_duration":%d}`, f.evalCount, dur)
		case "/v1/chat/completions":
			var req struct {
				MaxTokens int `json:"max_tokens"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.chatMaxTokens = append(f.chatMaxTokens, req.MaxTokens)
			fmt.Fprint(w, `{"usage":{"completion_tokens":8},"choices":[{"message":{"content":"..."}}]}`)
		default:
			http.NotFound(w, r)
		}
	}
}

// TestRunBootBenchmark_NoEngineSkips covers the documented short-
// circuits: an agent with EngineKind="none" or "" or port 0 must
// return Capacity=0 without any HTTP call.
func TestRunBootBenchmark_NoEngineSkips(t *testing.T) {
	for _, c := range []struct {
		name string
		deps BenchDeps
	}{
		{"engine_kind_none", BenchDeps{EngineKind: signer.InferenceTypeNone, EnginePort: 11434}},
		{"engine_kind_empty", BenchDeps{EngineKind: "", EnginePort: 11434}},
		{"port_zero", BenchDeps{EngineKind: signer.InferenceTypeOllama, EnginePort: 0}},
		{"openai_compat", BenchDeps{EngineKind: "openai-compat", EnginePort: 11434}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := RunBootBenchmark(context.Background(), c.deps)
			if got.Capacity != 0 {
				t.Errorf("Capacity = %d, want 0 (= unlimited skip)", got.Capacity)
			}
			if got.Failed {
				t.Errorf("Failed = true; skip paths should be silent successes")
			}
		})
	}
}

// TestRunBootBenchmark_HappyPath drives the benchmark against a fake
// ollama serving native eval counters (#764), then asserts the decode
// rate is the median of the samples and Capacity = floor(tokps / 30).
func TestRunBootBenchmark_HappyPath(t *testing.T) {
	// Three samples at 76.1 / 78.0 / 82.3 tok/s (eval_count 200 each):
	// median 78.0, spread (82.3−76.1)/78.0 ≈ 7.9%.
	engine := &fakeOllamaEngine{
		evalCount:       200,
		evalDurationsNS: []int64{2_628_120_894, 2_564_102_564, 2_430_133_657},
	}
	srv := httptest.NewServer(engine.handler())
	t.Cleanup(srv.Close)
	port := portFromBenchURL(t, srv.URL)

	got := RunBootBenchmark(context.Background(), BenchDeps{
		EngineKind:  signer.InferenceTypeOllama,
		EnginePort:  port,
		EngineModel: "qwen3:8b-q4_K_M",
		VariantID:   "q4-gguf",
		Now:         fakeNow(time.Unix(1_700_000_000, 0), time.Second),
		HTTPClient:  http.DefaultClient,
		Logger:      slog.Default(),
	})
	if got.Failed {
		t.Fatalf("Failed=true, want successful happy-path; err=%q", got.Err)
	}
	if got.Method != benchMethodOllamaEval {
		t.Errorf("Method = %q, want %q", got.Method, benchMethodOllamaEval)
	}
	if got.TokensPerSec < 77.9 || got.TokensPerSec > 78.1 {
		t.Errorf("TokensPerSec = %.2f, want median ≈ 78.0", got.TokensPerSec)
	}
	if got.SpreadPct < 7 || got.SpreadPct > 9 {
		t.Errorf("SpreadPct = %.2f, want ≈ 7.9", got.SpreadPct)
	}
	// floor(78.0/30) = 2.
	if got.Capacity != 2 {
		t.Errorf("Capacity = %d, want 2 (median 78 tok/s / 30)", got.Capacity)
	}
	if got.VariantID != "q4-gguf" {
		t.Errorf("VariantID = %q, want q4-gguf", got.VariantID)
	}
	if n := engine.generateCalls.Load(); n != int64(benchSampleCount) {
		t.Errorf("engine saw %d generate calls, want %d", n, benchSampleCount)
	}
}

// TestRunBootBenchmark_WarmupPrecedesMeasurement asserts the cold-load
// fix: the benchmark issues one tiny untimed completion first (so the
// engine loads the model outside the measured window) and only then
// the native measurement samples. A cold 17 GB load inside the window
// used to read as single-digit tok/s and trigger bogus lighter-model
// recommendations.
func TestRunBootBenchmark_WarmupPrecedesMeasurement(t *testing.T) {
	var order []string
	engine := &fakeOllamaEngine{evalCount: 200, evalDurationsNS: []int64{1_000_000_000}}
	inner := engine.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		order = append(order, r.URL.Path)
		inner(w, r)
	}))
	t.Cleanup(srv.Close)
	port := portFromBenchURL(t, srv.URL)

	got := RunBootBenchmark(context.Background(), BenchDeps{
		EngineKind:  signer.InferenceTypeOllama,
		EnginePort:  port,
		EngineModel: "qwen3:8b-q4_K_M",
		Now:         fakeNow(time.Unix(1_700_000_000, 0), time.Second),
	})
	if got.Failed {
		t.Fatalf("Failed=true, want success; err=%q", got.Err)
	}
	if len(order) != 1+benchSampleCount {
		t.Fatalf("engine saw %d requests %v, want %d (warm-up + %d samples)",
			len(order), order, 1+benchSampleCount, benchSampleCount)
	}
	if order[0] != "/v1/chat/completions" {
		t.Errorf("first request path = %q, want the OpenAI-compat warm-up", order[0])
	}
	if len(engine.chatMaxTokens) != 1 || engine.chatMaxTokens[0] != benchWarmupCompletionTokens {
		t.Errorf("warm-up max_tokens = %v, want [%d]", engine.chatMaxTokens, benchWarmupCompletionTokens)
	}
	for i, p := range order[1:] {
		if p != "/api/generate" {
			t.Errorf("request %d path = %q, want /api/generate", i+1, p)
		}
	}
	// Decode rate comes from the engine's eval counters (200 tokens in
	// 1 s), not the fake wall clock — the warm-up cannot pollute it.
	if got.Capacity != 6 {
		t.Errorf("Capacity = %d, want 6 (200 eval tok/s / 30)", got.Capacity)
	}
}

// TestRunBootBenchmark_WarmupFailureShortCircuits: a warm-up failure
// is a benchmark failure (Capacity=1, Failed=true, never cached) and
// the timed measurement is not attempted.
func TestRunBootBenchmark_WarmupFailureShortCircuits(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "model load failed", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	port := portFromBenchURL(t, srv.URL)

	cache := newBenchCache(filepath.Join(t.TempDir(), "bench.json"), discardLogger())
	got := RunBootBenchmark(context.Background(), BenchDeps{
		EngineKind:  signer.InferenceTypeOllama,
		EnginePort:  port,
		EngineModel: "qwen3:8b-q4_K_M",
		GPUModel:    "RTX TEST",
		VRAMTotalMB: 24000,
		VariantSHA:  "sha-test",
		Cache:       cache,
		Now:         fakeNow(time.Unix(1_700_000_000, 0), time.Second),
	})
	if !got.Failed || got.Capacity != 1 {
		t.Errorf("got Failed=%v Capacity=%d, want Failed=true Capacity=1", got.Failed, got.Capacity)
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("engine saw %d requests, want 1 (no measurement after failed warm-up)", n)
	}
	if _, _, hit, _ := cache.Load(benchCacheKey(BenchDeps{
		GPUModel: "RTX TEST", VRAMTotalMB: 24000, VariantSHA: "sha-test",
		EngineKind: signer.InferenceTypeOllama, EngineModel: "qwen3:8b-q4_K_M",
	})); hit {
		t.Error("failed warm-up was persisted to the cache")
	}
}

// TestRunBootBenchmark_LowThroughputClampsToOne ensures very slow
// hosts still get Capacity=1 rather than Capacity=0 (= unlimited).
// 0 would conflate "slow" with "external endpoint, no cap" and
// over-admit on a feeble peer.
func TestRunBootBenchmark_LowThroughputClampsToOne(t *testing.T) {
	// 200 tokens in 40 s of decode = 5 tok/s → floor(5/30) = 0 → 1.
	engine := &fakeOllamaEngine{evalCount: 200, evalDurationsNS: []int64{40_000_000_000}}
	srv := httptest.NewServer(engine.handler())
	t.Cleanup(srv.Close)
	port := portFromBenchURL(t, srv.URL)

	got := RunBootBenchmark(context.Background(), BenchDeps{
		EngineKind:  signer.InferenceTypeOllama,
		EnginePort:  port,
		EngineModel: "qwen3:8b-q4_K_M",
		Now:         fakeNow(time.Unix(1_700_000_000, 0), time.Second),
	})
	if got.Capacity != 1 {
		t.Errorf("Capacity = %d, want 1 (low-throughput clamp)", got.Capacity)
	}
}

// TestRunBootBenchmark_EngineErrorReturnsCap1 confirms a 5xx from
// the engine produces a fallback Capacity=1 with Failed=true rather
// than blocking startup.
func TestRunBootBenchmark_EngineErrorReturnsCap1(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "out of memory", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	port := portFromBenchURL(t, srv.URL)

	got := RunBootBenchmark(context.Background(), BenchDeps{
		EngineKind:  signer.InferenceTypeVLLM,
		EnginePort:  port,
		EngineModel: "Qwen/Qwen3-8B",
		Now:         fakeNow(time.Unix(1_700_000_000, 0), time.Second),
	})
	if !got.Failed {
		t.Errorf("Failed = false; engine 5xx should set Failed=true")
	}
	if got.Capacity != 1 {
		t.Errorf("Capacity = %d, want 1 (failure fallback)", got.Capacity)
	}
	if !strings.Contains(got.Err, "HTTP 500") {
		t.Errorf("Err = %q, want HTTP 500 mention", got.Err)
	}
}

// TestRunBootBenchmark_SlopeCancelsFixedOverhead is the headline #764
// case: a vLLM-style engine (no native eval counters) whose every
// request carries 1.4 s of fixed overhead on top of true 78 tok/s
// decode. The legacy single-run formula reads ~55 tok/s off the long
// run (256 / 4.68 s); the two-length slope recovers the true rate:
// (256−64) / (4.68 s − 2.22 s) = 78.05.
func TestRunBootBenchmark_SlopeCancelsFixedOverhead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			MaxTokens int `json:"max_tokens"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"usage":{"completion_tokens":%d},"choices":[{"message":{"content":"..."}}]}`, req.MaxTokens)
	}))
	t.Cleanup(srv.Close)
	port := portFromBenchURL(t, srv.URL)

	// Now-call pattern per pair: [start short, end short, start long,
	// end long] → steps [2.22s, 1ms, 4.68s, 1ms] repeating. The warm-up
	// does not consult the clock.
	steps := []time.Duration{
		2220 * time.Millisecond, time.Millisecond,
		4680 * time.Millisecond, time.Millisecond,
	}
	var script []time.Duration
	for i := 0; i < benchSampleCount; i++ {
		script = append(script, steps...)
	}
	got := RunBootBenchmark(context.Background(), BenchDeps{
		EngineKind:  signer.InferenceTypeVLLM,
		EnginePort:  port,
		EngineModel: "Qwen/Qwen3-8B",
		Now:         fakeNowScript(time.Unix(1_700_000_000, 0), script...),
	})
	if got.Failed {
		t.Fatalf("Failed=true, want success; err=%q", got.Err)
	}
	if got.Method != benchMethodSlope {
		t.Errorf("Method = %q, want %q", got.Method, benchMethodSlope)
	}
	if got.TokensPerSec < 77.5 || got.TokensPerSec > 78.5 {
		t.Errorf("TokensPerSec = %.2f, want ≈ 78.05 (slope must cancel the 1.4 s overhead; legacy formula reads ~55)", got.TokensPerSec)
	}
	// floor(78.05/30) = 2.
	if got.Capacity != 2 {
		t.Errorf("Capacity = %d, want 2", got.Capacity)
	}
}

// TestRunBootBenchmark_OllamaMissingCountersFallsBackToSlope covers an
// ollama-kind engine whose /api/generate response carries no eval
// counters (older ollama, an OpenAI-compat proxy on the engine port):
// the benchmark degrades to the two-length slope instead of failing.
func TestRunBootBenchmark_OllamaMissingCountersFallsBackToSlope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/generate" {
			fmt.Fprint(w, `{"response":"..."}`) // no eval counters
			return
		}
		var req struct {
			MaxTokens int `json:"max_tokens"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		fmt.Fprintf(w, `{"usage":{"completion_tokens":%d},"choices":[{"message":{"content":"..."}}]}`, req.MaxTokens)
	}))
	t.Cleanup(srv.Close)
	port := portFromBenchURL(t, srv.URL)

	steps := []time.Duration{
		time.Second, time.Millisecond,
		2 * time.Second, time.Millisecond,
	}
	var script []time.Duration
	for i := 0; i < benchSampleCount; i++ {
		script = append(script, steps...)
	}
	got := RunBootBenchmark(context.Background(), BenchDeps{
		EngineKind:  signer.InferenceTypeOllama,
		EnginePort:  port,
		EngineModel: "qwen3:8b-q4_K_M",
		Now:         fakeNowScript(time.Unix(1_700_000_000, 0), script...),
	})
	if got.Failed {
		t.Fatalf("Failed=true, want slope fallback success; err=%q", got.Err)
	}
	if got.Method != benchMethodSlope {
		t.Errorf("Method = %q, want %q", got.Method, benchMethodSlope)
	}
	// (256−64) / (2s − 1s) = 192 tok/s → floor(192/30) = 6.
	if got.Capacity != 6 {
		t.Errorf("Capacity = %d, want 6", got.Capacity)
	}
}

// TestRunBootBenchmark_DegenerateSlopeFallsBackToWallClock covers the
// last rung of the #764 chain: an engine (or proxy) that returns a
// fixed-size response in constant time defeats both corrected methods;
// the benchmark salvages the legacy single-run wall-clock rate via the
// content word count rather than failing. The estimate is off but the
// admission cap is order-of-magnitude — good enough, and warn-logged.
func TestRunBootBenchmark_DegenerateSlopeFallsBackToWallClock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 60 words in content, no usage block, same for every request.
		content := strings.Repeat("alpha beta gamma delta epsilon zeta ", 10)
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}]}`, content)
	}))
	t.Cleanup(srv.Close)
	port := portFromBenchURL(t, srv.URL)

	// Fixed-step clock: every request takes 1 s → every slope pair is
	// degenerate (same tokens, same elapsed).
	got := RunBootBenchmark(context.Background(), BenchDeps{
		EngineKind:  signer.InferenceTypeVLLM,
		EnginePort:  port,
		EngineModel: "Qwen/Qwen3-8B",
		Now:         fakeNow(time.Unix(1_700_000_000, 0), time.Second),
	})
	if got.Failed {
		t.Errorf("Failed = true; wall-clock fallback should still succeed; err=%q", got.Err)
	}
	if got.Method != benchMethodWallClock {
		t.Errorf("Method = %q, want %q", got.Method, benchMethodWallClock)
	}
	// 60 tokens / 1 s / 30 = 2.
	if got.Capacity != 2 {
		t.Errorf("Capacity = %d, want 2", got.Capacity)
	}
}

// TestRunBootBenchmark_PartialSamplesTruncate asserts an error after
// at least one valid native sample keeps the completed samples rather
// than failing the whole benchmark (budget expiry mid-loop is the
// usual cause).
func TestRunBootBenchmark_PartialSamplesTruncate(t *testing.T) {
	var generateCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/generate" {
			if generateCalls.Add(1) > 1 {
				http.Error(w, "engine wedged", http.StatusInternalServerError)
				return
			}
			fmt.Fprint(w, `{"eval_count":200,"eval_duration":2000000000}`) // 100 tok/s
			return
		}
		fmt.Fprint(w, `{"usage":{"completion_tokens":8},"choices":[{"message":{"content":"..."}}]}`)
	}))
	t.Cleanup(srv.Close)
	port := portFromBenchURL(t, srv.URL)

	got := RunBootBenchmark(context.Background(), BenchDeps{
		EngineKind:  signer.InferenceTypeOllama,
		EnginePort:  port,
		EngineModel: "qwen3:8b-q4_K_M",
		Now:         fakeNow(time.Unix(1_700_000_000, 0), time.Second),
	})
	if got.Failed {
		t.Fatalf("Failed=true, want truncated success; err=%q", got.Err)
	}
	if got.Method != benchMethodOllamaEval {
		t.Errorf("Method = %q, want %q", got.Method, benchMethodOllamaEval)
	}
	if got.TokensPerSec != 100 {
		t.Errorf("TokensPerSec = %.2f, want 100 (single completed sample)", got.TokensPerSec)
	}
	if got.SpreadPct != 0 {
		t.Errorf("SpreadPct = %.2f, want 0 for a single sample", got.SpreadPct)
	}
}

// TestExtractCompletionTokens_UsesUsageWhenPresent isolates the
// parsing helper. With both usage and content present, usage wins.
func TestExtractCompletionTokens_UsesUsageWhenPresent(t *testing.T) {
	body := []byte(`{
		"usage": {"completion_tokens": 200},
		"choices": [{"message": {"content": "ignored"}}]
	}`)
	n, err := extractCompletionTokens(body)
	if err != nil {
		t.Fatalf("extractCompletionTokens: %v", err)
	}
	if n != 200 {
		t.Errorf("got %d, want 200", n)
	}
}

// TestExtractCompletionTokens_MissingUsageFallsThrough verifies the
// fallback path returns the whitespace-split estimate.
func TestExtractCompletionTokens_MissingUsageFallsThrough(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"content":"a b c"}}]}`)
	n, err := extractCompletionTokens(body)
	if err != nil {
		t.Fatalf("extractCompletionTokens: %v", err)
	}
	if n != 3 {
		t.Errorf("got %d, want 3 (whitespace-split)", n)
	}
}

// TestExtractCompletionTokens_EmptyEnvelope returns an error rather
// than silently producing zero.
func TestExtractCompletionTokens_EmptyEnvelope(t *testing.T) {
	_, err := extractCompletionTokens([]byte(`{}`))
	if err == nil {
		t.Error("empty envelope should error")
	}
	if !errors.Is(err, err) { // sanity: error is non-nil
		t.Errorf("err = %v", err)
	}
}

// portFromBenchURL adapts the existing portFromURL helper in
// inference_probe_test.go (returns (int, error)) into a t.Fatalf
// shape so the table-driven tests above stay concise.
func portFromBenchURL(t *testing.T, urlStr string) int {
	t.Helper()
	port, err := portFromURL(urlStr)
	if err != nil {
		t.Fatalf("portFromURL(%q): %v", urlStr, err)
	}
	return port
}

// Compile-time guard: io should remain imported even if the bench
// implementation later changes — keeps the test file robust to
// production refactors.
var _ = io.Discard

// TestRunBootBenchmark_CacheHitShortCircuits seeds the cache with a
// known measurement and asserts RunBootBenchmark returns that
// measurement WITHOUT making an HTTP call to the engine. This is the
// boot-time saving the cache exists for.
func TestRunBootBenchmark_CacheHitShortCircuits(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, `{"usage":{"completion_tokens":200},"choices":[{"message":{"content":"."}}]}`)
	}))
	t.Cleanup(srv.Close)
	port := portFromBenchURL(t, srv.URL)

	dir := t.TempDir()
	cache := newBenchCache(filepath.Join(dir, "bench.json"), nil)

	cached := BenchResult{
		TokensPerSec: 99.0, Capacity: 3, VariantID: "qwen3-8b-q4-gguf",
		Method: benchMethodOllamaEval, SpreadPct: 4.2,
	}
	meta := benchCacheHumanMeta{
		VariantID: "qwen3-8b-q4-gguf", GPUModel: "RTX 4090", VRAMTotalMB: 24576,
		DriverVersion: "595.0", EngineKind: "ollama", EngineModel: "qwen3:8b",
	}
	deps := BenchDeps{
		EngineKind:    signer.InferenceTypeOllama,
		EnginePort:    port,
		EngineModel:   "qwen3:8b",
		VariantID:     "qwen3-8b-q4-gguf",
		GPUModel:      "RTX 4090",
		VRAMTotalMB:   24576,
		DriverVersion: "595.0",
		VariantSHA:    "abc123",
		Cache:         cache,
	}
	key := benchCacheKey(deps)
	if key == "" {
		t.Fatalf("benchCacheKey returned empty key for full deps")
	}
	if err := cache.Store(key, cached, meta, time.Now()); err != nil {
		t.Fatalf("seed Store: %v", err)
	}

	got := RunBootBenchmark(context.Background(), deps)
	if got.Capacity != 3 || got.TokensPerSec != 99.0 || got.VariantID != "qwen3-8b-q4-gguf" {
		t.Errorf("Cache hit returned wrong result: %+v", got)
	}
	if got.Method != benchMethodOllamaEval || got.SpreadPct != 4.2 {
		t.Errorf("Method/SpreadPct did not round-trip through the cache: %+v", got)
	}
	if hits.Load() != 0 {
		t.Errorf("engine was hit %d time(s); cache hit should short-circuit", hits.Load())
	}
}

// TestRunBootBenchmark_CacheMissMeasuresAndStores covers the path
// where the cache is configured but empty: the benchmark runs, the
// result is persisted, and a subsequent call hits the cache.
func TestRunBootBenchmark_CacheMissMeasuresAndStores(t *testing.T) {
	var hits atomic.Int64
	engine := &fakeOllamaEngine{evalCount: 200, evalDurationsNS: []int64{1_000_000_000}}
	inner := engine.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		inner(w, r)
	}))
	t.Cleanup(srv.Close)
	port := portFromBenchURL(t, srv.URL)

	dir := t.TempDir()
	cachePath := filepath.Join(dir, "bench.json")
	cache := newBenchCache(cachePath, nil)

	deps := BenchDeps{
		EngineKind:    signer.InferenceTypeOllama,
		EnginePort:    port,
		EngineModel:   "qwen3:8b",
		VariantID:     "qwen3-8b-q4-gguf",
		GPUModel:      "RTX 4090",
		VRAMTotalMB:   24576,
		DriverVersion: "595.0",
		VariantSHA:    "abc123",
		Cache:         cache,
		Now:           fakeNow(time.Unix(1_700_000_000, 0), time.Second),
	}
	first := RunBootBenchmark(context.Background(), deps)
	if first.Failed || first.Capacity == 0 {
		t.Fatalf("first run: failed=%v cap=%d", first.Failed, first.Capacity)
	}
	// A fresh measurement is warm-up + benchSampleCount native samples.
	wantHits := int64(1 + benchSampleCount)
	if hits.Load() != wantHits {
		t.Fatalf("after first run, engine hit %d times; want %d (warm-up + samples)", hits.Load(), wantHits)
	}
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("cache file not written after measurement: %v", err)
	}

	// Second run with a fresh Now closure (cache lookup uses Now to
	// compute age, so reusing the exhausted closure would panic).
	deps.Now = fakeNow(time.Unix(1_700_000_999, 0), time.Second)
	second := RunBootBenchmark(context.Background(), deps)
	if second.Capacity != first.Capacity || second.TokensPerSec != first.TokensPerSec {
		t.Errorf("second run did not return cached result: first=%+v second=%+v", first, second)
	}
	if hits.Load() != wantHits {
		t.Errorf("after second run, engine hit %d times; want %d (cache should serve, no new requests)", hits.Load(), wantHits)
	}
}

// TestRunBootBenchmark_FailedMeasurementNotPersisted asserts a 5xx
// engine response does not get cached — transient OOM / warmup blips
// would otherwise stick across reboots.
func TestRunBootBenchmark_FailedMeasurementNotPersisted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "out of memory", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	port := portFromBenchURL(t, srv.URL)

	dir := t.TempDir()
	cachePath := filepath.Join(dir, "bench.json")
	cache := newBenchCache(cachePath, nil)

	got := RunBootBenchmark(context.Background(), BenchDeps{
		EngineKind:    signer.InferenceTypeOllama,
		EnginePort:    port,
		EngineModel:   "qwen3:8b",
		GPUModel:      "RTX 4090",
		DriverVersion: "595.0",
		VariantSHA:    "abc123",
		Cache:         cache,
	})
	if !got.Failed {
		t.Fatalf("Failed = false; want failure for 500 response")
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Errorf("failed measurement was persisted (cache file exists): %v", err)
	}
}

// TestRunBootBenchmark_NoCacheKeyDisablesCaching covers CPU-only
// hosts (empty GPUModel) and variants with no SHA: the benchmark
// still measures, but no file is written.
func TestRunBootBenchmark_NoCacheKeyDisablesCaching(t *testing.T) {
	engine := &fakeOllamaEngine{evalCount: 200, evalDurationsNS: []int64{1_000_000_000}}
	srv := httptest.NewServer(engine.handler())
	t.Cleanup(srv.Close)
	port := portFromBenchURL(t, srv.URL)

	dir := t.TempDir()
	cachePath := filepath.Join(dir, "bench.json")
	cache := newBenchCache(cachePath, nil)

	// GPUModel="" → no key → caching disabled.
	got := RunBootBenchmark(context.Background(), BenchDeps{
		EngineKind:  signer.InferenceTypeOllama,
		EnginePort:  port,
		EngineModel: "qwen3:8b",
		VariantSHA:  "abc123",
		Cache:       cache,
		Now:         fakeNow(time.Unix(1_700_000_000, 0), time.Second),
	})
	if got.Capacity == 0 {
		t.Fatalf("expected real measurement, got Capacity=0")
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Errorf("cache file written despite empty GPUModel: %v", err)
	}
}

func TestResolveInteractiveFloor(t *testing.T) {
	// The default is the #670 selection floor, NOT the admission divisor
	// (avgCodingAgentTokRate) — the two deliberately diverged when the
	// floor went to 100.
	if got := resolveInteractiveFloor(0); got != router.CodingAgentSelectionFloorTokps {
		t.Errorf("resolveInteractiveFloor(0) = %v, want default %v", got, router.CodingAgentSelectionFloorTokps)
	}
	if got := resolveInteractiveFloor(12.5); got != 12.5 {
		t.Errorf("resolveInteractiveFloor(12.5) = %v, want passthrough 12.5", got)
	}
	// Negative is treated as "unset" → default (Validate rejects it anyway).
	if got := resolveInteractiveFloor(-3); got != router.CodingAgentSelectionFloorTokps {
		t.Errorf("resolveInteractiveFloor(-3) = %v, want default %v", got, router.CodingAgentSelectionFloorTokps)
	}
}
