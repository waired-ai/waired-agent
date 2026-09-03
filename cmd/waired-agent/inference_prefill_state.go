package main

import (
	"context"
	"fmt"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/inference"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// The readiness half of waired-agent#1127.
//
// Owner ruling, 2026-08-29: a node must not accept inference from other
// nodes until it knows what it costs to use — "kubernetes の readiness
// probe のように" — because until then nothing can place it in a ranking,
// and the ranking is what decides whose nine minutes a turn takes. Before
// this, nothing gated peer traffic on a measurement at all: a host started
// answering the moment its overlay listener was up and its engine reported
// a model on disk.
//
// EngineReady could not carry it. That predicate is read by the peer
// /healthz, the observability gauges, `waired doctor`, the setup engine
// gate AND the benchmark's own entry gate, so a measurement gated on it
// could never start.

// beginSpeedMeasurement arms the gate. Called on the session goroutine
// before anything can serve, and only where a measurement is going to be
// attempted — an armed latch that nothing clears would take the host out
// of the mesh for the life of the daemon.
func (p *agentInferenceProvider) beginSpeedMeasurement() {
	if p == nil {
		return
	}
	p.speedMeasuring.Store(true)
}

// endSpeedMeasurement clears it, whatever the outcome. A host that CANNOT
// be measured still serves: the gate says "not yet", never "not ever".
func (p *agentInferenceProvider) endSpeedMeasurement() {
	if p == nil {
		return
	}
	p.speedMeasuring.Store(false)
}

// IsMeasuringSpeed is inference.Config.IsMeasuringSpeed.
func (p *agentInferenceProvider) IsMeasuringSpeed() bool {
	return p != nil && p.speedMeasuring.Load()
}

// SetLastPrefill records a completed measurement.
func (p *agentInferenceProvider) SetLastPrefill(m PrefillMeasurement) {
	if p == nil {
		return
	}
	p.benchMu.Lock()
	defer p.benchMu.Unlock()
	p.lastPrefill = &m
}

// PrefillRateForHealth is inference.Config.PrefillRate: what this host
// publishes to peers about its own prefill speed.
//
// It returns nothing when the measurement was taken on a DIFFERENT variant
// from the one being served. A model switch through /model, the tray or the
// desired-model channel changes the rate, and publishing the old figure
// against the new model would hand every requester a number for a model
// this host no longer runs. Unmeasured is safe — an endpoint nobody has
// measured is not punished (docs/decisions/20260822/0218) — where a wrong
// number is not.
func (p *agentInferenceProvider) PrefillRateForHealth() *inference.PrefillRate {
	if p == nil {
		return nil
	}
	p.benchMu.Lock()
	m := p.lastPrefill
	p.benchMu.Unlock()
	if m == nil || !m.Known() {
		return nil
	}
	// Read through the provider's own store rather than the default state
	// path: /healthz answers this on every probe, and EngineReady beside
	// it already reads the same record the same way.
	if p.store != nil {
		st, _ := p.store.Load()
		if st.Active != nil && st.Active.VariantID != "" && st.Active.VariantID != m.VariantID {
			return nil
		}
	}
	out := &inference.PrefillRate{VariantID: m.VariantID}
	for _, r := range m.Rungs {
		out.Rungs = append(out.Rungs, inference.PrefillRung{
			Depth:     r.Depth,
			Tokps:     r.Tokps,
			Bound:     r.Bound,
			Samples:   r.Samples,
			SpreadPct: r.SpreadPct,
		})
	}
	return out
}

// speedMeasurementPoll is how often the loop below asks whether this host
// still needs measuring. Cheap — a cached adapter health read and a state
// file — and the same cadence the inference probe loop already runs at.
const speedMeasurementPoll = 15 * time.Second

// runSpeedMeasurement keeps this host's published prefill rate matched to
// the model it is serving, for as long as the daemon runs.
//
// A loop rather than a one-shot at boot, for two reasons found the hard
// way. The boot benchmark it would otherwise hang off is gated on
// EngineReady and fires once: on a host whose engine takes ~60 s to come
// up it loses that race almost every time — measured on one vLLM host, 5
// completions in 82 boots — and a measurement wired behind it would have
// inherited exactly that. And the model changes: a switch through /model,
// the tray or the desired-model channel makes the old figure a number for
// a model this host no longer runs.
//
// depsFor is called per attempt so the engine kind, port and model are
// read live rather than captured at boot.
func (p *agentInferenceProvider) runSpeedMeasurement(ctx context.Context, depsFor func() PrefillDeps, poll time.Duration) {
	if p == nil || depsFor == nil {
		return
	}
	if poll <= 0 {
		poll = speedMeasurementPoll
	}
	for {
		p.maybeMeasureSpeed(ctx, depsFor)
		select {
		case <-ctx.Done():
			return
		case <-time.After(poll):
		}
	}
}

// maybeMeasureSpeed runs one round of the decision above.
//
// The gate is held while a measurement is OWED, and cleared only once one
// has been recorded — not merely because this round decided not to try.
// Measured on real hardware: an engine takes about fifteen seconds to come
// up after the daemon does, so a first round that cleared the gate on
// "engine not ready yet" opened it for the whole measurement that
// followed, and peers were served by a host that did not yet know what it
// cost. The gate was decorative on every host, because every engine is a
// moment behind its daemon.
func (p *agentInferenceProvider) maybeMeasureSpeed(ctx context.Context, depsFor func() PrefillDeps) {
	ready, _ := p.EngineReady()
	if !ready {
		// Not a decision, just "not yet". EngineReady already refuses peer
		// traffic on its own, so a gate held here costs nothing — and a
		// gate cleared here would be cleared before the thing it is
		// waiting for has even started.
		return
	}
	variant := p.activeVariantID()
	if variant == "" || p.speedMeasuredFor(variant) {
		// Nothing is owed: either there is no committed model to measure,
		// or this variant has already been attempted.
		p.endSpeedMeasurement()
		return
	}
	// A measurement is owed. Re-arm rather than assume the startup arm is
	// still standing: the operator can switch model at any time, and the
	// answer for the new one is not known either.
	p.beginSpeedMeasurement()
	deps := depsFor()
	deps.VariantID = variant
	deps.AppliedWindow = p.appliedServeTuning(ctx).ContextLength
	deps.Nonce = fmt.Sprintf("prefill-%d", time.Now().UnixNano())
	// Get out of the way of real work. The engine claim answers this once,
	// at the start; a rung is a minute and a half of saturated engine, and
	// a request that arrives after the claim would queue behind all of it.
	deps.Yield = func() bool { return p.servingInFlight() > 0 }
	p.measureSpeedForMesh(ctx, deps)
}

// activeVariantID is the catalog variant this host has committed to
// serving, read through the provider's own store.
func (p *agentInferenceProvider) activeVariantID() string {
	if p == nil || p.store == nil {
		return ""
	}
	st, _ := p.store.Load()
	if st.Active == nil {
		return ""
	}
	return st.Active.VariantID
}

// speedMeasuredFor reports whether this variant has already been
// attempted. A FAILED attempt counts: retrying it every tick would
// saturate the engine of a host that cannot answer, and the failure is
// already published as "no rate" rather than as a slow one. A model
// change is what earns another attempt.
func (p *agentInferenceProvider) speedMeasuredFor(variantID string) bool {
	if p == nil {
		return true
	}
	p.benchMu.Lock()
	defer p.benchMu.Unlock()
	return p.lastPrefill != nil && p.lastPrefill.VariantID == variantID
}

// measureSpeedForMesh runs the prefill measurement and clears the readiness
// gate on every path, including the ones that decide not to measure at all.
//
// It holds the engine for the run (claimEngineForBench, waired-agent#703),
// the same latch that keeps the boot benchmark and the install-time
// host-speed measurement off each other. A prefill measurement is minutes
// of saturated engine; running it against traffic would measure the
// traffic, and running it under a serve-env reconcile would measure a
// restart.
func (p *agentInferenceProvider) measureSpeedForMesh(ctx context.Context, deps PrefillDeps) {
	if p == nil {
		return
	}
	release, ok := p.claimBench()
	if !ok {
		// Deliberately WITHOUT clearing the gate: nothing was measured, so
		// nothing is known, and the loop will ask again in a few seconds.
		// The install-time host-speed probe holds this claim for a minute
		// or two on a fresh host, which is exactly when a peer must not be
		// told this host is ready to take work.
		p.logger.Info("prefill measurement skipped: the engine is busy",
			"variant", deps.VariantID)
		return
	}
	defer release()

	m := MeasurePrefillRate(ctx, deps)
	if !m.Failed && len(m.Rungs) == 0 {
		// It yielded to serving traffic before anything completed. Nothing
		// was measured, so there is nothing to record and nothing is
		// known — the loop asks again once the engine is free, and the
		// readiness gate stays closed until it does.
		return
	}
	if m.Failed {
		// Recorded anyway, so a later reader can tell "measured and
		// failed" from "never ran". PrefillRateForHealth publishes
		// nothing for it.
		p.logger.Warn("prefill measurement failed; this host publishes no speed",
			"variant", deps.VariantID, "err", m.Err)
	}
	p.SetLastPrefill(m)
	p.endSpeedMeasurement()
}

// appliedServeTuning is the tuning of the engine this host actually
// serves from, once its verify/degrade cycle has settled. It is the same
// read the depth benchmark makes and for the same reason: starting a
// multi-minute prefill mid-restart measures a dying engine.
//
// The zero value (ContextLength 0) means the engine is untuned, and the
// measurement then plans against an unknown window — every rung is
// attempted and the engine's own truncation, caught by the read-back
// guard, drops the ones that do not fit.
func (p *agentInferenceProvider) appliedServeTuning(ctx context.Context) infruntime.ModelTuning {
	if p == nil {
		return infruntime.ModelTuning{}
	}
	if p.servingEngine() == catalog.RuntimeVLLM {
		tuner, ok := p.vllmAdapter().(appliedTuningReader)
		if !ok {
			return infruntime.ModelTuning{}
		}
		return waitForAppliedTuning(ctx, tuner, 5*time.Second, appliedTuningWait)
	}
	if p.ollama == nil {
		return infruntime.ModelTuning{}
	}
	return waitForAppliedTuning(ctx, p.ollama, 5*time.Second, appliedTuningWait)
}

// claimBench is claimEngineForBench with the test seam folded in.
func (p *agentInferenceProvider) claimBench() (func(), bool) {
	if p.claimForBench != nil {
		return p.claimForBench()
	}
	return p.claimEngineForBench()
}

// appliedTuningReader is the slice of *infruntime.OllamaAdapter the
// depth scheduler needs.
type appliedTuningReader interface {
	AppliedTuning() infruntime.ModelTuning
}

// appliedTuningWait bounds how long a measurement waits
// for the #621 tuning verification to settle before reading the
// applied window. Generous: a fresh install pulls a 20+ GB model
// before the verify pass can even load it.
const appliedTuningWait = 15 * time.Minute

// waitForAppliedTuning polls until the applied tuning reports
// Verified (the one-shot verify/degrade cycle is over — starting a
// multi-minute prefill mid-restart would measure a dying engine), or
// the deadline passes; it returns the latest tuning either way. The
// caller skips the run when ContextLength is 0 (untuned engine).
func waitForAppliedTuning(ctx context.Context, r appliedTuningReader, poll, timeout time.Duration) infruntime.ModelTuning {
	deadline := time.Now().Add(timeout)
	for {
		t := r.AppliedTuning()
		if t.Verified || time.Now().After(deadline) {
			return t
		}
		select {
		case <-ctx.Done():
			return r.AppliedTuning()
		case <-time.After(poll):
		}
	}
}
