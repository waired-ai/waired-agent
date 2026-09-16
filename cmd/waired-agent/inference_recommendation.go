package main

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/notice"
	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/proto/hostfit"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// SetLastBench records the most recent boot/explicit benchmark result so
// Status() and the catalog endpoint can derive the #133 faster-model
// recommendation. Called from the probe goroutine in main.go after
// RunBootBenchmark and from RunBenchmark.
func (p *agentInferenceProvider) SetLastBench(b BenchResult) {
	p.benchMu.Lock()
	defer p.benchMu.Unlock()
	bc := b
	p.lastBench = &bc
	// When this host learned it — what a requester orders two readings of
	// one peer by (HealthSnapshot.Speed.MeasuredAt).
	p.lastBenchAt = time.Now()
}

// bootBenchFailure returns the last boot-path result when it is a
// failure worth reporting, or nil.
//
// "Worth reporting" excludes the two endings that are not verdicts about
// this host. A skipped run (no engine, an external endpoint) is a
// deliberate Capacity 0 and never a fault — three separate places
// document that a skip must not read as one. An engine-not-ready run did
// not reach the engine, which is the ordinary shape of a fresh install:
// the benchmark runs while `waired init` is still installing the engine
// and pulling the first model, and it self-heals minutes later. Both
// would turn a normal first boot into a reported failure.
//
// What is left is a run that reached a working engine and could not
// measure it — the ending #203 is about, and the one that until now
// existed only as a WARN line.
func (p *agentInferenceProvider) bootBenchFailure() *BenchResult {
	p.benchMu.Lock()
	defer p.benchMu.Unlock()
	if p.lastBench == nil || !p.lastBench.Failed {
		return nil
	}
	if p.lastBench.Outcome != benchOutcomeFailed {
		return nil
	}
	b := *p.lastBench
	return &b
}

// bootBenchWaiting returns the last boot-path result when it stopped at
// the readiness gate, or nil.
//
// Not a failure — bootBenchFailure above is right to exclude it — but not
// nothing either. With no persisted record and no job in flight,
// /inference/benchmark/status answered a bare "idle" for a host that has
// been TRYING and cannot get to the engine, which is byte-identical to a
// host nobody has asked yet. That is the reading gap waired-agent#1150
// had to close by hand, from journal lines, on live hardware.
//
// State stays idle. "failed" would be untrue, and "running" is the
// wizard's re-run guard's word for a job in flight — moving it here would
// either stall a measurement that needs kicking or claim one is under way
// when none is. What this adds is the reason beside the state.
func (p *agentInferenceProvider) bootBenchWaiting() *BenchResult {
	p.benchMu.Lock()
	defer p.benchMu.Unlock()
	if p.lastBench == nil || p.lastBench.Outcome != benchOutcomeEngineNotReady {
		return nil
	}
	b := *p.lastBench
	return &b
}

// AdvertisedCapacity is the admission cap the probe loop publishes, read
// per tick like Hardware / RecommendedMaxParallel / DeclaredContextWindow
// (#387). 0 = nothing measured yet, which the probe treats as "leave the
// field off the push".
//
// Reading it live is what lets a later successful /inference/benchmark
// raise a boot-time de-rating without a daemon restart (#203): a fresh
// install benchmarks before `waired init` has finished installing the
// engine, so the boot result is Capacity=1 on a host that may be far
// faster than that.
func (p *agentInferenceProvider) AdvertisedCapacity() int {
	p.benchMu.Lock()
	defer p.benchMu.Unlock()
	if p.lastBench == nil {
		return 0
	}
	return p.lastBench.Capacity
}

// currentRecommendation derives the live faster-model recommendation
// from the last benchmark result: non-nil when it measured over the line
// and a faster model is available. Safe to call with no benchmark
// recorded yet (nil). There is no upgrade recommendation any more
// (waired-ai/waired-agent#1342; decision 6 of
// docs/decisions/20260913/2245-speed-is-one-request-at-32768-tokens.md).
func (p *agentInferenceProvider) currentRecommendation(ctx context.Context) *management.BenchmarkRecommendation {
	p.benchMu.Lock()
	last := p.lastBench
	p.benchMu.Unlock()
	if last == nil {
		return nil
	}
	hw := p.profiler.Profile(ctx)
	engineVersion := p.servingEngineVersion(ctx)
	return recommendationFromBench(*last, p.store, hw, p.manifests, p.cfg, engineVersion)
}

// benchDescribes reports whether a stored benchmark is evidence about
// activeModelID.
//
// A rate measured on one model says nothing about another, and the two
// come apart on the ordinary paths: a pull finishing activates a model
// the boot benchmark never saw, and a switch replaces the active
// selection without re-measuring. On the browser-takeover path that gap
// is the whole defect — init exits while the download is still running,
// so the only measurement on file belongs to whatever was serving before
// (waired-ai/waired-agent#783).
//
// An unlabelled result (a cache entry, a build predating BenchResult.ModelID)
// is treated as evidence, which is the behaviour every reader had before
// the field existed. Withholding a recommendation from those hosts would
// trade a stale number for no number at all.
func benchDescribes(bench BenchResult, activeModelID string) bool {
	return bench.ModelID == "" || bench.ModelID == activeModelID
}

// benchMeasurement turns a completed run into the ledger entry the
// ranking reads, and the VariantSHA key to file it under. An empty key
// means "record nothing" (waired-agent#784).
//
// Unlike benchDescribes above, an UNLABELLED result is refused here
// rather than trusted. The two are asked different questions and the
// safe answer differs. benchDescribes asks "may this number be shown
// against the active model" and treating an unlabelled figure as
// evidence keeps the behaviour hosts had before the label existed.
// This asks "which model does this number condemn", and there is no
// prior behaviour to preserve — a guess here files a real measurement
// against a model that was never run, and the ranking would then refuse
// to recommend a model on evidence about a different one.
//
// A failed run and a zero rate are refused for the reason
// recommendationFromBench refuses them: they are not measurements. A
// run whose variant is not in the catalog is refused because VariantSHA
// is the key, and without it the entry could only be filed under a
// variant id, which collides across models (qwen3-8b and llama3-8b can
// both ship a "q4-gguf").
// PublishedMeasurements is the persisted ledger in wire form
// (waired-agent#970): what this host has actually run and timed, one
// entry per variant, for the control plane to rank on the same facts
// this agent does — in seconds per request since waired-ai/waired-agent#1341.
//
// Sorted by model then variant so the pushed bytes are stable across
// ticks. The map they come from is not ordered, and an unordered slice
// would make every push differ from the last — which the control plane
// compares by content to decide whether to store and notify, so the
// churn would be a re-store and a map-changed notification per tick, on
// every host, forever.
//
// Nil for a host that has measured nothing, which every fresh install
// is, and which the wire reads as "no claim". An entry from before the
// seconds verdict (a decode rate and nothing else) is not published: it
// judges nothing any more, and the control plane would drop it anyway.
func (p *agentInferenceProvider) PublishedMeasurements() []signer.ModelMeasurement {
	st, err := p.store.Load()
	if err != nil || len(st.MeasuredVariants) == 0 {
		return nil
	}
	out := make([]signer.ModelMeasurement, 0, len(st.MeasuredVariants))
	for _, m := range st.MeasuredVariants {
		if m.TurnSeconds <= 0 || m.ModelID == "" || m.VariantID == "" {
			// Nothing keyable, or nothing judged, so nothing to say. The
			// ledger writer already refuses these, and this is the second
			// reader of the same rule rather than a new one.
			continue
		}
		out = append(out, signer.ModelMeasurement{
			ModelID:       m.ModelID,
			VariantID:     m.VariantID,
			DecodeTokps:   m.MeasuredTokps,
			Method:        m.Method,
			EngineKind:    m.EngineKind,
			EngineVersion: m.EngineVersion,
			MeasuredAt:    m.MeasuredAt.UTC().Format(time.RFC3339Nano),
			PrefillTokps:  m.PrefillTokps,
			DepthTokens:   m.DepthTokens,
			TurnSeconds:   m.TurnSeconds,
		})
	}
	if len(out) == 0 {
		return nil
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ModelID != out[j].ModelID {
			return out[i].ModelID < out[j].ModelID
		}
		return out[i].VariantID < out[j].VariantID
	})
	return out
}

// measuredRatesFrom projects the persisted ledger onto the shape the
// ranking reads. Nil for a host that has measured nothing, which is
// what every fresh install reports and what disables the pass.
func measuredRatesFrom(st catalog.State) map[string]router.MeasuredRate {
	if len(st.MeasuredVariants) == 0 {
		return nil
	}
	rates := make(map[string]router.MeasuredRate, len(st.MeasuredVariants))
	for sha, m := range st.MeasuredVariants {
		rates[sha] = router.MeasuredRate{Tokps: m.MeasuredTokps, TurnSeconds: m.TurnSeconds}
	}
	return rates
}

// benchMeasurement turns a completed run into the ledger entry the ranking
// reads, and the VariantSHA key to file it under. An empty key means
// "record nothing" (waired-agent#784).
//
// An UNLABELLED result is refused here rather than trusted: this asks
// "which model does this number condemn", and a guess files a real
// measurement against a model that was never run. A failed run, a run with
// no finished figure (a lower bound is a verdict for now, not a stored
// speed), and a run whose variant is not in the catalog are refused for the
// same reason — VariantSHA is the key, and without it the entry could only
// be filed under a variant id, which collides across models.
//
// deps carries the engine release, the GPU and the serving configuration
// the figure was measured under, which the ledger keeps so it can answer
// for the same configuration later and for no other
// (storedSpeedMeasurement).
func benchMeasurement(bench BenchResult, manifests []catalog.Manifest, deps BenchDeps) (string, catalog.VariantMeasurement) {
	if bench.Failed || bench.TurnSeconds <= 0 {
		return "", catalog.VariantMeasurement{}
	}
	sha := activeVariantSHA(manifests, bench.ModelID, bench.VariantID)
	if sha == "" {
		return "", catalog.VariantMeasurement{}
	}
	return sha, catalog.VariantMeasurement{
		ModelID:       bench.ModelID,
		VariantID:     bench.VariantID,
		MeasuredTokps: bench.TokensPerSec,
		Method:        bench.Method,
		EngineKind:    deps.EngineKind,
		EngineVersion: deps.EngineVersion,
		MeasuredAt:    time.Now().UTC(),
		PrefillTokps:  bench.PrefillTokps,
		DepthTokens:   bench.DepthTokens,
		TurnSeconds:   bench.TurnSeconds,
		Samples:       bench.Samples,
		SpreadPct:     bench.SpreadPct,
		GPUModel:      deps.GPUModel,
		VRAMTotalMB:   deps.VRAMTotalMB,
		DriverVersion: deps.DriverVersion,
		AppliedWindow: deps.AppliedWindow,
		KVCacheType:   deps.KVCacheType,
		NumParallel:   deps.NumParallel,

		SpeculativeMethod: deps.SpeculativeMethod,
		SpeculativeTokens: deps.SpeculativeTokens,
	}
}

// speedVerdict is what one measurement says against the line, before
// anything is decided about it (decision 3 of docs/decisions/20260913/2245).
//
// Extracted so the "is this request over the line" fact can be reported
// even when there is no faster model to propose: a host already serving
// the fastest model Waired offers produces no recommendation, and the CLI
// must still be able to tell that apart from a comfortable one
// (waired-agent#784).
type speedVerdict struct {
	Over             bool
	TurnSeconds      float64
	TurnFloorSeconds float64
	Budget           float64
}

// speedVerdictOf judges a measurement: a finished figure over the line, or
// a lower bound already past it. It assumes the caller has rejected failed
// and skipped runs — it answers "how fast", not "was there a measurement".
func speedVerdictOf(bench BenchResult) speedVerdict {
	v := speedVerdict{
		TurnSeconds:      bench.TurnSeconds,
		TurnFloorSeconds: bench.TurnFloorSeconds,
		Budget:           hostfit.ModelTurnBudgetSeconds,
	}
	switch {
	case v.TurnSeconds > 0:
		v.Over = v.TurnSeconds > v.Budget
		v.TurnFloorSeconds = 0
	default:
		v.Over = v.TurnFloorSeconds > v.Budget
	}
	return v
}

// judged reports whether the measurement carries a figure the line can be
// held against at all.
func (v speedVerdict) judged() bool { return v.TurnSeconds > 0 || v.TurnFloorSeconds > 0 }

// stepDownTurnSpeedFor is the seconds-per-request lookup the step-down
// compares (router.PickInput.TurnSpeedFor). nil reads the shipped store,
// catalog.TurnSpeeds, whose lookup order is tested there; the package's
// tests seal it in TestMain so their fixture models have seconds.
var stepDownTurnSpeedFor func(catalog.Manifest, catalog.Variant) (float64, bool)

// recommendationFromBench holds a measurement against the line and, when
// over it, computes a single-step recommendation of a faster model (issue
// #133; router.FasterCandidate since waired-ai/waired-agent#1400; in seconds per request since waired-ai/waired-agent#1341). Returns
// nil when there is nothing to suggest:
//
//   - the benchmark failed (never nag on an unreliable run)
//   - the benchmark was skipped (no engine / external / port 0)
//   - the request is inside the line, or there is no figure to judge
//   - no active model is committed yet
//   - the engine pick or the faster-candidate search yields nothing
//
// When the user has already declined this exact (active variant → target)
// pairing, the recommendation is still returned but with Dismissed=true so
// the CLI/tray can stay quiet without re-deriving the decision.
func recommendationFromBench(
	bench BenchResult,
	store *catalog.Store,
	hw hardware.Profile,
	manifests []catalog.Manifest,
	cfg agentconfig.InferenceConfig,
	engineVersion string,
) *management.BenchmarkRecommendation {
	// Unreliable / skipped runs: Capacity==0 is the "skipped" encoding
	// (no engine, external endpoint, or engine off); a real measurement
	// clamps Capacity to >= 1.
	if bench.Failed || bench.Capacity == 0 {
		return nil
	}
	v := speedVerdictOf(bench)
	if !v.Over {
		return nil
	}
	st, err := store.Load()
	if err != nil || st.Active == nil {
		return nil
	}
	if !benchDescribes(bench, st.Active.ModelID) {
		return nil
	}

	// The engine the measurement was taken on: PickEngine's hardware
	// heuristic can disagree with the engine actually serving, and cfg.PreferredEngine is
	// empty on every wizard-installed host (waired-agent#1028). st.Active is
	// non-nil here — the guard above returned otherwise.
	engine := st.Active.Runtime
	if engine == "" {
		enginePick, err := router.PickEngine(router.EnginePickInput{
			Hardware:   hw,
			Preference: cfg.PreferredEngine,
			Catalog:    manifests,
		})
		if err != nil {
			return nil
		}
		engine = enginePick.Engine
	}

	// PreferredModelID is deliberately left empty so a pinned-but-too-heavy
	// model can still be stepped down across families — the whole point of
	// the recommendation is to override a pick that the host can't sustain.
	cand, ok := router.FasterCandidate(router.PickInput{
		Catalog:       manifests,
		Hardware:      hw,
		Engine:        engine,
		EngineVersion: engineVersion,
		// Do not offer a step-down onto a model this host has ALREADY
		// measured over the line. Without this, a host that walked
		// 9B -> 4B and measured the 4B slow too would be offered the 4B
		// again on its next benchmark, because the proposal only knew
		// the 4B was lighter, not that it had been tried
		// (waired-agent#784).
		Measured:          measuredRatesFrom(st),
		TurnBudgetSeconds: v.Budget,
		TurnSpeedFor:      stepDownTurnSpeedFor,
	}, st.Active.ModelID, st.Active.VariantID)
	if !ok {
		return nil
	}

	rec := &management.BenchmarkRecommendation{
		Direction:        management.RecommendationLighter,
		FromModelID:      st.Active.ModelID,
		FromVariantID:    st.Active.VariantID,
		ToModelID:        cand.Manifest.ModelID,
		ToVariantID:      cand.Variant.VariantID,
		MeasuredTokps:    bench.DecodeTokps,
		TurnSeconds:      v.TurnSeconds,
		TurnFloorSeconds: v.TurnFloorSeconds,
		BudgetSeconds:    v.Budget,
		Reason: fmt.Sprintf("one request takes %s on this host %s",
			notice.RequestSeconds(v.TurnSeconds, v.TurnFloorSeconds), notice.TargetClause(v.Budget)),
	}

	// Dismissed marker: keyed by the active variant's content digest so a
	// later switch (which changes the SHA) clears stale dismissals.
	if sha := activeVariantSHA(manifests, st.Active.ModelID, st.Active.VariantID); sha != "" {
		key := catalog.DismissalKey(sha, cand.Variant.VariantID)
		if _, dismissed := st.DismissedRecommendations[key]; dismissed {
			rec.Dismissed = true
		}
	}
	return rec
}

// benchJobTimeout bounds one detached measurement: a cold warm-up, the
// calibration, and two requests each allowed to run to the stall cap. It
// is the backstop for a job nothing else ends, not a verdict — the line is
// judged inside the measurement (decision 4 of docs/decisions/20260913/2245).
const benchJobTimeout = 2*modelSpeedStallCap + 10*time.Minute

// RunBenchmark measures the active model — or, with
// management.BenchmarkModeEnsure, answers from the stored measurement — and
// returns the result plus the faster-model recommendation when the request
// is over the line. ok is false (with a nil error) when the engine/model is
// not ready yet — the handler maps that to 425 so an installer flow can poll.
//
// The measurement itself runs as a single-flight job detached from ctx
// (waired#835 §12): if the caller times out or disconnects, the run
// completes anyway, is persisted (catalog.State.LastBenchmark), and is
// retrievable via BenchmarkStatus / GET /inference/benchmark/status.
// Concurrent calls join the in-flight run rather than starting a second
// engine-saturating measurement.
func (p *agentInferenceProvider) RunBenchmark(ctx context.Context, mode string) (management.BenchmarkOutcome, bool, error) {
	ready, _ := p.EngineReady()
	if !ready {
		return management.BenchmarkOutcome{}, false, nil
	}

	done := p.startBenchmarkJob(0, mode)
	select {
	case <-done:
	case <-ctx.Done():
		// The job keeps running detached; the result lands in
		// BenchmarkStatus once it completes.
		return management.BenchmarkOutcome{}, false, ctx.Err()
	}

	p.benchJobMu.Lock()
	defer p.benchJobMu.Unlock()
	if p.benchJobOutcome == nil {
		// Defensive: the job closed done without recording an outcome.
		return management.BenchmarkOutcome{}, false, nil
	}
	if p.benchJobOutcomeKind == benchOutcomeEngineNotReady {
		// The run this call started — or joined — stopped at the readiness
		// gate and never reached the engine. That is what ok=false means on
		// this interface, and the handler answers 425 "poll
		// /inference/status and retry", which is what the caller then does.
		//
		// Reported through BenchmarkOutcome.Failed it left by the 503
		// benchmark_did_not_complete door instead, which `waired init` reads
		// as a fault: exit 3, branched on by install.sh
		// (WAIRED_INIT_LOCAL_AI_DOWN) and install.ps1
		// ($WairedInitLocalAIDown). The gate fires on an engine that is
		// merely still coming up, so a slow-starting host was reported as a
		// failed install (#576).
		return management.BenchmarkOutcome{}, false, nil
	}
	return *p.benchJobOutcome, true, nil
}

// startBenchmarkJob starts the detached single-flight measurement under the
// given declarative generation (0 = not counter-driven) and mode, and
// returns a channel closed when it completes. If a run is already in flight
// its channel is returned instead (join semantics).
//
// A request that joins a run measuring the model this host still serves
// raises the generation the run will answer to its own, when higher: the
// run is the measurement that request asked for, and answering the lower
// generation made the setup reconciler start a second full measurement of
// the same model the moment the first finished (waired-agent#980). A run
// of another model — the switch landed mid-run — keeps its generation; the
// switch stops that run, and the request is answered by the next one.
func (p *agentInferenceProvider) startBenchmarkJob(gen int, mode string) <-chan struct{} {
	p.benchJobMu.Lock()
	defer p.benchJobMu.Unlock()
	if p.benchJobDone != nil {
		if p.benchJobJoined != nil {
			p.benchJobJoined()
		}
		if gen > p.benchJobGen && p.benchJobVariant == p.activeSelectionKey() {
			p.benchJobGen = gen
		}
		return p.benchJobDone
	}
	done := make(chan struct{})
	p.benchJobDone = done
	p.benchJobGen = gen
	p.benchJobVariant = p.activeSelectionKey()
	// A fresh run starts with no progress of its own; the previous run's
	// last sample must not be served as this one's first.
	p.benchJobProgress = nil
	go p.runBenchmarkJob(mode, done)
	return done
}

// runBenchmarkJob is the detached job body: measure (or answer from the
// stored measurement), derive the recommendation, persist the completion
// record and the ledger entry, publish the outcome, close done. Runs against
// its own bounded context — never a request's.
//
// Bounded by the DAEMON's lifetime as well as by benchJobTimeout: a job
// started from context.Background() went on measuring, and went on
// persisting its completion record with store.Update, after everything
// that could have told it to stop was gone. backgroundCtx falls back to
// context.Background() for the narrow providers that have no agent context.
func (p *agentInferenceProvider) runBenchmarkJob(mode string, done chan struct{}) {
	ctx, cancel := context.WithTimeout(p.backgroundCtx(), benchJobTimeout)
	defer cancel()
	// Deferred rather than the last statement it used to be: the guard
	// below returns early, and a joiner waiting on an unclosed channel
	// waits for the life of the process.
	defer close(done)

	// Asked ONCE, and used both to configure the run and to name what it
	// measured. Two reads could straddle an adoptEngine and file a figure
	// under an engine that did not produce it.
	deps := p.speedDeps(ctx, mode)
	// No engine on this host: there is nothing to measure, and — the part
	// that matters — nothing to RECORD (waired-agent#1206). A recorded
	// ending at the requested generation satisfies the setup reconciler's
	// guard forever, and the wizard then shows a finished speed check with
	// no figure in it.
	//
	// Not applied to the benchRun seam: a test that injects a result is
	// not asking about this host's engine.
	if p.benchRun == nil && (deps.EngineKind == signer.InferenceTypeNone || deps.EnginePort == 0) {
		p.finishBenchmarkJob(nil, "")
		return
	}

	hw := p.profiler.Profile(ctx)
	var bench BenchResult
	if p.benchRun != nil {
		bench = p.benchRun(ctx)
	} else {
		bench = RunBootBenchmark(ctx, deps)
	}
	// #203's node-rating path: a run that never reached the engine still
	// tells the mesh this host takes one request at a time (Capacity 1; 0
	// means UNLIMITED). Except when a verdict for the same variant is
	// already in hand — a measurement that gave the engine back to this
	// host's own traffic, or was declined for a moment, must not erase the
	// figure /healthz and the recommendation read.
	if bench.Outcome != benchOutcomeEngineNotReady || !p.holdsSpeedVerdictFor(bench.VariantID) {
		p.SetLastBench(bench)
	}
	if p.onSpeedVerdict != nil && benchReachedAVerdict(bench) {
		p.onSpeedVerdict(bench)
	}
	p.benchJobMu.Lock()
	b := bench
	p.benchJobBench = &b
	p.benchJobMu.Unlock()

	engineVersion := deps.EngineVersion
	v := speedVerdictOf(bench)
	outcome := management.BenchmarkOutcome{
		MeasuredTokps: bench.TokensPerSec,
		// Named from the same selection the run was configured from, so
		// the figure and the name cannot come from different models
		// (waired-agent#1027).
		ModelID: deps.ModelID,
		Lighter: recommendationFromBench(bench, p.store, hw, p.manifests, p.cfg, engineVersion),
		// Carried, not dropped: a failed run's outcome was the only place
		// these were lost — which is what let the handler answer 200 for a
		// run that failed (waired-agent#29).
		Failed: bench.Failed,
		Error:  bench.Err,
	}
	// The verdict travels whether or not there is a faster model to
	// propose. On a host already serving the fastest model Waired offers
	// there is nothing faster, so Lighter is nil — and without this the
	// caller read that absence as "fast enough" (waired-agent#784).
	if !bench.Failed && bench.Capacity > 0 && v.judged() {
		outcome.Speed = speedMeasurementOf(bench, v)
	}

	// A run that stopped at the readiness gate never reached the engine, so
	// it is not a result and does not become the completed one (#576):
	// recording it replaced this host's last real measurement with a
	// failure, and satisfied the setup reconciler's retry guard for a host
	// whose engine came up seconds later.
	record := catalog.BenchmarkRecord{
		MeasuredTokps:    bench.TokensPerSec,
		ModelID:          bench.ModelID,
		VariantID:        bench.VariantID,
		Method:           bench.Method,
		SpreadPct:        bench.SpreadPct,
		Trials:           bench.Samples,
		Failed:           bench.Failed,
		Error:            bench.Err,
		Outcome:          bench.Outcome,
		MeasuredAt:       time.Now().UTC(),
		PrefillTokps:     bench.PrefillTokps,
		DecodeTokps:      bench.DecodeTokps,
		DepthTokens:      bench.DepthTokens,
		TurnSeconds:      bench.TurnSeconds,
		TurnFloorSeconds: bench.TurnFloorSeconds,
		OverBudget:       v.Over,
		Cached:           bench.Cached,
	}
	ranAtAll := bench.Outcome != benchOutcomeEngineNotReady
	// Filed under the variant it measured, from the run's own identity
	// fields, so a run that finishes after a switch files its figure under
	// the model it actually measured. A cached answer is not re-filed: it
	// was filed when it was measured, with that date.
	measuredSHA, measurement := "", catalog.VariantMeasurement{}
	if !bench.Cached {
		measuredSHA, measurement = benchMeasurement(bench, p.manifests, deps)
	}
	if ranAtAll {
		if err := p.store.Update(func(s *catalog.State) {
			p.benchJobMu.Lock()
			record.Gen = p.benchJobGen
			p.benchJobMu.Unlock()
			// A gen-0 (the daemon's own loop, or the CLI) run must not
			// regress a counter-driven generation the CP already saw —
			// keep the stored gen then.
			if record.Gen == 0 && s.LastBenchmark != nil && s.LastBenchmark.Gen > 0 {
				record.Gen = s.LastBenchmark.Gen
			}
			s.LastBenchmark = &record
			if measuredSHA != "" {
				if s.MeasuredVariants == nil {
					s.MeasuredVariants = map[string]catalog.VariantMeasurement{}
				}
				s.MeasuredVariants[measuredSHA] = measurement
			}
		}); err != nil {
			p.logger.Warn("benchmark: persist completion record", "err", err)
		}
	}
	if ranAtAll {
		p.finishBenchmarkJob(&outcome, bench.Outcome, &record)
	} else {
		p.finishBenchmarkJob(&outcome, bench.Outcome)
	}
}

// holdsSpeedVerdictFor reports whether the last recorded result is a
// verdict about variant.
func (p *agentInferenceProvider) holdsSpeedVerdictFor(variant string) bool {
	p.benchMu.Lock()
	defer p.benchMu.Unlock()
	return p.lastBench != nil && benchReachedAVerdict(*p.lastBench) && p.lastBench.VariantID == variant
}

// finishBenchmarkJob publishes a finished job's outcome and clears the
// in-flight state. record is recorded only for a run that reached the
// engine.
func (p *agentInferenceProvider) finishBenchmarkJob(outcome *management.BenchmarkOutcome, kind string, record ...*catalog.BenchmarkRecord) {
	p.benchJobMu.Lock()
	defer p.benchJobMu.Unlock()
	if outcome != nil {
		p.benchJobOutcome = outcome
		p.benchJobOutcomeKind = kind
	}
	if len(record) > 0 && record[0] != nil {
		p.benchJobResult = record[0]
	}
	p.benchJobDone = nil
	// The run is over; its live progress would otherwise be reported
	// forever beside a finished result.
	p.benchJobProgress = nil
}

// speedMeasurementOf is a finished measurement in wire form.
func speedMeasurementOf(bench BenchResult, v speedVerdict) management.SpeedMeasurement {
	return management.SpeedMeasurement{
		TurnSeconds:      v.TurnSeconds,
		TurnFloorSeconds: v.TurnFloorSeconds,
		BudgetSeconds:    v.Budget,
		OverBudget:       v.Over,
		PrefillTokps:     bench.PrefillTokps,
		DecodeTokps:      bench.DecodeTokps,
		DepthTokens:      bench.DepthTokens,
		Cached:           bench.Cached,
	}
}

// publishBenchProgress records one in-flight measurement report for
// /benchmark/status to serve (waired-agent#199).
func (p *agentInferenceProvider) publishBenchProgress(bp BenchProgress) {
	p.benchJobMu.Lock()
	p.benchJobProgress = &bp
	p.benchJobMu.Unlock()
}

// MeasuredRates reports the persisted per-variant measurements and the
// line this host judges them against (waired-agent#784; seconds since
// waired-ai/waired-agent#1341).
//
// Read from the store on every call rather than cached: a measurement
// that finishes between two catalog polls has to move the badge, and the
// store is already the single writer of that record.
func (p *agentInferenceProvider) MeasuredRates() (map[string]router.MeasuredRate, float64) {
	st, err := p.store.Load()
	if err != nil {
		return nil, 0
	}
	rates := measuredRatesFrom(st)
	if rates == nil {
		return nil, 0
	}
	return rates, hostfit.ModelTurnBudgetSeconds
}

// benchmarkFigure is the part of a benchmark status that is a claim
// about a MODEL rather than about a run: a throughput, and what it was
// measured on.
type benchmarkFigure struct {
	ModelID    string
	Tokps      float64
	MeasuredAt time.Time
	Method     string
	SpreadPct  float64
	Trials     int

	// The served-model measurement behind the figure
	// (waired-ai/waired-agent#1341).
	PrefillTokps     float64
	DepthTokens      int
	TurnSeconds      float64
	TurnFloorSeconds float64
	Cached           bool
}

// servedModelFigure picks the throughput BenchmarkStatus may report, and
// the model it describes. false means there is nothing to report about
// what this host serves — which the wire renders as absent, not as zero.
//
// The rule is one sentence: report the figure filed against the model
// this host is serving. Before waired-agent#971 there was no such rule
// because there was no subject — catalog.BenchmarkRecord identified a run
// by the generation it was requested under and by nothing else — so the
// number outlived the model it described, and the setup wizard renders it
// directly above the button that changes the model.
//
// Four arms, in this order, each answering a different situation:
//
//   - The record names the model being served. Everything it carries is
//     coherent, including the run-shaped fields (spread, trials) the
//     ledger does not keep, so it is used whole.
//   - It does not, but the ledger has timed the served model in some
//     earlier run. That is a real measurement OF THE RIGHT MODEL; only
//     the run-shaped detail is missing, so it is left out rather than
//     borrowed from a different run.
//   - The record is UNLABELLED — written by a build predating the field.
//     Kept, for the reason benchDescribes keeps one: an unlabelled figure
//     is the behaviour those hosts already had, and withholding it would
//     make an upgrade look like a regression.
//   - The record names a DIFFERENT model and nothing has timed this one.
//     Nothing honest can be said, so nothing is.
//
// A failed run is not a measurement and never yields a figure — attaching
// the ledger's number to it would report a speed for a run that reported
// an error.
func servedModelFigure(rec catalog.BenchmarkRecord, st catalog.State, active string) (benchmarkFigure, bool) {
	if rec.Failed {
		return benchmarkFigure{}, false
	}
	whole := benchmarkFigure{
		ModelID:          rec.ModelID,
		Tokps:            rec.MeasuredTokps,
		MeasuredAt:       rec.MeasuredAt,
		Method:           rec.Method,
		SpreadPct:        rec.SpreadPct,
		Trials:           rec.Trials,
		PrefillTokps:     rec.PrefillTokps,
		DepthTokens:      rec.DepthTokens,
		TurnSeconds:      rec.TurnSeconds,
		TurnFloorSeconds: rec.TurnFloorSeconds,
		Cached:           rec.Cached,
	}
	if rec.ModelID != "" && rec.ModelID == active {
		return whole, true
	}
	if m, ok := measurementOfModel(st, active); ok {
		return benchmarkFigure{
			ModelID:      m.ModelID,
			Tokps:        m.MeasuredTokps,
			MeasuredAt:   m.MeasuredAt,
			Method:       m.Method,
			PrefillTokps: m.PrefillTokps,
			DepthTokens:  m.DepthTokens,
			TurnSeconds:  m.TurnSeconds,
		}, true
	}
	if rec.ModelID == "" {
		return whole, true
	}
	return benchmarkFigure{}, false
}

// measurementOfModel finds the ledger entry for modelID. The ledger is
// keyed by catalog.VariantSHA — a model can have several variants and
// each is timed separately — so this is a scan, and the newest entry
// wins when a host has measured more than one variant of one model.
func measurementOfModel(st catalog.State, modelID string) (catalog.VariantMeasurement, bool) {
	if modelID == "" {
		return catalog.VariantMeasurement{}, false
	}
	var best catalog.VariantMeasurement
	found := false
	for _, m := range st.MeasuredVariants {
		if m.ModelID != modelID || m.MeasuredTokps <= 0 {
			continue
		}
		if !found || m.MeasuredAt.After(best.MeasuredAt) {
			best, found = m, true
		}
	}
	return best, found
}

// activeModelIDOf is activeModelID against a state already in hand, so a
// caller that needs the served model AND something else out of the same
// state reads it once.
func activeModelIDOf(st catalog.State) string {
	if st.Active == nil {
		return ""
	}
	return st.Active.ModelID
}

// BenchmarkStatus reports the job's current state for
// GET /waired/v1/inference/benchmark/status (waired#835 §12). Falls
// back to the persisted completion record after a restart.
func (p *agentInferenceProvider) BenchmarkStatus() management.BenchmarkStatusResponse {
	p.benchJobMu.Lock()
	running := p.benchJobDone != nil
	last := p.benchJobResult
	live := p.benchJobProgress
	p.benchJobMu.Unlock()

	// ONE snapshot for both halves of the answer. The served model and
	// the ledger it is looked up in have to be read together, or the
	// figure can be paired with a selection it does not belong to — the
	// exact confusion this is here to end (waired-agent#971).
	st, stErr := p.store.Load()
	if last == nil && stErr == nil && st.LastBenchmark != nil {
		// Nothing completed this process lifetime — consult the
		// persisted record (survives restarts).
		rec := *st.LastBenchmark
		last = &rec
	}

	resp := management.BenchmarkStatusResponse{State: management.BenchmarkStateIdle}
	if last != nil {
		resp.State = management.BenchmarkStateDone
		if last.Failed {
			resp.State = management.BenchmarkStateFailed
			resp.Error = last.Error
		}
		// Gen and Outcome describe the RUN and are reported whatever the
		// figure below turns out to be. Gen especially: the setup
		// reconciler's re-run guard is `bs.Gen < d.benchmarkGen`
		// (setup_desired.go), so moving it here would either re-run a
		// measurement that already answered or stop one that never did.
		resp.Gen = last.Gen
		resp.Outcome = last.Outcome
		if fig, ok := servedModelFigure(*last, st, activeModelIDOf(st)); ok {
			resp.ModelID = fig.ModelID
			resp.MeasuredTokps = fig.Tokps
			resp.MeasuredAt = fig.MeasuredAt.Format(time.RFC3339)
			resp.Method = fig.Method
			resp.SpreadPct = fig.SpreadPct
			resp.Trials = fig.Trials
			v := speedVerdictOf(BenchResult{TurnSeconds: fig.TurnSeconds, TurnFloorSeconds: fig.TurnFloorSeconds})
			if v.judged() {
				resp.SpeedMeasurement = management.SpeedMeasurement{
					TurnSeconds:      v.TurnSeconds,
					TurnFloorSeconds: v.TurnFloorSeconds,
					BudgetSeconds:    v.Budget,
					OverBudget:       v.Over,
					PrefillTokps:     fig.PrefillTokps,
					DecodeTokps:      fig.Tokps,
					DepthTokens:      fig.DepthTokens,
					Cached:           fig.Cached,
				}
				if v.Over {
					resp.Recommendation = p.currentRecommendation(context.Background())
				}
			}
		}
	} else if boot := p.bootBenchFailure(); boot != nil {
		// The boot benchmark reached no surface at all: it warn-logged
		// and returned. It does not persist a record, does not move this
		// status, and does not appear in SetupProgress — so the failure
		// waired-agent#203 actually reported, an engine install that
		// left nothing listening, was observable only by reading the
		// daemon log (#203 proposal 2).
		//
		// Reported, never persisted. Writing a gen-0 boot failure into
		// catalog.State would overwrite a good higher-generation record
		// and — because a gen-0 write keeps the stored generation — make
		// the wizard show THAT generation as failed. This fills the gap
		// only while there is nothing else to report, which is exactly
		// the case that was silent.
		resp.State = management.BenchmarkStateFailed
		resp.Error = boot.Err
		resp.Outcome = boot.Outcome
	} else if boot := p.bootBenchWaiting(); boot != nil {
		// State stays idle — see bootBenchWaiting on why neither of the
		// other two words is true here. The reason is what was missing:
		// a host still waiting for its engine used to answer exactly
		// what a host nobody has asked answers (waired-agent#1150).
		resp.Error = boot.Err
		resp.Outcome = boot.Outcome
	}
	if running {
		resp.State = management.BenchmarkStateRunning
		// A run in flight replaces the previous run's summary figures
		// with its own progress. Leaving the old ones would present a
		// finished measurement as if it belonged to the one now running,
		// which is exactly the "stale result read as current" confusion
		// the generation counter exists to prevent.
		resp.MeasuredTokps = 0
		resp.MeasuredAt = ""
		resp.SpreadPct = 0
		resp.Method = ""
		resp.Trials = 0
		resp.SpeedMeasurement = management.SpeedMeasurement{}
		resp.Recommendation = nil
		if live != nil {
			resp.Phase = live.Phase
			resp.Trial = live.Trial
			resp.Trials = live.Trials
			resp.SampleTokps = live.SampleTokps
			resp.MedianTokps = live.MedianTokps
			resp.SpreadPct = live.SpreadPct
			resp.Method = live.Method
			resp.SpeedMeasurement = management.SpeedMeasurement{
				ElapsedSeconds:   live.ElapsedSeconds,
				BudgetSeconds:    live.BudgetSeconds,
				OverBudget:       live.OverBudget,
				TurnFloorSeconds: live.TurnFloorSeconds,
				DepthTokens:      live.DepthTokens,
			}
			// Past the line the switch is offered now, while the
			// measurement goes on (decision 4 of
			// docs/decisions/20260913/2245): the faster model for a
			// request already known to take at least this long.
			if live.OverBudget {
				resp.Recommendation = p.recommendationForRunningBound(live.TurnFloorSeconds)
			}
		}
	}
	return resp
}

// recommendationForRunningBound is the faster-model recommendation for the
// model being measured, judged on the lower bound a running measurement has
// already passed.
func (p *agentInferenceProvider) recommendationForRunningBound(floor float64) *management.BenchmarkRecommendation {
	if p == nil || floor <= 0 {
		return nil
	}
	ctx := context.Background()
	bound := BenchResult{
		TurnFloorSeconds: floor,
		Capacity:         unmeasuredCapacity,
		ModelID:          p.activeModelID(),
		VariantID:        p.activeVariantID(),
	}
	return recommendationFromBench(bound, p.store, p.profiler.Profile(ctx), p.manifests, p.cfg, p.servingEngineVersion(ctx))
}

// modelSpeedStatus is InferenceStatus.ModelSpeed: the measurement running
// now, or the stored one of the model this host serves.
func (p *agentInferenceProvider) modelSpeedStatus() *management.ModelSpeedStatus {
	if p == nil {
		return nil
	}
	p.benchJobMu.Lock()
	running := p.benchJobDone != nil
	live := p.benchJobProgress
	variant := p.benchJobVariant
	p.benchJobMu.Unlock()
	active := p.activeModelID()
	if running && live != nil && variant == p.activeSelectionKey() && live.Phase == benchPhaseMeasuring {
		return &management.ModelSpeedStatus{
			ModelID:   active,
			VariantID: variant,
			Running:   true,
			SpeedMeasurement: management.SpeedMeasurement{
				ElapsedSeconds:   live.ElapsedSeconds,
				BudgetSeconds:    live.BudgetSeconds,
				OverBudget:       live.OverBudget,
				TurnFloorSeconds: live.TurnFloorSeconds,
				DepthTokens:      live.DepthTokens,
			},
		}
	}
	b, at, ok := p.servedSpeed()
	if !ok {
		return nil
	}
	v := speedVerdictOf(b)
	out := &management.ModelSpeedStatus{
		ModelID:          active,
		VariantID:        b.VariantID,
		SpeedMeasurement: speedMeasurementOf(b, v),
	}
	if !at.IsZero() {
		out.MeasuredAt = at.UTC().Format(time.RFC3339)
	}
	return out
}

// DismissRecommendation records that the user declined a model-switch
// suggestion (either direction) so a re-benchmark of the same pairing
// stays quiet. Keyed by the active variant's content digest + the
// target variant ID. Empty toVariantID resolves the current live
// recommendation's target (faster first, then upgrade — at most one
// is ever live); when there is no current recommendation (or no active
// model) this is a no-op. The fromVariantID argument is advisory (the
// active variant is authoritative).
func (p *agentInferenceProvider) DismissRecommendation(_ /*fromVariantID*/, toVariantID string) error {
	st, err := p.store.Load()
	if err != nil {
		return err
	}
	if st.Active == nil {
		return nil
	}
	to := toVariantID
	if to == "" {
		rec := p.currentRecommendation(context.Background())
		if rec == nil || rec.ToVariantID == "" {
			return nil // nothing to dismiss
		}
		to = rec.ToVariantID
	}
	sha := activeVariantSHA(p.manifests, st.Active.ModelID, st.Active.VariantID)
	if sha == "" {
		// Fall back to the variant ID so the dismissal still sticks for
		// this active selection (a switch changes the ID and clears it).
		sha = st.Active.VariantID
	}
	key := catalog.DismissalKey(sha, to)
	if err := p.store.Update(func(s *catalog.State) {
		if s.DismissedRecommendations == nil {
			s.DismissedRecommendations = map[string]time.Time{}
		}
		s.DismissedRecommendations[key] = time.Now().UTC()
	}); err != nil {
		return err
	}
	// Take the notice down now rather than at the next heartbeat. The
	// person just answered this suggestion; watching the row they
	// declined sit there for another fifteen seconds reads as the
	// answer not having registered (waired-agent#1205).
	p.publishRecommendationNotices(context.Background())
	return nil
}

// activeVariantSHA resolves catalog.VariantSHA for (modelID, variantID)
// from the bundled manifests. Empty when the variant is not found — which
// disables the dismissal marker for that run rather than colliding on a
// degenerate key.
func activeVariantSHA(manifests []catalog.Manifest, modelID, variantID string) string {
	for _, m := range manifests {
		if m.ModelID != modelID {
			continue
		}
		for _, v := range m.Variants {
			if v.VariantID == variantID {
				return catalog.VariantSHA(v)
			}
		}
	}
	return ""
}
