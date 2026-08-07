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
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/download"
	"github.com/waired-ai/waired-agent/internal/hardware"
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
	case "/api/generate":
		raw, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(raw, &parsed)
		e.mu.Lock()
		e.bodies = append(e.bodies, parsed)
		status := e.status
		answer := map[string]any(nil)
		if n := len(e.answers); n > 0 {
			answer = e.answers[min(len(e.bodies)-1, n-1)]
		}
		e.mu.Unlock()
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
	if _, measured := p.ensureHostSpeedMeasured(context.Background()); !measured {
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
	if _, measured := p.ensureHostSpeedMeasured(context.Background()); !measured {
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
	if _, measured := p.ensureHostSpeedMeasured(context.Background()); !measured {
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
	var mu sync.Mutex
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		time.Sleep(40 * time.Millisecond)
		body, _ := json.Marshal(cpuOnlyCounters)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	m, err := measureHostCutoff(context.Background(), hostCutoffDeps{
		BaseURL: srv.URL, EngineModel: hostCutoffFixtureTag,
		HTTPClient: srv.Client(), Logger: slog.New(slog.DiscardHandler),
		Nonce: "budget", MeasureBudget: 50 * time.Millisecond,
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
	mu.Lock()
	got := requests
	mu.Unlock()
	if got != 2 {
		t.Fatalf("/api/generate requests = %d, want 2 (the calibration, then one sample)", got)
	}
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

// The measurement is taken once per ENGINE BUILD. Re-measuring on every
// install path would cost a minute or two of a user's install for an
// answer already on disk; never re-measuring would keep serving pre-bump
// numbers after an Ollama bundle bump, which is waired#668 exactly.
func TestEnsureHostSpeedMeasured_OncePerEngineBuild(t *testing.T) {
	p, eng, _ := hostCutoffProvider(t, gpuCounters, 0)
	ctx := context.Background()

	if _, measured := p.ensureHostSpeedMeasured(ctx); !measured {
		t.Fatal("first call did not measure")
	}
	afterFirst := len(eng.generateBodies())

	if _, measured := p.ensureHostSpeedMeasured(ctx); !measured {
		t.Fatal("second call lost the measurement")
	}
	if got := len(eng.generateBodies()); got != afterFirst {
		t.Fatalf("/api/generate requests = %d after a second call, want %d — the answer was "+
			"already taken on this engine build", got, afterFirst)
	}

	// A bundle bump is a different engine, so the figure no longer
	// describes what is running.
	p.profiler = hostCutoffProfiler(t, "0.32.0")
	if _, measured := p.ensureHostSpeedMeasured(ctx); !measured {
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

// The published record says what it is, on what, and how it was taken.
// Every one of these fields is load-bearing for a consumer that has to
// decide whether the number is comparable to its own threshold.
func TestHostSpeed_PublishedRecordCarriesItsProvenance(t *testing.T) {
	p, _, _ := hostCutoffProvider(t, gpuCounters, 0)
	if _, measured := p.ensureHostSpeedMeasured(context.Background()); !measured {
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

	if _, measured := p.ensureHostSpeedMeasured(context.Background()); measured {
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
