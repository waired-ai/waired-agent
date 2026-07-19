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
	floor := resolveInteractiveFloor(cfg.InteractiveFloorTokps)
	// #624: a host can decode fine at an empty context and still crawl
	// at depth (intentional spill, KV pressure) — so the depth sweep
	// participates in the comparison. The shallow floor already prices
	// in the expected long-context degradation (#670: 100 shallow was
	// chosen to keep ~80 at depth), so the depth leg is held to
	// floor × CodingAgentDepthFloorFraction rather than the full floor
	// — demanding 100 at 200k depth would double-count the degradation
	// and nag on essentially every host.
	measured := bench.TokensPerSec
	depthReason := ""
	below := measured < floor
	if dec, target, ok := worstCompletedDepthDecode(depth); ok && dec < floor*router.CodingAgentDepthFloorFraction {
		below = true
		if dec < measured {
			measured = dec
		}
		depthReason = fmt.Sprintf(" (decode at ~%dk context measured %.0f tok/s, below the %.0f tok/s depth floor)",
			target/1024, dec, floor*router.CodingAgentDepthFloorFraction)
	}
	if !below {
		return nil
	}

	st, err := store.Load()
	if err != nil || st.Active == nil {
		return nil
	}

	enginePick, err := router.PickEngine(router.EnginePickInput{
		Hardware:   hw,
		Preference: cfg.PreferredEngine,
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
		return p.benchJobDone
	}
	done := make(chan struct{})
	p.benchJobDone = done
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
			EngineKind:    engineKind,
			EnginePort:    enginePort,
			EngineModel:   engineModelForActive(p.cfg),
			VariantID:     variantIDForActive(),
			GPUModel:      firstGPU.Model,
			VRAMTotalMB:   firstGPU.VRAMTotalMB,
			DriverVersion: firstGPU.DriverVersion,
			Logger:        p.logger,
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
	}

	record := catalog.BenchmarkRecord{
		Gen:           gen,
		MeasuredTokps: bench.TokensPerSec,
		Failed:        bench.Failed,
		Error:         bench.Err,
		MeasuredAt:    time.Now().UTC(),
	}
	if err := p.store.Update(func(s *catalog.State) {
		// A gen-0 (boot/CLI) run must not regress a counter-driven
		// generation the CP already saw — keep the stored gen then.
		if record.Gen == 0 && s.LastBenchmark != nil && s.LastBenchmark.Gen > 0 {
			record.Gen = s.LastBenchmark.Gen
		}
		s.LastBenchmark = &record
	}); err != nil {
		p.logger.Warn("benchmark: persist completion record", "err", err)
	}

	p.benchJobMu.Lock()
	p.benchJobOutcome = &outcome
	p.benchJobResult = &record
	p.benchJobDone = nil
	p.benchJobMu.Unlock()
	close(done)
}

// BenchmarkStatus reports the job's current state for
// GET /waired/v1/inference/benchmark/status (waired#835 §12). Falls
// back to the persisted completion record after a restart.
func (p *agentInferenceProvider) BenchmarkStatus() management.BenchmarkStatusResponse {
	p.benchJobMu.Lock()
	running := p.benchJobDone != nil
	last := p.benchJobResult
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
	}
	if running {
		resp.State = management.BenchmarkStateRunning
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
