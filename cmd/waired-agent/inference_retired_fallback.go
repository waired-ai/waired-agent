package main

import (
	"context"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/router"
)

// resolveWrittenModel is catalog.ResolveModel for a name that was WRITTEN
// DOWN before this build: preferred-model.json, agent.json's bundled pin.
// A live name and a name retired to a successor resolve exactly as
// ResolveModel resolves them.
//
// A name retired with no successor (gpt-oss, docs/decisions/20260916/0340
// decision 4) falls back to the model this host would be given now — the
// same PickModel answer a fresh install gets, on the engine this host
// serves with. That is the no-successor counterpart of a named successor:
// the file outlives the catalog it was written against, its author is not
// present to edit it, and "no model" would leave the host downloading
// nothing and routing nothing while the engine still holds the old
// weights.
//
// An instruction given NOW — a pull, a model switch, a request naming the
// model — does not come through here. It is refused with
// catalog.RetirementRefusal, because a person or a script is there to
// choose another model.
func (p *agentInferenceProvider) resolveWrittenModel(name string) (catalog.Manifest, catalog.Retirement, bool) {
	m, r, ok := catalog.ResolveModel(name, p.catalogManifests())
	if ok || len(r.Names) == 0 || catalog.HasSuccessor(r) {
		return m, r, ok
	}
	rec, found := p.recommendedModelHere()
	if !found {
		return catalog.Manifest{}, r, false
	}
	p.logRetiredFallbackOnce(name, rec.ModelID, r)
	return rec, r, true
}

// recommendedModelHere is PickModel over the offered catalog for this host
// and the engine it serves with. ok=false when nothing fits.
func (p *agentInferenceProvider) recommendedModelHere() (catalog.Manifest, bool) {
	offered := make([]catalog.Manifest, 0, len(p.catalogManifests()))
	for _, m := range p.catalogManifests() {
		if m.InternalOnly == "" {
			offered = append(offered, m)
		}
	}
	ctx := context.Background()
	var hw hardware.Profile
	if p.profiler != nil {
		hw = p.profiler.Profile(ctx)
	}
	pick, err := router.PickModel(router.PickInput{
		Catalog:       offered,
		Hardware:      hw,
		Engine:        p.servingEngine(),
		EngineVersion: p.servingEngineVersion(ctx),
	})
	if err != nil || pick.Manifest.ModelID == "" {
		return catalog.Manifest{}, false
	}
	return pick.Manifest, true
}

// logRetiredFallbackOnce reports a written name's fallback the first time
// it is resolved per name. The resolvers run on every pull, activation and
// status read, the same reason bundledRetirementLogged exists.
func (p *agentInferenceProvider) logRetiredFallbackOnce(name, used string, r catalog.Retirement) {
	if _, seen := p.retiredFallbackLogged.LoadOrStore(name, true); seen {
		return
	}
	p.logger.Info(catalog.RecommendedInsteadNotice(name, used), "reason", r.Reason)
}
