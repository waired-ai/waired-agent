package main

import (
	"context"
	"sort"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/router"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/proto/signer"
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

// smallerAlternative names a model this computer could run instead of the
// one that did not load, or "" when the catalog has nothing to offer.
//
// "Smaller" is by estimated weight, because weight is what ran out. The
// step-down that already exists (router.FasterCandidate, #1400) answers a
// different question — which model is FASTER inside a time budget — and a
// host that cannot load the weights at all has no measurement to be fast or
// slow about. So this walks the same ranking, which is already restricted to
// what fits here, and takes the first lighter thing.
//
// Three exclusions, each for a reason the product has already learned:
//
//   - another build of the same model, because the failure was about these
//     weights and a sibling build is the tuning ladder's job, not this one;
//   - anything at least as heavy, which would be offering the same problem
//     under a different name;
//   - anything this host has ALREADY recorded as not loading, so a machine
//     cannot be walked down a list of models it has each tried once.
func (p *agentInferenceProvider) smallerAlternative(ctx context.Context, failed catalog.VariantLoadFailure) string {
	if p == nil || p.profiler == nil || p.store == nil {
		return ""
	}
	st, err := p.store.Load()
	if err != nil {
		return ""
	}
	ranked, err := router.RankModels(router.PickInput{
		Catalog:       p.manifests,
		Hardware:      p.profiler.Profile(ctx),
		Engine:        failed.Context.EngineKind,
		EngineVersion: failed.Context.EngineVersion,
		Measured:      measuredRatesFrom(st),
	})
	if err != nil {
		return ""
	}
	failedWeight := variantWeightGB(p.manifests, failed.ModelID, failed.VariantID)
	here, shape := p.loadContextNow(ctx), p.loadShapeNow()
	for _, c := range ranked {
		if c.Manifest.ModelID == failed.ModelID {
			continue
		}
		if failedWeight > 0 && c.Variant.EstimatedWeightGB >= failedWeight {
			continue
		}
		if sha := activeVariantSHA(p.manifests, c.Manifest.ModelID, c.Variant.VariantID); sha != "" {
			if rec, ok := st.FailedLoads[sha]; ok && rec.Blocks(here, shape) {
				continue
			}
		}
		return c.Manifest.ModelID
	}
	return ""
}

// variantWeightGB is the catalog's estimate of what a build weighs. 0 when
// the build is not in this catalog, which disables the size comparison
// rather than guessing at it.
func variantWeightGB(manifests []catalog.Manifest, modelID, variantID string) float64 {
	for _, m := range manifests {
		if m.ModelID != modelID {
			continue
		}
		for _, v := range m.Variants {
			if v.VariantID == variantID {
				return v.EstimatedWeightGB
			}
		}
	}
	return 0
}

// PublishedLoadFailures is what this host could not put in memory, in the
// shape the control plane reads (waired-agent#1453).
//
// The sibling of PublishedMeasurements, and it follows the same three rules.
// A record it cannot key is not published — the ledger writer already
// refuses those, and this is the second reader of that rule rather than a
// new one. The order is stable so an unchanged set pushes the same bytes
// every tick. And what travels is the FACTS of the attempt: neither the
// operator-facing sentence nor the engine's own words, because the engine's
// text names blob paths and other things particular to this machine, and a
// consumer ranking builds has no use for prose.
func (p *agentInferenceProvider) PublishedLoadFailures() []signer.ModelLoadFailure {
	if p == nil || p.store == nil {
		return nil
	}
	st, err := p.store.Load()
	if err != nil || len(st.FailedLoads) == 0 {
		return nil
	}
	out := make([]signer.ModelLoadFailure, 0, len(st.FailedLoads))
	for sha, f := range st.FailedLoads {
		if sha == "" || f.ModelID == "" || f.VariantID == "" {
			continue
		}
		out = append(out, signer.ModelLoadFailure{
			ModelID:       f.ModelID,
			VariantID:     f.VariantID,
			VariantSHA:    sha,
			EngineKind:    f.Context.EngineKind,
			EngineVersion: f.Context.EngineVersion,
			GPUModel:      f.Context.GPUModel,
			DriverVersion: f.Context.DriverVersion,
			VRAMTotalMB:   f.Context.VRAMTotalMB,
			RAMTotalMB:    f.Context.RAMTotalMB,
			ContextLength: f.Shape.ContextLength,
			KVCacheType:   f.Shape.KVCacheType,
			NumParallel:   f.Shape.NumParallel,
			Backend:       f.Shape.Backend,
			FailedAt:      f.FailedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	if len(out) == 0 {
		return nil
	}
	sort.Slice(out, func(i, j int) bool { return out[i].VariantSHA < out[j].VariantSHA })
	return out
}
