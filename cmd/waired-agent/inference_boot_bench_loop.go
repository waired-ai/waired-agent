package main

import (
	"context"
	"strings"
	"time"
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
// Four terms, each of which changes the answer:
//
//   - ModelID / VariantID — a rate measured on one model says nothing
//     about another (benchDescribes draws the same line).
//   - EngineKind — ollama and vLLM serve the same weights at different
//     rates, and a host can move between them without changing model.
//   - EngineVersion — the release is what an upgrade moves, which is why
//     waired-agent#1131 put it in the cache key. Empty here is not fatal:
//     it makes a distinct key, so a host that measured before its version
//     could be read measures again once it can.
//
// ModelID is what makes it empty. A host with no committed selection has
// nothing to measure, and keying on the empty model would let the first
// real selection inherit "already attempted".
func bootBenchSelectionKey(d BenchDeps) string {
	if d.ModelID == "" {
		return ""
	}
	return strings.Join([]string{d.ModelID, d.VariantID, d.EngineKind, d.EngineVersion}, "\x00")
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

// runBootBenchmarkLoop keeps this host's decode measurement matched to the
// model it serves, for as long as the daemon runs.
//
// A loop rather than the one-shot it replaces. The benchmark is gated on
// EngineReady and used to fire exactly once, from the boot tail: on a host
// whose engine takes about a minute to come up it lost that race almost
// every time — 5 completions in 82 boots on one vLLM host, and on the
// same hardware a boot where it stood down 33 seconds before the prefill
// measurement beside it completed against the very same engine. Nothing
// re-ran it, so the disk cache — whose only writer this is — was never
// populated, and the host had no standing decode rate between the times
// an operator pressed the button (waired-agent#1150).
//
// It is NOT a periodic re-measurement, which waired-agent#202 argues
// against on good grounds: a synthetic benchmark pins the model in VRAM
// and measures contention on a busy host. maybeRunBootBenchmark makes at
// most one attempt per selection — the loop's cost at rest is the poll
// itself, and the answer to "measure again" is only ever yes when the
// thing being measured has changed.
//
// depsFor is called per attempt so the engine kind, port, model, variant
// digest and engine release are read live. The one-shot read them once at
// boot, which on a fresh install is before any of them exist: the catalog
// has no selection, so the variant digest is empty (silently disabling the
// cache) and probeTargetForActive answers "ollama" for a host that is
// about to serve with vLLM.
func (p *agentInferenceProvider) runBootBenchmarkLoop(
	ctx context.Context,
	depsFor func() BenchDeps,
	onVerdict func(BenchResult, BenchDeps),
	poll time.Duration,
) {
	if p == nil || depsFor == nil {
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
		p.maybeRunBootBenchmark(ctx, depsFor, onVerdict)
	}
}

// maybeRunBootBenchmark runs one round of the decision above.
//
// The three gates ahead of depsFor are there to keep a quiet host quiet.
// RunBootBenchmark logs its own decline on each of them, which is the
// right volume for the single synchronous attempt at boot and the wrong
// volume for a fifteen-second loop — a WARN per tick for the minutes an
// engine takes to come up, or for the minutes the prefill measurement
// holds the engine, is the line that gets filtered out and takes the real
// ones with it (the reasoning waired-agent#633 recorded for the same log).
//
//   - EngineReady covers the toggle, a parked engine, an engine still
//     starting, and a selection not yet committed. It is a live read, so
//     a host that has local inference turned ON after boot starts
//     measuring on the next tick rather than never.
//   - engineExclusiveHeld is the other measurement. Racy by construction
//     — the claim can be taken between this read and RunBootBenchmark's
//     own — and that is fine: losing the race costs one logged decline,
//     not a tick's worth of them.
func (p *agentInferenceProvider) maybeRunBootBenchmark(
	ctx context.Context,
	depsFor func() BenchDeps,
	onVerdict func(BenchResult, BenchDeps),
) {
	if p == nil || depsFor == nil {
		return
	}
	if ready, _ := p.EngineReady(); !ready {
		return
	}
	if p.engineExclusiveHeld() {
		return
	}
	deps := depsFor()
	key := bootBenchSelectionKey(deps)
	if key == "" || p.bootBenchSettledFor(key) {
		return
	}
	res := RunBootBenchmark(ctx, deps)
	if !benchReachedAVerdict(res) {
		return
	}
	p.markBootBenchSettled(key)
	if onVerdict != nil {
		onVerdict(res, deps)
	}
}

// seedBootBenchmark takes the one synchronous attempt the boot tail has
// always taken, and records its verdict so the loop does not repeat it.
// The deps it measured with are returned alongside, because what runs
// after a verdict — the depth sweep — is described by the same live read
// and must not go back to the boot snapshot for it.
//
// A nil provider still measures. There is no settle state to record, but
// the benchmark itself needs none of the provider's gates: refusing here
// would change what an engine-less or not-yet-built subsystem reports,
// which is not what this issue is about.
func (p *agentInferenceProvider) seedBootBenchmark(ctx context.Context, depsFor func() BenchDeps) (BenchResult, BenchDeps) {
	if depsFor == nil {
		return BenchResult{}, BenchDeps{}
	}
	deps := depsFor()
	res := RunBootBenchmark(ctx, deps)
	if p != nil && benchReachedAVerdict(res) {
		if key := bootBenchSelectionKey(deps); key != "" {
			p.markBootBenchSettled(key)
		}
	}
	return res, deps
}
