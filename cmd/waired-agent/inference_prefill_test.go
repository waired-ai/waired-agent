package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/proto/signer"
)

func TestPrefillRungsFor(t *testing.T) {
	cases := []struct {
		name   string
		window int
		want   []int
	}{
		{"unknown window attempts every rung", 0, prefillRungs},
		{"a 131k window holds them all", 131072, prefillRungs},
		{"the margin is left clear", 32768 + prefillDepthMarginTokens, prefillRungs},
		{"one token short of the top rung drops it", 32768 + prefillDepthMarginTokens - 1, []int{4096, 8192}},
		{"a 16k window keeps the two shallow rungs", 16384, []int{4096, 8192}},
		{"a tiny window holds none", 4096, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := prefillRungsFor(c.window)
			if fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Errorf("prefillRungsFor(%d) = %v, want %v", c.window, got, c.want)
			}
		})
	}
}

// TestPrefillLinesFor is the arithmetic behind the calibration. The
// tokens-per-line rate is the MODEL's to decide — the same synthetic text
// measured 35 tokens/line on the #625 harness and 51.6 on a 0.8 B model —
// so the line count has to be derived from a measured rate, not a constant
// (docs/knowledges/20260805/1830-ollama-prompt-depth-two-traps.md).
func TestPrefillLinesFor(t *testing.T) {
	cases := []struct {
		depth         int
		tokensPerLine float64
		want          int
	}{
		{4096, 51.6, 80},  // ceil(79.4)
		{4096, 35, 118},   // ceil(117.0) — the constant the note says is wrong for other families
		{8192, 51.6, 159}, // ceil(158.8)
		{1024, 51.6, 20},  // ceil(19.8)
		{4096, 0, 118},    // no calibration yet → the #625 figure stands in
		{0, 51.6, 1},      // never zero lines
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%d@%.1f", c.depth, c.tokensPerLine), func(t *testing.T) {
			if got := prefillLinesFor(c.depth, c.tokensPerLine); got != c.want {
				t.Errorf("prefillLinesFor(%d, %v) = %d, want %d", c.depth, c.tokensPerLine, got, c.want)
			}
		})
	}
}

// TestPrefillDepthAccepted is the read-back guard, the same 0.7-1.5 band
// the host-cutoff probe applies: the engine silently truncates a prompt
// that overflows its window, and a truncated prefill measures the
// truncation rather than the host.
func TestPrefillDepthAccepted(t *testing.T) {
	cases := []struct {
		want, got int
		ok        bool
	}{
		{8192, 8192, true},
		{8192, 8000, true},
		{8192, 5735, true},  // 0.70
		{8192, 5700, false}, // just under
		{8192, 12288, true}, // 1.50
		{8192, 12300, false},
		// The measured trap: asking for 21,000 with the wrong
		// tokens-per-line prefilled 11,526 — 55 %, and it reads FAST.
		{21000, 11526, false},
		{8192, 0, false},
		{0, 8192, false},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%d_got_%d", c.want, c.got), func(t *testing.T) {
			if got := prefillDepthAccepted(c.want, c.got); got != c.ok {
				t.Errorf("prefillDepthAccepted(%d, %d) = %v, want %v", c.want, c.got, got, c.ok)
			}
		})
	}
}

func TestPrefillSettled(t *testing.T) {
	cases := []struct {
		name       string
		samples    []float64
		wantMedian float64
		wantOK     bool
	}{
		{"nothing", nil, 0, false},
		{"one reading never settles", []float64{100}, 100, false},
		// The host-cutoff probe's calibration: idle runs sat within ±2 %.
		{"two close readings settle", []float64{100, 102}, 101, true},
		// And the one contended run in that calibration landed +21 %.
		{"a contended reading does not settle", []float64{100, 121}, 110.5, false},
		{"three readings take the middle", []float64{95, 100, 101}, 100, true},
		{"a zero median cannot settle", []float64{0, 0}, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			median, _, ok := prefillSettled(c.samples)
			if median != c.wantMedian || ok != c.wantOK {
				t.Errorf("= (%v, _, %v), want (%v, %v)", median, ok, c.wantMedian, c.wantOK)
			}
		})
	}
}

// fakePrefillEngine answers a prompt of N lines. It takes and records the
// LINE count it was actually asked for, converts it to tokens at its own
// tokens-per-line rate, and advances the clock by however long that prefill
// would take — so a fake that ignored the depth could not pass the
// assertions below (CLAUDE.md §Test discipline).
type fakePrefillEngine struct {
	clk           *testClock
	tokensPerLine float64
	// rate answers with the tok/s for this call. took overrides the derived
	// duration (used to model a run that never finished).
	rate  func(call, tokens int) (tokps float64, took time.Duration, err error)
	lines []int
}

func (f *fakePrefillEngine) sample(_ context.Context, lines int) (float64, int, error) {
	f.lines = append(f.lines, lines)
	tokens := int(float64(lines) * f.tokensPerLine)
	tokps, took, err := f.rate(len(f.lines), tokens)
	if err == nil && took == 0 && tokps > 0 {
		took = time.Duration(float64(tokens) / tokps * float64(time.Second))
	}
	f.clk.advance(took)
	if err != nil {
		return 0, 0, err
	}
	return tokps, tokens, nil
}

// prefillHost builds a fixture for a host of a given speed. tokensPerLine
// is deliberately NOT depthPromptTokensPerLine: the measured figure for a
// real model was 51.6, and a run that used the constant landed at 55 % of
// the depth it asked for.
func prefillHost(t *testing.T, tokps, tokensPerLine float64) (*fakePrefillEngine, PrefillDeps) {
	t.Helper()
	clk := &testClock{t: time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)}
	eng := &fakePrefillEngine{
		clk:           clk,
		tokensPerLine: tokensPerLine,
		rate: func(int, int) (float64, time.Duration, error) {
			return tokps, 0, nil
		},
	}
	deps := PrefillDeps{
		EngineKind:    signer.InferenceTypeOllama,
		EngineModel:   "qwen3:8b",
		VariantID:     "q4-gguf",
		AppliedWindow: 131072,
		Nonce:         "n",
		Budget:        prefillMeasureBudget,
		Now:           clk.Now,
		Sample:        eng.sample,
	}
	return eng, deps
}

func rungDepths(m PrefillMeasurement) []int {
	out := make([]int, 0, len(m.Rungs))
	for _, r := range m.Rungs {
		out = append(out, r.Depth)
	}
	return out
}

// TestMeasurePrefillRate_EveryHostClimbsTheSameRungs is the property the
// first draft of this file got wrong and this one exists to hold: the
// depths are CONSTANTS, so a fast host and a slow host are measured at the
// same places.
//
// A depth chosen per host makes the depth a confounder of the very
// quantity being compared. Prefill throughput falls with depth — 833 tok/s
// at 11,526 tokens against 583 at 21,247, one machine, one model
// (docs/knowledges/20260805/1830-ollama-prompt-depth-two-traps.md) — so
// measuring a fast peer deeper and a slow one shallower biases the
// comparison in favour of the slow peer.
//
// Product contract — the fixed-probe rule hostfit.HostCutoffProbeDepthTokens
// already states ("the point of a FIXED probe is comparability"), applied
// to the served model.
func TestMeasurePrefillRate_EveryHostClimbsTheSameRungs(t *testing.T) {
	fast, fastDeps := prefillHost(t, 690, 51.6)
	slow, slowDeps := prefillHost(t, 690.0/12, 51.6) // ~57 tok/s
	slowDeps.Budget = time.Hour                      // let it reach every rung too

	gotFast := MeasurePrefillRate(context.Background(), fastDeps)
	gotSlow := MeasurePrefillRate(context.Background(), slowDeps)

	if fmt.Sprint(rungDepths(gotFast)) != fmt.Sprint(prefillRungs) {
		t.Errorf("fast host reached %v, want %v", rungDepths(gotFast), prefillRungs)
	}
	if fmt.Sprint(rungDepths(gotSlow)) != fmt.Sprint(prefillRungs) {
		t.Errorf("slow host reached %v, want %v", rungDepths(gotSlow), prefillRungs)
	}
	// The line counts differ only by the model's own tokens-per-line, which
	// is the same here — so the two hosts sent the same prompts.
	if fmt.Sprint(fast.lines) != fmt.Sprint(slow.lines) {
		t.Errorf("hosts were asked different prompts:\n  fast=%v\n  slow=%v", fast.lines, slow.lines)
	}
}

// TestMeasurePrefillRate_CalibratesThePromptFromTheWarmUp covers the trap
// the measured note documents: the synthetic prompt's tokens-per-line is
// the model's to decide, and building rungs from the #625 constant put a
// prompt at 55 % of the requested depth — which reads FAST.
func TestMeasurePrefillRate_CalibratesThePromptFromTheWarmUp(t *testing.T) {
	eng, deps := prefillHost(t, 690, 51.6)
	got := MeasurePrefillRate(context.Background(), deps)
	if got.Failed {
		t.Fatalf("Failed: %s", got.Err)
	}
	// Warm-up: 1024 tokens at the UNCALIBRATED constant, since nothing is
	// known yet.
	if want := prefillLinesFor(prefillWarmupTokens, 0); eng.lines[0] != want {
		t.Errorf("warm-up asked for %d lines, want %d", eng.lines[0], want)
	}
	// Everything after it is built from the measured rate. Under the
	// constant the first rung would have been 118 lines and landed at
	// 6,089 tokens — 49 % over the rung it claimed to be.
	if want := prefillLinesFor(4096, 51.6); eng.lines[1] != want {
		t.Errorf("first rung asked for %d lines, want %d (calibrated)", eng.lines[1], want)
	}
	for _, r := range got.Rungs {
		if !prefillDepthAccepted(r.Depth, r.PromptTokens) {
			t.Errorf("rung %d published a %d-token prefill, outside the tolerance band",
				r.Depth, r.PromptTokens)
		}
	}
}

// TestMeasurePrefillRate_StopsShortOfARungTheBudgetCannotHold: a rung is
// only started when the budget can hold one sample of it, estimated from
// the rung just measured. The host publishes what it reached.
func TestMeasurePrefillRate_StopsShortOfARungTheBudgetCannotHold(t *testing.T) {
	// 117 tok/s — the M4 16 GB peer. 4,096 takes 35 s a sample; 8,192 takes
	// 70; 32,768 takes 280 and cannot fit in what is left.
	_, deps := prefillHost(t, 117, 51.6)
	got := MeasurePrefillRate(context.Background(), deps)
	if got.Failed {
		t.Fatalf("Failed: %s", got.Err)
	}
	depths := rungDepths(got)
	if len(depths) == 0 {
		t.Fatal("want at least the shallowest rung")
	}
	if depths[0] != prefillRungs[0] {
		t.Errorf("reached %v, want the shallowest rung first", depths)
	}
	for _, d := range depths {
		if d == 32768 {
			t.Errorf("reached %v: the top rung does not fit a 3-minute budget at 117 tok/s", depths)
		}
	}
}

// TestMeasurePrefillRate_SlowestPeerStillGetsACheckedReading is the reason
// the shallowest rung is 4,096 rather than 8,192. The 96 GB APU peer of
// waired-agent#1082 measured 54 tok/s; at 8,192 it could take one reading
// inside the budget and would have nothing to check it against.
func TestMeasurePrefillRate_SlowestPeerStillGetsACheckedReading(t *testing.T) {
	_, deps := prefillHost(t, 54, 51.6)
	got := MeasurePrefillRate(context.Background(), deps)
	if got.Failed {
		t.Fatalf("Failed: %s", got.Err)
	}
	if len(got.Rungs) == 0 {
		t.Fatal("the slowest measured peer must still reach the shallowest rung")
	}
	if got.Rungs[0].Depth != 4096 {
		t.Errorf("first rung = %d, want 4096", got.Rungs[0].Depth)
	}
	if got.Rungs[0].Samples < 2 {
		t.Errorf("Samples = %d, want >= 2: a single reading is never checked against another",
			got.Rungs[0].Samples)
	}
}

// TestMeasurePrefillRate_BoundWhenTheFirstRungNeverFinishes is the #579
// shape (owner ruling, 2026-08-09): a host that could not finish publishes
// what IS known — it did not get through Depth tokens in that time, so it
// is no faster than depth/elapsed. A fact, not an estimate.
func TestMeasurePrefillRate_BoundWhenTheFirstRungNeverFinishes(t *testing.T) {
	eng, deps := prefillHost(t, 100, 51.6)
	eng.rate = func(call, tokens int) (float64, time.Duration, error) {
		if call == 1 {
			return 100, 0, nil // the warm-up finishes and calibrates
		}
		return 0, 200 * time.Second, context.DeadlineExceeded
	}
	got := MeasurePrefillRate(context.Background(), deps)
	if got.Failed {
		t.Fatalf("Failed: %s", got.Err)
	}
	if len(got.Rungs) != 1 || !got.Rungs[0].Bound {
		t.Fatalf("want one bounded rung, got %+v", got.Rungs)
	}
	if want := float64(4096) / 200; got.Rungs[0].Tokps != want {
		t.Errorf("Tokps = %v, want %v (depth/elapsed)", got.Rungs[0].Tokps, want)
	}
	if got.Rungs[0].Samples != 0 {
		t.Errorf("Samples = %d, want 0 — a bound is not a sample", got.Rungs[0].Samples)
	}
	if !got.Known() {
		t.Error("a bound still orders a host; it is not 'no measurement'")
	}
}

// TestMeasurePrefillRate_DropsARungItDidNotActuallyPrefill: an engine that
// truncated the prompt measured its own truncation. That rung is not
// published, and the shallower rungs already taken still are.
func TestMeasurePrefillRate_DropsARungItDidNotActuallyPrefill(t *testing.T) {
	eng, deps := prefillHost(t, 690, 51.6)
	// The window silently caps the prompt at ~5,000 tokens, so the 8,192
	// rung comes back at 61 % of what it asked for.
	base := eng.sample
	deps.Sample = func(ctx context.Context, lines int) (float64, int, error) {
		tokps, tokens, err := base(ctx, lines)
		if tokens > 5000 {
			tokens = 5000
		}
		return tokps, tokens, err
	}
	got := MeasurePrefillRate(context.Background(), deps)
	if got.Failed {
		t.Fatalf("Failed: %s", got.Err)
	}
	if fmt.Sprint(rungDepths(got)) != fmt.Sprint([]int{4096}) {
		t.Errorf("reached %v, want only the rung that was actually prefilled", rungDepths(got))
	}
}

// TestMeasurePrefillRate_EngineFailureIsNotSlowness keeps a broken engine
// from being published as a slow one. The warm-up is what calibrates, so
// nothing can be measured or bounded without it.
func TestMeasurePrefillRate_EngineFailureIsNotSlowness(t *testing.T) {
	eng, deps := prefillHost(t, 690, 51.6)
	eng.rate = func(int, int) (float64, time.Duration, error) {
		return 0, time.Second, fmt.Errorf("HTTP 500: out of memory")
	}
	got := MeasurePrefillRate(context.Background(), deps)
	if !got.Failed {
		t.Fatal("want Failed=true when the warm-up itself errors")
	}
	if got.Known() {
		t.Errorf("a failed measurement must not read as a rate; got %+v", got)
	}
	if !strings.Contains(got.Err, "out of memory") {
		t.Errorf("Err = %q, want the engine's own reason", got.Err)
	}
}

func TestMeasurePrefillRate_WindowTooSmallForAnyRung(t *testing.T) {
	eng, deps := prefillHost(t, 690, 51.6)
	deps.AppliedWindow = 4096
	got := MeasurePrefillRate(context.Background(), deps)
	if !got.Failed {
		t.Fatal("want Failed=true")
	}
	if len(eng.lines) != 0 {
		t.Errorf("engine was asked %d prompts; nothing should have been sent", len(eng.lines))
	}
}

func TestMeasurePrefillRate_UnknownEngineHasNoSampler(t *testing.T) {
	got := MeasurePrefillRate(context.Background(), PrefillDeps{
		EngineKind: "some-future-engine", EnginePort: 1, EngineModel: "m",
	})
	if !got.Failed || got.Known() {
		t.Errorf("got %+v, want a failure", got)
	}
}

// --- the two real samplers -------------------------------------------
//
// Table-tested against a fake engine rather than only through the scripted
// seam above: a seam whose real implementation no test calls is a hole
// (CLAUDE.md §Test discipline).

func TestOllamaPrefillSampler_ReadsTheEnginesOwnCounters(t *testing.T) {
	var gotNumPredict []int
	var gotPrompts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Prompt  string `json:"prompt"`
			Options struct {
				NumPredict int `json:"num_predict"`
			} `json:"options"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotNumPredict = append(gotNumPredict, req.Options.NumPredict)
		gotPrompts = append(gotPrompts, req.Prompt)
		w.Header().Set("Content-Type", "application/json")
		// 8,000 prompt tokens in 4 s = 2,000 tok/s. The wall clock here is
		// ~0, so a sampler reading it instead would report nonsense.
		fmt.Fprint(w, `{"prompt_eval_count":8000,"prompt_eval_duration":4000000000,"eval_count":1,"eval_duration":1000000}`)
	}))
	t.Cleanup(srv.Close)

	sample := ollamaPrefillSampler(http.DefaultClient, srv.URL, "qwen3:8b", "nonce")
	tokps, tokens, err := sample(context.Background(), 160)
	if err != nil {
		t.Fatalf("sample: %v", err)
	}
	if tokps != 2000 {
		t.Errorf("tokps = %v, want 2000 (the engine's counters, not the wall clock)", tokps)
	}
	if tokens != 8000 {
		t.Errorf("promptTokens = %d, want 8000 (read back, not the estimate)", tokens)
	}
	if gotNumPredict[0] != 1 {
		t.Errorf("num_predict = %d, want 1 — every decoded token measures something else", gotNumPredict[0])
	}

	// A second call must not share a prefix with the first, or the engine
	// serves it from cache and the reading measures nothing.
	if _, _, err := sample(context.Background(), 160); err != nil {
		t.Fatalf("second sample: %v", err)
	}
	if gotPrompts[0] == gotPrompts[1] {
		t.Error("two samples sent the same prompt; the second would be a cache hit")
	}
}

func TestOllamaPrefillSampler_RefusesAResponseWithoutCounters(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"eval_count":1,"eval_duration":1000000}`)
	}))
	t.Cleanup(srv.Close)
	sample := ollamaPrefillSampler(http.DefaultClient, srv.URL, "qwen3:8b", "nonce")
	if _, _, err := sample(context.Background(), 160); err == nil {
		t.Fatal("want an error: a response with no prefill counters measures nothing")
	}
}

func TestOpenAIPrefillSampler_DividesPromptTokensByTheWallClock(t *testing.T) {
	var gotMaxTokens []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			MaxTokens int `json:"max_tokens"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotMaxTokens = append(gotMaxTokens, req.MaxTokens)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"usage":{"prompt_tokens":20000,"completion_tokens":1},"choices":[{"message":{"content":"."}}]}`)
	}))
	t.Cleanup(srv.Close)

	// 20,000 prompt tokens over a 10 s wall clock = 2,000 tok/s.
	sample := openAIPrefillSampler(http.DefaultClient, srv.URL, "Qwen/Qwen3-8B", "nonce",
		fakeNow(time.Unix(1_700_000_000, 0), 10*time.Second))
	tokps, tokens, err := sample(context.Background(), 400)
	if err != nil {
		t.Fatalf("sample: %v", err)
	}
	if tokps != 2000 {
		t.Errorf("tokps = %v, want 2000", tokps)
	}
	if tokens != 20000 {
		t.Errorf("promptTokens = %d, want 20000 (usage, not the estimate)", tokens)
	}
	if gotMaxTokens[0] != 1 {
		t.Errorf("max_tokens = %d, want 1", gotMaxTokens[0])
	}
}

func TestOpenAIPrefillSampler_CarriesTheEnginesOwnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"engine is out of memory"}`, http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	sample := openAIPrefillSampler(http.DefaultClient, srv.URL, "m", "n", nil)
	if _, _, err := sample(context.Background(), 160); err == nil {
		t.Fatal("want an error on a 500")
	} else if !strings.Contains(err.Error(), "out of memory") {
		t.Errorf("err = %v, want the engine's own reason", err)
	}
}

// TestMeasurePrefillRate_YieldsToServingTraffic is a defect CI caught: on
// a CPU-only host a 4,096-token rung took 86.5 s at 46 tok/s, and two
// integration cases timed out behind it at 45 s. The engine claim answers
// "is the engine free" ONCE, at the start; a request that arrives after
// it queues behind the whole rung.
func TestMeasurePrefillRate_YieldsToServingTraffic(t *testing.T) {
	t.Run("between rungs, keeping what it measured", func(t *testing.T) {
		eng, deps := prefillHost(t, 690, 51.6)
		busy := false
		deps.Yield = func() bool { return busy }
		base := eng.sample
		deps.Sample = func(ctx context.Context, lines int) (float64, int, error) {
			// Traffic arrives once the first rung is done.
			if len(eng.lines) >= 3 {
				busy = true
			}
			return base(ctx, lines)
		}
		got := MeasurePrefillRate(context.Background(), deps)
		if got.Failed {
			t.Fatalf("yielding is not a failure: %s", got.Err)
		}
		if len(got.Rungs) == 0 {
			t.Fatal("a rung that finished before the traffic arrived is still a measurement")
		}
		if len(got.Rungs) == len(prefillRungs) {
			t.Error("it climbed the whole ladder while something else wanted the engine")
		}
	})

	t.Run("before anything completes, it is not a result", func(t *testing.T) {
		_, deps := prefillHost(t, 690, 51.6)
		deps.Yield = func() bool { return true }
		got := MeasurePrefillRate(context.Background(), deps)
		if got.Failed {
			t.Errorf("nothing was measured, but nothing failed either: %s", got.Err)
		}
		if len(got.Rungs) != 0 {
			t.Errorf("got %d rungs, want none", len(got.Rungs))
		}
		if got.Known() {
			t.Error("an unmeasured host must not read as measured")
		}
	})

	t.Run("a nil Yield never yields", func(t *testing.T) {
		_, deps := prefillHost(t, 690, 51.6)
		deps.Yield = nil
		if got := MeasurePrefillRate(context.Background(), deps); len(got.Rungs) != len(prefillRungs) {
			t.Errorf("got %d rungs, want the whole ladder", len(got.Rungs))
		}
	})
}
