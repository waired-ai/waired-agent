package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/proto/hostfit"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// PRODUCT CONTRACT (decision 1 of docs/decisions/20260913/2245): a second
// sample is taken only when the first lands within ±10 % of the line, and
// the published figure is the mean of the two samples' rates.
func TestMeasureModelSpeed_ASecondSampleOnlyNearTheLine(t *testing.T) {
	near := &fakeOllamaEngine{perSample: func(n int) (float64, float64) {
		if n == 0 {
			return 300, 20 // ~187 s: inside 171-209
		}
		return 320, 22
	}}
	got, err := measureModelSpeed(context.Background(), withDefaults(speedEngine(t, near)))
	if err != nil {
		t.Fatal(err)
	}
	if got.Samples != 2 || near.samples.Load() != 2 {
		t.Fatalf("near the line: Samples=%d requests=%d, want 2", got.Samples, near.samples.Load())
	}
	if math.Abs(got.PrefillTokps-310) > 0.5 || math.Abs(got.DecodeTokps-21) > 0.1 {
		t.Errorf("rates = %.1f / %.2f, want the means 310 / 21", got.PrefillTokps, got.DecodeTokps)
	}
	if want := hostfit.TurnSecondsAt(hostfit.SpeedMeasurementDepthTokens, got.PrefillTokps, got.DecodeTokps); got.TurnSeconds != want {
		t.Errorf("TurnSeconds = %v, want it from the mean rates %v", got.TurnSeconds, want)
	}
	if got.SpreadPct <= 0 {
		t.Errorf("SpreadPct = %v, want the two samples' spread", got.SpreadPct)
	}

	for _, far := range []struct{ prefill, decode float64 }{{252.9, 15.8}, {901.3, 45.7}} {
		f := &fakeOllamaEngine{prefillTokps: far.prefill, decodeTokps: far.decode}
		got, err := measureModelSpeed(context.Background(), withDefaults(speedEngine(t, f)))
		if err != nil {
			t.Fatal(err)
		}
		if got.Samples != 1 || f.samples.Load() != 1 {
			t.Errorf("%.0f s: Samples=%d requests=%d, want 1", got.TurnSeconds, got.Samples, f.samples.Load())
		}
	}
}

// A model that stops before the minimum sample length gets one more try
// with another prompt; a prefill shallower than asked is refused.
func TestMeasureModelSpeed_ShortDecodesAndTruncatedPrompts(t *testing.T) {
	stopsOnce := &fakeOllamaEngine{evalCap: func(n int) int {
		if n == 0 {
			return 20
		}
		return 0
	}}
	got, err := measureModelSpeed(context.Background(), withDefaults(speedEngine(t, stopsOnce)))
	if err != nil {
		t.Fatalf("a model that stopped once then decoded: %v", err)
	}
	if stopsOnce.samples.Load() != 2 || got.Samples != 1 {
		t.Errorf("requests=%d Samples=%d, want 2 requests for 1 counted sample", stopsOnce.samples.Load(), got.Samples)
	}
	bodies := stopsOnce.sampleBodies()
	if len(bodies) == 2 && bodies[0]["prompt"] == bodies[1]["prompt"] {
		t.Error("the retry reused the prompt; a shared prefix would be answered from the engine's cache")
	}

	alwaysShort := &fakeOllamaEngine{evalCap: func(int) int { return 20 }}
	if _, err := measureModelSpeed(context.Background(), withDefaults(speedEngine(t, alwaysShort))); err == nil {
		t.Error("a model that never reached the minimum decode was accepted")
	}

	truncated := &fakeOllamaEngine{maxPromptTokens: 16000}
	if _, err := measureModelSpeed(context.Background(), withDefaults(speedEngine(t, truncated))); err == nil ||
		!strings.Contains(err.Error(), "truncated") {
		t.Errorf("a truncated prefill was accepted: %v", err)
	}
}

// PRODUCT CONTRACT (plan §a): a host whose served window cannot hold the
// canonical prompt measures as deep as it can and records the depth; a
// window too small for a meaningful figure is an error.
func TestMeasureModelSpeed_DepthFollowsTheServedWindow(t *testing.T) {
	if got := modelSpeedDepth(0); got != hostfit.SpeedMeasurementDepthTokens {
		t.Errorf("unknown window: depth %d", got)
	}
	if got := modelSpeedDepth(200704); got != hostfit.SpeedMeasurementDepthTokens {
		t.Errorf("200k window: depth %d", got)
	}
	if got := modelSpeedDepth(32768); got != 30592 {
		t.Errorf("32,768 window: depth %d, want 30592", got)
	}

	f := &fakeOllamaEngine{}
	deps := withDefaults(speedEngine(t, f))
	deps.AppliedWindow = 32768
	got, err := measureModelSpeed(context.Background(), deps)
	if err != nil {
		t.Fatal(err)
	}
	if got.DepthTokens < 30592 || got.DepthTokens > 30592+20 {
		t.Errorf("DepthTokens = %d, want about 30592", got.DepthTokens)
	}
	if want := hostfit.TurnSecondsAt(hostfit.SpeedMeasurementDepthTokens, got.PrefillTokps, got.DecodeTokps); got.TurnSeconds != want {
		t.Errorf("TurnSeconds = %v, want it normalised to the canonical depth (%v)", got.TurnSeconds, want)
	}

	deps.AppliedWindow = 8192
	if _, err := measureModelSpeed(context.Background(), deps); err == nil {
		t.Error("an 8,192 window was measured")
	}
}

// PRODUCT CONTRACT (decision 4): a request past the line publishes that it
// is over, with a lower bound that the prompt alone has already cost, and
// keeps going; the finished figure is judged afresh.
func TestMeasureModelSpeed_PastTheLineReportsABoundAndContinues(t *testing.T) {
	f := &fakeOllamaEngine{hold: make(chan struct{}), sampleArrived: make(chan struct{})}
	deps := withDefaults(speedEngine(t, f))
	deps.LineSeconds = 0.2
	deps.ProgressEvery = 20 * time.Millisecond
	var mu sync.Mutex
	var reports []BenchProgress
	deps.Progress = func(p BenchProgress) {
		mu.Lock()
		reports = append(reports, p)
		mu.Unlock()
	}
	go func() {
		<-f.sampleArrived
		time.Sleep(400 * time.Millisecond)
		close(f.hold)
	}()
	got, err := measureModelSpeed(context.Background(), deps)
	if err != nil {
		t.Fatal(err)
	}
	if got.TurnSeconds <= 0 {
		t.Fatalf("the measurement did not finish past the line: %+v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	var over *BenchProgress
	for i := range reports {
		if reports[i].OverBudget {
			over = &reports[i]
			break
		}
	}
	if over == nil {
		t.Fatalf("no report said over the line: %+v", reports)
	}
	if over.ElapsedSeconds <= 0.2 || over.BudgetSeconds != 0.2 {
		t.Errorf("over report = %+v, want elapsed past the 0.2 s line", *over)
	}
	want := over.ElapsedSeconds * float64(hostfit.SpeedMeasurementDepthTokens) / float64(over.DepthTokens)
	if math.Abs(over.TurnFloorSeconds-want) > 1e-9 {
		t.Errorf("TurnFloorSeconds = %v, want elapsed × 32768 / depth = %v", over.TurnFloorSeconds, want)
	}
	if reports[0].OverBudget || reports[0].ElapsedSeconds > 0.1 {
		t.Errorf("the first report = %+v, want a fresh request inside the line", reports[0])
	}
}

// PRODUCT CONTRACT (plan, "裁定の文面と実装がずれる点" 1): a request that
// stalls to the cap ends as a lower bound — a verdict for now — and is never
// stored: a stopped engine is not a speed.
func TestRunBootBenchmark_AStalledRequestIsABoundThatIsNotStored(t *testing.T) {
	f := &fakeOllamaEngine{hold: make(chan struct{})}
	t.Cleanup(func() { close(f.hold) })
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	cachePath := filepath.Join(t.TempDir(), "bench.json")
	deps := cachedDeps(t, portFromBenchURL(t, srv.URL), newBenchCache(cachePath, nil))
	deps.StallCap = 300 * time.Millisecond

	got := RunBootBenchmark(context.Background(), deps)
	if got.Failed || got.Outcome != benchOutcomeMeasured {
		t.Fatalf("stall = %+v, want a verdict", got)
	}
	if got.TurnSeconds != 0 || got.TurnFloorSeconds <= 0 {
		t.Errorf("stall = TurnSeconds %v / floor %v, want only a bound", got.TurnSeconds, got.TurnFloorSeconds)
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Errorf("a stalled measurement was stored: %v", err)
	}
	if sha, _ := benchMeasurement(got, bootBenchLoopManifests(), deps); sha != "" {
		t.Error("a bound would be filed in the ledger")
	}
}

// PRODUCT CONTRACT (plan §a): this host's own traffic takes the engine back
// at once — the request is cancelled, not finished and discarded — and the
// run is not a verdict.
func TestRunBootBenchmark_ServingTrafficTakesTheEngineBack(t *testing.T) {
	f := &fakeOllamaEngine{hold: make(chan struct{}), sampleArrived: make(chan struct{})}
	t.Cleanup(func() { close(f.hold) })
	deps := speedEngine(t, f)
	// Bounded, so a measurement that failed to give the engine back ends
	// as a stall bound and fails the assertions below instead of hanging.
	deps.StallCap = 5 * time.Second
	var serving atomic.Int64
	deps.ServingInFlight = func() int { return int(serving.Load()) }
	go func() {
		<-f.sampleArrived
		serving.Store(1)
	}()

	start := time.Now()
	got := RunBootBenchmark(context.Background(), deps)
	if got.Outcome != benchOutcomeEngineNotReady || benchReachedAVerdict(got) {
		t.Fatalf("yield = %+v, want not-ready and no verdict", got)
	}
	if !strings.Contains(got.Err, "serving traffic") {
		t.Errorf("Err = %q", got.Err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("the yield took %v; the request should be cancelled within a poll", time.Since(start))
	}
	deadline := time.Now().Add(waitBackstop)
	for f.cancelled.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if f.cancelled.Load() == 0 {
		t.Error("the engine never saw the request cancelled")
	}
}

// A model switch mid-request stops the measurement: the figure would
// describe a model this host no longer serves.
func TestRunBootBenchmark_ASwitchStopsTheMeasurement(t *testing.T) {
	f := &fakeOllamaEngine{hold: make(chan struct{}), sampleArrived: make(chan struct{})}
	t.Cleanup(func() { close(f.hold) })
	cachePath := filepath.Join(t.TempDir(), "bench.json")
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	deps := cachedDeps(t, portFromBenchURL(t, srv.URL), newBenchCache(cachePath, nil))
	var selected atomic.Value
	selected.Store(deps.VariantID)
	deps.Selected = func() string { return selected.Load().(string) }
	go func() {
		<-f.sampleArrived
		selected.Store("another-variant")
	}()
	got := RunBootBenchmark(context.Background(), deps)
	if got.Outcome != benchOutcomeEngineNotReady || !strings.Contains(got.Err, "switched") {
		t.Fatalf("switch = %+v, want not-ready naming the switch", got)
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Errorf("a stopped measurement was stored: %v", err)
	}
}

func TestAwaitServingIdle(t *testing.T) {
	var serving atomic.Int64
	serving.Store(1)
	deps := BenchDeps{Now: time.Now, ServingInFlight: func() int { return int(serving.Load()) }}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if awaitServingIdle(ctx, deps, 200*time.Millisecond) {
		t.Error("returned idle while traffic never stopped")
	}
	serving.Store(0)
	if !awaitServingIdle(context.Background(), deps, 200*time.Millisecond) {
		t.Error("never returned idle")
	}
}

// The vLLM path times the stream: prefill from request to first token,
// decode from the first token to the last, token counts from usage — and it
// asks for min_tokens so a model that would stop early still decodes.
func TestMeasureModelSpeed_VLLMTimesTheStream(t *testing.T) {
	var minTokens atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		stream, _ := body["stream"].(bool)
		if !stream {
			fmt.Fprint(w, `{"usage":{"completion_tokens":8},"choices":[{"message":{"content":"."}}]}`)
			return
		}
		maxTokens := int(body["max_tokens"].(float64))
		msgs := body["messages"].([]any)
		prompt := msgs[0].(map[string]any)["content"].(string)
		promptTokens := promptLines(prompt) * 20
		if v, ok := body["min_tokens"].(float64); ok && maxTokens > 1 {
			minTokens.Store(int64(v))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		if maxTokens > 1 {
			time.Sleep(30 * time.Millisecond)
		}
		for i := 0; i < maxTokens; i++ {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"x"}}]}`+"\n\n")
			fl.Flush()
			if maxTokens > 1 {
				time.Sleep(time.Millisecond)
			}
		}
		fmt.Fprintf(w, `data: {"choices":[],"usage":{"prompt_tokens":%d,"completion_tokens":%d}}`+"\n\n", promptTokens, maxTokens)
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	t.Cleanup(srv.Close)
	deps := withDefaults(BenchDeps{
		EngineKind: signer.InferenceTypeVLLM, EnginePort: portFromBenchURL(t, srv.URL),
		EngineModel: "Qwen/Qwen3-8B", Logger: discardLogger(),
	})
	got, err := measureModelSpeed(context.Background(), deps)
	if err != nil {
		t.Fatal(err)
	}
	if got.Method != signer.BenchmarkMethodOpenAIStreamTTFT {
		t.Errorf("Method = %q", got.Method)
	}
	if minTokens.Load() != int64(hostfit.SpeedMeasurementCompletionTokens) {
		t.Errorf("min_tokens = %d, want %d", minTokens.Load(), hostfit.SpeedMeasurementCompletionTokens)
	}
	if got.PrefillTokps <= 0 || got.DecodeTokps <= 0 || got.TurnSeconds <= 0 {
		t.Errorf("figure = %+v", got)
	}
	if !modelSpeedDepthAccepted(hostfit.SpeedMeasurementDepthTokens, got.DepthTokens) {
		t.Errorf("DepthTokens = %d", got.DepthTokens)
	}
}

// errors is imported for the sentinel checks below.
func TestModelSpeedErrorsAreDistinct(t *testing.T) {
	if errors.Is(errYieldedToTraffic, errSelectionChanged) {
		t.Error("the two stop reasons must be told apart")
	}
}

func withDefaults(d BenchDeps) BenchDeps {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.HTTPClient == nil {
		d.HTTPClient = http.DefaultClient
	}
	if d.Logger == nil {
		d.Logger = discardLogger()
	}
	return d
}
