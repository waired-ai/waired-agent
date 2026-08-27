package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// BenchResult captures the outcome of one boot-time token/s probe.
// Capacity is the Phase 7 admission cap the agent will advertise to
// the mesh.
type BenchResult struct {
	// TokensPerSec is the measured decode rate (#764): tokens the
	// engine generates per second of decode time, excluding prompt
	// prefill and fixed request overhead. Method records how it was
	// obtained; only the wall_clock fallback still contains overhead.
	TokensPerSec float64
	Capacity     int
	VariantID    string
	// ModelID is the catalog model this rate was measured on. It is what
	// lets a later reader tell whether the number still describes what
	// the host serves: the active model can change under a stored result
	// (a switch, a pull finishing), and the floor comparison would
	// otherwise judge the NEW model by the OLD one's rate
	// (waired-ai/waired-agent#783).
	//
	// Empty on a result built before this field existed — a cache entry,
	// a test literal — which reads as "unknown" and keeps the previous
	// behaviour, the same convention Outcome uses.
	ModelID string
	// Method is the benchMethod* constant that produced TokensPerSec.
	Method string
	// SpreadPct is (max-min)/median over the samples behind
	// TokensPerSec, in percent. 0 for single-sample results.
	SpreadPct float64
	Failed    bool
	Err       string
	// Outcome says WHY there is or is not a number, so an absent engine
	// stops reading as a slow host (#203). Failed stays the "do not treat
	// this as a measurement" flag every consumer already gates on --
	// buildRecommendation would otherwise compare a zero rate against the
	// interactive floor and recommend a lighter model for a host nobody
	// measured. Outcome is the finer-grained reason on top of it.
	//
	// benchOutcome* below. Empty on a result built before this field
	// existed (a cache entry, a test literal), which reads as "unknown".
	Outcome string
}

// benchOutcome* are the values BenchResult.Outcome takes. "engine_not_ready"
// is the term the management layer already uses for the same condition
// (internal/management/inference_recommendation.go maps it to 425).
const (
	benchOutcomeMeasured       = "measured"
	benchOutcomeSkipped        = "skipped"
	benchOutcomeEngineNotReady = "engine_not_ready"
	benchOutcomeFailed         = "failed"
)

// BenchProgress is one report from a measurement in flight
// (waired-agent#199).
//
// Phase separates the two things that take time. Warm-up can take ~180 s
// on a cold multi-GB model and is NOT a measurement; three minutes of no
// output reads as a hang, so it is shown as its own phase rather than
// hidden inside "measuring".
//
// The wire has no phase field and does not need one: waired#934's
// contract expresses warm-up as Trials set with Trial still 0 — nothing
// has been measured yet. Keeping the distinction here as a named phase
// rather than an implied zero is for the reader of this package.
type BenchProgress struct {
	Phase  string
	Trial  int // 1-based index of the sample just completed; 0 during warm-up
	Trials int // planned sample count
	// SampleTokps is the sample just completed; MedianTokps and SpreadPct
	// are over the samples completed SO FAR, which is what makes the
	// figure the wizard shows converge instead of jump.
	SampleTokps float64
	MedianTokps float64
	SpreadPct   float64
	Method      string
}

// Benchmark phases — values of BenchProgress.Phase.
const (
	benchPhaseWarmup    = "warmup"
	benchPhaseMeasuring = "measuring"
)

// engineGen is a nil-safe EngineGen call. A caller that does not wire it
// gets a constant 0, so the generation never appears to move — the same
// nil-safety engineProcessGen gives the unit fixtures that construct a
// provider without an adapter.
func (d BenchDeps) engineGen() uint64 {
	if d.EngineGen == nil {
		return 0
	}
	return d.EngineGen()
}

// report is a nil-safe Progress call.
func (d BenchDeps) report(p BenchProgress) {
	if d.Progress != nil {
		d.Progress(p)
	}
}

// avgCodingAgentTokRate is the rough steady-state token throughput
// one coding-agent session consumes (claude / codex /
// continue.dev-style). Used as the divisor in N = floor(tokps / 30):
// a host that benches at ~120 tok/s ends up advertising Capacity=4.
//
// 30 is conservative — real coding-agent traffic spikes higher
// during code generation but stalls during tool use, so the
// effective sustained rate sits below the wall-clock token/s the
// benchmark measures. Easier to bump this up in a follow-up than
// to silently over-admit and flood a single peer.
//
// This is deliberately NOT the interactive/selection floor (#670/#765,
// router.CodingAgentSelectionFloorTokps = 60): the divisor models
// how much throughput one admitted session CONSUMES on average, the
// floor models the decode rate below which a session FEELS too slow.
// They used to share this constant when the floor was also 30; moving
// the floor (30→100→60) must not swing every host's advertised mesh
// Capacity with it.
const avgCodingAgentTokRate = 30.0

// unmeasuredCapacity is the admission ceiling of a host that has an engine
// but has not yet measured what it can take: one request at a time.
//
// The value is not new. RunBootBenchmark already returns it for every way a
// measurement can fail to happen on a host that has an engine, for the
// reason notReadyBenchResult carries — on the wire 0 means UNLIMITED, so
// returning 0 would advertise a host with no working engine as accepting
// unbounded concurrency, and 1 is the fail-safe. What waired-agent#738
// found is that the fail-safe covered only the ADVERTISED figure: the
// overlay listener was constructed with Capacity 0 and so enforced nothing
// until the network map echoed a measured figure back — the benchmark's
// duration plus a publish round trip, six minutes on the first install the
// EngineReady path above was reported from.
//
// So the same "one at a time until we know" now seeds Config.Capacity and
// backs capacityFn's boot fallback. Peers see it: /healthz reports the live
// counter, and a probing peer reads total>0 && used>=total as not-ready and
// routes to someone else rather than piling on. The host's owner is
// unaffected — AcquireOwner never enforces the ceiling.
//
// Deliberately NOT applied to a host with no engine at all (EnginePort 0,
// engine kind none): RunBootBenchmark's skip paths return 0 on purpose, and
// that encoding stays.
const unmeasuredCapacity = 1

// resolveInteractiveFloor returns the throughput (tokens/sec) below
// which the agent recommends a lighter model (issue #133). A
// configured value > 0 wins; 0 (the default) falls back to the
// coding-agent selection floor (#670/#765): true decode below
// ~60 tok/s at shallow context degrades to under ~48 tok/s at the
// ~200k coding window, below the band interactive coding-agent use
// tolerates (see router.CodingAgentSelectionFloorTokps).
func resolveInteractiveFloor(cfg float64) float64 {
	if cfg > 0 {
		return cfg
	}
	return router.CodingAgentSelectionFloorTokps
}

// benchPromptCompletionTokens is the target completion length the
// benchmark requests. 200 tokens is long enough to cover the first
// few decoder iterations (where most overhead lives), short enough
// to keep the boot path under ~10 s on a midrange GPU.
const benchPromptCompletionTokens = 200

// benchSampleCount is how many measurements the benchmark takes after
// warm-up; the reported rate is their median (#764). Run-to-run spread
// was measured at ~8%, so the median mostly guards against a single
// warm-up blip rather than averaging noise.
const benchSampleCount = 3

// benchSlopeShortTokens / benchSlopeLongTokens are the two completion
// lengths of the slope method (#764): measuring a short and a long run
// and dividing the token delta by the elapsed delta cancels the fixed
// per-request overhead (HTTP, scheduling, prompt prefill, first-token
// latency) that a single wall-clock run silently attributes to decode
// — that bias understated fast hosts by ~35%. Used on engines whose
// OpenAI-compat response carries no decode-timing counters (vLLM).
const (
	benchSlopeShortTokens = 64
	benchSlopeLongTokens  = 256
)

// benchMeasureBudget bounds the whole multi-sample measurement loop.
// When it expires with at least one valid sample, the median of what
// completed is used; a healthy host finishes all samples in seconds.
const benchMeasureBudget = 120 * time.Second

// benchMethod* record how BenchResult.TokensPerSec was measured, in
// descending order of fidelity. The fallback chain is
// ollama_eval → openai_slope → wall_clock (#764).
const (
	// benchMethodOllamaEval: pure decode rate from ollama's native
	// /api/generate eval_count / eval_duration counters — the same
	// source the depth benchmark uses, so the #133 shallow-vs-depth
	// floor comparison is apples to apples.
	benchMethodOllamaEval = "ollama_eval"
	// benchMethodSlope: two-length wall-clock slope over the
	// OpenAI-compat endpoint; overhead-corrected but engine-agnostic.
	benchMethodSlope = "openai_slope"
	// benchMethodWallClock: the legacy completion_tokens/elapsed of the
	// best single run. Still overhead-contaminated (understates fast
	// hosts); only used when both corrected methods are unavailable.
	benchMethodWallClock = "wall_clock"
)

// benchTimeout caps the timed measurement request only — the warm-up
// that precedes it absorbs model-load latency under its own deadline.
// CUDA OOM, network errors, or a misbehaving engine should not block
// agent startup — RunBootBenchmark logs and returns Capacity=1
// (= serialise) on timeout so the agent comes up degraded rather than
// not at all.
const benchTimeout = 30 * time.Second

// benchWarmupCompletionTokens is the tiny completion the warm-up
// requests — just enough to force the engine to fully load the model
// before the timed window opens.
const benchWarmupCompletionTokens = 8

// benchWarmupTimeout bounds the untimed warm-up request. Generous: a
// 17–62 GB model cold-loading from disk takes tens of seconds, and
// that load used to land INSIDE the measured window — a host that
// decodes at ~100 tok/s warm read as ~5 tok/s cold and got a bogus
// lighter-model recommendation (observed live on sv-mag, 2026-06-09).
const benchWarmupTimeout = 180 * time.Second

// benchPrompt is the boilerplate user message the benchmark sends.
// Kept generic so the chosen model can complete it regardless of
// fine-tuning bias; keep under 100 tokens so the prompt processing
// stage doesn't dominate the wall-clock measurement.
const benchPrompt = "Briefly describe what a Linux process is, in one short paragraph."

// BenchDeps lists everything RunBootBenchmark touches. Passed in
// (rather than read from globals) so unit tests can inject a
// fake engine / clock / engine kind.
type BenchDeps struct {
	// EngineKind is the runtime's wire kind (signer.InferenceTypeOllama
	// / signer.InferenceTypeVLLM / signer.InferenceTypeNone). The
	// benchmark skips entirely for "none" or anything else, so a kind
	// this build does not know how to drive costs no tokens.
	EngineKind string

	// EnginePort is the loopback port the engine listens on. 0
	// short-circuits the benchmark (same effect as the probe loop's
	// skip).
	EnginePort int

	// VariantID is the catalog variant the engine is configured to
	// serve. Recorded on the result for traceability; the benchmark
	// does NOT use it to pick what to send — the engine answers
	// whatever it has loaded.
	VariantID string

	// ModelID is the catalog model id behind EngineModel, recorded on the
	// result so a consumer can tell what was measured. Same relationship
	// to the run as VariantID: recorded, never used to choose what to
	// send.
	ModelID string

	// EngineModel is the engine-native model name (Ollama tag or
	// vLLM /v1/models id). The benchmark inserts this verbatim into
	// the chat-completions request body.
	EngineModel string

	// Phase 7 follow-up (C2): cache key inputs. When all four are
	// populated AND Cache is non-nil, RunBootBenchmark consults the
	// on-disk cache before measuring and persists successful
	// measurements after. Empty GPUModel or VariantSHA disables
	// caching (CPU-only host or unknown variant — both would produce
	// un-discriminating keys across machines).
	GPUModel      string
	VRAMTotalMB   int
	DriverVersion string
	VariantSHA    string

	// Cache, when non-nil, is consulted before measuring and updated
	// after a successful measurement. Failed measurements
	// (Failed=true) are NEVER persisted so transient OOM / engine
	// warmup blips don't stick. nil = caching disabled.
	Cache *benchCache

	// Progress, when non-nil, is called as the measurement advances
	// (waired-agent#199). The benchmark aggregates internally and used to
	// emit nothing until it was over, so the wizard could only show a
	// spinner for up to two minutes — three, counting a cold warm-up.
	//
	// Called from the measuring goroutine, synchronously; keep it cheap.
	Progress func(BenchProgress)

	// EngineReady, when non-nil, is the provider's own readiness answer
	// (agentInferenceProvider.EngineReady) — the same predicate
	// /inference/benchmark gates on before it returns 425. The boot
	// benchmark consults it so an engine that is not up yet stops being
	// reported as a performance verdict (#203): on a fresh install the
	// benchmark used to fire the instant enrollment succeeded, while
	// `waired init` was still installing the engine, and logged
	// "boot benchmark failed ... connection refused" — which sent every
	// investigation at the benchmark instead of the engine (#382).
	//
	// nil means "assume ready", so every existing caller and test keeps
	// today's straight-to-warm-up behaviour. A listening engine that
	// errors is NOT this case and still de-rates.
	EngineReady func() (bool, string)

	// EngineQuiet, when non-nil, reports whether anything else on this host
	// is about to take the engine away — a download in flight, or a
	// serve-env reconcile queued behind one. Ready and quiet are different
	// questions: an engine serving a loaded model answers the first yes
	// while a finished pull is about to stop and respawn it.
	//
	// Consulted because a benchmark that starts anyway loses either way
	// (#582/#601). It either dies to the restart — `EOF` mid-warm-up,
	// reported as a host that cannot answer — or, if it survives, it
	// measures a machine that is concurrently downloading a model, which is
	// the contention awaitQuietEngine's own doc records as the one thing
	// the median of three samples cannot correct for.
	//
	// Not waited on here: this returns the not-ready outcome instead, and
	// the 425 door it leaves through is already a poll-and-retry loop in
	// `waired init` and a re-kick in the setup reconciler.
	//
	// nil means "assume quiet", so every existing caller and test keeps
	// today's behaviour.
	EngineQuiet func(context.Context) bool

	// EngineClaim, when non-nil, TAKES the engine for the length of this
	// benchmark and reports whether it got it. The release is always
	// non-nil.
	//
	// EngineQuiet above is the same question asked a moment earlier, and
	// asking is not enough on its own: the install-time host-speed
	// measurement runs from a background goroutine, so between the answer
	// and the first request there is a window in which it can start. On
	// real hardware it did, and the resulting figure described the two
	// measurements evicting each other rather than the host
	// (waired-agent#703).
	//
	// Declined ⇒ the not-ready outcome and the 425 door, exactly as a
	// busy engine already produces. Never waited on.
	//
	// nil means "the engine is yours", so every existing caller and test
	// keeps today's behaviour.
	EngineClaim func() (release func(), ok bool)

	// EngineGen, when non-nil, is the engine's process generation
	// (agentInferenceProvider.engineProcessGen). Sampled before the
	// warm-up and re-read on failure: a run whose engine generation moved
	// under it was killed by a restart THIS AGENT ordered, which is not a
	// statement about the host's speed.
	//
	// Counting our own restarts rather than classifying the error is the
	// shape runPullJob already uses for the same hazard (#359) — the engine
	// surfaces a killed connection as a bare EOF, so there is no error text
	// to key on.
	//
	// nil returns a constant 0, so the generation never appears to move and
	// every existing caller keeps today's straight-to-failBench behaviour.
	EngineGen func() uint64

	// Now defaults to time.Now if nil. Test injection.
	Now func() time.Time

	// HTTPClient defaults to http.DefaultClient if nil. Test injection.
	HTTPClient *http.Client

	Logger *slog.Logger
}

// RunBootBenchmark issues one token/s benchmark against the local
// engine and returns the derived Capacity. Failures (engine
// unreachable, malformed response, timeout) are warn-logged and
// returned as Capacity=1 (single-stream) so the agent still comes
// up — the alternative (refuse to start) would hide the typical
// "engine still warming up" race in installer flows.
//
// Skipped paths return Capacity=0 ("unlimited") with Failed=false:
//
//   - EngineKind == "none" / ""        — no engine to bench
//   - any other EngineKind             — not a kind this build drives
//   - EnginePort == 0                  — engine intentionally off
//
// The Capacity=0 backward-compat value is the right encoding for
// "no admission cap" — the receiver-side capacityGate skips itself
// at Capacity=0 and the sender-side InFlightTracker permits any
// in-flight count.
func RunBootBenchmark(ctx context.Context, deps BenchDeps) BenchResult {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.HTTPClient == nil {
		deps.HTTPClient = http.DefaultClient
	}
	// Skip paths: no engine, or engine off.
	if deps.EnginePort == 0 ||
		deps.EngineKind == "" ||
		deps.EngineKind == signer.InferenceTypeNone {
		return BenchResult{Capacity: 0, Outcome: benchOutcomeSkipped}
	}
	// Ollama and vLLM both expose an OpenAI-compatible
	// /v1/chat/completions surface; the benchmark talks to it
	// directly rather than going through the agent's own gateway
	// (avoids a self-loop at boot, before the loopback listener
	// is up).
	switch deps.EngineKind {
	case signer.InferenceTypeOllama, signer.InferenceTypeVLLM:
		// supported
	default:
		// Any unknown kind: skip.
		return BenchResult{Capacity: 0, VariantID: deps.VariantID, Outcome: benchOutcomeSkipped}
	}

	// An engine that is not up yet is not a slow one. Checked here rather
	// than left to the warm-up's dial error, because the two are
	// indistinguishable once they reach failBench and only one of them is
	// a statement about this host's performance (#203).
	//
	// Capacity stays 1, not 0: on the wire 0 means UNLIMITED
	// (proto/signer/inference_state.go), and the probe loop only
	// overwrites s.Capacity when non-zero — so returning 0 here would
	// advertise a host with no working engine as accepting unbounded
	// concurrency. 1 is the fail-safe, and it is no longer permanent:
	// inferenceProbeDeps.Capacity now re-reads the provider each tick, so
	// the first successful /inference/benchmark lifts it without a restart.
	if deps.EngineReady != nil {
		if ready, model := deps.EngineReady(); !ready {
			// Two different situations wore one WARN and an empty `model`
			// (waired-agent#633). EngineReady names a model only when it
			// has one to name — four of its five not-ready paths return
			// "", and on a first install the one that fires is "no
			// selection committed yet", because the boot benchmark runs
			// while `waired init` is still installing the engine and
			// pulling the first model.
			//
			// So the field is dropped rather than logged empty, and the
			// no-selection case is Info: it is the expected shape of a
			// fresh install, self-healing (the measurement ran six
			// minutes later on the reported host), and the neighbouring
			// cache-miss line is already Info. A WARN on every first
			// install is the line that gets filtered out, taking the real
			// ones with it. A NAMED model whose engine is unhealthy is a
			// different claim and stays WARN.
			attrs := []any{
				"reason", benchOutcomeEngineNotReady,
				"engine", deps.EngineKind,
				"port", deps.EnginePort,
			}
			if model == "" {
				deps.Logger.Info("inference boot benchmark not run: no model selected yet", attrs...)
			} else {
				deps.Logger.Warn("inference boot benchmark not run: engine not ready",
					append(attrs, "model", model)...)
			}
			return notReadyBenchResult(deps, "engine not ready")
		}
	}

	// Phase 7 follow-up (C2): consult the on-disk cache before
	// burning ~5-30 s on a measurement. The key embeds the host's
	// GPU + driver + the variant's content digest, so a cache hit
	// implies "we already measured this exact (machine, variant,
	// engine) combination once".
	cacheKey := benchCacheKey(deps)
	if cacheKey != "" && deps.Cache != nil {
		if cached, measuredAt, hit, err := deps.Cache.Load(cacheKey); err != nil {
			deps.Logger.Warn("inference boot benchmark: cache load failed; will measure",
				"err", err)
		} else if hit {
			deps.Logger.Info("inference boot benchmark: cache hit",
				"key", cacheKey,
				"capacity", cached.Capacity,
				"tokens_per_sec", cached.TokensPerSec,
				"method", cached.Method,
				"measured_at", measuredAt.UTC().Format(time.RFC3339),
				"age", deps.Now().Sub(measuredAt).Truncate(time.Second).String())
			return cached
		} else {
			deps.Logger.Info("inference boot benchmark: cache miss; measuring",
				"key", cacheKey)
		}
	}

	// Take the engine before the loop and hold it across every retry: the
	// other measurement on this host is the install-time host-speed probe,
	// and it runs minutes long from a background goroutine. Claiming per
	// iteration would hand it the gap between a bounce-grace `continue`
	// and the next request (waired-agent#703).
	//
	// Declining leaves through the same 425 door a busy engine already
	// answers on, which `waired init` and the setup reconciler both
	// already retry.
	if deps.EngineClaim != nil {
		release, ok := deps.EngineClaim()
		if !ok {
			deps.Logger.Warn("inference boot benchmark not run: the engine is busy",
				"reason", benchOutcomeEngineNotReady,
				"engine", deps.EngineKind,
				"port", deps.EnginePort,
				"detail", "another measurement has the engine")
			return notReadyBenchResult(deps, "engine busy: this host is being measured")
		}
		defer release()
	}

	// The measurement, retried without charge across restarts this agent
	// ordered (#582/#601). Every iteration re-asks whether the engine is
	// quiet, so a run that arrives while the host is still installing
	// leaves through the 425 door instead of measuring the contention.
	var (
		tokps   float64
		spread  float64
		samples int
		method  string
	)
	bounceGrace := benchEngineBounceGrace
	for {
		// A busy engine is not a slow one — the same distinction #203 draws
		// for an absent one, reached from the other direction. The reconcile
		// a finishing pull fires stops and respawns `ollama serve`, so
		// starting a measurement while a download is in flight is starting
		// one under a restart that has already been decided.
		if deps.EngineQuiet != nil && !deps.EngineQuiet(ctx) {
			deps.Logger.Warn("inference boot benchmark not run: the engine is busy",
				"reason", benchOutcomeEngineNotReady,
				"engine", deps.EngineKind,
				"port", deps.EnginePort)
			return notReadyBenchResult(deps, "engine busy: a download or an engine restart is in flight")
		}
		// Sampled before the first byte is asked for, and compared against
		// on every failure below.
		gen := deps.engineGen()

		// Warm-up: one tiny untimed completion so the engine loads the
		// model OUTSIDE the measured window. Without it a cold multi-GB
		// load dominated the elapsed time and the host read as an order of
		// magnitude slower than its real decode rate.
		//
		// Announced before it starts: this is the longest silent stretch of
		// the whole run (#199).
		deps.report(BenchProgress{Phase: benchPhaseWarmup, Trials: benchSampleCount})
		if err := warmUpEngine(ctx, deps); err != nil {
			if bounceGrace > 0 && deps.engineGen() != gen {
				bounceGrace--
				deps.Logger.Info("inference boot benchmark interrupted by an engine restart during warm-up; retrying without charging the attempt",
					"grace_left", bounceGrace, "err", err)
				continue
			}
			return failBench(deps, "warmup", err)
		}

		var err error
		tokps, spread, samples, method, err = measureDecodeRate(ctx, deps)
		if err != nil {
			if bounceGrace > 0 && deps.engineGen() != gen {
				bounceGrace--
				deps.Logger.Info("inference boot benchmark interrupted by an engine restart during the measurement; retrying without charging the attempt",
					"grace_left", bounceGrace, "err", err)
				continue
			}
			// Distinguish timeout (context deadline) from other errors
			// in the log line so operators can tell "model loading too
			// slow" from "engine not listening".
			if errors.Is(err, context.DeadlineExceeded) {
				return failBench(deps, "timeout", err)
			}
			return failBench(deps, "measure", err)
		}
		break
	}
	cap := int(tokps / avgCodingAgentTokRate)
	if cap < 1 {
		cap = 1
	}
	deps.Logger.Info("inference boot benchmark completed",
		"engine_kind", deps.EngineKind,
		"variant", deps.VariantID,
		"engine_model", deps.EngineModel,
		"method", method,
		"samples", samples,
		"spread_pct", fmt.Sprintf("%.1f", spread),
		"tokens_per_sec", tokps,
		"capacity", cap)
	result := BenchResult{
		TokensPerSec: tokps,
		Capacity:     cap,
		VariantID:    deps.VariantID,
		ModelID:      deps.ModelID,
		Method:       method,
		SpreadPct:    spread,
		Outcome:      benchOutcomeMeasured,
	}
	// Phase 7 follow-up (C2): persist only successful measurements.
	// failBench paths return above without reaching this point so
	// transient OOM / engine warmup blips never become sticky.
	if cacheKey != "" && deps.Cache != nil {
		meta := benchCacheHumanMeta{
			VariantID:     deps.VariantID,
			GPUModel:      deps.GPUModel,
			VRAMTotalMB:   deps.VRAMTotalMB,
			DriverVersion: deps.DriverVersion,
			EngineKind:    deps.EngineKind,
			EngineModel:   deps.EngineModel,
		}
		if err := deps.Cache.Store(cacheKey, result, meta, deps.Now()); err != nil {
			deps.Logger.Warn("inference boot benchmark: cache store failed",
				"key", cacheKey, "err", err)
		} else {
			deps.Logger.Info("inference boot benchmark: cache stored",
				"key", cacheKey, "capacity", cap)
		}
	}
	return result
}

// benchChatRequest builds the OpenAI-compatible chat-completions
// request both the warm-up and the timed measurement send; only the
// completion budget differs.
func benchChatRequest(ctx context.Context, deps BenchDeps, maxTokens int) (*http.Request, error) {
	body, err := json.Marshal(map[string]any{
		"model":      deps.EngineModel,
		"max_tokens": maxTokens,
		"messages": []map[string]string{
			{"role": "user", "content": benchPrompt},
		},
		"stream": false,
	})
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", deps.EnginePort)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

// warmUpEngine issues one tiny untimed completion so the engine loads
// the model outside the measured window. Failure is treated as a
// benchmark failure by the caller: a host that cannot serve 8 tokens
// within benchWarmupTimeout will not produce a usable measurement
// either.
func warmUpEngine(ctx context.Context, deps BenchDeps) error {
	wctx, cancel := context.WithTimeout(ctx, benchWarmupTimeout)
	defer cancel()
	req, err := benchChatRequest(wctx, deps, benchWarmupCompletionTokens)
	if err != nil {
		return err
	}
	resp, err := deps.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Status first, because the failure path wants the body the success
	// path only needs to drain. engineHTTPError drains what it does not
	// read, so the keep-alive connection is reusable either way.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return engineHTTPError(resp)
	}
	// Drain so the keep-alive connection is immediately reusable for
	// the timed request.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
	return nil
}

// engineErrorBodyLimit bounds how much of a failed response is read for
// the error message. ollama's is one sentence; an HTML error page from
// something else on the port is not, and neither belongs in a log line
// in full.
//
// Raised from 512 with waired-agent#1058, which gave this limit a second
// job: the resulting string is what runtime.EngineOutOfMemory is asked
// about, so a marker cut off here is a classification that never
// happens. The measured out-of-memory body is 99 bytes, but it arrives
// wrapped ("an error was encountered while running the model: ...") and
// a future engine that prefixes a trace would push the marker back.
// 2 KiB keeps a wrapped engine sentence whole while still refusing to
// put a whole error page in a log line.
const engineErrorBodyLimit = 2 << 10

// engineHTTPError turns a non-2xx engine response into an error carrying
// the engine's OWN reason, and drains the rest of the body so the
// keep-alive connection stays reusable.
//
// It exists because every failure in this file used to read as
// `HTTP 500` and nothing else. ollama answers a failed completion with
// `{"error": "..."}` and that sentence was discarded at the status
// check. waired-ai/waired-agent#552 spent three CI runs and a complete
// engine.log unable to say why a benchmark failed, and the reason was
// sitting in a body nobody read.
//
// The result is one line: it reaches a slog attribute and the `waired
// init` transcript, neither of which survives an embedded newline
// legibly.
func engineHTTPError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, engineErrorBodyLimit))
	// Whatever is left after the prefix, so the connection is reusable.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))

	reason := strings.TrimSpace(string(body))
	// ollama and the OpenAI-compat surface both answer with an error
	// object; the message alone is what a reader wants. Anything that
	// does not parse falls back to the raw prefix, which is still more
	// than the status code was.
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil && len(envelope.Error) > 0 {
		var msg string
		var obj struct {
			Message string `json:"message"`
		}
		switch {
		case json.Unmarshal(envelope.Error, &msg) == nil && msg != "":
			reason = msg
		case json.Unmarshal(envelope.Error, &obj) == nil && obj.Message != "":
			reason = obj.Message
		}
	}
	reason = strings.Join(strings.Fields(reason), " ")
	if reason == "" {
		return fmt.Errorf("HTTP %d (engine sent no reason)", resp.StatusCode)
	}
	return fmt.Errorf("HTTP %d: %s", resp.StatusCode, reason)
}

// errNoEvalCounters signals the engine's native endpoint is absent or
// its response carries no usable eval counters (older ollama, an
// OpenAI-compat proxy on the engine port). The caller falls back to
// the slope method instead of failing the benchmark.
var errNoEvalCounters = errors.New("no decode counters in engine response")

// errSlopeDegenerate signals every slope pair collapsed (long run not
// measurably longer than the short one — proxy caching, coarse clock).
// The caller may salvage a wall-clock rate from the best single run.
var errSlopeDegenerate = errors.New("all slope sample pairs degenerate")

// measureDecodeRate runs the #764 measurement chain and returns the
// median decode rate with its sample spread and the benchMethod* that
// produced it: ollama's native eval counters when available, the
// two-length slope on the OpenAI-compat surface otherwise, and the
// legacy single-run wall clock when even the slope is degenerate. The
// whole loop shares one benchMeasureBudget deadline; each request
// keeps its own benchTimeout.
func measureDecodeRate(ctx context.Context, deps BenchDeps) (float64, float64, int, string, error) {
	mctx, cancel := context.WithTimeout(ctx, benchMeasureBudget)
	defer cancel()
	if deps.EngineKind == signer.InferenceTypeOllama {
		tokps, spread, samples, err := measureOllamaNative(mctx, deps)
		if err == nil {
			return tokps, spread, samples, benchMethodOllamaEval, nil
		}
		if !errors.Is(err, errNoEvalCounters) {
			return 0, 0, 0, "", err
		}
		deps.Logger.Warn("inference boot benchmark: engine returned no decode counters; falling back to two-length slope",
			"err", err)
	}
	tokps, spread, samples, best, err := measureOpenAISlope(mctx, deps)
	if err == nil {
		return tokps, spread, samples, benchMethodSlope, nil
	}
	if errors.Is(err, errSlopeDegenerate) && best.tokens > 0 && best.elapsed > 0 {
		deps.Logger.Warn("inference boot benchmark: slope degenerate; falling back to single-run wall clock (overhead-contaminated, understates fast hosts)",
			"err", err)
		return float64(best.tokens) / best.elapsed.Seconds(), 0, 1, benchMethodWallClock, nil
	}
	return 0, 0, 0, "", err
}

// ollamaGenerateOnce issues one native /api/generate completion and
// returns the pure decode rate from eval_count/eval_duration — the
// same counters (and endpoint) the depth benchmark reads, so the #133
// shallow-vs-depth floor comparison shares one measurement basis.
// timeout is passed rather than read from benchTimeout so the caller can
// size it to the completion it just asked for — a 200-token request under
// a 30 s cap is unsatisfiable below ~7 tok/s, which is how a working
// engine came to report a benchmark failure (#203). Taking it as an
// argument is also what makes that case writable in a test.
func ollamaGenerateOnce(ctx context.Context, deps BenchDeps, numPredict int, timeout time.Duration) (float64, error) {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	body, err := json.Marshal(map[string]any{
		"model":  deps.EngineModel,
		"prompt": benchPrompt,
		"stream": false,
		"options": map[string]any{
			"num_predict": numPredict,
			"temperature": 0,
		},
	})
	if err != nil {
		return 0, err
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/api/generate", deps.EnginePort)
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := deps.HTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// Nothing native behind this port (proxy, non-ollama server).
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return 0, fmt.Errorf("%w: /api/generate returned 404", errNoEvalCounters)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, engineHTTPError(resp)
	}
	var gen struct {
		EvalCount    int   `json:"eval_count"`
		EvalDuration int64 `json:"eval_duration"` // ns
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256*1024)).Decode(&gen); err != nil {
		return 0, err
	}
	if gen.EvalCount <= 0 || gen.EvalDuration <= 0 {
		return 0, fmt.Errorf("%w: eval_count=%d eval_duration=%d",
			errNoEvalCounters, gen.EvalCount, gen.EvalDuration)
	}
	return float64(gen.EvalCount) / (float64(gen.EvalDuration) / 1e9), nil
}

// measureOllamaNative takes up to benchSampleCount native decode
// samples and reduces them to (median, spread, count). An error after
// at least one valid sample truncates the loop instead of discarding
// it — the shared measurement budget is the usual cause.
// Each sample is sized from what the previous ones measured
// (planBenchSizing): the first is a short probe, and the rest grow to
// benchPromptCompletionTokens only on a host fast enough to decode that
// many inside its share of the shared budget. Before that, every sample
// asked for 200 tokens under a fixed 30 s cap, so a host below ~7 tok/s
// failed at i=0 with nothing to salvage and the benchmark reported a
// working engine as broken (#203).
func measureOllamaNative(ctx context.Context, deps BenchDeps) (float64, float64, int, error) {
	var rates []float64
	// The budget is tracked here as well as read off the context: a caller
	// without a deadline (a direct measureOllamaNative, a test) would
	// otherwise see the full budget before every sample and keep sizing as
	// if nothing had been spent.
	started := time.Now()
	deadline, hasDeadline := ctx.Deadline()
	for i := 0; i < benchSampleCount; i++ {
		remaining := benchMeasureBudget - time.Since(started)
		if hasDeadline {
			remaining = min(remaining, time.Until(deadline))
		}
		plan := planBenchSizing(benchSizingFacts{
			ObservedTokps: medianFloat(rates),
			Remaining:     remaining,
			SamplesLeft:   benchSampleCount - i,
		})
		r, err := ollamaGenerateOnce(ctx, deps, plan.CompletionTokens, plan.RequestTimeout)
		if err != nil {
			if len(rates) > 0 {
				deps.Logger.Warn("inference boot benchmark: sample failed; using completed samples",
					"completed", len(rates), "err", err)
				break
			}
			return 0, 0, 0, err
		}
		rates = append(rates, r)
		deps.report(sampleProgress(rates, r, benchMethodOllamaEval))
	}
	return medianFloat(rates), spreadPercent(rates), len(rates), nil
}

// sampleProgress builds the report for one completed sample: the sample
// itself plus the running median and spread over everything measured so
// far. Running rather than final on purpose — the number on screen then
// converges instead of jumping, and MeasuredTokps stays what it has
// always meant (the finished answer, waired#934 §7.2).
func sampleProgress(all []float64, sample float64, method string) BenchProgress {
	return BenchProgress{
		Phase:       benchPhaseMeasuring,
		Trial:       len(all),
		Trials:      benchSampleCount,
		SampleTokps: sample,
		MedianTokps: medianFloat(all),
		SpreadPct:   spreadPercent(all),
		Method:      method,
	}
}

// benchSingleRun is one completed OpenAI-compat run, retained so the
// wall-clock fallback can salvage a rate when every slope pair is
// degenerate.
type benchSingleRun struct {
	tokens  int
	elapsed time.Duration
}

// track keeps the run with the highest wall-clock rate seen so far.
func (b *benchSingleRun) track(tokens int, elapsed time.Duration) {
	if tokens <= 0 || elapsed <= 0 {
		return
	}
	if b.elapsed <= 0 ||
		float64(tokens)/elapsed.Seconds() > float64(b.tokens)/b.elapsed.Seconds() {
		b.tokens, b.elapsed = tokens, elapsed
	}
}

// measureOpenAISlope estimates the decode rate as the slope between a
// short and a long completion of the same prompt:
//
//	tokps = (tok_long − tok_short) / (elapsed_long − elapsed_short)
//
// The subtraction cancels the fixed per-request overhead (HTTP,
// scheduling, prefill, first-token latency) that a single wall-clock
// run silently attributes to decode. Up to benchSampleCount pairs are
// measured; the median slope wins. Degenerate pairs are skipped; if
// none survive, errSlopeDegenerate is returned along with the best
// single run for the caller's wall-clock fallback.
func measureOpenAISlope(ctx context.Context, deps BenchDeps) (float64, float64, int, benchSingleRun, error) {
	var slopes []float64
	var best benchSingleRun
	for i := 0; i < benchSampleCount; i++ {
		shortTok, shortEl, err := timedChatCompletion(ctx, deps, benchSlopeShortTokens)
		if err != nil {
			if len(slopes) > 0 {
				deps.Logger.Warn("inference boot benchmark: sample failed; using completed samples",
					"completed", len(slopes), "err", err)
				break
			}
			return 0, 0, 0, best, err
		}
		best.track(shortTok, shortEl)
		longTok, longEl, err := timedChatCompletion(ctx, deps, benchSlopeLongTokens)
		if err != nil {
			if len(slopes) > 0 {
				break
			}
			return 0, 0, 0, best, err
		}
		best.track(longTok, longEl)
		if longEl <= shortEl || longTok <= shortTok {
			continue // degenerate pair; nothing to divide
		}
		slope := float64(longTok-shortTok) / (longEl - shortEl).Seconds()
		slopes = append(slopes, slope)
		// One PAIR is one data point here, and that is what the wizard
		// counts. #199 settles the vocabulary: the UI says "measurement n
		// of 3" and never exposes that a slope sample is two requests.
		deps.report(sampleProgress(slopes, slope, benchMethodSlope))
	}
	if len(slopes) == 0 {
		return 0, 0, 0, best, errSlopeDegenerate
	}
	return medianFloat(slopes), spreadPercent(slopes), len(slopes), best, nil
}

// timedChatCompletion issues one non-streaming chat completion and
// returns (completion tokens, wall-clock elapsed). On its own the
// elapsed still contains fixed request overhead — callers cancel it
// via the slope, or accept the bias in the wall-clock fallback.
func timedChatCompletion(ctx context.Context, deps BenchDeps, maxTokens int) (int, time.Duration, error) {
	rctx, cancel := context.WithTimeout(ctx, benchTimeout)
	defer cancel()
	req, err := benchChatRequest(rctx, deps, maxTokens)
	if err != nil {
		return 0, 0, err
	}
	start := deps.Now()
	resp, err := deps.HTTPClient.Do(req)
	elapsed := deps.Now().Sub(start)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, 0, engineHTTPError(resp)
	}
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return 0, 0, err
	}
	tokens, err := extractCompletionTokens(respBody)
	if err != nil {
		return 0, 0, err
	}
	if tokens <= 0 {
		return 0, 0, fmt.Errorf("response reported %d completion tokens", tokens)
	}
	if elapsed <= 0 {
		// Clock that doesn't move — only happens with broken Now
		// injection. Error out so the test surface doesn't paper
		// over a real wiring bug.
		return 0, 0, fmt.Errorf("elapsed time was %v", elapsed)
	}
	return tokens, elapsed, nil
}

// medianFloat returns the median of xs (0 for an empty slice); xs is
// not mutated.
func medianFloat(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	return (s[mid-1] + s[mid]) / 2
}

// spreadPercent is (max−min)/median in percent — a cheap dispersion
// signal recorded with the measurement so noisy hosts are visible in
// logs and the bench cache. 0 for fewer than two samples.
func spreadPercent(xs []float64) float64 {
	m := medianFloat(xs)
	if len(xs) < 2 || m <= 0 {
		return 0
	}
	lo, hi := xs[0], xs[0]
	for _, x := range xs[1:] {
		if x < lo {
			lo = x
		}
		if x > hi {
			hi = x
		}
	}
	return (hi - lo) / m * 100
}

// notReadyBench is what both readiness gates return: a run that never
// produced a measurement because the engine was not there to measure —
// not up yet (#203), or about to be restarted under it (#582/#601).
//
// Capacity stays 1, not 0: on the wire 0 means UNLIMITED
// (proto/signer/inference_state.go), and the probe loop only overwrites
// s.Capacity when non-zero — so returning 0 here would advertise a host
// with no working engine as accepting unbounded concurrency. 1 is the
// fail-safe, and it is no longer permanent: inferenceProbeDeps.Capacity
// re-reads the provider each tick, so the first successful
// /inference/benchmark lifts it without a restart.
//
// Failed stays true because every consumer gates on it to skip an
// unusable measurement; Outcome is what tells this ending apart from a
// run that reached the engine and failed, and it is the value the 425
// door keys on (RunBenchmark, internal/management maps it to 425).
func notReadyBenchResult(deps BenchDeps, reason string) BenchResult {
	return BenchResult{
		Capacity:  unmeasuredCapacity,
		VariantID: deps.VariantID,
		Failed:    true,
		Err:       reason,
		Outcome:   benchOutcomeEngineNotReady,
	}
}

// benchEngineBounceGrace bounds how many times one benchmark run may be
// restarted out from under itself before it reports an honest failure.
//
// Two, for the reason enginePullBounceGrace documents for downloads: two
// is the worst case the daemon can inflict in one go (a backend fallback
// restart and a tuning degrade, one each), and a bound rather than an
// unbounded free pass keeps an engine that restarts forever reaching a
// verdict — `waired init`'s exit 3 is what install.sh and install.ps1
// branch on for a host whose local AI is genuinely down.
const benchEngineBounceGrace = 2

// failBench logs a warning and returns unmeasuredCapacity so the agent
// continues with a single-stream admission rather than refusing to
// start. Reason is a short slug for log filtering.
func failBench(deps BenchDeps, reason string, err error) BenchResult {
	deps.Logger.Warn("inference boot benchmark failed; falling back to Capacity=1",
		"reason", reason,
		"err", err)
	return BenchResult{
		Capacity:  unmeasuredCapacity,
		VariantID: deps.VariantID,
		Failed:    true,
		Err:       err.Error(),
		Outcome:   benchOutcomeFailed,
	}
}

// extractCompletionTokens reads the OpenAI-compatible response
// envelope and pulls out usage.completion_tokens. Ollama mirrors
// this shape since v0.5 and vLLM does so by spec. Falls back to
// counting tokens from the message content (whitespace-split) when
// the engine omits usage — a degraded but non-fatal accuracy hit.
func extractCompletionTokens(body []byte) (int, error) {
	var env struct {
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return 0, err
	}
	if env.Usage.CompletionTokens > 0 {
		return env.Usage.CompletionTokens, nil
	}
	if len(env.Choices) == 0 {
		return 0, errors.New("response has no choices and no usage")
	}
	// Whitespace-based fallback. Off by ~10% vs the real tokeniser
	// but adequate for tok/s on the order-of-magnitude scale the
	// admission cap consumes.
	content := env.Choices[0].Message.Content
	if content == "" {
		return 0, errors.New("choices[0].message.content is empty")
	}
	tokens := 1 // start at 1 to capture the leading non-space chunk
	for _, c := range content {
		if c == ' ' || c == '\n' || c == '\t' {
			tokens++
		}
	}
	return tokens, nil
}
