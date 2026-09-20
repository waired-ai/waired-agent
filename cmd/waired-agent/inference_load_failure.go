package main

import (
	"context"

	"github.com/waired-ai/waired-agent/internal/catalog"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// A build that this computer could not put in memory is a fact worth
// keeping. Before waired-agent#1453 nothing kept it: the load failed, the
// runner died, and the next trigger — a reconcile, a restart, a request
// through the gateway — loaded exactly the same weights again, on the same
// machine, with the same result. The product learned nothing, and on a
// unified-memory host each attempt put the machine back under the memory
// pressure that had already hung it twice (#1443).
//
// onLoadMemoryFailure is where both ways of finding out converge: the
// adapter classifying a dead runner as a memory failure (load_memory.go),
// and the load guard stopping a load before the host starves (load_guard.go).

// loadContextNow reads the facts that decide whether a past failure still
// describes this computer.
//
// Read at the moment of the failure rather than at the moment of the
// lookup: what has to be recorded is the machine the load failed ON.
func (p *agentInferenceProvider) loadContextNow(ctx context.Context) catalog.LoadContext {
	c := catalog.LoadContext{
		EngineKind:    string(p.servingEngine()),
		EngineVersion: p.servingEngineVersion(ctx),
	}
	if p.profiler != nil {
		prof := p.profiler.Profile(ctx)
		c.RAMTotalMB = prof.RAMTotalGB * 1024
		if len(prof.GPUs) > 0 {
			c.GPUModel = prof.GPUs[0].Model
			c.DriverVersion = prof.GPUs[0].DriverVersion
			c.VRAMTotalMB = prof.GPUs[0].VRAMTotalMB
		}
	}
	return c
}

// loadShapeNow is what the engine was asked for: the same tuple the
// residency warm-up keys its failure count on (warmLoadKey), so the two
// agree on what "the same load" means.
func (p *agentInferenceProvider) loadShapeNow() catalog.LoadShape {
	t := p.ollama.AppliedTuning()
	return catalog.LoadShape{
		ContextLength: t.ContextLength,
		KVCacheType:   t.KVCacheType,
		NumParallel:   t.NumParallel,
		Backend:       string(p.ollama.ResolvedBackend()),
	}
}

// onLoadMemoryFailure records that this build did not load on this computer.
//
// It does NOT demote the engine and does not charge the restart budget
// (owner direction, 2026-09-20): the engine is serving, and on the reference
// host it went on serving smaller models immediately afterwards. What failed
// is the choice of weights.
//
// A failure it cannot key is not recorded at all. The store is keyed by the
// variant's content digest, and filing a verdict under the wrong key would
// condemn a build that was never tried — the hazard
// TestBenchMeasurement_RefusesToGuessTheSubject exists for on the
// measurement side (waired-agent#784).
func (p *agentInferenceProvider) onLoadMemoryFailure(f infruntime.LoadMemoryFailure) {
	if p == nil || p.store == nil {
		return
	}
	ctx := p.backgroundCtx()
	sha := p.activeVariantSHA()
	modelID, variantID := p.activeModelID(), p.activeVariantID()
	if sha == "" || modelID == "" || variantID == "" {
		if p.logger != nil {
			p.logger.Warn("a model load ran out of memory, but there is no build to record it against",
				"reason", f.Reason, "model_id", modelID, "variant_id", variantID)
		}
		return
	}
	rec := catalog.VariantLoadFailure{
		ModelID:   modelID,
		VariantID: variantID,
		Reason:    f.Reason,
		Detail:    f.Detail,
		Context:   p.loadContextNow(ctx),
		Shape:     p.loadShapeNow(),
		FailedAt:  f.At.UTC(),
	}
	if err := p.store.Update(func(s *catalog.State) {
		if s.FailedLoads == nil {
			s.FailedLoads = map[string]catalog.VariantLoadFailure{}
		}
		s.FailedLoads[sha] = rec
	}); err != nil {
		if p.logger != nil {
			p.logger.Warn("could not record a model load that ran out of memory",
				"model_id", modelID, "err", err)
		}
		return
	}
	if p.logger != nil {
		p.logger.Warn("this computer could not load this model; it will not be loaded again automatically",
			"model_id", modelID, "variant_id", variantID,
			"reason", f.Reason, "engine_said", f.Detail,
			"context_length", rec.Shape.ContextLength, "kv_cache_type", rec.Shape.KVCacheType)
	}
}

// loadIsBlocked reports whether this host has already learned that the
// active build does not load here, under the configuration it would be
// loaded with now.
//
// Consulted before a load rather than after it, which is the whole point:
// the cost being avoided is the attempt itself.
func (p *agentInferenceProvider) loadIsBlocked() (catalog.VariantLoadFailure, bool) {
	if p == nil || p.store == nil {
		return catalog.VariantLoadFailure{}, false
	}
	sha := p.activeVariantSHA()
	if sha == "" {
		return catalog.VariantLoadFailure{}, false
	}
	st, err := p.store.Load()
	if err != nil {
		return catalog.VariantLoadFailure{}, false
	}
	rec, ok := st.FailedLoads[sha]
	if !ok {
		return catalog.VariantLoadFailure{}, false
	}
	if !rec.Blocks(p.loadContextNow(p.backgroundCtx()), p.loadShapeNow()) {
		return catalog.VariantLoadFailure{}, false
	}
	return rec, true
}
