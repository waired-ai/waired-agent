package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDepthStagePlan(t *testing.T) {
	cases := []struct {
		name       string
		appliedCtx int
		want       []int
	}{
		// Full floor window: all three canonical stages.
		{"floor-window", 200704, []int{65536, 131072, 198656}},
		// The #625 no-spill window: 64k intact, the 131k stage clipped
		// to ctx − margin, 200k dropped as a duplicate after clipping.
		{"anchor-nospill-114k", 114688, []int{65536, 112640}},
		// Tiny window: single clipped stage.
		{"small-64k", 65536, []int{63488}},
		// Unknown ctx (0): no plan — never guess depths.
		{"unknown", 0, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := depthStagePlan(c.appliedCtx)
			if len(got) != len(c.want) {
				t.Fatalf("plan = %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("plan = %v, want %v", got, c.want)
				}
			}
		})
	}
}

func TestDepthBenchPrompt(t *testing.T) {
	p1 := depthBenchPrompt(65536, depthPromptTokensPerLine, "nonce-a")
	p2 := depthBenchPrompt(65536, depthPromptTokensPerLine, "nonce-b")
	// The #625 calibration for this line shape is ~2.5 chars/token
	// (dense digits tokenize short); the estimate only needs the right
	// ballpark — the real depth is read back from prompt_eval_count.
	if len(p1) < 65536*2 || len(p1) > 65536*4 {
		t.Errorf("prompt length %d chars implausible for 65536 tokens", len(p1))
	}
	// No shared prefix between runs — different nonces must diverge
	// immediately, or the engine's prompt cache poisons the prefill
	// measurement.
	limit := 64
	if p1[:limit] == p2[:limit] {
		t.Error("prompts share a prefix; prompt caching would skew prefill")
	}
}

// fakeDepthEngine answers /api/generate with canned eval counters.
type fakeDepthEngine struct {
	calls    int
	failFrom int // 1-based call index that starts failing; 0 = never
	// failBody is what a failing call answers with. Empty means a bare
	// 500 with no body, which is what this fake sent before
	// waired-agent#1058 — and why no depth test had ever exercised a
	// classifiable engine error.
	failBody string
}

func (f *fakeDepthEngine) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			http.NotFound(w, r)
			return
		}
		f.calls++
		if f.failFrom > 0 && f.calls >= f.failFrom {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(f.failBody))
			return
		}
		var req struct {
			Prompt string `json:"prompt"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		promptTokens := len(req.Prompt) / 4
		_ = json.NewEncoder(w).Encode(map[string]any{
			"done":                 true,
			"prompt_eval_count":    promptTokens,
			"prompt_eval_duration": int64(promptTokens) * 500_000, // 2000 tok/s
			"eval_count":           200,
			"eval_duration":        int64(2_000_000_000), // 100 tok/s
		})
	}
}

func TestRunDepthBenchmark(t *testing.T) {
	f := &fakeDepthEngine{}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	port := portOf(t, srv.URL)

	res := RunDepthBenchmark(context.Background(), DepthBenchDeps{
		EnginePort:    port,
		EngineModel:   "test:tag",
		VariantID:     "mtp-q4",
		ContextLength: 200704,
		KVCacheType:   "q8_0",
		HTTPClient:    srv.Client(),
		Logger:        testLogger(),
		Nonce:         "t1",
	})
	if !res.Completed || len(res.Stages) != 3 {
		t.Fatalf("result = %+v, want 3 completed stages", res)
	}
	for _, st := range res.Stages {
		if st.Failed {
			t.Errorf("stage %d failed: %s", st.TargetTokens, st.Err)
		}
		if st.PrefillTokps < 1500 || st.PrefillTokps > 2500 {
			t.Errorf("stage %d prefill = %.0f tok/s, want ≈2000", st.TargetTokens, st.PrefillTokps)
		}
		if st.DecodeTokps < 95 || st.DecodeTokps > 105 {
			t.Errorf("stage %d decode = %.0f tok/s, want ≈100", st.TargetTokens, st.DecodeTokps)
		}
		if st.PromptTokens <= 0 {
			t.Errorf("stage %d PromptTokens not read back", st.TargetTokens)
		}
	}
}

func TestRunDepthBenchmark_PartialOnFailure(t *testing.T) {
	f := &fakeDepthEngine{failFrom: 2}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	res := RunDepthBenchmark(context.Background(), DepthBenchDeps{
		EnginePort:    portOf(t, srv.URL),
		EngineModel:   "test:tag",
		ContextLength: 200704,
		HTTPClient:    srv.Client(),
		Logger:        testLogger(),
		Nonce:         "t2",
	})
	if res.Completed {
		t.Fatal("Completed should be false after a stage failure")
	}
	if len(res.Stages) != 2 {
		t.Fatalf("stages = %d, want 2 (one ok + the failed one, rest aborted)", len(res.Stages))
	}
	if res.Stages[0].Failed || !res.Stages[1].Failed {
		t.Errorf("stage failure placement wrong: %+v", res.Stages)
	}
	// A bare 500 is not an out-of-memory, and must not be reported as
	// one: an unexplained failure is still an unexplained failure.
	if res.Stages[1].OutOfMemory {
		t.Error("a bodyless 500 was classified as an accelerator out-of-memory")
	}
}

// depthOOMBody is the reply measured on sv-mag (RTX PRO 4000
// Blackwell, ollama 0.32.13) serving qwen3.8:27b-mtp-q4_K_M-wb2048 for a
// ~2,000-token prompt, 2026-08-27 — the same fixture
// internal/runtime/ollama_fitfailure_test.go pins the classifier
// against (waired-agent#1038).
const depthOOMBody = `{"error":"an error was encountered while running the model: CUDA error\nCUDA error: out of memory"}`

// TestRunDepthBenchmark_OutOfMemoryIsEvidenceNotSilence is
// waired-agent#1058. The sweep that PROVES this window does not work on
// this host used to record the stage as merely failed, which every
// consumer skipped — so the strongest result the benchmark can produce
// was read as no result at all.
func TestRunDepthBenchmark_OutOfMemoryIsEvidenceNotSilence(t *testing.T) {
	f := &fakeDepthEngine{failFrom: 1, failBody: depthOOMBody}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	var reported []string
	res := RunDepthBenchmark(context.Background(), DepthBenchDeps{
		EnginePort:    portOf(t, srv.URL),
		EngineModel:   "test:tag",
		ContextLength: 200704,
		HTTPClient:    srv.Client(),
		Logger:        testLogger(),
		Nonce:         "t3",
		OnFitFailure:  func(d string) { reported = append(reported, d) },
	})

	if len(res.Stages) != 1 {
		t.Fatalf("stages = %d, want 1 — a stage that ran out of memory ends the sweep", len(res.Stages))
	}
	if !res.Stages[0].Failed || !res.Stages[0].OutOfMemory {
		t.Fatalf("stage = %+v, want Failed and OutOfMemory", res.Stages[0])
	}
	if res.Completed {
		t.Error("Completed should stay false")
	}
	// The fit ladder is told, on the same route a served request's
	// out-of-memory takes. Without this the evidence reaches a log line
	// and nothing else.
	if len(reported) != 1 {
		t.Fatalf("OnFitFailure fired %d times, want 1", len(reported))
	}
	if !strings.Contains(reported[0], "64k") {
		t.Errorf("the report does not name the depth: %q", reported[0])
	}

	// And the sweep says which of the two things happened.
	target, ok := depthOutOfMemory(&res)
	if !ok || target != 65536 {
		t.Errorf("depthOutOfMemory = (%d, %v), want (65536, true)", target, ok)
	}
	if _, _, ok := worstCompletedDepthDecode(&res); ok {
		t.Error("a sweep with no completed stage must still report no rate")
	}
}

// TestRunDepthBenchmark_OutOfMemoryWithNoHandlerDoesNotPanic keeps the
// seam optional: every existing caller that passes no OnFitFailure —
// and every test above — must keep working.
func TestRunDepthBenchmark_OutOfMemoryWithNoHandlerDoesNotPanic(t *testing.T) {
	f := &fakeDepthEngine{failFrom: 1, failBody: depthOOMBody}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	res := RunDepthBenchmark(context.Background(), DepthBenchDeps{
		EnginePort: portOf(t, srv.URL), EngineModel: "test:tag",
		ContextLength: 200704, HTTPClient: srv.Client(),
		Logger: testLogger(), Nonce: "t4",
	})
	if len(res.Stages) != 1 || !res.Stages[0].OutOfMemory {
		t.Fatalf("stages = %+v", res.Stages)
	}
}

func TestRunDepthBenchmark_SkipPaths(t *testing.T) {
	res := RunDepthBenchmark(context.Background(), DepthBenchDeps{EnginePort: 0, ContextLength: 200704})
	if res.Completed || len(res.Stages) != 0 {
		t.Errorf("port 0 must skip: %+v", res)
	}
	res = RunDepthBenchmark(context.Background(), DepthBenchDeps{EnginePort: 1234, ContextLength: 0})
	if res.Completed || len(res.Stages) != 0 {
		t.Errorf("unknown ctx must skip: %+v", res)
	}
}

func portOf(t *testing.T, url string) int {
	t.Helper()
	i := strings.LastIndex(url, ":")
	p, err := strconv.Atoi(url[i+1:])
	if err != nil {
		t.Fatalf("port from %q: %v", url, err)
	}
	return p
}

func TestWorstCompletedDepthDecode(t *testing.T) {
	// The third stage is the shape waired-agent#1058 is about, and this
	// function's answer about it is unchanged and still right: a stage
	// that produced no rate contributes no rate. What changed is that
	// it is no longer the ONLY answer — depthOutOfMemory reads the same
	// stage and says what happened, which is asserted below.
	d := &DepthBenchResult{Stages: []DepthStageResult{
		{TargetTokens: 65536, DecodeTokps: 100},
		{TargetTokens: 131072, DecodeTokps: 22},
		{TargetTokens: 198656, Failed: true, OutOfMemory: true, DecodeTokps: 0},
	}}
	dec, target, ok := worstCompletedDepthDecode(d)
	if !ok || dec != 22 || target != 131072 {
		t.Errorf("got (%v, %v, %v), want (22, 131072, true)", dec, target, ok)
	}
	if oomAt, ok := depthOutOfMemory(d); !ok || oomAt != 198656 {
		t.Errorf("depthOutOfMemory = (%d, %v), want (198656, true)", oomAt, ok)
	}
	if _, ok := depthOutOfMemory(&DepthBenchResult{Stages: []DepthStageResult{
		{TargetTokens: 65536, Failed: true},
	}}); ok {
		t.Error("a stage that failed for some other reason is not an out-of-memory")
	}
	if _, ok := depthOutOfMemory(nil); ok {
		t.Error("nil result must report no out-of-memory")
	}
	if _, _, ok := worstCompletedDepthDecode(nil); ok {
		t.Error("nil result must report no data")
	}
	if _, _, ok := worstCompletedDepthDecode(&DepthBenchResult{}); ok {
		t.Error("empty stages must report no data")
	}
}

func TestDepthBenchCache_RoundTripAndKeying(t *testing.T) {
	dir := t.TempDir()
	cache := newBenchCache(dir+"/bench.json", testLogger())

	deps := DepthBenchDeps{
		EngineModel: "m:tag", VariantID: "v", ContextLength: 200704, KVCacheType: "q8_0",
		GPUModel: "RTX", VRAMTotalMB: 24467, DriverVersion: "580", VariantSHA: "sha1",
		EngineVersion: "0.33.2",
	}
	key := depthBenchCacheKey(deps)
	if key == "" {
		t.Fatal("key should be non-empty with full inputs")
	}

	// Different applied window or KV type → different key.
	//
	// The generation ubatch was a third term, so a 512 sweep and a
	// forced-2048 sweep could not collide. Retiring that override
	// (waired-agent#1079) left the engine to size its own batch from the
	// window and the model, which this key already carries.
	d2 := deps
	d2.ContextLength = 114688
	d3 := deps
	d3.KVCacheType = "f16"
	if depthBenchCacheKey(d2) == key || depthBenchCacheKey(d3) == key {
		t.Error("key must change with the applied window / KV type")
	}

	res := DepthBenchResult{
		VariantID: "v", EngineModel: "m:tag", ContextLength: 200704, KVCacheType: "q8_0",
		Stages:    []DepthStageResult{{TargetTokens: 65536, PromptTokens: 65000, PrefillTokps: 2000, DecodeTokps: 100}},
		Completed: true, MeasuredAt: time.Now().UTC(),
	}
	if err := cache.StoreDepth(key, res, "RTX", 24467, "580", "0.33.2"); err != nil {
		t.Fatalf("StoreDepth: %v", err)
	}
	got, hit, err := cache.LoadDepth(key)
	if err != nil || !hit {
		t.Fatalf("LoadDepth: hit=%v err=%v", hit, err)
	}
	if len(got.Stages) != 1 || got.Stages[0].DecodeTokps != 100 || got.ContextLength != 200704 {
		t.Errorf("round-trip mismatch: %+v", got)
	}

	// Incomplete sweeps must never be persisted.
	res.Completed = false
	if err := cache.StoreDepth(depthBenchCacheKey(d2), res, "RTX", 24467, "580", "0.33.2"); err == nil {
		t.Error("StoreDepth should refuse an incomplete sweep")
	}

	// The boot-bench entries must survive a depth store (shared file).
	boot := BenchResult{TokensPerSec: 145, Capacity: 4, VariantID: "v"}
	if err := cache.Store("bootkey", boot, benchCacheHumanMeta{VariantID: "v", GPUModel: "RTX"}, time.Now()); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if _, hit, _ := cache.LoadDepth(key); !hit {
		t.Error("depth entry lost after a boot-bench store")
	}
	if _, _, hit, _ := cache.Load("bootkey"); !hit {
		t.Error("boot entry lost")
	}
}

func TestLongContextBenchFor(t *testing.T) {
	if longContextBenchFor(nil) != nil {
		t.Error("nil sweep → nil wire value")
	}
	if longContextBenchFor(&DepthBenchResult{ContextLength: 200704}) != nil {
		t.Error("stage-less sweep → nil wire value (nothing to show)")
	}
	d := &DepthBenchResult{
		ContextLength: 200704, KVCacheType: "q8_0", Completed: true,
		MeasuredAt: time.Now().UTC(),
		Stages: []DepthStageResult{
			{TargetTokens: 65536, PromptTokens: 65000, PrefillTokps: 2000, DecodeTokps: 100},
			{TargetTokens: 198656, Failed: true},
		},
	}
	got := longContextBenchFor(d)
	if got == nil || len(got.Stages) != 2 || !got.Completed || got.ContextLength != 200704 {
		t.Fatalf("mapping mismatch: %+v", got)
	}
	if got.Stages[0].DecodeTokps != 100 || !got.Stages[1].Failed {
		t.Errorf("stage mapping mismatch: %+v", got.Stages)
	}
}
