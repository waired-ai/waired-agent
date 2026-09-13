package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/proto/hostfit"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// fakeNow returns a closure that advances by step on every call.
func fakeNow(start time.Time, step time.Duration) func() time.Time {
	var mu sync.Mutex
	cur := start
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		out := cur
		cur = cur.Add(step)
		return out
	}
}

// fakeOllamaEngine serves the two surfaces the served-model speed
// measurement uses (waired-ai/waired-agent#1341): the OpenAI-compat
// /v1/chat/completions the warm-up asks, and /api/generate with the
// engine's own counters for the calibration and the timed sample.
//
// The counters are DERIVED from the prompt it was sent — the prompt's
// filler lines times tokensPerLine, at prefillTokps and decodeTokps — so a
// test that sizes the prompt wrong reads a wrong depth, the way a real
// engine would report it. Everything the measurement sent is recorded,
// because what it sends (num_predict, the absence of num_ctx) is part of
// the contract.
type fakeOllamaEngine struct {
	tokensPerLine int     // default 20
	prefillTokps  float64 // default 1000
	decodeTokps   float64 // default 100
	// evalCap, when > 0, is the most tokens a sample decodes — a model
	// that stops early. perSample overrides the rates per timed sample
	// (0-based).
	evalCap   func(sample int) int
	perSample func(sample int) (prefill, decode float64)
	// maxPromptTokens, when > 0, truncates the prefill the way an engine
	// with a smaller window does.
	maxPromptTokens int
	// hold, when non-nil, blocks a timed sample until it is closed or the
	// request is cancelled; sampleArrived is closed when the first sample
	// arrives.
	hold          chan struct{}
	sampleArrived chan struct{}
	arrivedOnce   sync.Once

	mu            sync.Mutex
	generates     []map[string]any
	chatMaxTokens []int
	samples       atomic.Int64
	cancelled     atomic.Int64
}

func (f *fakeOllamaEngine) tpl() int {
	if f.tokensPerLine > 0 {
		return f.tokensPerLine
	}
	return 20
}

func (f *fakeOllamaEngine) rates(sample int) (float64, float64) {
	if f.perSample != nil {
		return f.perSample(sample)
	}
	prefill, decode := f.prefillTokps, f.decodeTokps
	if prefill <= 0 {
		prefill = 1000
	}
	if decode <= 0 {
		decode = 100
	}
	return prefill, decode
}

// promptLines counts the filler lines syntheticPromptLinesAsking wrote.
func promptLines(prompt string) int {
	return strings.Count(prompt, "\nentry ")
}

func (f *fakeOllamaEngine) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/generate":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.generates = append(f.generates, body)
			f.mu.Unlock()
			prompt, _ := body["prompt"].(string)
			opts, _ := body["options"].(map[string]any)
			numPredict := 0
			if v, ok := opts["num_predict"].(float64); ok {
				numPredict = int(v)
			}
			promptTokens := promptLines(prompt) * f.tpl()
			if f.maxPromptTokens > 0 && promptTokens > f.maxPromptTokens {
				promptTokens = f.maxPromptTokens
			}
			if numPredict <= 1 {
				fmt.Fprintf(w, `{"prompt_eval_count":%d,"prompt_eval_duration":%d,"eval_count":1,"eval_duration":1000000}`,
					promptTokens, int64(float64(promptTokens)/1000*1e9))
				return
			}
			n := int(f.samples.Add(1)) - 1
			if f.sampleArrived != nil {
				f.arrivedOnce.Do(func() { close(f.sampleArrived) })
			}
			if f.hold != nil {
				select {
				case <-f.hold:
				case <-r.Context().Done():
					f.cancelled.Add(1)
					return
				}
			}
			prefill, decode := f.rates(n)
			evalCount := numPredict
			if f.evalCap != nil {
				if c := f.evalCap(n); c > 0 && c < evalCount {
					evalCount = c
				}
			}
			fmt.Fprintf(w, `{"prompt_eval_count":%d,"prompt_eval_duration":%d,"eval_count":%d,"eval_duration":%d}`,
				promptTokens, int64(float64(promptTokens)/prefill*1e9),
				evalCount, int64(float64(evalCount)/decode*1e9))
		case "/v1/chat/completions":
			var req struct {
				MaxTokens int `json:"max_tokens"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.mu.Lock()
			f.chatMaxTokens = append(f.chatMaxTokens, req.MaxTokens)
			f.mu.Unlock()
			fmt.Fprint(w, `{"usage":{"completion_tokens":8},"choices":[{"message":{"content":"..."}}]}`)
		default:
			http.NotFound(w, r)
		}
	}
}

// sampleBodies are the /api/generate bodies of the timed samples.
func (f *fakeOllamaEngine) sampleBodies() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, b := range f.generates {
		if opts, _ := b["options"].(map[string]any); opts != nil {
			if v, _ := opts["num_predict"].(float64); v > 1 {
				out = append(out, b)
			}
		}
	}
	return out
}

// speedEngine starts the fake and returns deps pointed at it.
func speedEngine(t *testing.T, f *fakeOllamaEngine) BenchDeps {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return BenchDeps{
		EngineKind:  signer.InferenceTypeOllama,
		EnginePort:  portFromBenchURL(t, srv.URL),
		EngineModel: "qwen3.8:27b",
		ModelID:     "qwen3.8-27b",
		VariantID:   "mtp-q4",
		Logger:      discardLogger(),
	}
}

func portFromBenchURL(t *testing.T, urlStr string) int {
	t.Helper()
	port, err := portFromURL(urlStr)
	if err != nil {
		t.Fatalf("portFromURL(%q): %v", urlStr, err)
	}
	return port
}

// portOf extracts the port an httptest server bound.
func portOf(t *testing.T, url string) int {
	t.Helper()
	i := strings.LastIndex(url, ":")
	p, err := strconv.Atoi(url[i+1:])
	if err != nil {
		t.Fatalf("port from %q: %v", url, err)
	}
	return p
}

var _ = io.Discard

// TestRunBootBenchmark_NoEngineSkips covers the documented short-circuits:
// no engine, engine off or an unknown kind return Capacity=0 without any
// request.
func TestRunBootBenchmark_NoEngineSkips(t *testing.T) {
	for _, c := range []struct {
		name string
		deps BenchDeps
	}{
		{"engine_kind_none", BenchDeps{EngineKind: signer.InferenceTypeNone, EnginePort: 11434}},
		{"engine_kind_empty", BenchDeps{EngineKind: "", EnginePort: 11434}},
		{"port_zero", BenchDeps{EngineKind: signer.InferenceTypeOllama, EnginePort: 0}},
		{"unknown_kind", BenchDeps{EngineKind: "some-future-engine", EnginePort: 11434}},
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

// PRODUCT CONTRACT (decisions 1-2 of docs/decisions/20260913/2245): the
// measurement is one 32,768-token request on the served model, read from
// the engine's own counters, and the verdict figure is TurnSecondsAt at the
// canonical depth. The two calibration points the line was set against
// reproduce.
func TestRunBootBenchmark_OneRequestAtDepthGivesSecondsPerRequest(t *testing.T) {
	for _, c := range []struct {
		name            string
		prefill, decode float64
		wantTurn        float64
	}{
		{"27b on the 48 GB laptop", 252.9, 15.8, 228},
		{"35b-a3b on the 48 GB laptop", 901.3, 45.7, 70},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeOllamaEngine{prefillTokps: c.prefill, decodeTokps: c.decode}
			deps := speedEngine(t, f)
			got := RunBootBenchmark(context.Background(), deps)
			if got.Failed {
				t.Fatalf("Failed=%v err=%q", got.Failed, got.Err)
			}
			if math.Round(got.TurnSeconds) != c.wantTurn {
				t.Errorf("TurnSeconds = %.1f, want %.0f", got.TurnSeconds, c.wantTurn)
			}
			if math.Abs(got.PrefillTokps-c.prefill) > 0.5 || math.Abs(got.DecodeTokps-c.decode) > 0.2 {
				t.Errorf("rates = %.1f / %.1f, want %.1f / %.1f", got.PrefillTokps, got.DecodeTokps, c.prefill, c.decode)
			}
			if got.TokensPerSec != got.DecodeTokps {
				t.Errorf("TokensPerSec = %v, want the decode rate %v", got.TokensPerSec, got.DecodeTokps)
			}
			if got.Samples != 1 {
				t.Errorf("Samples = %d, want 1 — %v s is outside the second-sample band", got.Samples, c.wantTurn)
			}
			if !modelSpeedDepthAccepted(hostfit.SpeedMeasurementDepthTokens, got.DepthTokens) ||
				got.DepthTokens < hostfit.SpeedMeasurementDepthTokens {
				t.Errorf("DepthTokens = %d, want about %d", got.DepthTokens, hostfit.SpeedMeasurementDepthTokens)
			}
			if got.Method != signer.BenchmarkMethodOllamaEval {
				t.Errorf("Method = %q", got.Method)
			}
			bodies := f.sampleBodies()
			if len(bodies) != 1 {
				t.Fatalf("timed samples = %d, want 1", len(bodies))
			}
			opts := bodies[0]["options"].(map[string]any)
			if int(opts["num_predict"].(float64)) != hostfit.SpeedMeasurementCompletionTokens {
				t.Errorf("num_predict = %v, want %d", opts["num_predict"], hostfit.SpeedMeasurementCompletionTokens)
			}
			if _, ok := opts["num_ctx"]; ok {
				t.Error("the sample set num_ctx; it must be served as the runner serves it")
			}
			if _, ok := bodies[0]["keep_alive"]; ok {
				t.Error("the sample set keep_alive; it must not change the host's residency")
			}
			lines := promptLines(bodies[0]["prompt"].(string))
			if want := int(math.Ceil(float64(hostfit.SpeedMeasurementDepthTokens) / 20)); lines != want {
				t.Errorf("prompt lines = %d, want %d (depth / the calibrated tokens per line)", lines, want)
			}
		})
	}
}

// The warm-up is untimed and first: a cold model load must not land inside
// the measured request.
func TestRunBootBenchmark_WarmupPrecedesMeasurement(t *testing.T) {
	var order []string
	var mu sync.Mutex
	engine := &fakeOllamaEngine{}
	inner := engine.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		order = append(order, r.URL.Path)
		mu.Unlock()
		inner(w, r)
	}))
	t.Cleanup(srv.Close)

	got := RunBootBenchmark(context.Background(), BenchDeps{
		EngineKind:  signer.InferenceTypeOllama,
		EnginePort:  portFromBenchURL(t, srv.URL),
		EngineModel: "qwen3:8b-q4_K_M",
	})
	if got.Failed {
		t.Fatalf("Failed=true, want success; err=%q", got.Err)
	}
	want := []string{"/v1/chat/completions", "/api/generate", "/api/generate"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("requests = %v, want %v (warm-up, calibration, sample)", order, want)
	}
	if len(engine.chatMaxTokens) != 1 || engine.chatMaxTokens[0] != benchWarmupCompletionTokens {
		t.Errorf("warm-up max_tokens = %v, want [%d]", engine.chatMaxTokens, benchWarmupCompletionTokens)
	}
}

// TestRunBootBenchmark_WarmupFailureShortCircuits: a warm-up failure is a
// failure (Capacity=1, Failed=true, never cached) and nothing is measured.
func TestRunBootBenchmark_WarmupFailureShortCircuits(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "model load failed", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	cache := newBenchCache(filepath.Join(t.TempDir(), "bench.json"), discardLogger())
	deps := BenchDeps{
		EngineKind:    signer.InferenceTypeOllama,
		EnginePort:    portFromBenchURL(t, srv.URL),
		EngineModel:   "qwen3:8b-q4_K_M",
		EngineVersion: "0.33.3",
		GPUModel:      "RTX TEST",
		VRAMTotalMB:   24000,
		VariantSHA:    "sha-test",
		Cache:         cache,
	}
	got := RunBootBenchmark(context.Background(), deps)
	if !got.Failed || got.Capacity != 1 {
		t.Errorf("got Failed=%v Capacity=%d, want Failed=true Capacity=1", got.Failed, got.Capacity)
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("engine saw %d requests, want 1 (no measurement after failed warm-up)", n)
	}
	if _, _, hit, _ := cache.Load(benchCacheKey(deps)); hit {
		t.Error("failed warm-up was persisted to the cache")
	}
}

// TestRunBootBenchmark_CapacityIsTheWarmSlotCount pins what Capacity carries
// since waired-agent#1126: the conversations the host holds warm, not a
// function of the measured rate.
func TestRunBootBenchmark_CapacityIsTheWarmSlotCount(t *testing.T) {
	deps := speedEngine(t, &fakeOllamaEngine{})
	deps.WarmSlots = func() int { return 2 }
	if got := RunBootBenchmark(context.Background(), deps); got.Capacity != 2 {
		t.Errorf("Capacity = %d, want 2 (the engine's slot count)", got.Capacity)
	}
	deps.WarmSlots = func() int { return 0 }
	if got := RunBootBenchmark(context.Background(), deps); got.Capacity != unmeasuredCapacity {
		t.Errorf("unknown slots: Capacity = %d, want %d", got.Capacity, unmeasuredCapacity)
	}
}

// A slow host is still a measurement, never a Capacity of zero.
func TestRunBootBenchmark_SlowHostIsNotACapacityOfZero(t *testing.T) {
	deps := speedEngine(t, &fakeOllamaEngine{prefillTokps: 60, decodeTokps: 3})
	got := RunBootBenchmark(context.Background(), deps)
	if got.Failed {
		t.Fatalf("a slow host is still a measurement; err=%q", got.Err)
	}
	if got.Capacity != unmeasuredCapacity || got.TurnSeconds <= hostfit.ModelTurnBudgetSeconds {
		t.Errorf("Capacity=%d TurnSeconds=%.0f, want %d and over the line", got.Capacity, got.TurnSeconds, unmeasuredCapacity)
	}
}

// TestRunBootBenchmark_EngineErrorReturnsCap1: a 5xx is Failed with the
// fail-safe capacity rather than blocking startup.
func TestRunBootBenchmark_EngineErrorReturnsCap1(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "out of memory", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	got := RunBootBenchmark(context.Background(), BenchDeps{
		EngineKind:  signer.InferenceTypeVLLM,
		EnginePort:  portFromBenchURL(t, srv.URL),
		EngineModel: "Qwen/Qwen3-8B",
	})
	if !got.Failed || got.Capacity != 1 {
		t.Errorf("Failed=%v Capacity=%d, want true/1", got.Failed, got.Capacity)
	}
	if !strings.Contains(got.Err, "HTTP 500") {
		t.Errorf("Err = %q, want HTTP 500 mention", got.Err)
	}
}

func cachedDeps(t *testing.T, port int, cache *benchCache) BenchDeps {
	t.Helper()
	return BenchDeps{
		EngineKind:    signer.InferenceTypeOllama,
		EnginePort:    port,
		EngineModel:   "qwen3:8b",
		ModelID:       "qwen3.5-8b",
		VariantID:     "qwen3-8b-q4-gguf",
		EngineVersion: "0.33.3",
		GPUModel:      "RTX 4090",
		VRAMTotalMB:   24576,
		DriverVersion: "595.0",
		VariantSHA:    "abc123",
		AppliedWindow: 200704,
		KVCacheType:   "q8_0",
		NumParallel:   1,
		Cache:         cache,
		Logger:        discardLogger(),
	}
}

func refusingEngine(t *testing.T) (int, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return portFromBenchURL(t, srv.URL), &hits
}

// PRODUCT CONTRACT (decision 7): a stored measurement of the same weights,
// engine release, GPU and serving configuration answers without a request,
// and reads as a measurement — model named, Outcome measured, Cached set.
func TestRunBootBenchmark_CacheHitReadsAsAMeasurement(t *testing.T) {
	port, hits := refusingEngine(t)
	cache := newBenchCache(filepath.Join(t.TempDir(), "bench.json"), nil)
	deps := cachedDeps(t, port, cache)
	stored := BenchResult{
		TokensPerSec: 15.8, DecodeTokps: 15.8, PrefillTokps: 252.9, DepthTokens: 32780, TurnSeconds: 228.3,
		Capacity: 3, VariantID: "qwen3-8b-q4-gguf", Method: signer.BenchmarkMethodOllamaEval,
	}
	if err := cache.Store(benchCacheKey(deps), stored, benchCacheHumanMeta{VariantID: "qwen3-8b-q4-gguf"}, time.Now()); err != nil {
		t.Fatalf("seed Store: %v", err)
	}

	got := RunBootBenchmark(context.Background(), deps)
	if hits.Load() != 0 {
		t.Errorf("engine was hit %d time(s); a stored figure must not measure", hits.Load())
	}
	if got.ModelID != "qwen3.5-8b" || got.Outcome != benchOutcomeMeasured || !got.Cached || got.Failed {
		t.Errorf("hit = %+v, want the named model, measured, cached", got)
	}
	if got.TurnSeconds != 228.3 || got.PrefillTokps != 252.9 || got.DecodeTokps != 15.8 || got.DepthTokens != 32780 {
		t.Errorf("the measurement did not round-trip through the cache: %+v", got)
	}
}

// PRODUCT CONTRACT (decision 7): a person asking again (mode rerun,
// SkipCacheLoad) measures over a stored figure and the new one replaces it.
func TestRunBootBenchmark_RerunMeasuresAndOverwrites(t *testing.T) {
	f := &fakeOllamaEngine{prefillTokps: 901.3, decodeTokps: 45.7}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	cache := newBenchCache(filepath.Join(t.TempDir(), "bench.json"), nil)
	deps := cachedDeps(t, portFromBenchURL(t, srv.URL), cache)
	if err := cache.Store(benchCacheKey(deps), BenchResult{TurnSeconds: 228, DecodeTokps: 15.8}, benchCacheHumanMeta{}, time.Now()); err != nil {
		t.Fatal(err)
	}

	deps.SkipCacheLoad = true
	got := RunBootBenchmark(context.Background(), deps)
	if got.Cached || math.Round(got.TurnSeconds) != 70 {
		t.Fatalf("rerun = %+v, want a fresh 70 s measurement", got)
	}
	if f.samples.Load() != 1 {
		t.Errorf("samples = %d, want 1", f.samples.Load())
	}
	back, _, hit, _ := cache.Load(benchCacheKey(deps))
	if !hit || math.Round(back.TurnSeconds) != 70 {
		t.Errorf("stored after rerun = %+v (hit %v), want the new 70 s figure", back, hit)
	}
}

// TestRunBootBenchmark_CacheMissMeasuresAndStores: an empty store measures
// once, stores, and the next call answers from the store.
func TestRunBootBenchmark_CacheMissMeasuresAndStores(t *testing.T) {
	var hits atomic.Int64
	engine := &fakeOllamaEngine{}
	inner := engine.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		inner(w, r)
	}))
	t.Cleanup(srv.Close)
	cachePath := filepath.Join(t.TempDir(), "bench.json")
	deps := cachedDeps(t, portFromBenchURL(t, srv.URL), newBenchCache(cachePath, nil))

	first := RunBootBenchmark(context.Background(), deps)
	if first.Failed || first.TurnSeconds <= 0 {
		t.Fatalf("first run: %+v", first)
	}
	if hits.Load() != 3 {
		t.Fatalf("after first run, engine hit %d times; want 3 (warm-up, calibration, sample)", hits.Load())
	}
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("cache file not written after measurement: %v", err)
	}
	second := RunBootBenchmark(context.Background(), deps)
	if !second.Cached || second.TurnSeconds != first.TurnSeconds {
		t.Errorf("second run did not answer from the store: first=%+v second=%+v", first, second)
	}
	if hits.Load() != 3 {
		t.Errorf("after second run, engine hit %d times; want 3", hits.Load())
	}
}

// TestRunBootBenchmark_FailedMeasurementNotPersisted: a 5xx is not stored.
func TestRunBootBenchmark_FailedMeasurementNotPersisted(t *testing.T) {
	port, _ := refusingEngine(t)
	cachePath := filepath.Join(t.TempDir(), "bench.json")
	got := RunBootBenchmark(context.Background(), cachedDeps(t, port, newBenchCache(cachePath, nil)))
	if !got.Failed {
		t.Fatalf("Failed = false; want failure for 500 response")
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Errorf("failed measurement was persisted (cache file exists): %v", err)
	}
}

// TestRunBootBenchmark_NoCacheKeyDisablesCaching: an empty GPU model keys
// nothing — the host still measures, nothing is written.
func TestRunBootBenchmark_NoCacheKeyDisablesCaching(t *testing.T) {
	f := &fakeOllamaEngine{}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	cachePath := filepath.Join(t.TempDir(), "bench.json")
	deps := cachedDeps(t, portFromBenchURL(t, srv.URL), newBenchCache(cachePath, nil))
	deps.GPUModel = ""
	if got := RunBootBenchmark(context.Background(), deps); got.TurnSeconds <= 0 {
		t.Fatalf("expected a real measurement, got %+v", got)
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Errorf("cache file written despite empty GPUModel: %v", err)
	}
}

// PRODUCT CONTRACT (decision 7, "台帳は MeasuredVariants"): when the disk
// cache has nothing — bench.json may be cleared at any time — the state
// ledger answers, without a request.
func TestRunBootBenchmark_TheLedgerAnswersWhenTheCacheMisses(t *testing.T) {
	port, hits := refusingEngine(t)
	deps := cachedDeps(t, port, newBenchCache(filepath.Join(t.TempDir(), "bench.json"), nil))
	deps.WarmSlots = func() int { return 2 }
	deps.StoredMeasurement = func() (BenchResult, bool) {
		return BenchResult{TurnSeconds: 70, PrefillTokps: 901.3, DecodeTokps: 45.7, TokensPerSec: 45.7, DepthTokens: 32780}, true
	}
	got := RunBootBenchmark(context.Background(), deps)
	if hits.Load() != 0 {
		t.Errorf("engine hit %d times; the ledger should have answered", hits.Load())
	}
	if !got.Cached || got.TurnSeconds != 70 || got.Outcome != benchOutcomeMeasured || got.ModelID != "qwen3.5-8b" || got.Capacity != 2 {
		t.Errorf("ledger answer = %+v", got)
	}
	// A rerun does not consult it.
	deps.SkipCacheLoad = true
	deps.StoredMeasurement = func() (BenchResult, bool) {
		t.Error("a rerun consulted the stored measurement")
		return BenchResult{}, false
	}
	_ = RunBootBenchmark(context.Background(), deps)
}

// The boot tail's synchronous attempt answers from a stored figure or not at
// all: it must never make the daemon's start wait on a request.
func TestRunBootBenchmark_CacheOnlyNeverMeasures(t *testing.T) {
	port, hits := refusingEngine(t)
	deps := cachedDeps(t, port, newBenchCache(filepath.Join(t.TempDir(), "bench.json"), nil))
	deps.CacheOnly = true
	got := RunBootBenchmark(context.Background(), deps)
	if hits.Load() != 0 {
		t.Errorf("engine hit %d times in cache-only mode", hits.Load())
	}
	if got.Outcome != benchOutcomeEngineNotReady || benchReachedAVerdict(got) {
		t.Errorf("cache-only miss = %+v, want a not-ready non-verdict", got)
	}
}

// An engine whose post-load verification has not settled is not measured:
// it can still restart under the request with another window.
func TestRunBootBenchmark_AnUnverifiedTuningIsNotMeasured(t *testing.T) {
	port, hits := refusingEngine(t)
	deps := cachedDeps(t, port, nil)
	deps.TuningPending = true
	got := RunBootBenchmark(context.Background(), deps)
	if hits.Load() != 0 || got.Outcome != benchOutcomeEngineNotReady {
		t.Errorf("hits=%d outcome=%q, want no request and not-ready", hits.Load(), got.Outcome)
	}
}
