package main

import (
	"context"
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
	defer p.endSpeedMeasurement()

	release, ok := p.claimEngineForBench()
	if !ok {
		p.logger.Info("prefill measurement skipped: the engine is busy",
			"variant", deps.VariantID)
		return
	}
	defer release()

	m := MeasurePrefillRate(ctx, deps)
	if m.Failed {
		// Recorded anyway, so a later reader can tell "measured and
		// failed" from "never ran". PrefillRateForHealth publishes
		// nothing for it.
		p.logger.Warn("prefill measurement failed; this host publishes no speed",
			"variant", deps.VariantID, "err", m.Err)
	}
	p.SetLastPrefill(m)
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
		return waitForAppliedTuning(ctx, tuner, 5*time.Second, depthBenchTuningWait)
	}
	if p.ollama == nil {
		return infruntime.ModelTuning{}
	}
	return waitForAppliedTuning(ctx, p.ollama, 5*time.Second, depthBenchTuningWait)
}
