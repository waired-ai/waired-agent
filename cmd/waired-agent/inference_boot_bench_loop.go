package main

import (
	"context"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
)

// bootBenchPoll is how often the boot benchmark re-asks whether this host
// still owes a measurement. Deliberately the same cadence as the prefill
// measurement's loop and the inference probe: the question is the same
// size — an adapter health read and a state file — and three different
// periods on one host would only be three things to reason about.
const bootBenchPoll = speedMeasurementPoll

// bootBenchSelectionKey names what a measurement would be a measurement
// OF. Empty when there is nothing to measure yet.
//
// Seven terms, each of which changes the answer:
//
//   - ModelID / VariantID — a rate measured on one model says nothing
//     about another (benchDescribes draws the same line).
//   - EngineKind — ollama and vLLM serve the same weights at different
//     rates, and a host can move between them without changing model.
//   - EngineVersion — the release is what an upgrade moves, which is why
//     waired-agent#1131 put it in the cache key. Empty here is not fatal:
//     it makes a distinct key, so a host that measured before its version
//     could be read measures again once it can.
//   - AppliedWindow / KVCacheType / NumParallel — the serving
//     configuration. The same weights served with a smaller window or
//     another KV type are a different speed (waired-ai/waired-agent#1341).
//   - SpeculativeMethod / SpeculativeTokens, appended only when there is
//     a draft, so a host that drafts nothing keeps its key
//     (waired-ai/waired#1432).
//
// ModelID is what makes it empty. A host with no committed selection has
// nothing to measure, and keying on the empty model would let the first
// real selection inherit "already attempted".
func bootBenchSelectionKey(d BenchDeps) string {
	if d.ModelID == "" {
		return ""
	}
	terms := []string{d.ModelID, d.VariantID, d.EngineKind, d.EngineVersion,
		itoa(d.AppliedWindow), d.KVCacheType, itoa(d.NumParallel)}
	if d.SpeculativeMethod != "" {
		terms = append(terms, d.SpeculativeMethod, itoa(d.SpeculativeTokens))
	}
	return strings.Join(terms, "\x00")
}

// benchReachedAVerdict reports whether a run said something about this
// host, as opposed to declining to run.
//
// measured and failed are both verdicts. A failure is a statement — an
// accelerator out of memory, a warm-up that timed out — and retrying it
// every fifteen seconds would saturate the engine of a host that cannot
// answer while telling nobody anything new. speedMeasuredFor
// (inference_prefill_state.go) counts a failed attempt the same way and
// for the same reason; a model change or an engine upgrade is what earns
// another one, which the selection key already expresses.
//
// engine_not_ready and skipped are not verdicts. They are the two ways a
// run can decline before reaching the engine, and both are what
// waired-agent#1150 is about.
func benchReachedAVerdict(r BenchResult) bool {
	return r.Outcome == benchOutcomeMeasured || r.Outcome == benchOutcomeFailed
}

// bootBenchSettledFor reports whether this selection has already had its
// one attempt. A nil provider answers yes, the same fail-closed direction
// speedMeasuredFor takes: a fixture with no state is not a host owed a
// measurement.
func (p *agentInferenceProvider) bootBenchSettledFor(key string) bool {
	if p == nil {
		return true
	}
	p.benchMu.Lock()
	defer p.benchMu.Unlock()
	return p.bootBenchSettled == key
}

func (p *agentInferenceProvider) markBootBenchSettled(key string) {
	if p == nil {
		return
	}
	p.benchMu.Lock()
	defer p.benchMu.Unlock()
	p.bootBenchSettled = key
}

// settleBootBench records that this selection has had its attempt. The
// ledger entry and the completion record are the measurement job's to write
// (runBenchmarkJob), which every measurement now goes through.
func (p *agentInferenceProvider) settleBootBench(deps BenchDeps, res BenchResult) {
	if p == nil || !benchReachedAVerdict(res) {
		return
	}
	if key := bootBenchSelectionKey(deps); key != "" {
		p.markBootBenchSettled(key)
	}
}

// runBootBenchmarkLoop keeps this host's speed measurement matched to the
// model it serves, for as long as the daemon runs.
//
// A loop rather than a one-shot: the measurement is gated on EngineReady,
// and on a host whose engine takes about a minute to come up a single
// boot-tail attempt lost that race almost every time — 5 completions in 82
// boots on one vLLM host (waired-agent#1150).
//
// It is NOT a periodic re-measurement, which waired-agent#202 argues
// against on good grounds: a synthetic measurement pins the model in VRAM
// and measures contention on a busy host. maybeRunBootBenchmark makes at
// most one attempt per selection, and a stored measurement of the same
// weights and serving configuration answers without measuring at all, so
// the loop's cost at rest is the poll itself (decision 7 of
// docs/decisions/20260913/2245).
func (p *agentInferenceProvider) runBootBenchmarkLoop(ctx context.Context, poll time.Duration) {
	if p == nil {
		return
	}
	if poll <= 0 {
		poll = bootBenchPoll
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(poll):
		}
		p.maybeRunBootBenchmark(ctx)
	}
}

// maybeRunBootBenchmark runs one round of the decision above, through the
// same single-flight job a setup generation and `waired runtimes benchmark`
// use — so a generation the wizard asks for while this round measures joins
// it rather than waiting behind it, and the progress it reports is the one
// every surface reads.
//
// The gates ahead of the job keep a quiet host quiet: the job logs its own
// decline on each of them, which is the right volume for a request and the
// wrong volume for a fifteen-second loop.
//
//   - EngineReady covers the toggle, a parked engine, an engine still
//     starting, and a selection not yet committed.
//   - engineExclusiveHeld is the other measurement (the install-time host
//     cutoff). Racy by construction, and losing the race costs one logged
//     decline.
//
// A measurement that yielded to this host's own traffic is not a verdict;
// the next attempt waits for the traffic to be gone a while first
// (speedIdleAfterYield).
func (p *agentInferenceProvider) maybeRunBootBenchmark(ctx context.Context) {
	if p == nil {
		return
	}
	if ready, _ := p.EngineReady(); !ready {
		return
	}
	if p.engineExclusiveHeld() {
		return
	}
	deps := p.speedDeps(ctx, management.BenchmarkModeEnsure)
	key := bootBenchSelectionKey(deps)
	if key == "" || p.bootBenchSettledFor(key) {
		p.endSpeedMeasurement()
		return
	}
	p.beginSpeedMeasurement()
	if p.yieldedRecently() && !awaitServingIdle(ctx, deps, p.idleAfterYield()) {
		return
	}
	select {
	case <-p.startBenchmarkJob(0, management.BenchmarkModeEnsure):
	case <-ctx.Done():
		return
	}
	p.benchJobMu.Lock()
	res := p.benchJobBench
	p.benchJobMu.Unlock()
	if res == nil {
		return
	}
	p.noteYield(res.Err == "engine busy: this host is serving traffic")
	// Matched on the variant alone: a failed run carries no model id
	// (failBench), and a failure is a verdict that must settle, or a host
	// whose engine cannot answer is asked again every tick.
	if !benchReachedAVerdict(*res) || res.VariantID != deps.VariantID ||
		(res.ModelID != "" && res.ModelID != deps.ModelID) {
		return
	}
	p.settleBootBench(deps, *res)
	p.endSpeedMeasurement()
}

// speedIdleAfterYield is how long this host's own traffic must have been
// gone before a measurement that gave the engine back tries again: the next
// turn of the same session is usually seconds behind the last.
const speedIdleAfterYield = 60 * time.Second

// idleAfterYield is speedIdleAfterYield, or the provider's override when a
// test set one (a field rather than a package var, so tests stay parallel).
func (p *agentInferenceProvider) idleAfterYield() time.Duration {
	if p.speedIdleAfterYield > 0 {
		return p.speedIdleAfterYield
	}
	return speedIdleAfterYield
}

func (p *agentInferenceProvider) yieldedRecently() bool {
	p.benchMu.Lock()
	defer p.benchMu.Unlock()
	return p.speedYielded
}

func (p *agentInferenceProvider) noteYield(yielded bool) {
	p.benchMu.Lock()
	defer p.benchMu.Unlock()
	p.speedYielded = yielded
}

// seedBootBenchmark is the one synchronous attempt at boot: a stored
// measurement of what this host serves answers it in microseconds, so the
// first probe tick advertises a measured host rather than the fail-safe.
// It never measures — a 32,768-token request is minutes, and the daemon's
// start must not wait on it; the loop behind it does the measuring.
func (p *agentInferenceProvider) seedBootBenchmark(ctx context.Context) BenchResult {
	if p == nil {
		return BenchResult{}
	}
	deps := p.speedDeps(ctx, management.BenchmarkModeEnsure)
	deps.CacheOnly = true
	res := RunBootBenchmark(ctx, deps)
	if benchReachedAVerdict(res) {
		p.settleBootBench(deps, res)
	}
	return res
}
