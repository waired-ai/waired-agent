package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/buildinfo"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/download"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/hostspeed"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
	"github.com/waired-ai/waired-agent/proto/hostfit"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// hostCutoffFixtureTag is the ollama tag the fixture catalog serves the
// probe model under. Arbitrary; the code must find it through the
// manifest rather than knowing it.
const hostCutoffFixtureTag = "qwen3.5:0.8b-q8_0"

// The counters the fake engine returns, in the two shapes that matter.
// Both are the stage-1 measurement of qwen3.5-0.8b at 21066 tokens on the
// reference host — the CPU-only leg and the 24 GB card leg — expressed as
// the ollama /api/generate response they came from.
var (
	cpuOnlyCounters = map[string]any{
		"prompt_eval_count":    21066,
		"prompt_eval_duration": int64(31_390_000_000), // 671.2 tok/s
		"eval_count":           200,
		"eval_duration":        int64(7_025_000_000), // 28.5 tok/s
	}
	gpuCounters = map[string]any{
		"prompt_eval_count":    21066,
		"prompt_eval_duration": int64(1_040_000_000), // 20,256 tok/s
		"eval_count":           200,
		"eval_duration":        int64(685_000_000), // 292 tok/s
	}
)

// calibrationCounters is what the fake engine answers the line-cost
// calibration with: 50 lines reading 2750 tokens, i.e. 55 per line.
var calibrationCounters = map[string]any{
	"prompt_eval_count":    hostCutoffCalibrationLines * 55,
	"prompt_eval_duration": int64(1_000_000_000),
	"eval_count":           1,
	"eval_duration":        int64(10_000_000),
}

// scaledCounters is gpuCounters slowed down by factor, so a test can hand
// the sampler several DIFFERENT runs and see which one it publishes.
func scaledCounters(factor float64) map[string]any {
	return map[string]any{
		"prompt_eval_count":    21066,
		"prompt_eval_duration": int64(1_040_000_000 * factor),
		"eval_count":           200,
		"eval_duration":        int64(685_000_000 * factor),
	}
}

// hostCutoffEngine is a fake ollama serving the probe tag. It records the
// /api/generate request bodies it was sent — the request shape is half of
// what this file guards — and answers with whatever the test supplied.
type hostCutoffEngine struct {
	mu     sync.Mutex
	bodies []map[string]any
	status int
	// answers are returned one per request; the last one repeats. A
	// single entry is the ordinary case, more than one lets a test drive
	// the sampler or the widen-and-retry.
	answers []map[string]any
	serving []string
	// resident is what /api/ps reports as loaded. Empty is the ordinary
	// case; an entry is a model occupying the host while it is measured.
	resident []string
	// block, when non-nil, holds every /api/generate until it is closed —
	// a slow engine, which is what the host this measures actually is.
	block chan struct{}
}

func (e *hostCutoffEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/tags":
		e.mu.Lock()
		entries := make([]map[string]string, 0, len(e.serving))
		for _, tag := range e.serving {
			entries = append(entries, map[string]string{"name": tag})
		}
		e.mu.Unlock()
		body, _ := json.Marshal(map[string]any{"models": entries})
		_, _ = w.Write(body)
	case "/api/ps":
		e.mu.Lock()
		loaded := make([]map[string]any, 0, len(e.resident))
		for _, tag := range e.resident {
			loaded = append(loaded, map[string]any{"name": tag, "size_vram": 1 << 30})
		}
		e.mu.Unlock()
		body, _ := json.Marshal(map[string]any{"models": loaded})
		_, _ = w.Write(body)
	case "/api/generate":
		raw, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(raw, &parsed)
		e.mu.Lock()
		e.bodies = append(e.bodies, parsed)
		status := e.status
		block := e.block
		answer := map[string]any(nil)
		if n := len(e.answers); n > 0 {
			answer = e.answers[min(len(e.bodies)-1, n-1)]
		}
		e.mu.Unlock()
		if block != nil {
			<-block
		}
		if status != 0 && status/100 != 2 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"model requires more system memory"}`))
			return
		}
		body, _ := json.Marshal(answer)
		_, _ = w.Write(body)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (e *hostCutoffEngine) generateBodies() []map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]map[string]any(nil), e.bodies...)
}

// hostCutoffManifests is a catalog carrying the probe model under an
// ollama variant, plus an unrelated model so nothing can pass by being
// the only entry.
func hostCutoffManifests() []catalog.Manifest {
	return []catalog.Manifest{
		{
			ModelID: hostfit.HostCutoffProbeModelID,
			Variants: []catalog.Variant{{
				VariantID: "q8_0", Format: catalog.FormatOllamaTag,
				RuntimeSupport: []string{catalog.RuntimeOllama},
				Source:         catalog.VariantSource{Type: catalog.SourceOllama, Tag: hostCutoffFixtureTag},
			}},
		},
		{
			ModelID: "some-big-model",
			Variants: []catalog.Variant{{
				VariantID: "q4", Format: catalog.FormatOllamaTag,
				RuntimeSupport: []string{catalog.RuntimeOllama},
				Source:         catalog.VariantSource{Type: catalog.SourceOllama, Tag: "big:q4"},
			}},
		},
	}
}

// hostCutoffProfiler reports a host with no GPU and a named ollama build,
// so a test can move the engine version and watch the measurement follow
// it (waired#668).
func hostCutoffProfiler(t *testing.T, ollamaVersion string) *hardware.Profiler {
	t.Helper()
	return hardware.NewProfiler(t.TempDir(),
		hardware.WithGPU(func(context.Context) ([]hardware.GPU, hardware.Accelerators, error) {
			return nil, hardware.Accelerators{}, nil
		}),
		hardware.WithEngineVersion(func(_ context.Context, name string) (bool, string) {
			return name == "ollama", ollamaVersion
		}),
	)
}

// hostCutoffProvider builds a provider whose engine is the fake above,
// with the probe model already on disk so the measurement is what the
// test exercises rather than a download.
func hostCutoffProvider(t *testing.T, answer map[string]any, status int) (*agentInferenceProvider, *hostCutoffEngine, *int) {
	t.Helper()
	// The line-cost calibration is answered first, then every sample gets
	// the same counters (the last answer repeats).
	return hostCutoffProviderAnswering(t, []map[string]any{calibrationCounters, answer}, status)
}

// hostCutoffProviderAnswering is hostCutoffProvider with one answer per
// request, for the tests that drive more than one.
func hostCutoffProviderAnswering(t *testing.T, answers []map[string]any, status int) (*agentInferenceProvider, *hostCutoffEngine, *int) {
	t.Helper()
	// Taken FIRST so its cleanup is registered first and therefore runs
	// LAST: everything below has to be shut down before the directory it
	// writes into is removed.
	stateDir := t.TempDir()

	eng := &hostCutoffEngine{status: status, answers: answers, serving: []string{hostCutoffFixtureTag}}
	srv := httptest.NewServer(eng)
	t.Cleanup(srv.Close)
	host, port := hostPort(t, srv.URL)

	a := infruntime.NewOllamaAdapter(infruntime.OllamaConfig{
		Binary: "/fake/ollama",
		Host:   host, Port: port,
		Spawner: &fakeSpawner{}, HTTPClient: srv.Client(),
		HealthInterval: 5 * time.Millisecond, HealthSuccess: 1, HealthMaxFails: 5,
		StopTimeout: 50 * time.Millisecond,
	})
	disabled := 0
	agentCtx, cancelAgent := context.WithCancel(context.Background())
	p := &agentInferenceProvider{
		ollama:       a,
		manifests:    hostCutoffManifests(),
		stateDir:     stateDir,
		store:        catalog.NewStore(filepath.Join(stateDir, "state.json")),
		cfg:          agentconfig.InferenceConfig{AllowPull: true},
		profiler:     hostCutoffProfiler(t, "0.31.1"),
		logger:       slog.New(slog.DiscardHandler),
		agentCtx:     agentCtx,
		ollamaUsable: func() bool { return true },
		disableInference: func() error {
			disabled++
			return state.WriteDesiredInferenceState(stateDir, state.InferenceDisabled)
		},
	}
	// An operator model switch fires `go reconcileEngineServe` from
	// endPull, on the agent context — and endPull runs BEFORE
	// pullsWG.Done, so waitForPulls() returns while that goroutine is
	// still writing the Active selection into stateDir. Left unjoined it
	// races the TempDir removal above ("directory not empty"), which
	// surfaces only under load.
	t.Cleanup(func() {
		cancelAgent()
		for i := 0; i < 400 && p.engineReconcileInFlight.Load(); i++ {
			time.Sleep(5 * time.Millisecond)
		}
	})
	return p, eng, &disabled
}

// THE #496 BAR (docs/decisions/20260805/1620-host-cutoff-is-a-measured-probe.md):
// the host the measurement says cannot serve does not download 20-45 GB of
// weights, and is left with local inference off and a working opt-in
// rather than with a model it would wait minutes per turn for.
//
// The counters are the reference host's own CPU-only leg — a machine that
// really does take 227 s per turn on the model the picker chooses for it.
func TestApplyHostCutoff_BelowTheBudget_TurnsLocalInferenceOffInsteadOfDownloading(t *testing.T) {
	p, _, disabled := hostCutoffProvider(t, cpuOnlyCounters, 0)

	if p.applyHostCutoff(context.Background()) {
		t.Fatal("the pre-pull was allowed to proceed on a host measured at ~66 s/turn; " +
			"the whole point of measuring before the download is not to start it")
	}
	if *disabled != 1 {
		t.Fatalf("disableInference calls = %d, want 1 — the verdict has to be persisted, "+
			"or the next boot serves from a host that cannot", *disabled)
	}
	// And the machine can now say WHY, which is the half an operator sees.
	if !p.hostSpeedTurnedInferenceOff() {
		t.Fatal("the measurement is not recorded as the reason local inference is off; " +
			"`waired inference status` would report a bare \"off\"")
	}
}

// A host that clears the budget is left completely alone. The cutoff
// decides nothing else — not which model, not whether to pull, not the
// engine — and it does not claim to be why anything is off.
func TestApplyHostCutoff_AboveTheBudget_ChangesNothing(t *testing.T) {
	p, _, disabled := hostCutoffProvider(t, gpuCounters, 0)

	if !p.applyHostCutoff(context.Background()) {
		t.Fatal("the pre-pull was blocked on a host measured at ~4.5 s/turn")
	}
	if *disabled != 0 {
		t.Fatalf("disableInference calls = %d, want 0", *disabled)
	}
	if p.hostSpeedTurnedInferenceOff() {
		t.Fatal("a host that PASSED is recorded as having been turned off by the measurement")
	}
}

// No verdict means no change. An engine that errors, one that reports no
// counters, and one that truncated the prefill are all indistinguishable
// from a wedged engine — and turning local inference off on that evidence
// would cut hosts that serve perfectly well. Every one of these must leave
// the install path exactly as it was, and publish nothing: a consumer
// cannot tell a truncated prefill from a fast host.
func TestApplyHostCutoff_Undecided_LeavesTheInstallPathAlone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer map[string]any
		status int
	}{
		{"engine refuses the request", nil, http.StatusInternalServerError},
		{"engine reports no timing counters", map[string]any{"response": "hi"}, 0},
		{"engine reports a zero decode duration", map[string]any{
			"prompt_eval_count": 21066, "prompt_eval_duration": int64(31_390_000_000),
			"eval_count": 200, "eval_duration": int64(0),
		}, 0},
		{"engine silently truncated the prefill to its 4096 default", map[string]any{
			"prompt_eval_count": 4096, "prompt_eval_duration": int64(100_000_000),
			"eval_count": 200, "eval_duration": int64(685_000_000),
		}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _, disabled := hostCutoffProvider(t, tc.answer, tc.status)

			if !p.applyHostCutoff(context.Background()) {
				t.Fatal("the pre-pull was blocked without a verdict; an unrun or unusable " +
					"measurement is not evidence that a host is slow")
			}
			if *disabled != 0 {
				t.Fatalf("disableInference calls = %d, want 0", *disabled)
			}
			if s := p.hostSpeedNow(); s != nil {
				t.Fatalf("published %+v from an unusable measurement; nil is the only honest "+
					"thing to say, and a consumer reads a number as a claim", s)
			}
		})
	}
}

// The request the probe sends is the request the threshold was calibrated
// with. Three of these four fields silently invalidate the measurement if
// they go missing:
//
//   - num_ctx: without it the engine serves its 4096 default and
//     TRUNCATES the prompt, timing a fifth of the prefill asked for.
//   - the nonce: a repeat that shares a prefix is answered from the KV
//     cache at 697,222 tok/s, which passes every machine ever built.
//   - num_predict: the decode rate is measured over this many tokens.
//
// The fourth (keep_alive) is hygiene: the probe model must not stay
// resident on the host least able to spare the memory.
func TestMeasureHostCutoff_SendsTheCalibratedRequest(t *testing.T) {
	p, eng, _ := hostCutoffProvider(t, gpuCounters, 0)
	if v := p.ensureHostSpeedMeasured(context.Background(), p.hostSpeedMeasureWindow()); !v.Decided {
		t.Fatal("no measurement from a well-formed engine response")
	}

	bodies := eng.generateBodies()
	if len(bodies) != benchSampleCount+1 {
		t.Fatalf("/api/generate requests = %d, want %d — the line-cost calibration, then one "+
			"per sample", len(bodies), benchSampleCount+1)
	}
	// The calibration is short by design and asks for no answer.
	calOpts, _ := bodies[0]["options"].(map[string]any)
	if got, _ := calOpts["num_predict"].(float64); got != 1 {
		t.Fatalf("calibration num_predict = %v, want 1 — only the prompt side is wanted", calOpts["num_predict"])
	}
	calPrompt, _ := bodies[0]["prompt"].(string)
	body := bodies[1]
	prompt0, _ := body["prompt"].(string)
	if len(calPrompt) >= len(prompt0) {
		t.Fatalf("calibration prompt is %d bytes against the measured %d; it exists to be cheap",
			len(calPrompt), len(prompt0))
	}

	if got := body["model"]; got != hostCutoffFixtureTag {
		t.Fatalf("model = %v, want the probe model's tag %q resolved from the catalog", got, hostCutoffFixtureTag)
	}
	if got, ok := body["keep_alive"].(float64); !ok || got != 0 {
		t.Fatalf("keep_alive = %v, want 0 — the probe model must unload rather than sit "+
			"in memory while the real model downloads", body["keep_alive"])
	}
	opts, ok := body["options"].(map[string]any)
	if !ok {
		t.Fatalf("options = %v, want an object", body["options"])
	}
	wantCtx := float64(hostCutoffWindowSlots*hostfit.HostCutoffProbeDepthTokens + hostCutoffWindowMargin)
	if got, _ := opts["num_ctx"].(float64); got != wantCtx {
		t.Fatalf("options.num_ctx = %v, want %v — the engine serves its own default window "+
			"without it, and divides whatever it is given among its parallel slots",
			opts["num_ctx"], wantCtx)
	}
	if got, _ := opts["num_predict"].(float64); got != float64(hostfit.HostCutoffCompletionSampleTokens) {
		t.Fatalf("options.num_predict = %v, want %d", opts["num_predict"], hostfit.HostCutoffCompletionSampleTokens)
	}
	if got, _ := opts["temperature"].(float64); got != 0 {
		t.Fatalf("options.temperature = %v, want 0", opts["temperature"])
	}
	prompt, _ := body["prompt"].(string)
	if len(prompt) < 1000 {
		t.Fatalf("prompt is %d bytes; the measurement needs a ~%d-token prefill",
			len(prompt), hostfit.HostCutoffProbeDepthTokens)
	}
	if got, _ := body["stream"].(bool); got {
		t.Fatal("stream = true; the counters only arrive on the final non-streamed object")
	}
}

// The published figure is the MEDIAN SAMPLE, and it is one run's own
// numbers.
//
// Sampling is what makes the figure safe to publish and to act on: the
// runs that fixed this threshold sat within ±2 % of each other while a
// single contended run landed +21 %, which is enough on its own to move a
// host across the line (proto/signer/inference_state.go's
// memory_bandwidth_measured_gbs doc: the median of N samples with their
// spread, never a single reading).
//
// A field-wise median would satisfy "median" and still be wrong: it would
// pair a prefill from one run with a decode from another, and the turn
// time computed from that pair would belong to no measurement at all.
// Hence 3x, 1x, 2x — where the field-wise and sample-wise answers differ
// for prefill and decode alike.
func TestMeasureHostCutoff_PublishesTheMedianSampleNotAMedianOfFields(t *testing.T) {
	p, eng, _ := hostCutoffProviderAnswering(t, []map[string]any{
		calibrationCounters, scaledCounters(3), scaledCounters(1), scaledCounters(2),
	}, 0)
	if v := p.ensureHostSpeedMeasured(context.Background(), p.hostSpeedMeasureWindow()); !v.Decided {
		t.Fatal("no measurement")
	}
	if got := len(eng.generateBodies()); got != benchSampleCount+1 {
		t.Fatalf("/api/generate requests = %d, want %d", got, benchSampleCount+1)
	}

	got := p.hostSpeedNow()
	if got == nil {
		t.Fatal("nothing published after a usable measurement")
	}
	middle := scaledCountersProbe(2)
	if got.PrefillTokps != middle.PrefillTokps || got.DecodeTokps != middle.DecodeTokps {
		t.Fatalf("published prefill/decode = %.1f/%.2f, want the 2x sample's %.1f/%.2f — the "+
			"published rates must come from ONE run, the one whose turn is the median",
			got.PrefillTokps, got.DecodeTokps, middle.PrefillTokps, middle.DecodeTokps)
	}
	if got.TurnSeconds != middle.TurnSeconds() {
		t.Fatalf("published turn = %.3f s, want %.3f s", got.TurnSeconds, middle.TurnSeconds())
	}
	if got.Samples != benchSampleCount {
		t.Fatalf("samples = %d, want %d — a consumer reads this to know whether the figure "+
			"was ever checked against another run", got.Samples, benchSampleCount)
	}
	if got.SpreadPct <= 0 {
		t.Fatalf("spread_pct = %v over samples 3x/1x/2x apart, want a positive dispersion", got.SpreadPct)
	}
}

// TestHostCutoffSamplesStraddleBudget is the decision waired-agent#622 asked
// for, in isolation: the samples disagree about the verdict, not merely about
// each other.
//
// scaledCountersProbe(f) has a turn of ~4.462*f seconds against the 45 s
// budget, so the line sits at 10.09x: 9x clears it (40.2 s) and 12x misses it
// (53.5 s).
func TestHostCutoffSamplesStraddleBudget(t *testing.T) {
	for _, tc := range []struct {
		name    string
		factors []float64
		want    bool
	}{
		{"all-far-below-the-line", []float64{1, 2, 3}, false},
		{"all-above-the-line", []float64{30, 40, 50}, false},
		{"wide-spread-but-all-below", []float64{1, 5, 9}, false},
		{"straddling-the-line", []float64{9, 12, 9}, true},
		{"one-slow-run-out-of-three", []float64{1, 1, 30}, true},
		{"nothing-measured", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			samples := make([]hostfit.HostProbe, 0, len(tc.factors))
			for _, f := range tc.factors {
				samples = append(samples, scaledCountersProbe(f))
			}
			if got := hostCutoffSamplesStraddleBudget(samples); got != tc.want {
				t.Errorf("= %v, want %v (turns %v against a %.0f s budget)",
					got, tc.want, tc.factors, hostfit.HostCutoffTurnBudgetSeconds)
			}
		})
	}
}

// End to end: samples that disagree about the budget buy more samples, so the
// answer stops being decided by whichever run happened to be the middle one.
func TestMeasureHostCutoff_SamplesThatStraddleTheBudgetBuyMoreSamples(t *testing.T) {
	p, eng, _ := hostCutoffProviderAnswering(t, []map[string]any{
		calibrationCounters,
		scaledCounters(9), scaledCounters(12), scaledCounters(9),
		scaledCounters(9), scaledCounters(9),
	}, 0)
	if v := p.ensureHostSpeedMeasured(context.Background(), p.hostSpeedMeasureWindow()); !v.Decided {
		t.Fatal("no measurement")
	}

	if got := len(eng.generateBodies()); got != hostCutoffStraddleSampleCount+1 {
		t.Fatalf("/api/generate requests = %d, want %d — three samples that disagreed about "+
			"the budget must not be the whole of the defence",
			got, hostCutoffStraddleSampleCount+1)
	}
	got := p.hostSpeedNow()
	if got == nil || got.Samples != hostCutoffStraddleSampleCount {
		t.Fatalf("samples = %v, want %d", got, hostCutoffStraddleSampleCount)
	}
}

// The other half, and the one that keeps this affordable: a host nowhere near
// the line pays nothing for it, however much its samples disagree. That was
// the host #622 was found on — spread 106%, verdict never in doubt.
func TestMeasureHostCutoff_ADisagreementFarFromTheLineCostsNothing(t *testing.T) {
	p, eng, _ := hostCutoffProviderAnswering(t, []map[string]any{
		calibrationCounters, scaledCounters(3), scaledCounters(1), scaledCounters(2),
	}, 0)
	if v := p.ensureHostSpeedMeasured(context.Background(), p.hostSpeedMeasureWindow()); !v.Decided {
		t.Fatal("no measurement")
	}

	if got := len(eng.generateBodies()); got != benchSampleCount+1 {
		t.Fatalf("/api/generate requests = %d, want %d — samples 3x apart but all an order of "+
			"magnitude inside the budget change no answer", got, benchSampleCount+1)
	}
	if pub := p.hostSpeedNow(); pub == nil || pub.SpreadPct <= 0 {
		t.Fatalf("spread_pct = %v, want a positive dispersion still recorded", pub)
	}
}

// scaledCountersProbe is what scaledCounters(factor) measures, as the
// policy layer sees it.
func scaledCountersProbe(factor float64) hostfit.HostProbe {
	return hostfit.HostProbe{
		PromptTokens: 21066,
		PrefillTokps: 21066 / (1.040 * factor),
		DecodeTokps:  200 / (0.685 * factor),
	}
}

// No two samples share a prompt prefix. ollama's prefix KV cache answers a
// repeat with the full prompt_eval_count and a near-zero duration —
// measured at 697,222 tok/s on the reference host — so a fixed prompt
// would report every machine as instant, and three identical samples
// would agree perfectly while measuring nothing.
func TestMeasureHostCutoff_EverySampleUsesADifferentPrompt(t *testing.T) {
	p, eng, _ := hostCutoffProvider(t, gpuCounters, 0)
	if v := p.ensureHostSpeedMeasured(context.Background(), p.hostSpeedMeasureWindow()); !v.Decided {
		t.Fatal("no measurement")
	}

	bodies := eng.generateBodies()
	if len(bodies) < 2 {
		t.Fatalf("/api/generate requests = %d, want at least 2 to compare", len(bodies))
	}
	seen := map[string]int{}
	for i, b := range bodies {
		prompt, _ := b["prompt"].(string)
		if len(prompt) < 64 {
			t.Fatalf("sample %d: prompt is %d bytes", i, len(prompt))
		}
		head := prompt[:64]
		if prev, dup := seen[head]; dup {
			t.Fatalf("samples %d and %d open with the same 64 bytes (%q); the engine would "+
				"answer the later one from its prefix KV cache", prev, i, head)
		}
		seen[head] = i
	}
}

// The sampler stops when another sample would not fit the budget, and
// publishes what it actually took.
//
// The cost of sampling lands on the slowest host — three samples where
// one run already takes a minute — and this measurement runs BEFORE the
// model download rather than instead of it, so it cannot be allowed to
// grow without bound. One sample is still a measurement; it just says so,
// and a consumer reading Samples=1 knows it was never checked against
// another run.
func TestMeasureHostCutoff_StopsAtTheSampleBudget(t *testing.T) {
	// Time passes when the ENGINE works, not when the runner's scheduler
	// gets around to us. Advancing the clock from inside the handler makes
	// the arithmetic exact — calibration 4 s, sample 1 4 s, so the budget
	// check before sample 2 sees 8 + 4 > 10 — and insensitive to how many
	// times the code under test happens to read the clock.
	//
	// It used to sleep 40 ms per request against a 100 ms budget and bet on
	// real time. A loaded GitHub-hosted Windows runner only had to add
	// ~20 ms across the calibration and sample 1 for the deadline to land
	// INSIDE sample 1, cancelling it mid-flight and failing the first
	// assertion below on a premise that broke rather than on the behaviour
	// it guards (waired-agent#677).
	//
	// The figures are seconds rather than milliseconds because none of it
	// is waited on: the fake engine answers immediately and only the fake
	// clock moves. That also buys the real context deadline below a wide
	// margin — measureHostCutoff anchors it on started.Add(budget), so the
	// run now has ten real seconds to do work that takes microseconds.
	clk := &steppedClock{t: time.Now()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		clk.advance(4 * time.Second)
		body, _ := json.Marshal(cpuOnlyCounters)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	m, err := measureHostCutoff(context.Background(), hostCutoffDeps{
		BaseURL: srv.URL, EngineModel: hostCutoffFixtureTag,
		HTTPClient: srv.Client(), Logger: slog.New(slog.DiscardHandler),
		// The budget has to sit between (calibration + sample 1) = 8 s and
		// (+ sample 2) = 12 s. Since waired-agent#579 it is a real deadline
		// over the WHOLE run, so a budget below 8 s would cancel sample 1
		// mid-flight — a different behaviour from the one under test.
		Nonce: "budget", MeasureBudget: 10 * time.Second,
		Now: clk.now,
	})
	if err != nil {
		t.Fatalf("measureHostCutoff: %v", err)
	}
	if !m.Probe.Measured() {
		t.Fatal("the budget threw the whole measurement away; one sample is still a measurement")
	}
	if m.Samples != 1 {
		t.Fatalf("samples = %d, want 1 — the budget only had room for one", m.Samples)
	}
	if got := clk.calls(); got != 2 {
		t.Fatalf("/api/generate requests = %d, want 2 (the calibration, then one sample)", got)
	}
}

// steppedClock is a clock that only moves when the fake engine answers, and
// counts how often it did. The two belong together: every advance in this
// file is one request's cost, so a test that asserts on elapsed time and one
// that asserts on request count are reading the same ledger.
//
// The context deadline measureHostCutoff sets still runs on real time — this
// only feeds hostCutoffDeps.Now, which is the budget arithmetic. That is the
// intended split: the deadline is a backstop against a wedged engine, and a
// fake engine that answers immediately never approaches it.
type steppedClock struct {
	mu sync.Mutex
	t  time.Time
	n  int
}

func (c *steppedClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	c.n++
}

func (c *steppedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *steppedClock) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// A capped prompt is measured again, wider, rather than thrown away.
//
// num_ctx is not a prompt budget — ollama divides it among its parallel
// slots and truncates to what one slot holds. Measured on the reference
// host, a request for 23048 came back having prefilled 11526. Without
// this retry the measurement would reach no verdict on any host whose
// engine splits further than expected, which is a feature that silently
// does nothing.
func TestMeasureHostCutoff_WhenTheEngineCapsThePrompt_MeasuresAgainWider(t *testing.T) {
	capped := map[string]any{
		"prompt_eval_count": 11526, "prompt_eval_duration": int64(13_740_000_000),
		"eval_count": 200, "eval_duration": int64(5_700_000_000),
	}
	p, eng, disabled := hostCutoffProviderAnswering(t,
		[]map[string]any{calibrationCounters, capped, cpuOnlyCounters}, 0)

	if p.applyHostCutoff(context.Background()) {
		t.Fatal("the download proceeded; the retried measurement reached the full depth " +
			"and says this host takes ~66 s per turn")
	}
	if *disabled != 1 {
		t.Fatalf("disableInference calls = %d, want 1", *disabled)
	}

	bodies := eng.generateBodies()
	// The first sample is capped and retried; the rest answer at depth
	// first time, so the retry is not repeated.
	if len(bodies) != benchSampleCount+2 {
		t.Fatalf("/api/generate requests = %d, want %d (the calibration, one capped sample, its "+
			"wider retry, then the remaining samples)", len(bodies), benchSampleCount+2)
	}
	first, _ := bodies[1]["options"].(map[string]any)
	second, _ := bodies[2]["options"].(map[string]any)
	firstCtx, _ := first["num_ctx"].(float64)
	secondCtx, _ := second["num_ctx"].(float64)
	if secondCtx <= firstCtx {
		t.Fatalf("retry num_ctx = %v, want more than the capped attempt's %v", secondCtx, firstCtx)
	}
	// The retry has to clear the depth after the same split: the engine
	// gave us 11526 of 23048, so half of the new window must exceed 21000.
	if want := float64(hostfit.HostCutoffProbeDepthTokens); secondCtx*(11526.0/23048.0) <= want {
		t.Fatalf("retry num_ctx = %v; after the same %.2fx split it still would not reach %v tokens",
			secondCtx, 11526.0/23048.0, want)
	}
	firstPrompt, _ := bodies[1]["prompt"].(string)
	secondPrompt, _ := bodies[2]["prompt"].(string)
	if firstPrompt[:64] == secondPrompt[:64] {
		t.Fatal("the retry reused the capped attempt's prompt prefix; the engine would " +
			"answer it from its KV cache instead of measuring anything")
	}
}

// The measurement is taken once per INSTALL, and the engine build is one
// half of that. Re-measuring on every call would cost a minute or two of
// a user's install for an answer already on disk; never re-measuring
// would keep serving pre-bump numbers after an Ollama bundle bump, which
// is waired#668 exactly.
func TestEnsureHostSpeedMeasured_OncePerEngineBuild(t *testing.T) {
	p, eng, _ := hostCutoffProvider(t, gpuCounters, 0)
	ctx := context.Background()

	if v := p.ensureHostSpeedMeasured(ctx, p.hostSpeedMeasureWindow()); !v.Decided {
		t.Fatal("first call did not measure")
	}
	afterFirst := len(eng.generateBodies())

	if v := p.ensureHostSpeedMeasured(ctx, p.hostSpeedMeasureWindow()); !v.Decided {
		t.Fatal("second call lost the measurement")
	}
	if got := len(eng.generateBodies()); got != afterFirst {
		t.Fatalf("/api/generate requests = %d after a second call, want %d — the answer was "+
			"already taken on this engine build", got, afterFirst)
	}

	// A bundle bump is a different engine, so the figure no longer
	// describes what is running.
	p.profiler = hostCutoffProfiler(t, "0.32.0")
	if v := p.ensureHostSpeedMeasured(ctx, p.hostSpeedMeasureWindow()); !v.Decided {
		t.Fatal("no measurement after the engine version moved")
	}
	if got := len(eng.generateBodies()); got <= afterFirst {
		t.Fatalf("/api/generate requests = %d after an engine bump, want more than %d — a "+
			"figure from another engine build is not a figure for this one", got, afterFirst)
	}
	if got := p.hostSpeedNow(); got == nil || got.EngineVersion != "0.32.0" {
		t.Fatalf("published engine_version = %v, want 0.32.0", got)
	}
}

// The other half of "once per install": the AGENT build. An upgrade
// leaves the engine and the hardware exactly as they were, so nothing in
// the stored figure's own fields says it is stale — and waired#1099 is
// the ruling that a figure kept for as long as the hardware looks the
// same is the wrong rule. Every install and every upgrade restarts the
// daemon on a new version, so this is what makes it an install-time step.
func TestEnsureHostSpeedMeasured_AnUpgradeReMeasures(t *testing.T) {
	p, eng, _ := hostCutoffProvider(t, gpuCounters, 0)
	ctx := context.Background()

	if v := p.ensureHostSpeedMeasured(ctx, p.hostSpeedMeasureWindow()); !v.Decided {
		t.Fatal("first call did not measure")
	}
	afterFirst := len(eng.generateBodies())

	// The next daemon start on the same state dir, from a newer build.
	// Same engine, same hardware, same everything the record can see.
	rec, err := state.ReadHostSpeed(p.stateDir)
	if err != nil || rec.Measurement == nil {
		t.Fatalf("no stored record to age: %+v (%v)", rec, err)
	}
	if rec.AgentVersion != buildinfo.Version {
		t.Fatalf("stored agent_version = %q, want %q — the record cannot say which build measured it",
			rec.AgentVersion, buildinfo.Version)
	}
	rec.AgentVersion = "0.0.1-previous"
	if err := state.WriteHostSpeed(p.stateDir, rec); err != nil {
		t.Fatal(err)
	}
	next := &agentInferenceProvider{
		ollama: p.ollama, manifests: p.manifests, stateDir: p.stateDir, store: p.store,
		cfg: p.cfg, profiler: p.profiler, logger: p.logger, agentCtx: p.agentCtx,
		ollamaUsable: p.ollamaUsable,
	}
	if v := next.ensureHostSpeedMeasured(ctx, next.hostSpeedMeasureWindow()); !v.Decided {
		t.Fatal("no measurement after the agent build moved")
	}
	if got := len(eng.generateBodies()); got <= afterFirst {
		t.Fatalf("/api/generate requests = %d after an upgrade, want more than %d — an install "+
			"that has not measured this host has no figure of its own", got, afterFirst)
	}
	if got, err := state.ReadHostSpeed(p.stateDir); err != nil || got.AgentVersion != buildinfo.Version {
		t.Fatalf("stored agent_version = %q after the re-measure, want %q (%v)",
			got.AgentVersion, buildinfo.Version, err)
	}
}

// TestResidentBlocksMeasurement covers both engine ownerships, which the
// integration tests cannot: an adapter only learns it adopted an engine by
// finding a real exact-pin orphan at EnsureRunning time.
//
// PRODUCT CONTRACT for the adopted arm (waired#1139: the re-measurement is
// structurally contaminated by the resident serving model; host_memory.go's
// rule that "a resident model is never charged against the very host that
// serves it"). The spawned arm is the other half and is just as load-bearing:
// infruntime.MaxResidentModels reaches an engine we spawned, so its probe
// evicts and the reading is clean — blocking there would stop every host with
// a serving model from ever measuring.
func TestResidentBlocksMeasurement(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mode      infruntime.EngineMode
		resident  []string
		wantName  string
		wantBlock bool
	}{
		{"adopted-with-a-resident-model", infruntime.EngineModeAdopted, []string{"qwen3.6:35b-a3b"}, "qwen3.6:35b-a3b", true},
		{"adopted-with-nothing-loaded", infruntime.EngineModeAdopted, nil, "", false},
		{"adopted-ignores-a-nameless-row", infruntime.EngineModeAdopted, []string{""}, "", false},
		{"spawned-with-a-resident-model", infruntime.EngineModeSpawned, []string{"qwen3.6:35b-a3b"}, "", false},
		{"spawned-with-nothing-loaded", infruntime.EngineModeSpawned, nil, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name, block := residentBlocksMeasurement(tc.mode, tc.resident)
			if name != tc.wantName || block != tc.wantBlock {
				t.Errorf("= (%q, %v), want (%q, %v)", name, block, tc.wantName, tc.wantBlock)
			}
		})
	}
}

// The spawned arm, end to end: a model sitting in /api/ps must not stop the
// measurement, because the cap makes the probe evict it. This is the
// regression the guard above could plausibly cause — a host that serves a
// model is the normal host, and it would simply stop publishing a figure.
func TestEnsureHostSpeedMeasured_AResidentModelDoesNotBlockASpawnedEngine(t *testing.T) {
	p, eng, _ := hostCutoffProvider(t, gpuCounters, 0)
	eng.mu.Lock()
	eng.resident = []string{"qwen3.6:35b-a3b"}
	eng.mu.Unlock()

	if v := p.ensureHostSpeedMeasured(context.Background(), p.hostSpeedMeasureWindow()); !v.Decided {
		t.Fatal("a resident model stood down the measurement on an engine this agent spawned")
	}
}

// The serving model goes back after the measurement. The cap makes the probe
// evict whatever was loaded and the probe unloads itself with keep_alive:0,
// so without this a host is cold from the measurement until its next request
// pays a multi-GB load — the cost waired-agent#320 exists to remove.
func TestEnsureHostSpeedMeasured_PutsTheServingModelBack(t *testing.T) {
	p, eng, _ := hostCutoffProvider(t, gpuCounters, 0)
	hostCutoffEngineUp(t, p) // warmTarget declines unless the engine reports ready
	if err := p.store.Update(func(s *catalog.State) {
		s.Models["model-a"] = catalog.ModelState{
			State: catalog.ModelStateReady, VariantID: "q4", OllamaTag: "a:q4",
		}
		s.Active = &catalog.ActiveSelection{Runtime: catalog.RuntimeOllama, ModelID: "model-a"}
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	if v := p.ensureHostSpeedMeasured(context.Background(), p.hostSpeedMeasureWindow()); !v.Decided {
		t.Fatal("no measurement to warm after")
	}

	// The warm is detached from this call on purpose (a cold multi-GB load
	// must not be inside the install's window), so poll for it.
	deadline := time.Now().Add(waitBackstop)
	for {
		warmed := false
		for _, b := range eng.generateBodies() {
			if b["model"] == "a:q4" && b["keep_alive"] == infruntime.OllamaKeepAlive {
				warmed = true
			}
		}
		if warmed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the serving model was never re-loaded after the measurement: %+v",
				eng.generateBodies())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// PRODUCT CONTRACT (owner ruling 2026-08-09, waired-agent#599: a re-run of
// `waired init` replays the whole install conversation, "各種のベンチマークや
// ゲートも新規インストールと同じように設定する"). Without this the stored
// figure is kept for the life of the install, which is how three machines came
// to be carrying a measurement of a residency rather than of a host, with no
// way to retake it short of an upgrade (waired#1140).
func TestRemeasure_AReRunTakesAFreshMeasurement(t *testing.T) {
	p, eng, _ := hostCutoffProvider(t, gpuCounters, 0)
	hostCutoffEngineUp(t, p)
	ctx := context.Background()

	if v := p.ensureHostSpeedMeasured(ctx, p.hostSpeedMeasureWindow()); !v.Decided {
		t.Fatal("first call did not measure")
	}
	afterFirst := len(eng.generateBodies())

	// The next daemon start on the same state dir: it finds a usable stored
	// figure and measures nothing, which is the state a re-run days later
	// actually meets.
	next := &agentInferenceProvider{
		ollama: p.ollama, manifests: p.manifests, stateDir: p.stateDir, store: p.store,
		cfg: p.cfg, profiler: p.profiler, logger: p.logger, agentCtx: p.agentCtx,
		ollamaUsable: p.ollamaUsable,
	}
	if v := next.ensureHostSpeedMeasured(ctx, next.hostSpeedMeasureWindow()); !v.Decided {
		t.Fatal("the stored figure was not reused")
	}
	if got := len(eng.generateBodies()); got != afterFirst {
		t.Fatalf("/api/generate requests = %d, want %d — a boot with a usable figure measures nothing",
			got, afterFirst)
	}

	if !next.Remeasure(ctx) {
		t.Fatal("Remeasure declined on a daemon that had not measured in this process")
	}
	next.waitForPulls()
	if got := len(eng.generateBodies()); got <= afterFirst {
		t.Fatalf("/api/generate requests = %d after the ask, want more than %d", got, afterFirst)
	}
}

// The other half, and the constraint it protects: a fresh install measured
// seconds before `waired init` reaches step 6, and measuring the same host
// twice in one install is what
// docs/decisions/20260807/1700-host-speed-is-an-install-time-step.md rules
// out — three minutes becomes six on exactly the slow hosts the measurement
// exists to identify.
func TestRemeasure_AFreshInstallReusesWhatTheBootstrapTook(t *testing.T) {
	p, eng, _ := hostCutoffProvider(t, gpuCounters, 0)
	hostCutoffEngineUp(t, p)
	ctx := context.Background()

	if v := p.ensureHostSpeedMeasured(ctx, p.hostSpeedMeasureWindow()); !v.Decided {
		t.Fatal("the bootstrap measurement did not happen")
	}
	afterBootstrap := len(eng.generateBodies())

	if p.Remeasure(ctx) {
		t.Error("Remeasure started a second measurement in the same install")
	}
	p.waitForPulls()
	if got := len(eng.generateBodies()); got != afterBootstrap {
		t.Fatalf("/api/generate requests = %d, want %d — no second measurement in one install",
			got, afterBootstrap)
	}
}

// The force is consumed, not latched: one request must not turn the host into
// one that re-measures on every boot for the rest of the install.
func TestRemeasure_TheRequestIsConsumedOnce(t *testing.T) {
	p, eng, _ := hostCutoffProvider(t, gpuCounters, 0)
	hostCutoffEngineUp(t, p)
	ctx := context.Background()

	p.hostSpeedForce.Store(true)
	if v := p.ensureHostSpeedMeasured(ctx, p.hostSpeedMeasureWindow()); !v.Decided {
		t.Fatal("no measurement")
	}
	afterForced := len(eng.generateBodies())
	if p.hostSpeedForce.Load() {
		t.Error("the force flag survived the measurement it asked for")
	}

	if v := p.ensureHostSpeedMeasured(ctx, p.hostSpeedMeasureWindow()); !v.Decided {
		t.Fatal("the second call lost the figure")
	}
	if got := len(eng.generateBodies()); got != afterForced {
		t.Fatalf("/api/generate requests = %d, want %d — the force must not latch", got, afterForced)
	}
}

// A record written before AgentVersion existed cannot say which build
// took it, so it is re-measured. The first boot after the upgrade that
// introduces the field is exactly the case the field exists for.
func TestEnsureHostSpeedMeasured_ARecordWithNoAgentVersionIsReMeasured(t *testing.T) {
	p, eng, _ := hostCutoffProvider(t, gpuCounters, 0)
	ctx := context.Background()

	if v := p.ensureHostSpeedMeasured(ctx, p.hostSpeedMeasureWindow()); !v.Decided {
		t.Fatal("first call did not measure")
	}
	afterFirst := len(eng.generateBodies())

	rec, _ := state.ReadHostSpeed(p.stateDir)
	rec.AgentVersion = ""
	if err := state.WriteHostSpeed(p.stateDir, rec); err != nil {
		t.Fatal(err)
	}
	next := &agentInferenceProvider{
		ollama: p.ollama, manifests: p.manifests, stateDir: p.stateDir, store: p.store,
		cfg: p.cfg, profiler: p.profiler, logger: p.logger, agentCtx: p.agentCtx,
		ollamaUsable: p.ollamaUsable,
	}
	if v := next.ensureHostSpeedMeasured(ctx, next.hostSpeedMeasureWindow()); !v.Decided {
		t.Fatal("no measurement from a record that cannot say who took it")
	}
	if got := len(eng.generateBodies()); got <= afterFirst {
		t.Fatalf("/api/generate requests = %d, want more than %d", got, afterFirst)
	}
}

// hostCutoffEngineUp brings the fixture's adapter to StateReady, which is
// what the bootstrap trigger waits for before it measures. The fake
// answers /api/tags, so the adapter's own readiness poll is what runs.
func hostCutoffEngineUp(t *testing.T, p *agentInferenceProvider) {
	t.Helper()
	if err := p.ollama.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("fixture engine did not come up: %v", err)
	}
	t.Cleanup(func() { _ = p.ollama.Stop(context.Background()) })
}

// The bootstrap trigger: a host that is never told to install anything
// still publishes a figure. This is the whole point of waired#1099 — the
// browser wizard names a model, which stands the pre-pull down, so before
// this the majority install path measured nothing until after the
// operator had already chosen.
func TestStartHostSpeedMeasurement_MeasuresWithNoModelNamed(t *testing.T) {
	p, eng, disabled := hostCutoffProvider(t, cpuOnlyCounters, 0)
	hostCutoffEngineUp(t, p)

	p.startHostSpeedMeasurement(context.Background())
	p.waitForPulls()

	got := p.hostSpeedNow()
	if got == nil {
		t.Fatal("nothing published — a host nobody has chosen a model for still has a speed")
	}
	if got.TurnSeconds <= hostfit.HostCutoffTurnBudgetSeconds {
		t.Fatalf("published turn = %.1f s, want the below-budget figure the counters describe",
			got.TurnSeconds)
	}
	if len(eng.generateBodies()) == 0 {
		t.Fatal("the engine was never asked to generate anything")
	}
	// MEASURING is not DECIDING. The bootstrap has no one else's answer to
	// defer to yet — the wizard may be about to name a model — so it takes
	// the figure and nothing more.
	if *disabled != 0 {
		t.Fatalf("local inference was turned off %d times by a measurement; the decision "+
			"belongs to applyHostCutoff, which knows whether anyone has chosen", *disabled)
	}
	if st, err := state.ReadDesiredInferenceState(p.stateDir); err != nil || st != "" {
		t.Fatalf("the local-inference toggle reads %q after a measurement, want unset (%v)", st, err)
	}
}

// A second trigger reuses the first one's answer rather than measuring a
// host the first one is still measuring. The bootstrap's call and a later
// pre-pull or setup call overlap on a slow host by construction: the
// measurement takes minutes and the pre-pull hold releases on its own.
func TestStartHostSpeedMeasurement_DoesNotRaceASecondCaller(t *testing.T) {
	p, eng, _ := hostCutoffProvider(t, gpuCounters, 0)
	hostCutoffEngineUp(t, p)
	ctx := context.Background()

	p.startHostSpeedMeasurement(ctx)
	p.startHostSpeedMeasurement(ctx)
	p.waitForPulls()

	if v := p.ensureHostSpeedMeasured(ctx, p.hostSpeedMeasureWindow()); !v.Decided {
		t.Fatal("no measurement")
	}
	// One calibration + benchSampleCount samples, once. Anything more is a
	// second measurement of a host that had already answered.
	if got, want := len(eng.generateBodies()), 1+benchSampleCount; got != want {
		t.Fatalf("/api/generate requests = %d, want %d — the measurement ran more than once", got, want)
	}
}

// THE waired#1099 CI BAR. A running measurement must not stall the local
// management API.
//
// Status() reads the published figure on every poll, and while one mutex
// guarded both the field and the measurement, /waired/v1/inference/status
// blocked for as long as the engine took to answer. On a CI host that was
// ten minutes and 41 seconds: the installtest reads status with `curl
// --max-time 5`, got nothing, and reported the daemon as publishing no
// pinned_version and no engine mode. The tray, the CLI and the setup
// wizard all read the same route.
func TestHostSpeed_AMeasurementDoesNotStallTheStatusRoute(t *testing.T) {
	p, eng, _ := hostCutoffProvider(t, gpuCounters, 0)
	hostCutoffEngineUp(t, p)
	// A slow engine, which is the ordinary case this is about: the request
	// hangs until the test lets it answer.
	release := make(chan struct{})
	eng.mu.Lock()
	eng.block = release
	eng.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.ensureHostSpeedMeasured(context.Background(), p.hostSpeedMeasureWindow())
	}()

	// Wait until a request is actually in flight, so this is not asserting
	// against a goroutine that has not started yet.
	deadline := time.Now().Add(waitBackstop)
	for len(eng.generateBodies()) == 0 {
		if time.Now().After(deadline) {
			close(release)
			<-done
			t.Fatal("the measurement never reached the engine")
		}
		time.Sleep(5 * time.Millisecond)
	}

	read := make(chan struct{})
	go func() {
		defer close(read)
		p.hostSpeedNow()
	}()
	select {
	case <-read:
	case <-time.After(waitBackstop):
		close(release)
		<-done
		t.Fatal("reading the published figure blocked while a measurement was running — " +
			"this is the whole management API going quiet for the length of the probe")
	}
	close(release)
	<-done
}

// The measurement waits for the host to go quiet. It used to start the
// moment the engine was up, which on a CI host meant it began 400 ms
// before the operator's own model finished downloading and died 3 ms
// after the serve reconcile that pull triggered — `connection refused`,
// with nothing published and the download slowed for its trouble.
func TestStartHostSpeedMeasurement_WaitsForTheHostToGoQuiet(t *testing.T) {
	p, eng, _ := hostCutoffProvider(t, gpuCounters, 0)
	hostCutoffEngineUp(t, p)

	// The operator's model is downloading. Nothing else about the host has
	// changed: the engine is up and would answer.
	p.pullMu.Lock()
	p.pullsInFlight = map[string]*pullJob{"the-operators-model": {modelID: "the-operators-model"}}
	p.pullMu.Unlock()

	p.startHostSpeedMeasurement(context.Background())
	time.Sleep(150 * time.Millisecond)
	if got := len(eng.generateBodies()); got != 0 {
		t.Fatalf("%d /api/generate request(s) while a pull was in flight, want 0 — the "+
			"measurement is competing with the download it exists to precede", got)
	}

	p.pullMu.Lock()
	p.pullsInFlight = map[string]*pullJob{}
	p.pullMu.Unlock()
	p.waitForPulls()

	if p.hostSpeedNow() == nil {
		t.Fatal("nothing published once the host went quiet")
	}
}

// A reconcile is a stop-and-restart of the engine, so one that is pending
// counts as busy. Measuring into it is what produced the connection
// refused above; the pull it followed had already finished by then, so
// waiting on pulls alone would not have caught it.
func TestStartHostSpeedMeasurement_WaitsForAPendingEngineReconcile(t *testing.T) {
	p, eng, _ := hostCutoffProvider(t, gpuCounters, 0)
	hostCutoffEngineUp(t, p)

	p.engineReconcileInFlight.Store(true)
	p.startHostSpeedMeasurement(context.Background())
	time.Sleep(150 * time.Millisecond)
	if got := len(eng.generateBodies()); got != 0 {
		t.Fatalf("%d /api/generate request(s) with a reconcile in flight, want 0 — the "+
			"restart it is about to perform kills the measurement", got)
	}

	p.engineReconcileInFlight.Store(false)
	p.waitForPulls()
	if p.hostSpeedNow() == nil {
		t.Fatal("nothing published once the reconcile finished")
	}
}

// A host that never goes quiet is left unmeasured rather than measured
// badly. The next boot tries again.
func TestStartHostSpeedMeasurement_GivesUpWhenTheHostNeverSettles(t *testing.T) {
	p, eng, _ := hostCutoffProvider(t, gpuCounters, 0)
	hostCutoffEngineUp(t, p)
	prev := hostSpeedSettleWait
	hostSpeedSettleWait = 50 * time.Millisecond
	t.Cleanup(func() { hostSpeedSettleWait = prev })

	p.pullMu.Lock()
	p.pullsInFlight = map[string]*pullJob{"never-finishes": {modelID: "never-finishes"}}
	p.pullMu.Unlock()

	p.startHostSpeedMeasurement(context.Background())
	p.waitForPulls()

	if got := len(eng.generateBodies()); got != 0 {
		t.Fatalf("%d /api/generate request(s) after giving up, want 0", got)
	}
	if p.hostSpeedNow() != nil {
		t.Fatal("published a figure without measuring one")
	}
}

// The published record says what it is, on what, and how it was taken.
// Every one of these fields is load-bearing for a consumer that has to
// decide whether the number is comparable to its own threshold.
func TestHostSpeed_PublishedRecordCarriesItsProvenance(t *testing.T) {
	p, _, _ := hostCutoffProvider(t, gpuCounters, 0)
	if v := p.ensureHostSpeedMeasured(context.Background(), p.hostSpeedMeasureWindow()); !v.Decided {
		t.Fatal("no measurement")
	}
	got := p.hostSpeedNow()
	if got == nil {
		t.Fatal("nothing published")
	}
	if got.ProbeModelID != hostfit.HostCutoffProbeModelID {
		t.Fatalf("probe_model_id = %q, want %q — the threshold is calibrated against one model",
			got.ProbeModelID, hostfit.HostCutoffProbeModelID)
	}
	if got.DepthTokens != hostfit.HostCutoffProbeDepthTokens {
		t.Fatalf("depth_tokens = %d, want %d", got.DepthTokens, hostfit.HostCutoffProbeDepthTokens)
	}
	if got.Method != signer.BenchmarkMethodOllamaEval {
		t.Fatalf("method = %q, want %q", got.Method, signer.BenchmarkMethodOllamaEval)
	}
	if got.EngineKind != catalog.RuntimeOllama || got.EngineVersion == "" {
		t.Fatalf("engine = %q/%q, want ollama with a version", got.EngineKind, got.EngineVersion)
	}
	if _, err := time.Parse(time.RFC3339Nano, got.MeasuredAt); err != nil {
		t.Fatalf("measured_at = %q, want RFC3339Nano: %v", got.MeasuredAt, err)
	}
	if got.PromptTokens <= 0 || got.PrefillTokps <= 0 || got.DecodeTokps <= 0 || got.TurnSeconds <= 0 {
		t.Fatalf("published %+v with a non-positive figure", got)
	}
}

// The measurement outlives the daemon that took it. A host told local
// inference is off never reaches an install path again, so a figure held
// only in memory would leave `waired inference status` — and the control
// plane — with nothing to report after the first restart.
func TestHostSpeed_SurvivesARestart(t *testing.T) {
	p, eng, _ := hostCutoffProvider(t, cpuOnlyCounters, 0)
	if p.applyHostCutoff(context.Background()) {
		t.Fatal("the download proceeded on a host below the budget")
	}
	requests := len(eng.generateBodies())

	// A fresh provider on the same state dir is the next daemon start.
	next := &agentInferenceProvider{
		stateDir: p.stateDir,
		logger:   slog.New(slog.DiscardHandler),
	}
	got := next.hostSpeedNow()
	if got == nil {
		t.Fatal("the restarted daemon has no measurement; the reason local inference is off " +
			"is now unrecoverable from the machine itself")
	}
	if got.TurnSeconds <= hostfit.HostCutoffTurnBudgetSeconds {
		t.Fatalf("reloaded turn = %.1f s, want the below-budget figure that was measured", got.TurnSeconds)
	}
	if !next.hostSpeedTurnedInferenceOff() {
		t.Fatal("the restarted daemon no longer knows the measurement is why inference is off")
	}
	if now := len(eng.generateBodies()); now != requests {
		t.Fatalf("the restart re-measured (%d → %d requests)", requests, now)
	}
}

// "This is why local inference is off" is a claim about causation, so it
// stops being made the moment someone moves the toggle themselves. Without
// this, a person who ran `waired inference off` on a perfectly good
// machine would be told their machine is too slow.
func TestHostSpeed_TheReasonIsDroppedWhenSomeoneElseMovesTheToggle(t *testing.T) {
	p, _, _ := hostCutoffProvider(t, cpuOnlyCounters, 0)
	if p.applyHostCutoff(context.Background()) {
		t.Fatal("the download proceeded on a host below the budget")
	}
	if !p.hostSpeedTurnedInferenceOff() {
		t.Fatal("the cutoff did not record itself as the reason")
	}

	if err := state.WriteDesiredInferenceState(p.stateDir, state.InferenceEnabled); err != nil {
		t.Fatalf("opt back in: %v", err)
	}
	if p.hostSpeedTurnedInferenceOff() {
		t.Fatal("the measurement still claims to be why inference is off, after a person " +
			"turned it back on")
	}
	if p.hostSpeedNow() == nil {
		t.Fatal("the measurement itself was dropped; only the causal claim should be — the " +
			"figure is still a true fact about this host, and is still published")
	}
}

// The probe measures the model the threshold was calibrated on, or it does
// not run. Substituting whatever the host happens to have would produce a
// number that is not comparable to 45 seconds.
func TestHostCutoff_WithoutTheProbeModelInTheCatalog_NoVerdict(t *testing.T) {
	p, eng, disabled := hostCutoffProvider(t, cpuOnlyCounters, 0)
	p.manifests = bounceTestManifests() // no qwen3.5-0.8b in here

	if v := p.ensureHostSpeedMeasured(context.Background(), p.hostSpeedMeasureWindow()); v.Decided {
		t.Fatal("reached a measurement without the probe model; the threshold is calibrated " +
			"against one model and means nothing measured on another")
	}
	if got := len(eng.generateBodies()); got != 0 {
		t.Fatalf("/api/generate requests = %d, want 0", got)
	}
	if *disabled != 0 {
		t.Fatalf("disableInference calls = %d, want 0", *disabled)
	}
}

// A daemon started with --disable-inference has no controller to persist
// through, and the cutoff must not panic reaching for one. It still
// declines the download — a host that cannot serve has no use for the
// weights either way.
func TestApplyHostCutoff_WithNoInferenceController_DeclinesWithoutPanicking(t *testing.T) {
	p, _, _ := hostCutoffProvider(t, cpuOnlyCounters, 0)
	p.disableInference = nil

	if p.applyHostCutoff(context.Background()) {
		t.Fatal("the download proceeded on a host measured below the budget")
	}
}

// A failure to persist the verdict does not turn into a download. The
// measurement already said this host cannot serve; losing the write makes
// the next boot re-measure, which is recoverable — 45 GB of weights on a
// host that cannot use them is not.
func TestApplyHostCutoff_WhenPersistingFails_StillDeclinesTheDownload(t *testing.T) {
	p, _, _ := hostCutoffProvider(t, cpuOnlyCounters, 0)
	p.disableInference = func() error { return errors.New("read-only state dir") }

	if p.applyHostCutoff(context.Background()) {
		t.Fatal("the download proceeded because the verdict could not be written down")
	}
}

// THE #465 BAR. Turning local inference on is a choice the product keeps.
// The cutoff writes the same file `waired inference on` writes, so a
// cutoff that re-decided after the opt-in would silently take it back on
// the next restart — and on the slow host that is exactly the host that
// opted in, it would take it back EVERY restart. "A default with a working
// opt-in" would then be neither.
//
// The measurement still runs: it is a fact about the host, it is what the
// control plane and waired#1065 are asking for, and it is cached per
// engine build so it is taken once either way. Only the DECISION is
// withheld.
func TestApplyHostCutoff_OnceTheToggleHasBeenMoved_NeverOverridesIt(t *testing.T) {
	for _, choice := range []state.InferenceState{state.InferenceEnabled, state.InferenceDisabled} {
		t.Run(string(choice), func(t *testing.T) {
			// cpuOnlyCounters: a host the cutoff WOULD cut, so a pass here
			// can only come from standing down.
			p, _, disabled := hostCutoffProvider(t, cpuOnlyCounters, 0)
			if err := state.WriteDesiredInferenceState(p.stateDir, choice); err != nil {
				t.Fatalf("seed desired-inference: %v", err)
			}

			if !p.applyHostCutoff(context.Background()) {
				t.Fatal("the cutoff overrode a choice that had already been made on this host")
			}
			if *disabled != 0 {
				t.Fatalf("disableInference calls = %d, want 0", *disabled)
			}
			if p.hostSpeedTurnedInferenceOff() {
				t.Fatal("the cutoff claimed responsibility for a state it did not set")
			}
			if p.hostSpeedNow() == nil {
				t.Fatal("nothing was published; the figure is wanted from every host, not " +
					"only from the ones nobody has answered for")
			}
		})
	}
}

// The browser wizard's path measures too, and BEFORE the weights start
// arriving. It is the majority install path — a person picking a model
// stands the bundled pre-pull down entirely — so a measurement taken only
// on the other path would leave the control plane, the device page and
// waired#1065 with a figure for almost nobody.
//
// Measuring alongside a 20-45 GB transfer rather than before it would
// measure the contention: the one contended run in the calibration set
// landed 21 % off the median.
func TestSetupApplyModel_MeasuresBeforeTheDownloadStarts(t *testing.T) {
	p, eng, disabled := hostCutoffProvider(t, cpuOnlyCounters, 0)
	r := newBlockingRunner(t)
	p.puller = download.NewPuller("ollama-fake", r)

	// The error is not the subject: SwapPreferredModel's outcome depends
	// on engine state this test does not stand up. What matters is that
	// the host was measured on the way through.
	if _, err := p.setupApplyModel(context.Background(), "some-big-model"); err != nil {
		t.Logf("setupApplyModel returned %v (not the subject of this test)", err)
	}
	r.releaseAll()
	p.waitForPulls()

	if got := len(eng.generateBodies()); got == 0 {
		t.Fatal("the wizard path downloaded a model without measuring the host first")
	}
	if p.hostSpeedNow() == nil {
		t.Fatal("the wizard path published nothing")
	}
	// A person chose this model, so the cutoff does not get a vote.
	if *disabled != 0 {
		t.Fatalf("disableInference calls = %d, want 0 — the person driving the wizard said "+
			"they want to serve here", *disabled)
	}
}

// prePullCutoffProvider is hostCutoffProvider wired up as the pre-pull
// path sees it: an auto-selected bundled model, pulls allowed, and the
// #379 hold shortened so a test does not wait out real setup timings.
func prePullCutoffProvider(t *testing.T, answer map[string]any) (*agentInferenceProvider, *blockingRunner) {
	t.Helper()
	p, _, _ := hostCutoffProvider(t, answer, 0)
	r := newBlockingRunner(t)
	p.puller = download.NewPuller("ollama-fake", r)
	p.cfg.BundledModelID = "some-big-model"
	p.cfg.PullOnStartup = true
	p.prePullFrameGrace = 5 * time.Millisecond
	p.prePullHoldMax = time.Minute
	return p, r
}

// runHeldPrePull drives one bundled pre-pull to completion and returns the
// tags `ollama pull` was actually asked for.
func runHeldPrePull(t *testing.T, p *agentInferenceProvider, r *blockingRunner) []string {
	t.Helper()
	p.holdBundledPrePull(context.Background(), p.cfg.BundledModelID)
	// The control plane answered and nobody is driving this host, which
	// releases the #379 hold.
	p.setupNoteDesired("", false)
	r.releaseAll()
	p.waitForPulls()
	return r.pulledTags()
}

// THE END-TO-END #496 BAR: on a host below the recommended spec, no
// bundled weights are downloaded at all.
//
// Everything above tests the verdict; this tests that the verdict is
// actually in the path. The cutoff sits at the last point before the
// download and after the #379 hold, so a wizard that named a model has
// already stood the fallback down and never reaches it.
func TestPrePullHold_HostBelowTheBudget_DownloadsNothing(t *testing.T) {
	p, r := prePullCutoffProvider(t, cpuOnlyCounters)

	if got := runHeldPrePull(t, p, r); len(got) != 0 {
		t.Fatalf("tags pulled = %v, want none — this host measures ~66 s per turn, and "+
			"the measurement exists precisely to happen before the download", got)
	}
}

// The ordinary host is untouched. A cutoff that cost every capable machine
// its model would be worse than no cutoff.
func TestPrePullHold_HostAboveTheBudget_DownloadsAsBefore(t *testing.T) {
	p, r := prePullCutoffProvider(t, gpuCounters)

	got := runHeldPrePull(t, p, r)
	if len(got) != 1 || got[0] != "big:q4" {
		t.Fatalf("tags pulled = %v, want [big:q4]", got)
	}
}

// THE STRUCTURAL CLAIM OF waired-agent#579, at the bar the two tests above
// share: a measurement that cannot finish does not stop the download.
//
// The cutoff sits in front of the bundled pull by design (waired#1099 — measure
// before recommending), so anything unbounded there is unbounded in front of
// the install. Before the deadline this hung for hostCutoffProbeTimeout and the
// pull was dispatched only after; on the macOS runner that was 9m29s, and the
// pull that followed took 11.5 s. It was never slow — it was never started.
//
// Undecided still means "carry on unchanged", which is why the tags come back
// non-empty here: a wedged engine must never read as a slow host.
func TestPrePullHold_ASlowMeasurementDoesNotStarveTheDownload(t *testing.T) {
	p, eng, _ := hostCutoffProvider(t, cpuOnlyCounters, 0)
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	eng.mu.Lock()
	eng.block = block
	eng.mu.Unlock()

	r := newBlockingRunner(t)
	p.puller = download.NewPuller("ollama-fake", r)
	p.cfg.BundledModelID = "some-big-model"
	p.cfg.PullOnStartup = true
	p.prePullFrameGrace = 5 * time.Millisecond
	p.prePullHoldMax = time.Minute
	p.hostSpeedWindow = 200 * time.Millisecond

	started := time.Now()
	got := runHeldPrePull(t, p, r)
	elapsed := time.Since(started)

	if len(got) != 1 || got[0] != "big:q4" {
		t.Fatalf("tags pulled = %v, want [big:q4] — a measurement that could not finish is "+
			"undecided, and undecided leaves the download exactly as it was", got)
	}
	if elapsed > 20*time.Second {
		t.Fatalf("the pre-pull took %s behind a 200 ms measurement window", elapsed)
	}
}

// The budgets partition the install window, and the numbers in the failure
// messages are the measurements they were derived from.
//
// This is the whole of waired-agent#579 as arithmetic: a calibration at its
// ceiling plus one full-depth sample at its ceiling still fits the budget, and
// the budget plus the probe download still fits the call's deadline. Move any
// one of them and this says the window stopped closing.
//
// Record of today's behaviour, not a ratified contract: the figures come from
// run 31255547516's macOS leg (macos-14, 3 vCPU M1 / 7 GB — the permanent
// hardware of the install+inference leg) and from proto/hostfit's reference
// host, not from an owner ruling.
func TestHostSpeedBudgets_PartitionTheInstallWindow(t *testing.T) {
	if got := hostCutoffCalibrationTimeout + hostCutoffProbeTimeout; got != hostCutoffMeasureBudget {
		t.Errorf("calibration ceiling + sample ceiling = %s, want hostCutoffMeasureBudget %s — "+
			"a host sitting at both ceilings must still publish a one-sample measurement",
			got, hostCutoffMeasureBudget)
	}
	if got := hostCutoffPullTimeout + hostCutoffMeasureBudget; got != hostSpeedMeasureDeadline {
		t.Errorf("probe-download wait + measurement budget = %s, want hostSpeedMeasureDeadline %s",
			got, hostSpeedMeasureDeadline)
	}
	// The measured worst cases these ceilings exist to hold, with ~25% margin
	// for runner contention. Not "a slower machine could exist" — a slower
	// machine reaches the undecided arm on purpose.
	if hostCutoffProbeTimeout < 432*time.Second {
		t.Errorf("hostCutoffProbeTimeout = %s, but one sample measured 432 s on the macOS runner",
			hostCutoffProbeTimeout)
	}
	if hostCutoffCalibrationTimeout < 137*time.Second {
		t.Errorf("hostCutoffCalibrationTimeout = %s, but the calibration measured 137 s on the macOS runner",
			hostCutoffCalibrationTimeout)
	}
}

// Sample 1 is inside the budget. Before waired-agent#579 the budget was
// consulted only at `sample > 1`, so the first sample ran until
// hostCutoffProbeTimeout no matter what the budget said — which is how one
// measurement occupied the window the bundled download was waiting behind.
func TestMeasureHostCutoff_SampleOneIsInsideTheBudget(t *testing.T) {
	block := make(chan struct{})
	var mu sync.Mutex
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		requests++
		n := requests
		mu.Unlock()
		if n == 1 { // the calibration answers; the sample never does
			body, _ := json.Marshal(calibrationCounters)
			_, _ = w.Write(body)
			return
		}
		<-block
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(block) })

	started := time.Now()
	_, err := measureHostCutoff(context.Background(), hostCutoffDeps{
		BaseURL: srv.URL, EngineModel: hostCutoffFixtureTag,
		HTTPClient: srv.Client(), Logger: slog.New(slog.DiscardHandler),
		Nonce: "sample1", MeasureBudget: 200 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("a sample that never answered produced no error")
	}
	// The bound, not the exact figure: what must not happen is waiting
	// hostCutoffProbeTimeout for a budget that said 200 ms.
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("measureHostCutoff took %s on a 200 ms budget — sample 1 is outside it again", elapsed)
	}
}

// The calibration cannot eat the budget. It sends 50 filler lines with
// num_predict:1; giving it the ceiling a 21k-token prefill gets is what let it
// consume a whole measurement before any sample ran.
func TestMeasureHostCutoff_TheCalibrationCannotEatTheBudget(t *testing.T) {
	block := make(chan struct{})
	var mu sync.Mutex
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		requests++
		n := requests
		mu.Unlock()
		if n == 1 { // the calibration hangs; the samples answer
			<-block
			return
		}
		body, _ := json.Marshal(cpuOnlyCounters)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(block) })

	started := time.Now()
	m, err := measureHostCutoff(context.Background(), hostCutoffDeps{
		BaseURL: srv.URL, EngineModel: hostCutoffFixtureTag,
		HTTPClient: srv.Client(), Logger: slog.New(slog.DiscardHandler),
		Nonce:              "calib",
		MeasureBudget:      2 * time.Second,
		CalibrationTimeout: 100 * time.Millisecond,
	})
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("measureHostCutoff took %s — the calibration is unbounded again", elapsed)
	}
	// A calibration that times out is soft: the seed estimate stands in and
	// the samples still run. That is the pre-existing contract at
	// host_cutoff_probe.go's "using the seed estimate" arm; this only pins
	// that bounding it did not turn it fatal.
	if err != nil {
		t.Fatalf("a timed-out calibration ended the whole measurement: %v", err)
	}
	if !m.Probe.Measured() {
		t.Fatal("no usable measurement after a timed-out calibration; the seed estimate should have carried it")
	}
}

// One ensureHostSpeedMeasured call is bounded end to end — the probe-download
// wait and the measurement together — so the bundled download waiting behind
// it (inference_prepull_hold.go) has a ceiling it can be reasoned about.
func TestEnsureHostSpeedMeasured_IsBoundedByTheInstallWindow(t *testing.T) {
	p, eng, _ := hostCutoffProvider(t, cpuOnlyCounters, 0)
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	eng.mu.Lock()
	eng.block = block
	eng.mu.Unlock()
	p.hostSpeedWindow = 200 * time.Millisecond

	started := time.Now()
	v := p.ensureHostSpeedMeasured(context.Background(), p.hostSpeedMeasureWindow())
	elapsed := time.Since(started)

	if v.Decided {
		t.Fatalf("an engine that never answered produced a verdict: %+v", v)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("ensureHostSpeedMeasured took %s on a 200 ms window", elapsed)
	}
	if p.hostSpeedNow() != nil {
		t.Error("a measurement that never completed was published anyway")
	}
}

// A measurement whose engine version could not be read at the START is
// published with the version read at the END.
//
// This is the whole of waired-agent#637. Real hardware produced a record
// with the engine_version key simply ABSENT, and such a record can never
// be reused — hostSpeedStillApplies rejects an empty version by design
// (waired#668). So the host re-measured on every daemon start, ~82 s
// each, until something happened to overwrite it. Nothing guarantees the
// overwrite.
//
// The version is unreadable at the top of the call because that is the
// boot path: the adapter has recorded nothing yet, the profiler snapshot
// is cold, and probedOllamaVersion memoises a failed exec. A minute later
// a serving engine has been answering requests and can say what it is.
//
// Product contract (waired-agent#637).
func TestEnsureHostSpeedMeasured_RecordsTheVersionItCouldReadAtTheEnd(t *testing.T) {
	var reads atomic.Int64
	tick := int64(0)
	prof := hardware.NewProfiler(t.TempDir(),
		// An advancing clock, so the profiler's own snapshot cache expires
		// between the two reads rather than serving the first answer twice.
		hardware.WithNow(func() time.Time {
			tick++
			return time.Unix(1_800_000_000+tick*3600, 0)
		}),
		hardware.WithGPU(func(context.Context) ([]hardware.GPU, hardware.Accelerators, error) {
			return nil, hardware.Accelerators{}, nil
		}),
		hardware.WithEngineVersion(func(_ context.Context, name string) (bool, string) {
			if name != "ollama" {
				return false, ""
			}
			if reads.Add(1) == 1 {
				// The engine is still coming up.
				return true, ""
			}
			return true, "0.31.1"
		}),
	)

	p, _, _ := hostCutoffProvider(t, cpuOnlyCounters, 0)
	p.profiler = prof

	if v := p.ensureHostSpeedMeasured(context.Background(), p.hostSpeedMeasureWindow()); !v.Decided {
		t.Fatal("no measurement")
	}
	got := p.hostSpeedNow()
	if got == nil {
		t.Fatal("nothing published")
	}
	if got.EngineVersion != "0.31.1" {
		t.Fatalf("engine_version = %q, want %q. A record with no version can never be reused, "+
			"so this host would re-measure on every single daemon start (waired-agent#637)",
			got.EngineVersion, "0.31.1")
	}
}

// And the cost that record carries, pinned so the fix above cannot be
// dropped without something failing: a stored measurement with no engine
// version is measured again rather than reused.
//
// Record of today's behaviour AND the deliberate rule behind it
// (waired#668): a figure that cannot say what produced it must not
// survive an engine bump. The defect in waired-agent#637 was never this
// rule — it was writing a record the rule can only reject.
func TestEnsureHostSpeedMeasured_AStoredRecordWithNoEngineVersionIsNotReused(t *testing.T) {
	p, eng, _ := hostCutoffProvider(t, cpuOnlyCounters, 0)
	if v := p.ensureHostSpeedMeasured(context.Background(), p.hostSpeedMeasureWindow()); !v.Decided {
		t.Fatal("the first measurement did not land")
	}
	first := len(eng.generateBodies())

	// Poison it exactly the way the real host's record was poisoned.
	p.hostSpeedMu.Lock()
	p.hostSpeed.EngineVersion = ""
	p.persistHostSpeedLocked(false)
	p.hostSpeedMu.Unlock()

	if v := p.ensureHostSpeedMeasured(context.Background(), p.hostSpeedMeasureWindow()); !v.Decided {
		t.Fatal("the second call reached no verdict")
	}
	if got := len(eng.generateBodies()); got == first {
		t.Fatalf("/api/generate requests stayed at %d: a record with no engine version was "+
			"reused. waired#668 requires it not to be — the fix is to never write one, "+
			"not to start trusting it", first)
	}
}

// The two install callers, one after the other, measure ONCE between them.
//
// This is the SEQUENTIAL twin of the queued-caller test below, and the case
// that was never covered: applyHostCutoff (the bundled pre-pull) and
// setup_desired's apply path (a model chosen in the browser) both call this,
// and on a control-plane-driven install both run — minutes apart, with the
// single-flight lock free by the time the second arrives. Nothing but the
// cache stands between that and a second full measurement.
//
// Record of today's behaviour, and a floor rather than a reproduction: this
// passes today, and waired-agent#637 is a real host that measured twice
// anyway. What the fixture cannot express is the engine restart the second
// caller is suspected of landing in the middle of — engineVersionFor is
// stable here by construction. So this pins the contract and will catch a
// regression in the cache logic; it does not close #637.
func TestEnsureHostSpeedMeasured_TheSecondInstallCallerDoesNotMeasureAgain(t *testing.T) {
	p, eng, _ := hostCutoffProvider(t, cpuOnlyCounters, 0)
	hostCutoffEngineUp(t, p)

	// The pre-pull path: the install window, because a download waits on it.
	if v := p.ensureHostSpeedMeasured(context.Background(), p.hostSpeedInstallWindow()); !v.Decided {
		t.Fatal("the first caller did not measure")
	}
	first := len(eng.generateBodies())
	if first == 0 {
		t.Fatal("the first caller issued no requests at all")
	}

	// The wizard's apply path, later, on its own window. Same install, same
	// engine build, so there is nothing left to learn about this host.
	v := p.ensureHostSpeedMeasured(context.Background(), p.hostSpeedInstallWindow())
	if !v.Decided {
		t.Fatal("the second caller reached no verdict; it should have reused the first's")
	}
	if got := len(eng.generateBodies()); got != first {
		t.Fatalf("/api/generate requests %d -> %d: the second install caller measured this host "+
			"again. Each caller takes its own window, so two measurements is two windows of the "+
			"install spent deciding one thing (waired-agent#637)", first, got)
	}
}

// A caller that waited out the window still gets the first caller's answer.
//
// The deadline is anchored before the single-flight lock so a queued caller
// cannot add a second window to the one it just waited through. That makes it
// possible to acquire the lock with nothing left — and the cache read has to
// happen on the PARENT context anyway, or engineVersionFor returns "" on the
// dead one, hostSpeedStillApplies rejects the empty version, and the caller
// throws away a perfectly good measurement and re-measures.
func TestEnsureHostSpeedMeasured_AQueuedCallerStillReadsThePublishedMeasurement(t *testing.T) {
	p, eng, _ := hostCutoffProvider(t, cpuOnlyCounters, 0)

	if v := p.ensureHostSpeedMeasured(context.Background(), p.hostSpeedMeasureWindow()); !v.Decided {
		t.Fatal("the first call did not measure")
	}
	before := len(eng.generateBodies())

	// Enter with a window that has already run out, which is what a caller
	// that queued behind a full-length measurement sees.
	p.hostSpeedWindow = time.Nanosecond
	reused := p.ensureHostSpeedMeasured(context.Background(), p.hostSpeedMeasureWindow())
	if !reused.Decided {
		t.Fatal("the queued caller re-measured instead of reusing the published figure — " +
			"the cache read is on the deadline ctx, not the parent")
	}
	if reused.TurnSeconds <= 0 || reused.Method != signer.BenchmarkMethodOllamaEval {
		t.Fatalf("the reused verdict is not a full measurement: %+v", reused)
	}
	if after := len(eng.generateBodies()); after != before {
		t.Fatalf("/api/generate requests %d -> %d: the queued caller measured again", before, after)
	}
}

// The second half of waired-agent#579: the window is a property of the
// CALLER, not of the measurement. Stage 2 bounded the measurement and the
// bound held — on run 31316731884 the linux pre-pull released at 14:28:49
// and the model was dispatched at 14:45:11, sixteen minutes later, which is
// hostSpeedMeasureDeadline exactly. The download then took 21.9 s, and init
// had stopped waiting at minute ten.
//
// So the background window is generous here (30 s) and the install window
// is not (50 ms), and the pre-pull has to take the second one. Before this
// change the arrangement was impossible to express: there was one window,
// and the pull waited it out.
//
// Product contract (waired-agent#579). Not parallel: it swaps a
// package-level window.
func TestPrePullHold_TheMeasurementTakesOnlyTheShareTheDownloadCanSpare(t *testing.T) {
	defer hostspeed.SwapInstallWindowForTest(50 * time.Millisecond)()

	p, eng, _ := hostCutoffProvider(t, cpuOnlyCounters, 0)
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	eng.mu.Lock()
	eng.block = block
	eng.mu.Unlock()

	r := newBlockingRunner(t)
	p.puller = download.NewPuller("ollama-fake", r)
	p.cfg.BundledModelID = "some-big-model"
	p.cfg.PullOnStartup = true
	p.prePullFrameGrace = 5 * time.Millisecond
	p.prePullHoldMax = time.Minute
	// Generous, and deliberately far above the install window: this is what
	// the boot goroutine would get, and the pre-pull must not.
	p.hostSpeedWindow = 30 * time.Second

	started := time.Now()
	got := runHeldPrePull(t, p, r)
	elapsed := time.Since(started)

	if len(got) != 1 || got[0] != "big:q4" {
		t.Fatalf("tags pulled = %v, want [big:q4] — a measurement that could not finish is "+
			"undecided, and undecided leaves the download exactly as it was", got)
	}
	// The number that matters. On the background window this is ~30 s; on
	// the install window it is ~50 ms. 10 s is far below the one and far
	// above the other, so the assert says which window was used without
	// being a timing race.
	if elapsed > 10*time.Second {
		t.Fatalf("the pre-pull took %s — it waited out the 30 s BACKGROUND window instead of the "+
			"50 ms install window, which is #579 with a smaller number", elapsed)
	}
}

// hostSpeedInstallWindow is the smaller of the two, never a replacement for
// the caller's own. A test that shrinks the background window to
// milliseconds must not find the install path still holding five minutes.
//
// Record of today's behaviour, not a product contract: the clamp exists so
// the existing hostSpeedWindow seam keeps working for both paths.
func TestHostSpeedInstallWindow_IsNeverLongerThanTheBackgroundOne(t *testing.T) {
	defer hostspeed.SwapInstallWindowForTest(5 * time.Minute)()

	p := &agentInferenceProvider{}
	if got, want := p.hostSpeedInstallWindow(), 5*time.Minute; got != want {
		t.Fatalf("with no override: install window = %v, want %v", got, want)
	}
	p.hostSpeedWindow = 200 * time.Millisecond
	if got, want := p.hostSpeedInstallWindow(), 200*time.Millisecond; got != want {
		t.Fatalf("with a 200 ms background window: install window = %v, want %v — a test that "+
			"shrinks the measurement must shrink the wait in front of the download too", got, want)
	}
	if got, want := p.hostSpeedMeasureWindow(), 200*time.Millisecond; got != want {
		t.Fatalf("background window = %v, want %v", got, want)
	}
}

// ── waired-agent#703: two measurements, one engine ───────────────────
//
// The install-time host-speed probe and the boot/setup benchmark both
// monopolise the engine, and both used to ask "is it quiet" through a
// predicate that read four things — a pull, a reconcile, a parked engine,
// health — none of them a REQUEST and none of them the other measurement.
// On real hardware they overlapped and a host measured at 12.017 s
// published 39.473 s, the contended-host signature waired#1140 documents.

// PRODUCT CONTRACT (waired-agent#703): serving traffic is not a quiet
// engine. Since infruntime.MaxResidentModels the engine holds one model at
// a time, so a request arriving mid-measurement evicts the probe rather
// than merely competing with it.
func TestEngineIsQuiet_ServingTrafficIsNotQuiet(t *testing.T) {
	p, _, _ := hostCutoffProvider(t, gpuCounters, 0)
	hostCutoffEngineUp(t, p)
	ctx := context.Background()

	if !p.engineIsQuiet(ctx) {
		t.Fatal("an idle ready engine answered busy")
	}

	serving := 0
	p.servingInflight = func() int { return serving }
	if !p.engineIsQuiet(ctx) {
		t.Error("answered busy with nothing in flight")
	}
	serving = 1
	if p.engineIsQuiet(ctx) {
		t.Error("answered quiet while this host was serving a request")
	}
	// And the benchmark inherits it through the same predicate, which is
	// what stops the two measurements measuring the same contention from
	// opposite ends.
	if p.engineQuietForBench(ctx) {
		t.Error("engineQuietForBench answered quiet while this host was serving")
	}
	serving = 0
	if !p.engineIsQuiet(ctx) {
		t.Error("stayed busy after the request finished")
	}
}

// Record of today's behaviour: an unwired counter is a host with no
// inference server, and a host with no inference server is serving
// nothing. Pinned because the nil case is the one every unit test and the
// whole pre-session boot window take.
func TestEngineIsQuiet_NoAdmissionCounterIsNotBusy(t *testing.T) {
	p, _, _ := hostCutoffProvider(t, gpuCounters, 0)
	hostCutoffEngineUp(t, p)
	if p.servingInflight != nil {
		t.Fatal("the fixture wired a counter; this test is about the nil one")
	}
	if !p.engineIsQuiet(context.Background()) {
		t.Error("a provider with no admission counter answered busy")
	}
}

// PRODUCT CONTRACT (waired-agent#703): the exclusive claim is what keeps
// the two measurements apart, and the HOLDER must not be gated by its own
// claim. Both of them re-ask the quiet question while they hold it — the
// benchmark once per bounce-grace retry, the screen from inside
// measureHostCutoff — so a predicate that read the claim would make each
// stand down on itself.
func TestClaimEngineExclusive_TheHolderIsNotBlockedByItsOwnClaim(t *testing.T) {
	p, _, _ := hostCutoffProvider(t, gpuCounters, 0)
	hostCutoffEngineUp(t, p)
	ctx := context.Background()

	release, ok := p.claimEngineExclusive()
	if !ok {
		t.Fatal("could not claim a free engine")
	}
	if !p.engineIsQuiet(ctx) {
		t.Error("engineIsQuiet answered busy to the holder of the claim")
	}
	if !p.engineQuietForBench(ctx) {
		t.Error("engineQuietForBench answered busy to the holder of the claim")
	}
	// The waiter is the one that sees it.
	if p.engineIsQuietAndUnclaimed(ctx) {
		t.Error("engineIsQuietAndUnclaimed answered quiet while the engine was claimed")
	}
	if _, again := p.claimEngineExclusive(); again {
		t.Error("a second claim succeeded; the two measurements would run together")
	}

	release()
	if !p.engineIsQuietAndUnclaimed(ctx) {
		t.Error("still claimed after release")
	}
	second, ok := p.claimEngineExclusive()
	if !ok {
		t.Fatal("could not re-claim after release")
	}
	// Idempotent. The benchmark defers its release and returns through
	// several arms; a stale release firing twice would hand the engine
	// away from the run that holds it now.
	release()
	if _, stolen := p.claimEngineExclusive(); stolen {
		t.Error("a stale release freed a claim it no longer owned")
	}
	second()
}

// Record of today's behaviour: a host this provider does not drive with
// ollama has nothing here to collide with, so it is handed the engine
// rather than gated off its own benchmark — the same nil-end reasoning
// engineQuietForBench already applies (#582/#601).
func TestClaimEngineForBench_AVLLMHostIsAlwaysHandedTheEngine(t *testing.T) {
	p, _, _ := hostCutoffProvider(t, gpuCounters, 0)
	hostCutoffEngineUp(t, p)
	p.setServingEngine(catalog.RuntimeVLLM)

	held, ok := p.claimEngineExclusive()
	if !ok {
		t.Fatal("could not claim")
	}
	defer held()
	if _, got := p.claimEngineForBench(); !got {
		t.Error("a vLLM host was refused the engine; nothing here measures it, " +
			"so this would gate its benchmark off forever")
	}
}

// PRODUCT CONTRACT (waired-agent#703): a reading taken while this host was
// serving is not published. It describes the two of them sharing an engine
// that holds one model at a time, and nothing in the numbers says which is
// which — on real hardware the contended run's SPREAD across samples was
// 2.70% against a clean 1.78%, so the statistical shape is no help. The
// stored figure survives and a later start tries again, the answer the
// adopted-engine arm gives to the same question.
//
// The fake moves the counter BETWEEN the two reads rather than from a
// goroutine racing the probe: a request that starts and finishes inside
// the window is exactly the case a gauge cannot see, and it is the case
// worth pinning.
func TestEnsureHostSpeedMeasured_ServingDuringTheProbeDiscardsTheReading(t *testing.T) {
	p, _, _ := hostCutoffProvider(t, gpuCounters, 0)
	hostCutoffEngineUp(t, p)
	ctx := context.Background()

	// A good figure first, so the test can watch it SURVIVE.
	if v := p.ensureHostSpeedMeasured(ctx, p.hostSpeedMeasureWindow()); !v.Decided {
		t.Fatal("the first measurement did not decide")
	}
	stored := p.hostSpeedNow()
	if stored == nil {
		t.Fatal("nothing published")
	}

	reads := 0
	p.servingAdmitted = func() uint64 {
		reads++
		if reads == 1 {
			return 7 // before the probe
		}
		return 8 // one request served while it ran
	}
	p.hostSpeedTakenHere.Store(false)
	p.hostSpeedForce.Store(true)

	if v := p.ensureHostSpeedMeasured(ctx, p.hostSpeedMeasureWindow()); v.Decided {
		t.Error("a contended reading was returned as a verdict")
	}
	if reads != 2 {
		t.Errorf("the admission counter was read %d times, want 2 (once each side of the probe)", reads)
	}
	if now := p.hostSpeedNow(); now == nil || now.MeasuredAt != stored.MeasuredAt {
		t.Errorf("the stored measurement was overwritten by the discarded one: %+v", now)
	}
}

// PRODUCT CONTRACT (waired-agent#599): an install-flow re-run that loses
// the engine to the other measurement has still ASKED for a fresh figure,
// and the ask must survive.
//
// The gap this covers is not small — awaitQuietEngine asks its question
// before the probe model's own download — so losing the flag here would
// leave a re-run quietly reusing the stored figure for the life of the
// install, which is the whole thing #599 rules against.
func TestEnsureHostSpeedMeasured_ALostEngineKeepsTheAskAlive(t *testing.T) {
	p, _, _ := hostCutoffProvider(t, gpuCounters, 0)
	hostCutoffEngineUp(t, p)
	ctx := context.Background()

	if v := p.ensureHostSpeedMeasured(ctx, p.hostSpeedMeasureWindow()); !v.Decided {
		t.Fatal("the first measurement did not decide")
	}
	stored := p.hostSpeedNow()

	// The other measurement takes the engine, and the re-run arrives.
	release, ok := p.claimEngineExclusive()
	if !ok {
		t.Fatal("could not claim")
	}
	p.hostSpeedForce.Store(true)
	p.ensureHostSpeedMeasured(ctx, p.hostSpeedMeasureWindow())
	if now := p.hostSpeedNow(); now == nil || now.MeasuredAt != stored.MeasuredAt {
		t.Errorf("the stored measurement did not survive a call that never reached the engine: %+v", now)
	}
	if !p.hostSpeedForce.Load() {
		t.Error("the ask was consumed by a call that never reached the engine")
	}

	// And the next start honours it.
	release()
	if v := p.ensureHostSpeedMeasured(ctx, p.hostSpeedMeasureWindow()); !v.Decided {
		t.Fatal("the restored ask did not re-measure")
	}
	if p.hostSpeedForce.Load() {
		t.Error("the ask latched; this host would re-measure on every start")
	}
	if now := p.hostSpeedNow(); now == nil || now.MeasuredAt == stored.MeasuredAt {
		t.Error("the re-measure did not publish a new figure")
	}
}

// Record of today's behaviour: an untouched counter publishes. The guard
// above must not fire on the ordinary path, and a fake that answered the
// same number by accident would hide that.
func TestEnsureHostSpeedMeasured_AnIdleHostStillPublishes(t *testing.T) {
	p, _, _ := hostCutoffProvider(t, gpuCounters, 0)
	hostCutoffEngineUp(t, p)
	p.servingAdmitted = func() uint64 { return 42 }

	if v := p.ensureHostSpeedMeasured(context.Background(), p.hostSpeedMeasureWindow()); !v.Decided {
		t.Fatal("an idle host did not publish a measurement")
	}
	if p.hostSpeedNow() == nil {
		t.Error("nothing published on a host that served nothing")
	}
}
