package main

import (
	"context"
	"fmt"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/router"
)

// SetLastBench records the most recent boot/explicit benchmark result so
// Status() and the catalog endpoint can derive the #133 lighter-model
// recommendation. Called from the probe goroutine in main.go after
// RunBootBenchmark and from RunBenchmark.
func (p *agentInferenceProvider) SetLastBench(b BenchResult) {
	p.benchMu.Lock()
	defer p.benchMu.Unlock()
	bc := b
	p.lastBench = &bc
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

// SetLastDepthBench records the most recent depth-aware long-context
// sweep (#624). Called from the background depth goroutine in main.go;
// read by Status() and the recommendation derivation.
func (p *agentInferenceProvider) SetLastDepthBench(d DepthBenchResult) {
	p.benchMu.Lock()
	defer p.benchMu.Unlock()
	dc := d
	p.lastDepthBench = &dc
}

// currentRecommendations derives the live recommendations from the
// last benchmark result: lighter when it measured below the
// interactive floor, upgrade when it cleared the floor with enough
// headroom for a higher tier. At most one of the two is non-nil. Safe
// to call with no benchmark recorded yet (both nil).
func (p *agentInferenceProvider) currentRecommendations(ctx context.Context) (lighter, upgrade *management.BenchmarkRecommendation) {
	p.benchMu.Lock()
	last := p.lastBench
	depth := p.lastDepthBench
	p.benchMu.Unlock()
	if last == nil {
		return nil, nil
	}
	hw := p.profiler.Profile(ctx)
	engineVersion := p.ollamaEngineVersion(ctx)
	return recommendationFromBench(*last, depth, p.store, hw, p.manifests, p.cfg, engineVersion),
		upgradeFromBench(*last, p.store, hw, p.manifests, p.cfg, engineVersion)
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
// measuredRatesFrom projects the persisted ledger onto the shape the
// ranking reads. Nil for a host that has measured nothing, which is
// what every fresh install reports and what disables the pass.
func measuredRatesFrom(st catalog.State) map[string]router.MeasuredRate {
	if len(st.MeasuredVariants) == 0 {
		return nil
	}
	rates := make(map[string]router.MeasuredRate, len(st.MeasuredVariants))
	for sha, m := range st.MeasuredVariants {
		rates[sha] = router.MeasuredRate{Tokps: m.MeasuredTokps}
	}
	return rates
}

func benchMeasurement(
	bench BenchResult, manifests []catalog.Manifest, engineKind, engineVersion string,
) (string, catalog.VariantMeasurement) {
	if bench.Failed || bench.TokensPerSec <= 0 {
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
		EngineKind:    engineKind,
		EngineVersion: engineVersion,
		MeasuredAt:    time.Now().UTC(),
	}
}

// floorVerdict is what one benchmark result says against this host's
// interactive floor, before anything is decided about it.
//
// Extracted so the "is this host below its floor" fact can be reported
// even when there is no lighter model to propose. It used to live inside
// recommendationFromBench, which returns nil in that case — so a host
// already serving the smallest model Waired offers produced a
// below-floor measurement and no recommendation, and the CLI could not
// tell that apart from a comfortable one (waired-agent#784).
type floorVerdict struct {
	Below bool
	Floor float64
	// Measured is the figure the verdict rests on: the shallow rate, or
	// the worst completed depth rate when that is lower.
	Measured    float64
	DepthReason string
}

// interactiveFloorVerdict compares a completed benchmark against the
// floor this host judges itself by. It assumes the caller has already
// rejected failed and skipped runs — it answers "how fast", not
// "was there a measurement".
func interactiveFloorVerdict(
	bench BenchResult, depth *DepthBenchResult, cfg agentconfig.InferenceConfig,
) floorVerdict {
	v := floorVerdict{
		Floor:    resolveInteractiveFloor(cfg.InteractiveFloorTokps),
		Measured: bench.TokensPerSec,
	}
	v.Below = v.Measured < v.Floor
	// #624: a host can decode fine at an empty context and still crawl
	// at depth (intentional spill, KV pressure) — so the depth sweep
	// participates in the comparison. The shallow floor already prices
	// in the expected long-context degradation (#670: 100 shallow was
	// chosen to keep ~80 at depth), so the depth leg is held to
	// floor × CodingAgentDepthFloorFraction rather than the full floor
	// — demanding 100 at 200k depth would double-count the degradation
	// and nag on essentially every host.
	if dec, target, ok := worstCompletedDepthDecode(depth); ok &&
		dec < v.Floor*router.CodingAgentDepthFloorFraction {
		v.Below = true
		if dec < v.Measured {
			v.Measured = dec
		}
		v.DepthReason = fmt.Sprintf(
			" (decode at ~%dk context measured %.0f tok/s, below the %.0f tok/s depth floor)",
			target/1024, dec, v.Floor*router.CodingAgentDepthFloorFraction)
	}
	return v
}

// recommendationFromBench compares a benchmark result against the
// interactive floor and, if below, computes a single-step-down lighter
// model recommendation (issue #133). Returns nil when there is nothing
// to suggest:
//
//   - the benchmark failed or timed out (never nag on an unreliable run)
//   - the benchmark was skipped (no engine / external / port 0)
//   - measured throughput is at or above the floor
//   - no active model is committed yet
//   - the engine pick or lighter-candidate search yields nothing
//
// When the user has already declined this exact (active variant → target)
// pairing, the recommendation is still returned but with Dismissed=true so
// the CLI/tray can stay quiet without re-deriving the decision.
func recommendationFromBench(
	bench BenchResult,
	depth *DepthBenchResult,
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
	v := interactiveFloorVerdict(bench, depth, cfg)
	floor, measured, depthReason := v.Floor, v.Measured, v.DepthReason
	if !v.Below {
		return nil
	}

	st, err := store.Load()
	if err != nil || st.Active == nil {
		return nil
	}
	if !benchDescribes(bench, st.Active.ModelID) {
		return nil
	}

	enginePick, err := router.PickEngine(router.EnginePickInput{
		Hardware:   hw,
		Preference: cfg.PreferredEngine,
		Catalog:    manifests,
	})
	if err != nil {
		return nil
	}

	// PreferredModelID is deliberately left empty so a pinned-but-too-heavy
	// model can still be stepped down across families — the whole point of
	// the recommendation is to override a pick that the host can't sustain.
	cand, ok := router.LighterCandidate(router.PickInput{
		Catalog:       manifests,
		Hardware:      hw,
		Engine:        enginePick.Engine,
		EngineVersion: engineVersion,
		// Do not offer a step-down onto a model this host has ALREADY
		// measured below the floor. Without this, a host that walked
		// 9B -> 4B and measured the 4B slow too would be offered the 4B
		// again on its next benchmark, because the proposal only knew
		// the 4B was lighter, not that it had been tried
		// (waired-agent#784).
		Measured:   measuredRatesFrom(st),
		FloorTokps: floor,
	}, st.Active.ModelID, st.Active.VariantID)
	if !ok {
		return nil
	}

	rec := &management.BenchmarkRecommendation{
		Direction:     management.RecommendationLighter,
		FromModelID:   st.Active.ModelID,
		FromVariantID: st.Active.VariantID,
		ToModelID:     cand.Manifest.ModelID,
		ToVariantID:   cand.Variant.VariantID,
		MeasuredTokps: measured,
		FloorTokps:    floor,
		Reason: fmt.Sprintf("measured %.0f tok/s is below the %.0f tok/s interactive floor on this host%s",
			measured, floor, depthReason),
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

// upgradeFromBench is the inverse of recommendationFromBench: when a
// reliable benchmark measured AT/ABOVE the interactive floor, it asks
// router.UpgradeCandidate whether a higher-quality_tier model is
// predicted (bandwidth scaling, safety margin) to still clear the
// floor on this host, and surfaces it as a Direction="upgrade"
// recommendation. nil when:
//
//   - the benchmark failed / was skipped (same reliability gates as
//     the lighter flow)
//   - measured throughput is below the floor (the lighter flow owns it)
//   - no active model is committed yet
//   - no fitting higher-tier candidate clears floor × margin
//
// Dismissals share the lighter flow's keying (active variant SHA →
// target variant ID): direction never collides because a given target
// variant is either heavier or lighter than the active one, and
// switching the active model changes the SHA, clearing stale entries.
func upgradeFromBench(
	bench BenchResult,
	store *catalog.Store,
	hw hardware.Profile,
	manifests []catalog.Manifest,
	cfg agentconfig.InferenceConfig,
	engineVersion string,
) *management.BenchmarkRecommendation {
	if bench.Failed || bench.Capacity == 0 {
		return nil
	}
	floor := resolveInteractiveFloor(cfg.InteractiveFloorTokps)
	if bench.TokensPerSec < floor {
		return nil
	}

	st, err := store.Load()
	if err != nil || st.Active == nil {
		return nil
	}
	if !benchDescribes(bench, st.Active.ModelID) {
		return nil
	}

	// Candidates must come from the engine the measurement was taken
	// on (Active.Runtime) — the bandwidth scaling is only meaningful
	// within one runtime, and PickEngine's hardware heuristic can
	// disagree with the engine actually serving (NVIDIA hosts lean
	// vllm there while the agent runs ollama).
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

	// PreferredModelID is deliberately left empty: the upgrade looks
	// across families for the best model the host can actually sustain.
	cand, predicted, ok := router.UpgradeCandidate(router.UpgradeInput{
		Pick: router.PickInput{
			Catalog:       manifests,
			Hardware:      hw,
			Engine:        engine,
			EngineVersion: engineVersion,
			// An upgrade onto a model this host has already measured
			// below the floor would walk it straight back into the
			// step-down it just came out of. The prediction below scales
			// the measured rate by weight; a real figure for those exact
			// weights beats it (waired-agent#784).
			Measured:   measuredRatesFrom(st),
			FloorTokps: floor,
		},
		ActiveModelID:   st.Active.ModelID,
		ActiveVariantID: st.Active.VariantID,
		MeasuredTokps:   bench.TokensPerSec,
		FloorTokps:      floor,
	})
	if !ok {
		return nil
	}

	rec := &management.BenchmarkRecommendation{
		Direction:      management.RecommendationUpgrade,
		FromModelID:    st.Active.ModelID,
		FromVariantID:  st.Active.VariantID,
		ToModelID:      cand.Manifest.ModelID,
		ToVariantID:    cand.Variant.VariantID,
		MeasuredTokps:  bench.TokensPerSec,
		FloorTokps:     floor,
		PredictedTokps: predicted,
		Reason: fmt.Sprintf("measured %.0f tok/s leaves headroom above the %.0f tok/s floor; %s is predicted to run at ~%.0f tok/s here",
			bench.TokensPerSec, floor, cand.Manifest.ModelID, predicted),
	}

	if sha := activeVariantSHA(manifests, st.Active.ModelID, st.Active.VariantID); sha != "" {
		key := catalog.DismissalKey(sha, cand.Variant.VariantID)
		if _, dismissed := st.DismissedRecommendations[key]; dismissed {
			rec.Dismissed = true
		}
	}
	return rec
}

// benchJobTimeout bounds one detached benchmark run: warm-up is capped
// at 180s and the measurement budget at 120s (inference_bench.go), so
// 10 minutes covers the theoretical worst case with generous slack for
// engine restarts mid-run.
const benchJobTimeout = 10 * time.Minute

// RunBenchmark forces a fresh on-device throughput benchmark of the
// active model and returns the measurement plus the resulting
// recommendation: lighter when below the interactive floor, upgrade
// when there is headroom for a higher tier (mutually exclusive). ok is
// false (with a nil error) when the engine/model is not ready yet —
// the handler maps that to 425 so an installer flow can poll.
//
// The measurement itself runs as a single-flight job detached from ctx
// (waired#835 §12): if the caller times out or disconnects, the run
// completes anyway, is persisted (catalog.State.LastBenchmark), and is
// retrievable via BenchmarkStatus / GET /inference/benchmark/status.
// Concurrent calls join the in-flight run rather than starting a
// second engine-saturating measurement.
//
// Unlike the boot benchmark, this bypasses the on-disk cache (Cache nil)
// so an explicit re-run always re-measures — the user asked for a fresh
// number.
func (p *agentInferenceProvider) RunBenchmark(ctx context.Context) (management.BenchmarkOutcome, bool, error) {
	ready, _ := p.EngineReady()
	if !ready {
		return management.BenchmarkOutcome{}, false, nil
	}

	done := p.startBenchmarkJob(0)
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
		// failed install (#576). The front EngineReady gate above does not
		// catch it: the job's own gate is checked later, and startBenchmarkJob
		// can join a run that failed it before this call arrived.
		return management.BenchmarkOutcome{}, false, nil
	}
	return *p.benchJobOutcome, true, nil
}

// startBenchmarkJob starts the detached single-flight benchmark run
// under the given declarative generation (0 = not counter-driven) and
// returns a channel closed when it completes. If a run is already in
// flight its channel is returned instead (join semantics).
func (p *agentInferenceProvider) startBenchmarkJob(gen int) <-chan struct{} {
	p.benchJobMu.Lock()
	defer p.benchJobMu.Unlock()
	if p.benchJobDone != nil {
		if p.benchJobJoined != nil {
			p.benchJobJoined()
		}
		return p.benchJobDone
	}
	done := make(chan struct{})
	p.benchJobDone = done
	// A fresh run starts with no progress of its own; the previous run's
	// last sample must not be served as this one's first.
	p.benchJobProgress = nil
	go p.runBenchmarkJob(gen, done)
	return done
}

// runBenchmarkJob is the detached job body: measure, derive
// recommendations, persist the completion record, publish the outcome,
// close done. Runs against its own bounded context — never a request's.
func (p *agentInferenceProvider) runBenchmarkJob(gen int, done chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), benchJobTimeout)
	defer cancel()

	hw := p.profiler.Profile(ctx)
	var bench BenchResult
	if p.benchRun != nil {
		bench = p.benchRun(ctx)
	} else {
		engineKind, enginePort := probeTargetForActive(p.cfg)
		var firstGPU hardware.GPU
		if len(hw.GPUs) > 0 {
			firstGPU = hw.GPUs[0]
		}
		bench = RunBootBenchmark(ctx, BenchDeps{
			EngineKind: engineKind,
			EnginePort: enginePort,
			// RunBenchmark above already gates on EngineReady and answers
			// 425, but startSetupBenchmark -> startBenchmarkJob does not:
			// a control-plane-requested benchmark on a not-ready engine
			// used to de-rate the node exactly the way #203 describes.
			EngineReady: p.EngineReady,
			// #582/#601: ready is not enough. This is the path `waired init`
			// reaches (POST /inference/benchmark, or a join of the run one
			// already started), and a fresh install reaches it with the
			// host-speed probe's download still in flight — whose completion
			// reconcile then restarts the engine under the warm-up.
			EngineQuiet: p.engineQuietForBench,
			// And HOLD it. EngineQuiet is answered once, at the top; this
			// runs for minutes, and the install-time host-speed measurement
			// starts from a background goroutine that can land anywhere in
			// them (waired-agent#703).
			EngineClaim:   p.claimEngineForBench,
			EngineGen:     p.engineProcessGen,
			EngineModel:   engineModelForActive(p.cfg),
			VariantID:     variantIDForActive(),
			ModelID:       modelIDForActive(),
			GPUModel:      firstGPU.Model,
			VRAMTotalMB:   firstGPU.VRAMTotalMB,
			DriverVersion: firstGPU.DriverVersion,
			Logger:        p.logger,
			// Republish each sample so /benchmark/status — and through it
			// the setup wizard — can show a real measurement in place of a
			// two-minute spinner (waired-agent#199).
			Progress: p.publishBenchProgress,
		})
	}
	p.SetLastBench(bench)

	engineVersion := p.ollamaEngineVersion(ctx)
	p.benchMu.Lock()
	depth := p.lastDepthBench
	p.benchMu.Unlock()
	outcome := management.BenchmarkOutcome{
		MeasuredTokps: bench.TokensPerSec,
		Lighter:       recommendationFromBench(bench, depth, p.store, hw, p.manifests, p.cfg, engineVersion),
		Upgrade:       upgradeFromBench(bench, p.store, hw, p.manifests, p.cfg, engineVersion),
		// Carried, not dropped: the BenchmarkRecord below has recorded these
		// two fields all along, and the outcome was the only place they were
		// lost — which is what let the handler answer 200 for a run that
		// failed (waired-agent#29).
		Failed: bench.Failed,
		Error:  bench.Err,
	}
	// The speed verdict travels whether or not there is a lighter model
	// to propose. On a host already serving the smallest model Waired
	// offers there is nothing lighter, so Lighter is nil — and without
	// this the caller read that absence as "fast enough" and said so
	// over a rate this run had just judged too slow (waired-agent#784).
	//
	// Gated on the same two conditions recommendationFromBench gates on:
	// a failed run and a skipped one (Capacity==0) are not measurements,
	// and a zero rate must not read as the slowest possible host.
	if !bench.Failed && bench.Capacity > 0 {
		v := interactiveFloorVerdict(bench, depth, p.cfg)
		outcome.BelowFloor = v.Below
		outcome.FloorTokps = v.Floor
	}

	// A run that stopped at the readiness gate never reached the engine, so
	// it is not a benchmark result and does not become the completed one.
	// Recording it did two things (#576): it replaced this host's last real
	// measurement with a failure, so /benchmark/status answered `failed`
	// where it used to answer the number; and because the record carries the
	// REQUESTED generation, the setup reconciler's retry guard
	// (`bs.Gen < d.benchmarkGen`, setup_desired.go) was already satisfied, so
	// nothing ever re-ran it and the wizard's benchmark step stayed failed
	// for a host whose engine came up seconds later. Leaving the record alone
	// keeps the served generation behind the desired one, which is the
	// reconciler's own signal to kick the job again.
	//
	// p.SetLastBench(bench) above is deliberately NOT skipped: that is #203's
	// node-rating path, and its Capacity 1 fail-safe is exactly what a host
	// with no working engine should be advertising meanwhile.
	record := catalog.BenchmarkRecord{
		Gen:           gen,
		MeasuredTokps: bench.TokensPerSec,
		Method:        bench.Method,
		SpreadPct:     bench.SpreadPct,
		Trials:        benchSampleCount,
		Failed:        bench.Failed,
		Error:         bench.Err,
		Outcome:       bench.Outcome,
		MeasuredAt:    time.Now().UTC(),
	}
	ranAtAll := bench.Outcome != benchOutcomeEngineNotReady
	// What this run measured, keyed by the variant it measured — the
	// half of the result BenchmarkRecord has never carried, and the
	// input the ranking needs to stop recommending a model this host has
	// already timed as too slow (waired-agent#784).
	//
	// Derived from bench's OWN identity fields rather than from the
	// active selection, so a run that finishes after a switch files its
	// figure under the model it actually measured. Empty on every
	// condition that would make the key a guess — no figure, a failed
	// run, an unnamed model, a variant the bundled catalog does not have
	// — and an empty key records nothing rather than recording it wrong.
	benchEngineKind, _ := probeTargetForActive(p.cfg)
	measuredSHA, measurement := benchMeasurement(bench, p.manifests, benchEngineKind, engineVersion)
	if ranAtAll {
		if err := p.store.Update(func(s *catalog.State) {
			// A gen-0 (boot/CLI) run must not regress a counter-driven
			// generation the CP already saw — keep the stored gen then.
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

	p.benchJobMu.Lock()
	p.benchJobOutcome = &outcome
	p.benchJobOutcomeKind = bench.Outcome
	if ranAtAll {
		p.benchJobResult = &record
	}
	p.benchJobDone = nil
	// The run is over; its live progress would otherwise be reported
	// forever beside a finished result.
	p.benchJobProgress = nil
	p.benchJobMu.Unlock()
	close(done)
}

// publishBenchProgress records one in-flight measurement report for
// /benchmark/status to serve (waired-agent#199).
func (p *agentInferenceProvider) publishBenchProgress(bp BenchProgress) {
	p.benchJobMu.Lock()
	p.benchJobProgress = &bp
	p.benchJobMu.Unlock()
}

// MeasuredRates reports the persisted per-variant measurements and the
// interactive floor this host judges them against (waired-agent#784).
//
// Read from the store on every call rather than cached: a benchmark
// that finishes between two catalog polls has to move the badge, and
// the store is already the single writer of that record — a cache here
// would be a second copy of a fact one Update away.
//
// The floor comes from resolveInteractiveFloor, the SAME function the
// step-down proposal uses, so the badge and the proposal cannot end up
// disagreeing about what "too slow" means on a host whose operator set
// interactive_floor_tokps.
func (p *agentInferenceProvider) MeasuredRates() (map[string]router.MeasuredRate, float64) {
	st, err := p.store.Load()
	if err != nil {
		return nil, 0
	}
	rates := measuredRatesFrom(st)
	if rates == nil {
		return nil, 0
	}
	return rates, resolveInteractiveFloor(p.cfg.InteractiveFloorTokps)
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

	if last == nil {
		// Nothing completed this process lifetime — consult the
		// persisted record (survives restarts).
		if st, err := p.store.Load(); err == nil && st.LastBenchmark != nil {
			rec := *st.LastBenchmark
			last = &rec
		}
	}

	resp := management.BenchmarkStatusResponse{State: management.BenchmarkStateIdle}
	if last != nil {
		resp.State = management.BenchmarkStateDone
		if last.Failed {
			resp.State = management.BenchmarkStateFailed
			resp.Error = last.Error
		}
		resp.Gen = last.Gen
		resp.MeasuredTokps = last.MeasuredTokps
		resp.MeasuredAt = last.MeasuredAt.Format(time.RFC3339)
		resp.Method = last.Method
		resp.SpreadPct = last.SpreadPct
		resp.Trials = last.Trials
		resp.Outcome = last.Outcome
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
		if live != nil {
			resp.Phase = live.Phase
			resp.Trial = live.Trial
			resp.Trials = live.Trials
			resp.SampleTokps = live.SampleTokps
			resp.MedianTokps = live.MedianTokps
			resp.SpreadPct = live.SpreadPct
			resp.Method = live.Method
		}
	}
	return resp
}

// DismissRecommendation records that the user declined a model-switch
// suggestion (either direction) so a re-benchmark of the same pairing
// stays quiet. Keyed by the active variant's content digest + the
// target variant ID. Empty toVariantID resolves the current live
// recommendation's target (lighter first, then upgrade — at most one
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
		lighter, upgrade := p.currentRecommendations(context.Background())
		rec := lighter
		if rec == nil || rec.ToVariantID == "" {
			rec = upgrade
		}
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
	return p.store.Update(func(s *catalog.State) {
		if s.DismissedRecommendations == nil {
			s.DismissedRecommendations = map[string]time.Time{}
		}
		s.DismissedRecommendations[key] = time.Now().UTC()
	})
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
