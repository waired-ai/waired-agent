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

	// The served model's speed at depth (waired-ai/waired-agent#1341):
	// the engine's prefill and decode rates on a
	// hostfit.SpeedMeasurementDepthTokens request, the depth it actually
	// prefilled, and the verdict figure — TurnSeconds, or for a request
	// that stalled, only TurnFloorSeconds. TokensPerSec above is the same
	// DecodeTokps, kept for the readers that still quote a rate.
	PrefillTokps     float64
	DecodeTokps      float64
	DepthTokens      int
	TurnSeconds      float64
	TurnFloorSeconds float64
	Samples          int

	Failed bool
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
	// Cached says the figure came from the on-disk cache rather than
	// from an engine this run asked. It is still a measurement — Outcome
	// is "measured" either way — but it was taken at some earlier boot,
	// and a caller that files it under a timestamp has to know that.
	//
	// measuredRatesFrom keeps the most RECENT measurement per variant
	// (`m.MeasuredAt.After(best.MeasuredAt)`), so re-filing a cached
	// figure with today's date would let it outrank a fresher real one
	// (waired-agent#1150).
	Cached bool
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

	// ElapsedSeconds is how long the measurement request has run, from
	// when it was sent; BudgetSeconds the line; DepthTokens the planned
	// prompt depth. Past the line OverBudget is set and TurnFloorSeconds
	// carries the lower bound (decision 4 of
	// docs/decisions/20260913/2245).
	ElapsedSeconds   float64
	BudgetSeconds    float64
	DepthTokens      int
	OverBudget       bool
	TurnFloorSeconds float64
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
// routes to someone else rather than piling on. The host's own client is
// NOT unaffected any more: since the owner ruling of 2026-09-12
// (waired-agent#1302) a request from this device is an equal claimant on
// the ceiling with an own-network peer's, so on an unmeasured host a second
// concurrent turn queues for a slot rather than being admitted past it. The
// engine has one slot either way; what changed is that the two turns no
// longer contend and evict each other's prefixes.
//
// Deliberately NOT applied to a host with no engine at all (EnginePort 0,
// engine kind none): RunBootBenchmark's skip paths return 0 on purpose, and
// that encoding stays.
const unmeasuredCapacity = 1

// benchSampleCount is how many samples the install-time host cutoff takes;
// the reported figure is their median. Run-to-run spread was measured at
// ~8%, so the median mostly guards against a single blip rather than
// averaging noise.
const benchSampleCount = 3

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

	// EngineVersion is the RELEASE of EngineKind that produced the
	// measurement — "0.33.3", "0.28.0" — and it is a cache key input
	// rather than a label (waired-agent#1131). An engine release moves
	// decode rate on its own: #1079 retired the forced generation batch
	// because the engine started sizing its own, and #1038 was diagnosed
	// on the same axis. Without it a host that takes an agent update —
	// which is exactly when the engine pin moves — keeps serving, and
	// advertising, what the engine it no longer runs measured.
	//
	// Empty disables caching, the same fail-closed rule
	// hostSpeedStillApplies (host_cutoff.go) and
	// router.engineVersionSatisfies already apply: an engine whose
	// version cannot be read is not evidence that it is current.
	EngineVersion string

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

	// WarmSlots, when non-nil, reports how many conversations this host
	// can hold warm — the quantity BenchResult.Capacity carries since
	// waired-agent#1126. It is a function rather than a value because
	// the engine's tuning is applied when the engine spawns, which can
	// be after this benchmark starts; 0 means "not known yet" and the
	// result falls back to unmeasuredCapacity.
	//
	// The benchmark does not compute it: the slot count comes from the
	// tuning the engine applied, not from anything a decode measurement
	// can see.
	WarmSlots func() int

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

	// AppliedWindow is the context window the engine serves this model
	// with, 0 when unknown. The measurement prompt fits inside it
	// (modelSpeedDepth).
	AppliedWindow int

	// KVCacheType and NumParallel complete the serving configuration the
	// measurement describes. With AppliedWindow they are cache-key inputs:
	// a new window, KV type or slot count serves the same weights at a
	// different speed, so a stored figure must not answer for it.
	KVCacheType string
	NumParallel int

	// ServingInFlight, when non-nil, reports this host's serving traffic.
	// A measurement request gives the engine back the moment it is
	// non-zero rather than make a person's turn wait behind 32,768 tokens.
	ServingInFlight func() int

	// IdleBefore, when positive, is how long serving traffic must have been
	// absent before a measurement starts — set after a measurement yielded,
	// so it does not start again into the next turn of the same session.
	IdleBefore time.Duration

	// TuningPending reports that the engine's post-load verification has
	// not settled yet: it can still restart the engine with a smaller
	// window, and a measurement sent into that restart measures a dying
	// engine. The measurement declines (not-ready) meanwhile.
	TuningPending bool

	// StoredMeasurement, when non-nil, answers from the state ledger when
	// the disk cache misses (agentInferenceProvider.storedSpeedMeasurement).
	StoredMeasurement func() (BenchResult, bool)

	// Selected, when non-nil, reports the variant being served now. A
	// measurement stops when it changes: a model switch is one of the two
	// things that end one (decision 4 of docs/decisions/20260913/2245).
	Selected func() string

	// CacheOnly answers from a stored figure or not at all: the boot tail's
	// synchronous attempt, which must not hold the daemon's start for the
	// minutes a measurement takes. The loop behind it measures.
	CacheOnly bool

	// SkipCacheLoad measures even when a stored figure exists, and stores
	// the new one over it: a person asked for a new number
	// (management.BenchmarkModeRerun).
	SkipCacheLoad bool

	// Nonce leads the measurement prompt; empty derives one from Now.
	Nonce string

	// LineSeconds and StallCap are test seams; zero means
	// hostfit.ModelTurnBudgetSeconds and modelSpeedStallCap.
	LineSeconds float64
	StallCap    time.Duration

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
	if cacheKey == "" && deps.Cache != nil {
		// A cache was configured and cannot be used. Said out loud
		// because the two guards below skip silently, so "never cached"
		// and "cache not reached this boot" produced identical journals
		// (waired-agent#1150). Only when a cache was configured: the
		// explicit-benchmark path passes Cache nil by design and has no
		// key to be missing.
		deps.Logger.Info("inference boot benchmark: caching is off",
			"reason", benchCacheDisabledReason(deps.GPUModel, deps.VariantSHA, deps.EngineVersion))
	}
	if cacheKey != "" && deps.Cache != nil && !deps.SkipCacheLoad {
		if cached, measuredAt, hit, err := deps.Cache.Load(cacheKey); err != nil {
			deps.Logger.Warn("inference boot benchmark: cache load failed; will measure",
				"err", err)
		} else if hit {
			// A hit IS a measurement, so it comes back looking like one.
			// The entry stores what varies between runs; the identity
			// fields are rebuilt from deps because the key already pins
			// them — VariantSHA and EngineModel are IN it, so an entry
			// found under this key cannot belong to another selection.
			//
			// Without this the hit carries ModelID "" and Outcome "",
			// which activeModelNeedsMeasurement reads as "nothing has
			// measured this model" and BenchmarkStatus as neither done
			// nor failed: the host re-measures what the cache exists to
			// avoid, and reports nothing about either run
			// (waired-agent#1150).
			cached.ModelID = deps.ModelID
			cached.Outcome = benchOutcomeMeasured
			cached.Cached = true
			deps.Logger.Info("inference boot benchmark: cache hit",
				"key", cacheKey,
				"capacity", cached.Capacity,
				"turn_seconds", cached.TurnSeconds,
				"method", cached.Method,
				"measured_at", measuredAt.UTC().Format(time.RFC3339),
				"age", deps.Now().Sub(measuredAt).Truncate(time.Second).String())
			return cached
		} else {
			deps.Logger.Info("model speed measurement: cache miss",
				"key", cacheKey)
		}
	}
	if !deps.SkipCacheLoad && deps.StoredMeasurement != nil {
		if stored, ok := deps.StoredMeasurement(); ok {
			stored.ModelID = deps.ModelID
			stored.VariantID = deps.VariantID
			stored.Outcome = benchOutcomeMeasured
			stored.Cached = true
			stored.Capacity = unmeasuredCapacity
			if deps.WarmSlots != nil {
				if n := deps.WarmSlots(); n > 0 {
					stored.Capacity = n
				}
			}
			deps.Logger.Info("model speed measurement: answered from the state ledger",
				"turn_seconds", stored.TurnSeconds)
			return stored
		}
	}
	if deps.CacheOnly {
		return notReadyBenchResult(deps, "not measured yet")
	}
	if deps.TuningPending {
		return notReadyBenchResult(deps, "engine busy: its tuning is still being verified")
	}

	// A measurement that just gave the engine back to this host's own
	// traffic waits for the traffic to be gone a while before it takes the
	// engine again; the next turn of the same session is usually seconds
	// behind the last.
	if deps.IdleBefore > 0 && deps.ServingInFlight != nil {
		if !awaitServingIdle(ctx, deps, deps.IdleBefore) {
			return notReadyBenchResult(deps, "engine busy: this host is serving traffic")
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
			deps.Logger.Warn("model speed measurement not run: the engine is busy",
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
	var speed modelSpeed
	bounceGrace := benchEngineBounceGrace
	for {
		// A busy engine is not a slow one — the same distinction #203 draws
		// for an absent one, reached from the other direction. The reconcile
		// a finishing pull fires stops and respawns `ollama serve`, so
		// starting a measurement while a download is in flight is starting
		// one under a restart that has already been decided.
		if deps.EngineQuiet != nil && !deps.EngineQuiet(ctx) {
			deps.Logger.Warn("model speed measurement not run: the engine is busy",
				"reason", benchOutcomeEngineNotReady,
				"engine", deps.EngineKind,
				"port", deps.EnginePort)
			return notReadyBenchResult(deps, "engine busy: a download or an engine restart is in flight")
		}
		// Sampled before the first byte is asked for, and compared against
		// on every failure below.
		gen := deps.engineGen()

		// Warm-up: one tiny untimed completion so the engine loads the
		// model OUTSIDE the measured request. Announced before it starts:
		// on a cold multi-GB model this is minutes of silence (#199).
		deps.report(BenchProgress{Phase: benchPhaseWarmup, Trials: 1, BudgetSeconds: deps.modelSpeedLine()})
		if err := warmUpEngine(ctx, deps); err != nil {
			if bounceGrace > 0 && deps.engineGen() != gen {
				bounceGrace--
				deps.Logger.Info("model speed measurement interrupted by an engine restart during warm-up; retrying without charging the attempt",
					"grace_left", bounceGrace, "err", err)
				continue
			}
			return failBench(deps, "warmup", err)
		}

		var err error
		speed, err = measureModelSpeed(ctx, deps)
		if err != nil {
			if errors.Is(err, errSelectionChanged) {
				deps.Logger.Info("model speed measurement stopped: the model was switched")
				return notReadyBenchResult(deps, "the model was switched during the measurement")
			}
			if errors.Is(err, errYieldedToTraffic) {
				deps.Logger.Info("model speed measurement gave the engine back to serving traffic; it is owed again once the host is idle")
				return notReadyBenchResult(deps, "engine busy: this host is serving traffic")
			}
			if bounceGrace > 0 && deps.engineGen() != gen {
				bounceGrace--
				deps.Logger.Info("model speed measurement interrupted by an engine restart; retrying without charging the attempt",
					"grace_left", bounceGrace, "err", err)
				continue
			}
			if ctx.Err() != nil {
				return failBench(deps, "stopped", err)
			}
			return failBench(deps, "measure", err)
		}
		break
	}
	// Capacity is how many conversations this host holds warm, not a
	// function of the rate just measured (waired-agent#1126). The old
	// floor(tokps / 30) answered a different question from the one
	// routing asks and was never clamped by the engine's KV slot count.
	// A tuning that has not been applied yet reads as unmeasured, and
	// capacityFn lifts the advertised figure on the next probe tick
	// without a re-benchmark.
	cap := unmeasuredCapacity
	if deps.WarmSlots != nil {
		if n := deps.WarmSlots(); n > 0 {
			cap = n
		}
	}
	deps.Logger.Info("model speed measurement completed",
		"engine_kind", deps.EngineKind,
		"variant", deps.VariantID,
		"engine_model", deps.EngineModel,
		"method", speed.Method,
		"depth_tokens", speed.DepthTokens,
		"prefill_tokps", fmt.Sprintf("%.1f", speed.PrefillTokps),
		"decode_tokps", fmt.Sprintf("%.1f", speed.DecodeTokps),
		"turn_seconds", fmt.Sprintf("%.1f", speed.TurnSeconds),
		"turn_floor_seconds", fmt.Sprintf("%.1f", speed.TurnFloorSeconds),
		"line_seconds", deps.modelSpeedLine(),
		"samples", speed.Samples,
		"spread_pct", fmt.Sprintf("%.1f", speed.SpreadPct),
		"capacity", cap)
	result := BenchResult{
		TokensPerSec:     speed.DecodeTokps,
		Capacity:         cap,
		VariantID:        deps.VariantID,
		ModelID:          deps.ModelID,
		Method:           speed.Method,
		SpreadPct:        speed.SpreadPct,
		PrefillTokps:     speed.PrefillTokps,
		DecodeTokps:      speed.DecodeTokps,
		DepthTokens:      speed.DepthTokens,
		TurnSeconds:      speed.TurnSeconds,
		TurnFloorSeconds: speed.TurnFloorSeconds,
		Samples:          speed.Samples,
		Outcome:          benchOutcomeMeasured,
	}
	if speed.bound() {
		// A stalled engine: a verdict for now, never a stored figure. It
		// is not a speed, and storing it would keep the host marked slow
		// across restarts until someone asked again.
		return result
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
			EngineVersion: deps.EngineVersion,
			AppliedWindow: deps.AppliedWindow,
			KVCacheType:   deps.KVCacheType,
			NumParallel:   deps.NumParallel,
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
