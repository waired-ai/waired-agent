package main

import (
	"context"
	"sort"
	"strings"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// buildChoice is the build and KV-cache type chosen with a model
// (waired-agent#1348): the in-process twin of the variant_id /
// kv_cache_type fields preferred-model.json carries. Empty fields are "no
// instruction" — see router.FamilyDefaultBuild and
// hostfit.ResolveKVCacheType for what that resolves to.
type buildChoice struct {
	ModelID     string
	VariantID   string
	KVCacheType string
}

// effectiveBuildChoice is the build choice every in-process reader should
// use: the one published with the last in-process switch when there has
// been one, the boot preference otherwise.
func (p *agentInferenceProvider) effectiveBuildChoice() buildChoice {
	if p.preferredOverride.Load() != nil {
		if b := p.preferredBuild.Load(); b != nil {
			return *b
		}
		return buildChoice{ModelID: p.effectivePreferredModelID()}
	}
	return buildChoice{
		ModelID:     p.cfg.PreferredModelID,
		VariantID:   p.cfg.PreferredVariantID,
		KVCacheType: p.cfg.PreferredKVCacheType,
	}
}

// chosenVariantFor is the build the effective choice names for modelID,
// or "" when the choice names none (or names another model).
func (p *agentInferenceProvider) chosenVariantFor(modelID string) string {
	c := p.effectiveBuildChoice()
	if c.VariantID == "" || c.ModelID == "" {
		return ""
	}
	if m, _, ok := catalog.ResolveModel(c.ModelID, p.manifests); !ok || m.ModelID != modelID {
		return ""
	}
	return c.VariantID
}

// resolvePullVariant is the build a pull of manifest fetches.
//
// requested is a build somebody named. It wins when the engine can load
// it; an unknown or unloadable name is logged and treated as no
// instruction rather than failing the pull, because the control plane
// already validated it against a catalog that may be newer or older than
// this one.
//
// With no instruction the rule is the one waired-agent#1348 settles, in
// order:
//
//  1. a model already on disk keeps the build it has. Re-resolving would
//     download another build of a model that is serving — the double
//     download rc7 found — and, on a host that stepped down to a lighter
//     build, pull the heavier one straight back;
//  2. otherwise router.FamilyDefaultBuild: the owner's default build
//     where this host is recommended it, the build it can hold where not.
//
// fallback is the engine-loadable choice PullModel already made (manifest
// order), returned when neither rule can answer — no profiler wired, or
// nothing fits — so the engine-too-old and blind paths keep their
// behaviour.
func (p *agentInferenceProvider) resolvePullVariant(
	ctx context.Context, manifest catalog.Manifest, engine, engineVersion, requested string,
	fallback catalog.Variant, st catalog.State,
) (v catalog.Variant, explicit bool) {
	// The same test FirstPullableVariant applies: the engine serves the
	// build, and a version floor needs a known version that clears it.
	loadable := func(v catalog.Variant) bool {
		return router.VariantLoadable(v, engine, engineVersion)
	}
	if requested != "" {
		if rv, ok := variantByID(manifest, requested); ok && loadable(rv) {
			return rv, true
		}
		p.logger.Warn("the requested build is unknown or cannot be loaded by this engine; choosing one as if none was named",
			"model", manifest.ModelID, "requested", requested, "engine", engine, "engine_version", engineVersion)
	}
	if ms, ok := st.Models[manifest.ModelID]; ok && ms.State == catalog.ModelStateReady {
		if cur, ok := variantByID(manifest, ms.VariantID); ok && loadable(cur) {
			return cur, false
		}
	}
	if p.profiler == nil {
		return fallback, false
	}
	best := router.FamilyDefaultBuild(manifest, engine, engineVersion, p.Hardware(ctx))
	if !best.Fits {
		return fallback, false
	}
	return best.Variant, false
}

// commitBuild makes variantID the build modelID's Models row serves,
// inside a store update, and reports whether the row now names it.
//
// A build that is already the row is a no-op. One held in StagedVariants
// (Ready) or RetainedVariants is moved into the row, and the build it
// replaces is kept in RetainedVariants when its weights are a different
// tag — they are still on disk, and the user is offered their removal
// rather than having them vanish or leak. Anything else (not downloaded,
// still downloading) leaves the row alone and answers false.
func commitBuild(s *catalog.State, modelID, variantID string) bool {
	cur, ok := s.Models[modelID]
	if !ok {
		return false
	}
	if variantID == "" || cur.VariantID == variantID {
		return cur.State == catalog.ModelStateReady
	}
	var next catalog.ModelState
	found := false
	if st, ok := s.StagedVariants[modelID]; ok && st.VariantID == variantID && st.State == catalog.ModelStateReady {
		next, found = st, true
		delete(s.StagedVariants, modelID)
	}
	if !found {
		kept := s.RetainedVariants[modelID][:0:0]
		for _, r := range s.RetainedVariants[modelID] {
			if !found && r.VariantID == variantID {
				next, found = r, true
				continue
			}
			kept = append(kept, r)
		}
		if found {
			setRetained(s, modelID, kept)
		}
	}
	if !found {
		return false
	}
	next.State = catalog.ModelStateReady
	next.Error = ""
	if cur.State == catalog.ModelStateReady && cur.OllamaTag != "" && cur.OllamaTag != next.OllamaTag {
		prev := cur
		prev.Error = ""
		setRetained(s, modelID, append(s.RetainedVariants[modelID], prev))
	}
	s.Models[modelID] = next
	for k, e := range s.Endpoints {
		if e.ModelID == modelID {
			e.VariantID = variantID
			s.Endpoints[k] = e
		}
	}
	return true
}

func setRetained(s *catalog.State, modelID string, rows []catalog.ModelState) {
	if len(rows) == 0 {
		delete(s.RetainedVariants, modelID)
		return
	}
	if s.RetainedVariants == nil {
		s.RetainedVariants = make(map[string][]catalog.ModelState)
	}
	s.RetainedVariants[modelID] = rows
}

// storedVariants is the device's StoredVariants report: every retained
// build, sorted so the published map does not churn.
func storedVariants(st catalog.State) []signer.StoredVariant {
	var out []signer.StoredVariant
	for modelID, rows := range st.RetainedVariants {
		for _, r := range rows {
			if r.VariantID == "" {
				continue
			}
			out = append(out, signer.StoredVariant{ModelID: modelID, VariantID: r.VariantID, SizeBytes: r.SizeBytes})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ModelID != out[j].ModelID {
			return out[i].ModelID < out[j].ModelID
		}
		return out[i].VariantID < out[j].VariantID
	})
	return out
}

// RemoveStoredVariants deletes the retained builds the user asked for
// (InferenceState.DesiredRemoveVariants, "<model_id>/<variant_id>").
//
// Only a retained build is ever removed: a name that is the served build,
// staged, or not on disk is skipped, which is what makes a stale entry
// harmless after the user switched back to that build. Weights another
// record still names are kept and only the record is dropped, as
// DeleteModel does. Idempotent — the entry stays in the map until the
// control plane sees it gone from StoredVariants.
func (p *agentInferenceProvider) RemoveStoredVariants(ctx context.Context, entries []string) {
	if p == nil || len(entries) == 0 {
		return
	}
	for _, e := range entries {
		modelID, variantID, ok := strings.Cut(e, "/")
		if !ok || modelID == "" || variantID == "" {
			continue
		}
		st, err := p.store.Load()
		if err != nil {
			return
		}
		var row catalog.ModelState
		found := false
		for _, r := range st.RetainedVariants[modelID] {
			if r.VariantID == variantID {
				row, found = r, true
				break
			}
		}
		if !found {
			continue
		}
		if row.OllamaTag != "" && p.puller != nil {
			if shared := tagHolders(st, row.OllamaTag, modelID, variantID); len(shared) > 0 {
				p.logger.Info("stored build's record removed; weights kept, another record names the tag",
					"model", modelID, "variant", variantID, "tag", row.OllamaTag, "shared_with", shared)
			} else if err := p.puller.Remove(ctx, row.OllamaTag); err != nil {
				p.logger.Warn("deleting a stored build's weights failed; keeping its record",
					"model", modelID, "variant", variantID, "tag", row.OllamaTag, "err", err)
				continue
			}
		}
		if err := p.store.Update(func(s *catalog.State) {
			kept := s.RetainedVariants[modelID][:0:0]
			for _, r := range s.RetainedVariants[modelID] {
				if r.VariantID != variantID {
					kept = append(kept, r)
				}
			}
			setRetained(s, modelID, kept)
		}); err != nil {
			p.logger.Warn("dropping a removed build's record failed", "model", modelID, "variant", variantID, "err", err)
			continue
		}
		p.logger.Info("stored build removed", "model", modelID, "variant", variantID, "tag", row.OllamaTag)
	}
}

// tagHolders lists the records other than (modelID, variantID) — served,
// staged or retained — whose weights are tag.
func tagHolders(st catalog.State, tag, modelID, variantID string) []string {
	var out []string
	for id, m := range st.Models {
		if m.OllamaTag == tag && !(id == modelID && m.VariantID == variantID) {
			out = append(out, id)
		}
	}
	for id, m := range st.StagedVariants {
		if m.OllamaTag == tag && !(id == modelID && m.VariantID == variantID) {
			out = append(out, id+"/"+m.VariantID)
		}
	}
	for id, rows := range st.RetainedVariants {
		for _, m := range rows {
			if m.OllamaTag == tag && !(id == modelID && m.VariantID == variantID) {
				out = append(out, id+"/"+m.VariantID)
			}
		}
	}
	sort.Strings(out)
	return out
}

// buildOnDisk reports whether variantID of modelID is downloaded but not
// the row's build: staged and Ready, or retained.
func buildOnDisk(st catalog.State, modelID, variantID string) bool {
	if sv, ok := st.StagedVariants[modelID]; ok && sv.VariantID == variantID && sv.State == catalog.ModelStateReady {
		return true
	}
	for _, r := range st.RetainedVariants[modelID] {
		if r.VariantID == variantID {
			return true
		}
	}
	return false
}

// inFlightPull is the job currently fetching modelID, or nil.
func (p *agentInferenceProvider) inFlightPull(modelID string) *pullJob {
	p.pullMu.Lock()
	defer p.pullMu.Unlock()
	return p.pullsInFlight[modelID]
}

// ActiveBuild is the served build of the active model and the KV-cache
// type the engine was started with, for InferenceState.ActiveVariantID /
// ActiveKVCacheType. "" for either when not known.
func (p *agentInferenceProvider) ActiveBuild() (variantID, kvCacheType string) {
	if p == nil || p.store == nil {
		return "", ""
	}
	if st, err := p.store.Load(); err == nil && st.Active != nil {
		variantID = st.Active.VariantID
	}
	switch p.servingEngine() {
	case catalog.RuntimeOllama:
		if p.ollama != nil {
			kvCacheType = p.ollama.AppliedTuning().KVCacheType
		}
	case catalog.RuntimeVLLM:
		kvCacheType = p.vllmServedKVCacheType()
	}
	return variantID, kvCacheType
}

// StoredVariants is the InferenceState.StoredVariants report.
func (p *agentInferenceProvider) StoredVariants() []signer.StoredVariant {
	if p == nil || p.store == nil {
		return nil
	}
	st, err := p.store.Load()
	if err != nil {
		return nil
	}
	return storedVariants(st)
}

// ApplyRemoveStoredVariants runs RemoveStoredVariants for a frame's list
// off the network-map loop, one run at a time: a removal shells out to
// the engine, and the list arrives again on the next frame anyway.
func (p *agentInferenceProvider) ApplyRemoveStoredVariants(ctx context.Context, entries []string) {
	if p == nil || len(entries) == 0 {
		return
	}
	st, err := p.store.Load()
	if err != nil || len(st.RetainedVariants) == 0 {
		return
	}
	if !p.removeInFlight.CompareAndSwap(false, true) {
		return
	}
	list := append([]string(nil), entries...)
	go func() {
		defer p.removeInFlight.Store(false)
		p.RemoveStoredVariants(ctx, list)
	}()
}

// vllmServedKVCacheType is the KV-cache type the running vLLM engine was
// started with: fp8 when it was given --kv-cache-dtype fp8, fp16 when it
// was left at the model dtype, "" when no engine has been started.
func (p *agentInferenceProvider) vllmServedKVCacheType() string {
	a, ok := p.vllmAdapter().(interface{ CommandArgsForDiagnostics() []string })
	if !ok {
		return ""
	}
	args := a.CommandArgsForDiagnostics()
	if len(args) == 0 {
		return ""
	}
	for i, arg := range args {
		if arg == "--kv-cache-dtype" && i+1 < len(args) && strings.HasPrefix(args[i+1], "fp8") {
			return catalog.KVCacheFP8
		}
	}
	return catalog.KVCacheFP16
}
