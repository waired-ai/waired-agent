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
	"github.com/waired-ai/waired-agent/internal/router"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// hostCutoffProbeTag is the ollama tag the fixture catalog serves the
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

// hostCutoffEngine is a fake ollama serving the probe tag. It records the
// /api/generate request bodies it was sent — the request shape is half of
// what this file guards — and answers with whatever the test supplied.
type hostCutoffEngine struct {
	mu     sync.Mutex
	bodies []map[string]any
	status int
	// answers are returned one per request; the last one repeats. A
	// single entry is the ordinary case, more than one lets a test drive
	// the widen-and-retry.
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
			ModelID: router.HostCutoffProbeModelID,
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

// hostCutoffProvider builds a provider whose engine is the fake above,
// with the probe model already on disk so the measurement is what the
// test exercises rather than a download.
func hostCutoffProvider(t *testing.T, answer map[string]any, status int) (*agentInferenceProvider, *hostCutoffEngine, *int) {
	t.Helper()
	return hostCutoffProviderAnswering(t, []map[string]any{answer}, status)
}

// hostCutoffProviderAnswering is hostCutoffProvider with one answer per
// request, for the tests that drive more than one.
func hostCutoffProviderAnswering(t *testing.T, answers []map[string]any, status int) (*agentInferenceProvider, *hostCutoffEngine, *int) {
	t.Helper()
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
	stateDir := t.TempDir()
	p := &agentInferenceProvider{
		ollama:           a,
		manifests:        hostCutoffManifests(),
		stateDir:         stateDir,
		store:            catalog.NewStore(filepath.Join(stateDir, "state.json")),
		cfg:              agentconfig.InferenceConfig{AllowPull: true},
		profiler:         cpuSwapProfiler(t),
		logger:           slog.New(slog.DiscardHandler),
		agentCtx:         context.Background(),
		ollamaUsable:     func() bool { return true },
		disableInference: func() error { disabled++; return nil },
	}
	return p, eng, &disabled
}

// THE #496 BAR. PRODUCT CONTRACT: the host the measurement says cannot
// serve does not download 20-45 GB of weights, and is left with local
// inference off and a working opt-in rather than with a model it would
// wait minutes per turn for.
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
}

// PRODUCT CONTRACT: a host that clears the budget is left completely
// alone. The cutoff decides nothing else — not which model, not whether
// to pull, not the engine.
func TestApplyHostCutoff_AboveTheBudget_ChangesNothing(t *testing.T) {
	p, _, disabled := hostCutoffProvider(t, gpuCounters, 0)

	if !p.applyHostCutoff(context.Background()) {
		t.Fatal("the pre-pull was blocked on a host measured at ~4.5 s/turn")
	}
	if *disabled != 0 {
		t.Fatalf("disableInference calls = %d, want 0", *disabled)
	}
}

// PRODUCT CONTRACT: no verdict means no change. An engine that errors,
// one that reports no counters, and one that truncated the prefill are
// all indistinguishable from a wedged engine — and turning local
// inference off on that evidence would cut hosts that serve perfectly
// well. Every one of these must leave the install path exactly as it was.
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
		})
	}
}

// PRODUCT CONTRACT: the request the probe sends is the request the
// threshold was calibrated with. Three of these four fields silently
// invalidate the measurement if they go missing:
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
	if _, decided := p.hostMeetsRecommendedSpec(context.Background()); !decided {
		t.Fatal("no verdict from a well-formed engine response")
	}

	bodies := eng.generateBodies()
	if len(bodies) != 1 {
		t.Fatalf("/api/generate requests = %d, want exactly 1 — the cutoff is one measurement", len(bodies))
	}
	body := bodies[0]

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
	wantCtx := float64(hostCutoffWindowSlots*router.HostCutoffProbeDepthTokens + hostCutoffWindowMargin)
	if got, _ := opts["num_ctx"].(float64); got != wantCtx {
		t.Fatalf("options.num_ctx = %v, want %v — the engine serves its own default window "+
			"without it, and divides whatever it is given among its parallel slots",
			opts["num_ctx"], wantCtx)
	}
	if got, _ := opts["num_predict"].(float64); got != float64(router.HostCutoffCompletionSampleTokens) {
		t.Fatalf("options.num_predict = %v, want %d", opts["num_predict"], router.HostCutoffCompletionSampleTokens)
	}
	if got, _ := opts["temperature"].(float64); got != 0 {
		t.Fatalf("options.temperature = %v, want 0", opts["temperature"])
	}
	prompt, _ := body["prompt"].(string)
	if len(prompt) < 1000 {
		t.Fatalf("prompt is %d bytes; the measurement needs a ~%d-token prefill",
			len(prompt), router.HostCutoffProbeDepthTokens)
	}
	if got, _ := body["stream"].(bool); got {
		t.Fatal("stream = true; the counters only arrive on the final non-streamed object")
	}
}

// PRODUCT CONTRACT: a capped prompt is measured again, wider, rather
// than thrown away.
//
// num_ctx is not a prompt budget — ollama divides it among its parallel
// slots and truncates to what one slot holds. Measured on the reference
// host, a request for 23048 came back having prefilled 11526. Without
// this retry the cutoff would reach no verdict on any host whose engine
// splits further than expected, which is a feature that silently does
// nothing.
func TestMeasureHostCutoff_WhenTheEngineCapsThePrompt_MeasuresAgainWider(t *testing.T) {
	capped := map[string]any{
		"prompt_eval_count": 11526, "prompt_eval_duration": int64(13_740_000_000),
		"eval_count": 200, "eval_duration": int64(5_700_000_000),
	}
	p, eng, disabled := hostCutoffProviderAnswering(t, []map[string]any{capped, cpuOnlyCounters}, 0)

	if p.applyHostCutoff(context.Background()) {
		t.Fatal("the download proceeded; the second measurement reached the full depth " +
			"and says this host takes ~66 s per turn")
	}
	if *disabled != 1 {
		t.Fatalf("disableInference calls = %d, want 1", *disabled)
	}

	bodies := eng.generateBodies()
	if len(bodies) != 2 {
		t.Fatalf("/api/generate requests = %d, want 2 (one capped, one retried wider)", len(bodies))
	}
	first, _ := bodies[0]["options"].(map[string]any)
	second, _ := bodies[1]["options"].(map[string]any)
	firstCtx, _ := first["num_ctx"].(float64)
	secondCtx, _ := second["num_ctx"].(float64)
	if secondCtx <= firstCtx {
		t.Fatalf("retry num_ctx = %v, want more than the capped attempt's %v", secondCtx, firstCtx)
	}
	// The retry has to clear the depth after the same split: the engine
	// gave us 11526 of 23048, so half of the new window must exceed 21000.
	if want := float64(router.HostCutoffProbeDepthTokens); secondCtx*(11526.0/23048.0) <= want {
		t.Fatalf("retry num_ctx = %v; after the same %.2fx split it still would not reach %v tokens",
			secondCtx, 11526.0/23048.0, want)
	}
	firstPrompt, _ := bodies[0]["prompt"].(string)
	secondPrompt, _ := bodies[1]["prompt"].(string)
	if firstPrompt[:64] == secondPrompt[:64] {
		t.Fatal("the retry reused the capped attempt's prompt prefix; the engine would " +
			"answer it from its KV cache instead of measuring anything")
	}
}

// PRODUCT CONTRACT: two probes never share a prompt prefix. ollama's
// prefix KV cache answers a repeat with the full prompt_eval_count and a
// near-zero duration — measured at 697,222 tok/s on the reference host —
// so a fixed prompt would report every machine as instant.
func TestMeasureHostCutoff_NoTwoRunsSharePromptPrefix(t *testing.T) {
	p, eng, _ := hostCutoffProvider(t, gpuCounters, 0)
	ctx := context.Background()
	if _, decided := p.hostMeetsRecommendedSpec(ctx); !decided {
		t.Fatal("first probe reached no verdict")
	}
	// Nonces are timestamps; without a gap two runs in the same
	// nanosecond would be a flake rather than a bug.
	time.Sleep(time.Millisecond)
	if _, decided := p.hostMeetsRecommendedSpec(ctx); !decided {
		t.Fatal("second probe reached no verdict")
	}

	bodies := eng.generateBodies()
	if len(bodies) != 2 {
		t.Fatalf("/api/generate requests = %d, want 2", len(bodies))
	}
	first, _ := bodies[0]["prompt"].(string)
	second, _ := bodies[1]["prompt"].(string)
	if first == "" || first == second {
		t.Fatal("two probes sent the same prompt; the engine would answer the second " +
			"from its prefix KV cache and report a rate no host can achieve")
	}
	if first[:64] == second[:64] {
		t.Fatalf("the two prompts share their first 64 bytes (%q); the nonce has to lead "+
			"the prompt, not trail it", first[:64])
	}
}

// PRODUCT CONTRACT: the probe measures the model the threshold was
// calibrated on, or it does not run. Substituting whatever the host
// happens to have would produce a number that is not comparable to
// 45 seconds.
func TestHostCutoff_WithoutTheProbeModelInTheCatalog_NoVerdict(t *testing.T) {
	p, eng, disabled := hostCutoffProvider(t, cpuOnlyCounters, 0)
	p.manifests = bounceTestManifests() // no qwen3.5-0.8b in here

	if _, decided := p.hostMeetsRecommendedSpec(context.Background()); decided {
		t.Fatal("reached a verdict without the probe model; the threshold is calibrated " +
			"against one model and means nothing measured on another")
	}
	if got := len(eng.generateBodies()); got != 0 {
		t.Fatalf("/api/generate requests = %d, want 0", got)
	}
	if *disabled != 0 {
		t.Fatalf("disableInference calls = %d, want 0", *disabled)
	}
}

// PRODUCT CONTRACT: a daemon started with --disable-inference has no
// controller to persist through, and the cutoff must not panic reaching
// for one. It still declines the download — a host that cannot serve has
// no use for the weights either way.
func TestApplyHostCutoff_WithNoInferenceController_DeclinesWithoutPanicking(t *testing.T) {
	p, _, _ := hostCutoffProvider(t, cpuOnlyCounters, 0)
	p.disableInference = nil

	if p.applyHostCutoff(context.Background()) {
		t.Fatal("the download proceeded on a host measured below the budget")
	}
}

// PRODUCT CONTRACT: a failure to persist the verdict does not turn into
// a download. The measurement already said this host cannot serve; losing
// the write makes the next boot re-measure, which is recoverable — 45 GB
// of weights on a host that cannot use them is not.
func TestApplyHostCutoff_WhenPersistingFails_StillDeclinesTheDownload(t *testing.T) {
	p, _, _ := hostCutoffProvider(t, cpuOnlyCounters, 0)
	p.disableInference = func() error { return errors.New("read-only state dir") }

	if p.applyHostCutoff(context.Background()) {
		t.Fatal("the download proceeded because the verdict could not be written down")
	}
}

// THE #465 BAR. PRODUCT CONTRACT: turning local inference on is a choice
// the product keeps. The cutoff writes the same file
// `waired inference on` writes, so a cutoff that re-ran after the opt-in
// would silently take it back on the next restart — and on the slow host
// that is exactly the host that opted in, it would take it back EVERY
// restart. "A default with a working opt-in" would then be neither.
//
// The disabled row matters as much: re-measuring a host already told to
// stay off spends ~40 s and a gigabyte to reach the answer it is looking
// at.
func TestApplyHostCutoff_OnceTheToggleHasBeenMoved_NeverMeasuresAgain(t *testing.T) {
	for _, choice := range []state.InferenceState{state.InferenceEnabled, state.InferenceDisabled} {
		t.Run(string(choice), func(t *testing.T) {
			// cpuOnlyCounters: a host the measurement WOULD cut, so a
			// pass here can only come from not measuring.
			p, eng, disabled := hostCutoffProvider(t, cpuOnlyCounters, 0)
			if err := state.WriteDesiredInferenceState(p.stateDir, choice); err != nil {
				t.Fatalf("seed desired-inference: %v", err)
			}

			if !p.applyHostCutoff(context.Background()) {
				t.Fatal("the cutoff overrode a choice that had already been made on this host")
			}
			if got := len(eng.generateBodies()); got != 0 {
				t.Fatalf("/api/generate requests = %d, want 0 — nothing left to decide", got)
			}
			if *disabled != 0 {
				t.Fatalf("disableInference calls = %d, want 0", *disabled)
			}
		})
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

// THE END-TO-END #496 BAR. PRODUCT CONTRACT: on a host below the
// recommended spec, no bundled weights are downloaded at all.
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

// PRODUCT CONTRACT: the ordinary host is untouched. A cutoff that cost
// every capable machine its model would be worse than no cutoff.
func TestPrePullHold_HostAboveTheBudget_DownloadsAsBefore(t *testing.T) {
	p, r := prePullCutoffProvider(t, gpuCounters)

	got := runHeldPrePull(t, p, r)
	if len(got) != 1 || got[0] != "big:q4" {
		t.Fatalf("tags pulled = %v, want [big:q4]", got)
	}
}
