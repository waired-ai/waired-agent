package main

import (
	"context"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// A vLLM start that failed because the model does not fit this computer is
// remembered, and the engine is held off with the reason, as an ollama load
// that ran out of memory already is (#1453, #1464). Owner decision
// 2026-09-21 (waired-agent#1515): stop and say why. Nothing here retries on
// a timer, and nothing falls back to another model on its own.
//
// Until this existed a vLLM host that had been handed a model too large for
// its card — chosen, not refused, per the 2026-09-20 ruling — failed three
// start attempts, stopped trying until the next trigger, and failed the same
// three again on every restart, with a hint about the KV cache that sent the
// reader to the wrong setting.

// vllmStartFailedForMemory reports whether a failed vLLM start failed because
// the model did not fit in GPU memory, from the engine's own output for that
// start and the tuning's verdict that the weights alone exceed the budget.
//
// Weights over the budget come first: there the engine's own complaint is
// the symptom, not the cause. A real host whose weights were 23.4 GB against
// a 20.8 GB budget failed with "No available memory for the cache blocks",
// which reads as a KV setting to change (waired-agent#1515). Otherwise the
// reason is the log's own wording.
func vllmStartFailedForMemory(lastSpawnLog string, weightsOverBudget bool) (bool, string) {
	if weightsOverBudget {
		return true, "the model's weights are larger than the GPU memory vLLM may use on this computer"
	}
	for _, marker := range []string{
		"CUDA out of memory",
		"torch.OutOfMemoryError",
		"No available memory for the cache blocks",
		"larger than the maximum number of tokens that can be stored in KV cache",
		// vLLM v0.29.0's KV-cache shortfall, which names the window it could
		// hold (vllm/v1/core/kv_cache_utils.py; waired-ai/waired#1480).
		"the estimated maximum model length is",
	} {
		if strings.Contains(lastSpawnLog, marker) {
			return true, marker
		}
	}
	return false, ""
}

// vllmBlockedLoad names the build, and the shape it was started in, whose
// failed start holds a vLLM engine off. The records themselves are in the
// state file keyed by build; this says which one the stop is about.
//
// It cannot be read off the active selection, as ollama's is: on vLLM the
// model that was answering stays active until the chosen one is ready
// (waired-agent#1515), so when the chosen one fails to start it is the
// previous model that is named there. Reading the record through the active
// selection found none, and the review released the stop every 15 seconds
// on a real host, the bootstrap stopping it again each time.
type vllmBlockedLoad struct {
	SHA   string
	Shape catalog.LoadShape
}

// vllmBlockKey is the key the records use for this build in this shape.
func (p *agentInferenceProvider) vllmBlockKey(m catalog.Manifest, v catalog.Variant, shape catalog.LoadShape) vllmBlockedLoad {
	return vllmBlockedLoad{SHA: activeVariantSHA(p.catalogManifests(), m.ModelID, v.VariantID), Shape: shape}
}

// vllmLoadStillBlocked is loadIsBlocked on a vLLM host: the record for the
// build whose start failed, in the shape it was started in, and whether it
// still describes this computer.
func (p *agentInferenceProvider) vllmLoadStillBlocked() (catalog.VariantLoadFailure, bool) {
	if p == nil || p.store == nil {
		return catalog.VariantLoadFailure{}, false
	}
	b := p.vllmBlocked.Load()
	if b == nil || b.SHA == "" {
		return catalog.VariantLoadFailure{}, false
	}
	st, err := p.store.Load()
	if err != nil {
		return catalog.VariantLoadFailure{}, false
	}
	rec, ok := st.FailedLoads[b.SHA]
	if !ok || !rec.Blocks(p.loadContextNow(p.backgroundCtx()), b.Shape) {
		return catalog.VariantLoadFailure{}, false
	}
	return rec, true
}

// vllmLoadShape is the configuration a vLLM start asked for, in the shape a
// load-failure record keys on. Two starts with the same shape on the same
// machine are the same attempt; a smaller window, another KV type or a
// different card is a new one.
func vllmLoadShape(t infruntime.ModelTuning, kvCacheDType string, maxNumSeqs int) catalog.LoadShape {
	return catalog.LoadShape{
		ContextLength: t.ContextLength,
		KVCacheType:   kvCacheDType,
		NumParallel:   maxNumSeqs,
		Backend:       "cuda",
	}
}

// vllmLoadBlocked reports whether this computer has already recorded that
// this build does not start here in this shape.
func (p *agentInferenceProvider) vllmLoadBlocked(ctx context.Context, m catalog.Manifest, v catalog.Variant,
	shape catalog.LoadShape) (catalog.VariantLoadFailure, bool) {
	if p == nil || p.store == nil {
		return catalog.VariantLoadFailure{}, false
	}
	sha := activeVariantSHA(p.catalogManifests(), m.ModelID, v.VariantID)
	if sha == "" {
		return catalog.VariantLoadFailure{}, false
	}
	st, err := p.store.Load()
	if err != nil {
		return catalog.VariantLoadFailure{}, false
	}
	rec, ok := st.FailedLoads[sha]
	if !ok || !rec.Blocks(p.loadContextNow(ctx), shape) {
		return catalog.VariantLoadFailure{}, false
	}
	return rec, true
}

// recordVLLMLoadFailure keeps the fact that this build did not start here,
// and holds the engine off with the reason.
func (p *agentInferenceProvider) recordVLLMLoadFailure(ctx context.Context, m catalog.Manifest, v catalog.Variant,
	shape catalog.LoadShape, reason, detail string, kind string, engineMaxWindow int) {
	key, ok := p.noteVLLMLoadFailure(ctx, m, v, shape, reason, detail, kind, engineMaxWindow)
	if !ok {
		return
	}
	if kind == signer.LoadFailureMemory || kind == "" {
		p.parkVLLMForOutOfMemory(key, reason)
	} else {
		p.parkVLLMCannotStart(key, reason)
	}
	if p.logger != nil {
		p.logger.Warn("this computer could not start this model on vLLM; it will not be started again automatically",
			"model_id", m.ModelID, "variant_id", v.VariantID, "reason", reason,
			"context_length", shape.ContextLength, "kv_cache_type", shape.KVCacheType)
	}
}

// noteVLLMLoadFailure keeps the fact that this build did not start here and
// holds nothing off: what a start of a model no longer chosen leaves
// behind, so it is not started again to answer in the meantime
// (vllmStartable) while the model chosen now goes ahead (waired-agent#1515).
func (p *agentInferenceProvider) noteVLLMLoadFailure(ctx context.Context, m catalog.Manifest, v catalog.Variant,
	shape catalog.LoadShape, reason, detail string, kind string, engineMaxWindow int) (vllmBlockedLoad, bool) {
	if p == nil || p.store == nil {
		return vllmBlockedLoad{}, false
	}
	key := p.vllmBlockKey(m, v, shape)
	if key.SHA == "" {
		return vllmBlockedLoad{}, false // a failure it cannot key is not recorded (see onLoadMemoryFailure)
	}
	rec := catalog.VariantLoadFailure{
		ModelID:         m.ModelID,
		VariantID:       v.VariantID,
		Reason:          reason,
		Detail:          detail,
		Kind:            kind,
		EngineMaxWindow: engineMaxWindow,
		Context:         p.loadContextNow(ctx),
		Shape:           shape,
		FailedAt:        time.Now().UTC(),
	}
	if err := p.store.Update(func(s *catalog.State) {
		if s.FailedLoads == nil {
			s.FailedLoads = map[string]catalog.VariantLoadFailure{}
		}
		s.FailedLoads[key.SHA] = rec
	}); err != nil && p.logger != nil {
		p.logger.Warn("could not record a vLLM start that did not fit this computer", "model_id", m.ModelID, "err", err)
	}
	return key, true
}

// parkVLLMForOutOfMemory holds the vLLM engine off because the build in
// blocked does not fit here. The operator's own stop wins, as it does for
// ollama; the build is still remembered, so the notice names it.
func (p *agentInferenceProvider) parkVLLMForOutOfMemory(blocked vllmBlockedLoad, why string) {
	if p == nil {
		return
	}
	p.vllmBlocked.Store(&blocked)
	if p.parkedBecause() == parkCauseOperator {
		return
	}
	p.noteParked(parkCauseOutOfMemory)
	p.setVLLMParked(true)
	if p.logger != nil {
		p.logger.Warn("inference stopped: the chosen model does not fit this computer's GPU memory", "why", why)
	}
}

// parkVLLMCannotStart holds the vLLM engine off because the build in blocked
// cannot start on this computer at all — an architecture this engine does not
// know, a quantization this GPU is too old for, weights it cannot find
// (waired-ai/waired#1480). Held, reviewed and released exactly as a memory
// stop is (parkedForLoadFailure); only the words differ, so a surface never
// says "out of memory" about a model that did not run out of anything.
func (p *agentInferenceProvider) parkVLLMCannotStart(blocked vllmBlockedLoad, why string) {
	if p == nil {
		return
	}
	p.vllmBlocked.Store(&blocked)
	if p.parkedBecause() == parkCauseOperator {
		return
	}
	p.noteParked(parkCauseCannotStart)
	p.setVLLMParked(true)
	if p.logger != nil {
		p.logger.Warn("inference stopped: the chosen model cannot start on this computer", "why", why)
	}
}

// forgetVLLMLoadFailures drops every record for the model's builds: choosing
// the model again overrules them, and a person who chose a model did not
// choose a build of it.
func (p *agentInferenceProvider) forgetVLLMLoadFailures(m catalog.Manifest) {
	for _, v := range m.Variants {
		p.forgetLoadFailure(activeVariantSHA(p.catalogManifests(), m.ModelID, v.VariantID))
	}
}
