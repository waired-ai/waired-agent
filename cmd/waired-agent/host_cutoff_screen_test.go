package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/download"
	"github.com/waired-ai/waired-agent/internal/hostspeed"
	"github.com/waired-ai/waired-agent/proto/hostfit"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// The screen half of the install-time host cutoff (waired-agent#579
// Stage 3): reaching "this host is below the recommended spec" from the
// prefill rate alone, without paying for a ~21k-token sample.
//
// The saving is the point. One full sample took 7 min 12 s on the GitHub
// macos-14 runner, and those minutes stand in front of the model
// download — which is how a host that should have downloaded nothing
// spent the operator's first run deciding nothing.

// screenCounters is what the fake engine answers a screen request with:
// the same 50-line prompt the calibration always sent, prefilled at the
// given rate. eval_* are the num_predict:1 answer, which nothing reads.
func screenCounters(prefillTokps float64) map[string]any {
	const tokens = hostCutoffCalibrationLines * 55
	return map[string]any{
		"prompt_eval_count":    tokens,
		"prompt_eval_duration": int64(float64(tokens) / prefillTokps * 1e9),
		"eval_count":           1,
		"eval_duration":        int64(10_000_000),
	}
}

// The arithmetic the screen rests on, pinned so that moving any of the
// four constants involved fails here with the reason rather than in the
// nightly with a symptom.
//
// Product contract (waired-agent#579, and the owner ruling on #620 for
// the field the bound travels in).
func TestHostCutoffScreen_TheConstantsHoldTogether(t *testing.T) {
	// 1. THE FIRING LINE SITS ABOVE A HOST ALREADY KNOWN TO BE UNUSABLE.
	//
	// This repo's reference host measures a 66.6 s turn
	// (proto/hostfit/host_cutoff.go) and is a host the cutoff correctly
	// rejects. The screen fires only past 67.5 s, and a floor is a LOWER
	// bound on the turn — so every host the screen cuts is slower than one
	// that has already been shown, by full measurement, not to be worth
	// serving from. Between the budget and this line the cheap bound and
	// the real measurement can disagree about which side of 45 s a host
	// falls on, and there the measurement decides.
	const referenceHostTurnSeconds = 66.6
	line := hostfit.HostCutoffTurnBudgetSeconds * hostCutoffScreenMargin
	if line <= referenceHostTurnSeconds {
		t.Fatalf("the screen fires at %.1f s, at or below the reference host's measured %.1f s turn: "+
			"it would now reach verdicts about hosts better than one the full measurement judges",
			line, referenceHostTurnSeconds)
	}

	// 2. THE SCREEN FITS THE WINDOW THE DOWNLOAD IS WAITING BEHIND.
	//
	// ONE pass at its ceilings — the quiet wait plus both readings —
	// inside hostspeed.InstallWindow, so a host whose probe model is
	// already on disk always reaches a verdict in the window
	// applyHostCutoff can spare.
	//
	// Stated for one pass rather than for the whole call on purpose. Two
	// other things can consume the same window and neither can be bounded
	// here: the probe model's own download shares it, and a pass the
	// engine interrupted is re-read (hostCutoffScreenBounceGrace). Both
	// are held by the run deadline in ensureHostSpeedMeasured, which cuts
	// them short rather than letting them overrun — and a screen cut short
	// reaches no verdict, which is the fall-through this whole design
	// treats as safe.
	screen := hostCutoffScreenQuietWait + hostCutoffCalibrationTimeout + hostCutoffScreenConfirmTimeout
	if screen > hostspeed.InstallWindow {
		t.Fatalf("the screen can take %s at its ceilings (quiet wait + two readings), more than the "+
			"%s install window: the verdict the download is waiting for can arrive after the "+
			"download starts", screen, hostspeed.InstallWindow)
	}

	// 3. AND IT FITS THE MEASUREMENT'S OWN BUDGET, which the background
	// caller runs under.
	if screen > hostCutoffMeasureBudget {
		t.Fatalf("the screen (%s) does not fit hostCutoffMeasureBudget (%s)", screen, hostCutoffMeasureBudget)
	}

	// 4. THE CONFIRMING READING IS THE CHEAPER ONE. It pays no model load,
	// because the first reading left the model resident.
	if hostCutoffScreenConfirmTimeout >= hostCutoffCalibrationTimeout {
		t.Fatalf("the confirming ceiling (%s) is not under the first reading's (%s); "+
			"it is the reading that pays no model load",
			hostCutoffScreenConfirmTimeout, hostCutoffCalibrationTimeout)
	}
	if hostCutoffScreenKeepAlive <= 0 {
		t.Fatal("the first screen reading unloads the model, so the confirming reading reloads it — " +
			"which is the cost hostCutoffScreenConfirmTimeout is sized on the absence of")
	}
}

// The reference host's own shallow reading does not fire.
//
// 833 tok/s is this repo's measured prefill rate at 68 % of the canonical
// depth on the reference machine (proto/hostfit/host_cutoff.go), i.e. the
// rate a screen would read there. Its bound is 25.2 s — 2.7x under the
// firing line — so the machine that anchors the whole threshold falls
// through to the full measurement, and the full measurement is what
// judges it.
func TestScreenHostCutoff_TheReferenceAnchorDoesNotFire(t *testing.T) {
	p, eng, disabled := hostCutoffProviderAnswering(t,
		[]map[string]any{screenCounters(833), cpuOnlyCounters}, 0)
	hostCutoffEngineUp(t, p)

	v := p.ensureHostSpeedMeasured(context.Background(), p.hostSpeedMeasureWindow())
	if !v.Decided {
		t.Fatal("no verdict at all")
	}
	if v.Method != signer.BenchmarkMethodOllamaEval {
		t.Fatalf("method = %q, want %q: the screen concluded about the machine the "+
			"threshold is calibrated on", v.Method, signer.BenchmarkMethodOllamaEval)
	}
	if v.TurnSeconds <= 0 {
		t.Fatalf("no measured turn: %+v", v)
	}
	// The screen costs exactly what the calibration always cost — one
	// request — on every host that clears it.
	if got, want := len(eng.generateBodies()), benchSampleCount+1; got != want {
		t.Fatalf("/api/generate requests = %d, want %d (one screen, then %d samples): "+
			"a host that clears the screen must not pay for the confirming reading",
			got, want, benchSampleCount)
	}
	_ = disabled
}

// A host far past the line is concluded about from two shallow readings,
// and no sample is taken.
//
// 130 tok/s at the screen's depth is a bound of 161.5 s against a 45 s
// budget. This is the shape the GitHub macos-14 runner has, and the whole
// of the saving: two short requests instead of a 7-minute sample standing
// in front of the model download.
func TestScreenHostCutoff_AHostFarBelowFiresWithoutTheFullDepth(t *testing.T) {
	p, eng, _ := hostCutoffProviderAnswering(t,
		[]map[string]any{screenCounters(130), screenCounters(130)}, 0)
	hostCutoffEngineUp(t, p)

	v := p.ensureHostSpeedMeasured(context.Background(), p.hostSpeedMeasureWindow())
	if !v.Decided {
		t.Fatal("the screen read a host 3.6x past the firing line and reached no verdict")
	}
	if v.MeetsBudget {
		t.Fatalf("a host bounded below at 161 s was judged to clear a 45 s budget: %+v", v)
	}
	if v.Method != signer.BenchmarkMethodOllamaPrefillFloor {
		t.Fatalf("method = %q, want %q", v.Method, signer.BenchmarkMethodOllamaPrefillFloor)
	}
	if got := len(eng.generateBodies()); got != 2 {
		t.Fatalf("/api/generate requests = %d, want 2 (the two screen readings): "+
			"the point of the screen is that the full-depth sample never runs", got)
	}

	// What lands on the wire is the owner ruling on #620 holding at the
	// producer: TurnSeconds stays a measurement everywhere it appears, so
	// it is ABSENT here and the bound travels in its own field.
	got := p.hostSpeedNow()
	if got == nil {
		t.Fatal("nothing published")
	}
	if got.TurnSeconds != 0 {
		t.Fatalf("turn_seconds = %.1f, want 0: nothing measured a turn, and a consumer that has "+
			"not been taught turn_floor_seconds must read this as \"no measurement\"", got.TurnSeconds)
	}
	if want := hostfit.TurnFloorSeconds(130); math.Abs(got.TurnFloorSeconds-want) > 0.1 {
		t.Fatalf("turn_floor_seconds = %.1f, want %.1f", got.TurnFloorSeconds, want)
	}
	if got.TurnFloorSeconds <= hostfit.HostCutoffTurnBudgetSeconds {
		t.Fatalf("published a bound of %.1f s that does not exceed the %.0f s budget — "+
			"the one shape this method may never be emitted in",
			got.TurnFloorSeconds, hostfit.HostCutoffTurnBudgetSeconds)
	}
	if got.DecodeTokps != 0 {
		t.Fatalf("decode_tokps = %.1f, want 0: no decode was measured", got.DecodeTokps)
	}
	if got.Samples != 2 {
		t.Fatalf("samples = %d, want 2 (the two readings)", got.Samples)
	}
	if got.PromptTokens >= hostfit.HostCutoffProbeDepthTokens {
		t.Fatalf("prompt_tokens = %d, want far under the %d depth — the shallowness is what "+
			"lets a consumer check the claim rather than take it on the method string",
			got.PromptTokens, hostfit.HostCutoffProbeDepthTokens)
	}

	// A HostProbe rebuilt from this record must decline to judge: a
	// consumer that has not been taught the bound reads a shallow prefill
	// and falls back to "no verdict", which is the answer it should give.
	rebuilt := hostfit.HostProbe{
		PromptTokens: got.PromptTokens,
		PrefillTokps: got.PrefillTokps,
		DecodeTokps:  got.DecodeTokps,
	}
	if _, decided := rebuilt.MeetsRecommendedSpec(); decided {
		t.Fatal("a naive HostProbe rebuilt from a bound reached a verdict; it must fail closed")
	}
}

// The two readings have to AGREE. A wedged or briefly starved engine
// produces exactly the shape the screen is looking for, and it produces
// it once.
func TestScreenHostCutoff_OneReadingNeverFires(t *testing.T) {
	p, eng, _ := hostCutoffProviderAnswering(t, []map[string]any{
		screenCounters(130), // slow enough to fire
		screenCounters(833), // and then the host is its ordinary self
		cpuOnlyCounters,     // so the full measurement runs after all
	}, 0)
	hostCutoffEngineUp(t, p)

	v := p.ensureHostSpeedMeasured(context.Background(), p.hostSpeedMeasureWindow())
	if !v.Decided {
		t.Fatal("no verdict")
	}
	if v.Method != signer.BenchmarkMethodOllamaEval {
		t.Fatalf("method = %q, want %q: one slow reading turned into a verdict",
			v.Method, signer.BenchmarkMethodOllamaEval)
	}
	if got, want := len(eng.generateBodies()), benchSampleCount+2; got != want {
		t.Fatalf("/api/generate requests = %d, want %d (two screen readings, then %d samples)",
			got, want, benchSampleCount)
	}
}

// A truncated reading never fires.
//
// A short prefill carries a larger share of the request's fixed cost, so
// its rate UNDER-states the host — the one direction that turns a fine
// machine into a verdict. The full measurement answers the same hazard
// with a depth readback; this is that readback at this depth.
func TestScreenHostCutoff_ATruncatedReadingNeverFires(t *testing.T) {
	truncated := map[string]any{
		// 400 tokens where ~2750 were sent, at a rate that would bound the
		// turn at 21000/50 = 420 s if it were believed.
		"prompt_eval_count":    400,
		"prompt_eval_duration": int64(8_000_000_000),
		"eval_count":           1,
		"eval_duration":        int64(10_000_000),
	}
	p, eng, disabled := hostCutoffProviderAnswering(t,
		[]map[string]any{truncated, cpuOnlyCounters}, 0)
	hostCutoffEngineUp(t, p)

	v := p.ensureHostSpeedMeasured(context.Background(), p.hostSpeedMeasureWindow())
	if v.Method == signer.BenchmarkMethodOllamaPrefillFloor {
		t.Fatalf("a 400-token reading produced a bound: %+v", v)
	}
	if got, want := len(eng.generateBodies()), benchSampleCount+1; got != want {
		t.Fatalf("/api/generate requests = %d, want %d — the confirming reading was spent on "+
			"a reading that could never be concluded from", got, want)
	}
	_ = disabled
}

// screenServer answers every /api/generate with the same counters and
// lets the test move the engine generation and the quiet predicate. The
// provider fixture cannot express either, and both are preconditions for
// a verdict rather than inputs to one.
func screenServer(t *testing.T, counters map[string]any) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		body, _ := json.Marshal(counters)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

// A busy engine is not a slow host.
//
// Two readings taken seconds apart cannot substitute for this: sustained
// contention depresses both and they agree. The pair guards against a
// transient stall; this guards against the host being busy at all, and
// this is the one place a reading becomes a verdict with no measurement
// behind it.
func TestScreenHostCutoff_ABusyEngineIsNotASlowHost(t *testing.T) {
	srv, requests := screenServer(t, screenCounters(130))

	m, _, decided := screenHostCutoff(context.Background(), hostCutoffDeps{
		BaseURL: srv.URL, EngineModel: hostCutoffFixtureTag,
		HTTPClient: srv.Client(), Logger: slog.New(slog.DiscardHandler),
		Nonce:       "busy",
		EngineQuiet: func(context.Context) bool { return false },
		// The wait itself is exercised by
		// TestScreenHostCutoff_TheScreenWaitsForTheEngineToGoQuiet; here
		// the subject is what happens when it runs out.
		QuietWait: time.Millisecond,
	})
	if decided {
		t.Fatalf("a host with something else on the engine was cut on two shallow readings: %+v", m)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("/api/generate requests = %d, want 1 — the quiet check sits between the two "+
			"readings, so a busy host never pays for the confirming one", got)
	}
}

// The screen WAITS for the engine to go quiet rather than checking once.
//
// This is not a nicety. The call immediately before the screen is
// ensureHostCutoffProbeModel, and on a fresh install that downloads ~1 GB;
// endPull fires a serve reconcile when a model lands, and a reconcile
// stops and respawns the engine. So the first moment the screen can run
// is very often the moment the engine is being restarted underneath it,
// and a single check there would answer "busy" and stand the screen down
// on exactly the install path the screen exists for.
//
// Product contract (waired-agent#579).
func TestScreenHostCutoff_TheScreenWaitsForTheEngineToGoQuiet(t *testing.T) {
	srv, requests := screenServer(t, screenCounters(130))

	// Busy for the first few polls — a reconcile finishing — then idle.
	var asked atomic.Int64
	m, _, decided := screenHostCutoff(context.Background(), hostCutoffDeps{
		BaseURL: srv.URL, EngineModel: hostCutoffFixtureTag,
		HTTPClient: srv.Client(), Logger: slog.New(slog.DiscardHandler),
		Nonce:     "settling",
		QuietWait: 5 * time.Second,
		EngineQuiet: func(context.Context) bool {
			return asked.Add(1) > 3
		},
	})
	if !decided {
		t.Fatal("the screen gave up on a host whose engine was merely still settling; " +
			"on a fresh install that is the ordinary state when the screen first runs")
	}
	if m.Method != signer.BenchmarkMethodOllamaPrefillFloor {
		t.Fatalf("method = %q, want %q", m.Method, signer.BenchmarkMethodOllamaPrefillFloor)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("/api/generate requests = %d, want 2", got)
	}
	if asked.Load() < 4 {
		t.Fatalf("the quiet predicate was consulted %d times; the wait did not poll", asked.Load())
	}
}

// An engine restart under the screen is not a slow host either, and it
// costs the host nothing: the pass is re-read without charge.
//
// Same hazard and the same answer as the boot benchmark's bounce grace
// (#359/#582) — a restart this agent ordered surfaces as a bare EOF, so
// the generation counter is the only thing that can tell it apart from a
// machine that cannot answer.
func TestScreenHostCutoff_ARestartUnderTheScreenIsNotASlowHost(t *testing.T) {
	srv, requests := screenServer(t, screenCounters(130))

	// The generation moves once, under the first pass, and then holds.
	var gen atomic.Uint64
	var once sync.Once
	deps := hostCutoffDeps{
		BaseURL: srv.URL, EngineModel: hostCutoffFixtureTag,
		HTTPClient: srv.Client(), Logger: slog.New(slog.DiscardHandler),
		Nonce: "bounce",
		EngineGen: func() uint64 {
			if requests.Load() > 0 {
				once.Do(func() { gen.Add(1) })
			}
			return gen.Load()
		},
	}

	m, _, decided := screenHostCutoff(context.Background(), deps)
	if !decided {
		t.Fatal("a restart under the screen was charged to the host: the second pass, taken " +
			"under one engine process, agreed twice and still reached no verdict")
	}
	if m.Method != signer.BenchmarkMethodOllamaPrefillFloor {
		t.Fatalf("method = %q, want %q", m.Method, signer.BenchmarkMethodOllamaPrefillFloor)
	}
	// Pass 1 is discarded (2 requests) and pass 2 concludes (2 more).
	if got := requests.Load(); got != 4 {
		t.Fatalf("/api/generate requests = %d, want 4 (the interrupted pass, then the one that "+
			"concluded)", got)
	}
}

// A host that goes busy DURING the screen falls through rather than
// earning a re-read. It is not a restart, and re-reading would only wait
// out the quiet window again and spend the time the screen exists to save.
func TestScreenHostCutoff_AHostThatGoesBusyMidScreenFallsThrough(t *testing.T) {
	srv, requests := screenServer(t, screenCounters(130))

	var asked atomic.Int64
	m, _, decided := screenHostCutoff(context.Background(), hostCutoffDeps{
		BaseURL: srv.URL, EngineModel: hostCutoffFixtureTag,
		HTTPClient: srv.Client(), Logger: slog.New(slog.DiscardHandler),
		Nonce:     "went-busy",
		QuietWait: time.Millisecond,
		// Quiet when the confirming reading is taken, busy by the time the
		// verdict would be reached.
		EngineQuiet: func(context.Context) bool { return asked.Add(1) == 1 },
	})
	if decided {
		t.Fatalf("a host that started doing something else mid-screen was cut on it: %+v", m)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("/api/generate requests = %d, want 2 — the pass was re-read instead of "+
			"falling through", got)
	}
}

// The grace is finite. An engine restarting on every pass is a host whose
// screen cannot be trusted, and it must fall through rather than loop.
func TestScreenHostCutoff_AnEngineThatKeepsRestartingFallsThrough(t *testing.T) {
	srv, requests := screenServer(t, screenCounters(130))

	var gen atomic.Uint64
	m, _, decided := screenHostCutoff(context.Background(), hostCutoffDeps{
		BaseURL: srv.URL, EngineModel: hostCutoffFixtureTag,
		HTTPClient: srv.Client(), Logger: slog.New(slog.DiscardHandler),
		Nonce:     "restarting",
		EngineGen: func() uint64 { return gen.Add(1) },
	})
	if decided {
		t.Fatalf("an engine that restarted under every reading produced a verdict: %+v", m)
	}
	if got, want := requests.Load(), int64(2*(hostCutoffScreenBounceGrace+1)); got != want {
		t.Fatalf("/api/generate requests = %d, want %d — the grace is not bounded by "+
			"hostCutoffScreenBounceGrace", got, want)
	}
}

// The first reading keeps the model resident and the second one unloads
// it. Two readings would otherwise pay two cold ~1 GB loads, which on the
// hosts this is for is the larger half of the cost — and the reason a
// pair of readings looks expensive when it is not.
func TestScreenHostCutoff_TheFirstReadingCarriesTheModel(t *testing.T) {
	p, eng, _ := hostCutoffProviderAnswering(t,
		[]map[string]any{screenCounters(130), screenCounters(130)}, 0)
	hostCutoffEngineUp(t, p)

	if v := p.ensureHostSpeedMeasured(context.Background(), p.hostSpeedMeasureWindow()); !v.Decided {
		t.Fatal("no verdict")
	}
	bodies := eng.generateBodies()
	if len(bodies) != 2 {
		t.Fatalf("/api/generate requests = %d, want 2", len(bodies))
	}
	first, _ := bodies[0]["keep_alive"].(float64)
	second, _ := bodies[1]["keep_alive"].(float64)
	if int(first) != hostCutoffScreenKeepAlive {
		t.Fatalf("first reading keep_alive = %v, want %d — the confirming reading would reload "+
			"the model, and its ceiling is sized on it not having to", first, hostCutoffScreenKeepAlive)
	}
	if second != 0 {
		t.Fatalf("confirming reading keep_alive = %v, want 0 — it is the last request the screen "+
			"makes, and a ~1 GB model must not be left resident on the host least able to spare it",
			second)
	}
	// And the two prompts differ, or the engine answers the second from
	// its prefix KV cache at a rate no host can achieve.
	if bodies[0]["prompt"] == bodies[1]["prompt"] {
		t.Fatal("the two screen readings sent the same prompt; the second would be answered " +
			"from the engine's KV cache")
	}
}

// THE END-TO-END BAR for the screen, the twin of
// TestApplyHostCutoff_BelowTheBudget_TurnsLocalInferenceOffInsteadOfDownloading:
// a host concluded about from the bound alone downloads nothing, and can
// say why.
func TestApplyHostCutoff_AScreenVerdictTurnsLocalInferenceOff(t *testing.T) {
	p, eng, disabled := hostCutoffProviderAnswering(t,
		[]map[string]any{screenCounters(130), screenCounters(130)}, 0)
	hostCutoffEngineUp(t, p)

	if p.applyHostCutoff(context.Background()) {
		t.Fatal("the pre-pull proceeded on a host bounded below at 161 s per turn")
	}
	if *disabled != 1 {
		t.Fatalf("disableInference calls = %d, want 1", *disabled)
	}
	if !p.hostSpeedTurnedInferenceOff() {
		t.Fatal("the bound is not recorded as the reason local inference is off")
	}
	if got := len(eng.generateBodies()); got != 2 {
		t.Fatalf("/api/generate requests = %d, want 2: the verdict cost two shallow readings, "+
			"which is the whole of waired-agent#579 Stage 3", got)
	}
}

// And it is in the path, not merely in the verdict: no weights are
// downloaded.
func TestPrePullHold_AScreenVerdictDownloadsNothing(t *testing.T) {
	p, _, _ := hostCutoffProviderAnswering(t,
		[]map[string]any{screenCounters(130), screenCounters(130)}, 0)
	hostCutoffEngineUp(t, p)
	r := newBlockingRunner(t)
	p.puller = download.NewPuller("ollama-fake", r)
	p.cfg.BundledModelID = "some-big-model"
	p.cfg.PullOnStartup = true
	p.prePullFrameGrace = 5 * time.Millisecond
	p.prePullHoldMax = time.Minute

	if got := runHeldPrePull(t, p, r); len(got) != 0 {
		t.Fatalf("tags pulled = %v, want none — this host is bounded below at 161 s per turn", got)
	}
}

// A bound survives the restart, and the restarted daemon does NOT
// re-measure.
//
// Before hostSpeedStillApplies was taught the method, this was the
// expensive failure: a stored bound is not Measured(), so every boot on
// every slow host would have thrown the verdict away and screened again.
func TestHostSpeed_AScreenVerdictSurvivesARestart(t *testing.T) {
	p, eng, _ := hostCutoffProviderAnswering(t,
		[]map[string]any{screenCounters(130), screenCounters(130)}, 0)
	hostCutoffEngineUp(t, p)
	if p.applyHostCutoff(context.Background()) {
		t.Fatal("the download proceeded on a host below the budget")
	}
	requests := len(eng.generateBodies())

	next := &agentInferenceProvider{
		ollama:       p.ollama,
		manifests:    p.manifests,
		stateDir:     p.stateDir,
		store:        p.store,
		profiler:     p.profiler,
		logger:       slog.New(slog.DiscardHandler),
		ollamaUsable: func() bool { return true },
	}
	next.hostSpeedAgentVersion = p.hostSpeedAgentVersion

	got := next.hostSpeedNow()
	if got == nil {
		t.Fatal("the restarted daemon has no measurement; the reason local inference is off " +
			"is now unrecoverable from the machine itself")
	}
	if got.Method != signer.BenchmarkMethodOllamaPrefillFloor || got.TurnFloorSeconds <= 0 {
		t.Fatalf("the reloaded record is not the bound that was published: %+v", got)
	}
	if !next.hostSpeedTurnedInferenceOff() {
		t.Fatal("the restarted daemon no longer knows the bound is why inference is off")
	}
	if now := len(eng.generateBodies()); now != requests {
		t.Fatalf("the restart re-screened (%d → %d requests): a stored bound is not Measured(), "+
			"and hostSpeedStillApplies has to know that is not the same as \"no measurement\"",
			requests, now)
	}
}

// What a published record MEANS, read back.
//
// One function answers it for the figure just taken and for the figure
// loaded off disk a boot later, so a measurement cannot mean one thing
// when it is written and another when it is read.
func TestHostSpeedVerdictOf(t *testing.T) {
	measured := func(turn float64) *signer.HostSpeed {
		// Prefill fast, decode carrying the turn, at the canonical depth.
		return &signer.HostSpeed{
			PromptTokens: hostfit.HostCutoffProbeDepthTokens,
			PrefillTokps: 20000,
			DecodeTokps: (float64(hostfit.HostCutoffProbeDepthTokens) / hostfit.HostCutoffPromptCompletionRatio) /
				(turn - float64(hostfit.HostCutoffProbeDepthTokens)/20000),
			Method: signer.BenchmarkMethodOllamaEval,
		}
	}
	for _, tc := range []struct {
		name        string
		in          *signer.HostSpeed
		wantOK      bool
		wantMeets   bool
		wantDecided bool
	}{
		{"nil", nil, false, false, false},
		{"a measurement above the budget", measured(20), true, true, true},
		{"a measurement below the budget", measured(90), true, false, true},
		{
			// Written by a build from before the screen existed. The empty
			// method is a full measurement, because that is all there was.
			"a measurement with no method", &signer.HostSpeed{
				PromptTokens: hostfit.HostCutoffProbeDepthTokens,
				PrefillTokps: 20000, DecodeTokps: 300,
			}, true, true, true,
		},
		{"a truncated measurement", &signer.HostSpeed{
			PromptTokens: 4096, PrefillTokps: 20000, DecodeTokps: 300,
			Method: signer.BenchmarkMethodOllamaEval,
		}, false, false, false},
		{"a bound past the budget", &signer.HostSpeed{
			Method: signer.BenchmarkMethodOllamaPrefillFloor, TurnFloorSeconds: 161.5,
			PromptTokens: 2750, PrefillTokps: 130,
		}, true, false, true},
		{
			// Self-contradictory: the agent emits this method only once the
			// bound already exceeds the budget, so a record claiming it
			// while sitting under the budget is not one to believe.
			"a bound that does not clear the budget", &signer.HostSpeed{
				Method: signer.BenchmarkMethodOllamaPrefillFloor, TurnFloorSeconds: 10,
				PromptTokens: 2750, PrefillTokps: 2100,
			}, false, false, false,
		},
		{"a bound with no figure", &signer.HostSpeed{
			Method: signer.BenchmarkMethodOllamaPrefillFloor,
		}, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, ok := hostSpeedVerdictOf(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (%+v)", ok, tc.wantOK, v)
			}
			if v.Decided != tc.wantDecided {
				t.Fatalf("decided = %v, want %v", v.Decided, tc.wantDecided)
			}
			if v.MeetsBudget != tc.wantMeets {
				t.Fatalf("meets budget = %v, want %v (%+v)", v.MeetsBudget, tc.wantMeets, v)
			}
		})
	}
}
