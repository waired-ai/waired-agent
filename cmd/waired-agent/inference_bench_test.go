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

// TestRunBootBenchmark_HappyPath drives the benchmark against a
// fake engine that returns a realistic usage.completion_tokens
// value, then asserts Capacity = floor(tokps / 30).
func TestRunBootBenchmark_HappyPath(t *testing.T) {
	// Engine returns 200 tokens; clock advances by 1 s per call
	// (start time on .Do entry, end time on .Do return).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"usage": {"completion_tokens": 200},
			"choices": [{"message": {"content": "..."}}]
		}`)
	}))
	t.Cleanup(srv.Close)
	port := portFromBenchURL(t, srv.URL)

	// 200 tokens / 1 s = 200 tok/s → floor(200/30) = 6.
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
	if got.Capacity != 6 {
		t.Errorf("Capacity = %d, want 6 (200 tokens / 1s / 30)", got.Capacity)
	}
	if got.VariantID != "q4-gguf" {
		t.Errorf("VariantID = %q, want q4-gguf", got.VariantID)
	}
}

// TestRunBootBenchmark_WarmupPrecedesMeasurement asserts the cold-load
// fix: the benchmark issues one tiny untimed completion first (so the
// engine loads the model outside the measured window) and only then
// the timed 200-token request. A cold 17 GB load inside the window
// used to read as single-digit tok/s and trigger bogus lighter-model
// recommendations.
func TestRunBootBenchmark_WarmupPrecedesMeasurement(t *testing.T) {
	var maxTokensSeen []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			MaxTokens int `json:"max_tokens"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		maxTokensSeen = append(maxTokensSeen, req.MaxTokens)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"usage": {"completion_tokens": 200},
			"choices": [{"message": {"content": "..."}}]
		}`)
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
	if len(maxTokensSeen) != 2 {
		t.Fatalf("engine saw %d requests, want 2 (warm-up + measurement)", len(maxTokensSeen))
	}
	if maxTokensSeen[0] != benchWarmupCompletionTokens {
		t.Errorf("first request max_tokens = %d, want warm-up %d", maxTokensSeen[0], benchWarmupCompletionTokens)
	}
	if maxTokensSeen[1] != benchPromptCompletionTokens {
		t.Errorf("second request max_tokens = %d, want measurement %d", maxTokensSeen[1], benchPromptCompletionTokens)
	}
	// The fake clock is only consulted by the timed request (start +
	// end = 1 s): had the warm-up been measured too, the elapsed time
	// and therefore Capacity would differ from floor(200/1/30) = 6.
	if got.Capacity != 6 {
		t.Errorf("Capacity = %d, want 6 (warm-up must not consume measured time)", got.Capacity)
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{
			"usage": {"completion_tokens": 5},
			"choices": [{"message": {"content": "..."}}]
		}`)
	}))
	t.Cleanup(srv.Close)
	port := portFromBenchURL(t, srv.URL)

	// 5 tokens / 1 s = 5 tok/s → floor(5/30) = 0 → clamped to 1.
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

// TestRunBootBenchmark_FallsBackToContentWordCount exercises the
// secondary path: an engine that omits usage.completion_tokens
// (some Ollama versions) still produces a useful estimate via
// whitespace-split of the content. The estimate is off by ~10% but
// the admission cap is order-of-magnitude — that's good enough.
func TestRunBootBenchmark_FallsBackToContentWordCount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 60 words in content, no usage block.
		content := strings.Repeat("alpha beta gamma delta epsilon zeta ", 10)
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}]}`, content)
	}))
	t.Cleanup(srv.Close)
	port := portFromBenchURL(t, srv.URL)

	// 60 tokens / 1s / 30 = 2.
	got := RunBootBenchmark(context.Background(), BenchDeps{
		EngineKind:  signer.InferenceTypeOllama,
		EnginePort:  port,
		EngineModel: "qwen3:8b-q4_K_M",
		Now:         fakeNow(time.Unix(1_700_000_000, 0), time.Second),
	})
	if got.Failed {
		t.Errorf("Failed = true; word-count fallback should still succeed; err=%q", got.Err)
	}
	if got.Capacity < 1 {
		t.Errorf("Capacity = %d, want >= 1", got.Capacity)
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

	cached := BenchResult{TokensPerSec: 99.0, Capacity: 3, VariantID: "qwen3-8b-q4-gguf"}
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
	if hits.Load() != 0 {
		t.Errorf("engine was hit %d time(s); cache hit should short-circuit", hits.Load())
	}
}

// TestRunBootBenchmark_CacheMissMeasuresAndStores covers the path
// where the cache is configured but empty: the benchmark runs, the
// result is persisted, and a subsequent call hits the cache.
func TestRunBootBenchmark_CacheMissMeasuresAndStores(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, `{"usage":{"completion_tokens":200},"choices":[{"message":{"content":"."}}]}`)
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
	// A fresh measurement is warm-up + timed request.
	if hits.Load() != 2 {
		t.Fatalf("after first run, engine hit %d times; want 2 (warm-up + measure)", hits.Load())
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
	if hits.Load() != 2 {
		t.Errorf("after second run, engine hit %d times; want 2 (cache should serve, no new requests)", hits.Load())
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"usage":{"completion_tokens":200},"choices":[{"message":{"content":"."}}]}`)
	}))
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
