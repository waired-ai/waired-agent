package main

import (
	"time"

	"github.com/waired-ai/waired-agent/internal/inference"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// The readiness half of waired-agent#1127.
//
// Owner ruling, 2026-08-29: a node must not accept inference from other
// nodes until it knows what it costs to use — "kubernetes の readiness
// probe のように" — because until then nothing can place it in a ranking,
// and the ranking is what decides whose nine minutes a turn takes. The
// measurement that settles it is the served model's speed measurement
// (waired-ai/waired-agent#1341), run by the daemon's loop
// (maybeRunBootBenchmark), which arms this gate for a selection it still
// owes and clears it once the selection has its verdict.
//
// EngineReady could not carry it. That predicate is read by the peer
// /healthz, the observability gauges, `waired doctor`, the setup engine
// gate AND the measurement's own entry gate, so a measurement gated on it
// could never start.

// beginSpeedMeasurement arms the gate. Only where a measurement is going
// to be attempted — an armed latch that nothing clears would take the host
// out of the mesh for the life of the daemon.
func (p *agentInferenceProvider) beginSpeedMeasurement() {
	if p == nil {
		return
	}
	p.speedMeasuring.Store(true)
	// First arming wins: this is "how long this host has been trying", not
	// "how long since the last attempt started".
	p.speedMeasureArmedAt.CompareAndSwap(0, time.Now().UnixNano())
}

// endSpeedMeasurement clears it, whatever the outcome. A host that CANNOT
// be measured still serves: the gate says "not yet", never "not ever".
func (p *agentInferenceProvider) endSpeedMeasurement() {
	if p == nil {
		return
	}
	p.speedMeasuring.Store(false)
	p.speedMeasureArmedAt.Store(0)
}

// IsMeasuringSpeed is inference.Config.IsMeasuringSpeed.
func (p *agentInferenceProvider) IsMeasuringSpeed() bool {
	return p != nil && p.speedMeasuring.Load()
}

// speedMeasurementPoll is how often the daemon's measurement loop asks
// whether this host still owes a measurement. Cheap — a cached adapter
// health read and a state file — and the same cadence the inference probe
// loop already runs at.
const speedMeasurementPoll = 15 * time.Second

// servedSpeed is the measurement this host publishes about the model it
// serves: the last verdict, when it describes the served variant. A figure
// taken on another variant is withheld — publishing it against the new
// model would hand every requester a number for a model this host no longer
// runs, and unmeasured is safe where a wrong number is not
// (docs/decisions/20260822/0218).
func (p *agentInferenceProvider) servedSpeed() (BenchResult, time.Time, bool) {
	if p == nil {
		return BenchResult{}, time.Time{}, false
	}
	p.benchMu.Lock()
	b := p.lastBench
	at := p.lastBenchAt
	p.benchMu.Unlock()
	if b == nil || b.Failed || (b.TurnSeconds <= 0 && b.TurnFloorSeconds <= 0) {
		return BenchResult{}, time.Time{}, false
	}
	if v := p.activeVariantID(); v != "" && b.VariantID != "" && v != b.VariantID {
		return BenchResult{}, time.Time{}, false
	}
	return *b, at, true
}

// SpeedForHealth is inference.Config.Speed: the served model's
// measurement, for requesters that rank in seconds per request.
func (p *agentInferenceProvider) SpeedForHealth() *inference.SpeedReading {
	b, at, ok := p.servedSpeed()
	if !ok {
		return nil
	}
	out := &inference.SpeedReading{
		VariantID:        b.VariantID,
		DepthTokens:      b.DepthTokens,
		PrefillTokps:     b.PrefillTokps,
		DecodeTokps:      b.DecodeTokps,
		TurnSeconds:      b.TurnSeconds,
		TurnFloorSeconds: b.TurnFloorSeconds,
	}
	if !at.IsZero() {
		out.MeasuredAt = at.UTC().Format(time.RFC3339Nano)
	}
	return out
}

// PrefillRateForHealth is inference.Config.PrefillRate: the same
// measurement as one rung at the depth it was taken, for requesters older
// than HealthSnapshot.Speed, which compare peers at a common prefill rung.
// Kept for one release (waired-ai/waired-agent#1341). A measurement that
// stalled publishes its rung as a bound: no faster than depth / elapsed.
func (p *agentInferenceProvider) PrefillRateForHealth() *inference.PrefillRate {
	b, _, ok := p.servedSpeed()
	if !ok || b.DepthTokens <= 0 {
		return nil
	}
	rung := inference.PrefillRung{Depth: b.DepthTokens, Tokps: b.PrefillTokps, Samples: b.Samples, SpreadPct: b.SpreadPct}
	if b.TurnSeconds <= 0 {
		// TurnFloorSeconds is normalised to the canonical depth.
		rung.Bound = true
		rung.Tokps = float64(hostfit.SpeedMeasurementDepthTokens) / b.TurnFloorSeconds
	}
	if rung.Tokps <= 0 {
		return nil
	}
	return &inference.PrefillRate{VariantID: b.VariantID, Rungs: []inference.PrefillRung{rung}}
}

// activeVariantID is the committed active selection's variant, read from
// this provider's store.
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

// claimBench is claimEngineForBench with the test seam folded in.
func (p *agentInferenceProvider) claimBench() (func(), bool) {
	if p.claimForBench != nil {
		return p.claimForBench()
	}
	return p.claimEngineForBench()
}

// appliedTuningReader is the slice of an engine adapter the measurement
// reads its serving configuration from.
type appliedTuningReader interface {
	AppliedTuning() infruntime.ModelTuning
}
