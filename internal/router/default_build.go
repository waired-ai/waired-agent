package router

import (
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// FamilyDefaultBuild is the build of m this host serves when somebody
// names the model and not a build: the tray's one row per model,
// `waired models use`, a control-plane instruction with no variant.
//
// The owner hand-picks a default build per model and engine
// (manifest.default_variant; decision 6 of
// docs/decisions/20260913/2355-catalog-variant-kv-and-residency-rulings.md),
// and it is the answer whenever this host is RECOMMENDED it. Where it is
// not — a 22.6 GB default on a 16 GB card — the answer stays the build
// FamilyBestFit picks, which is the lighter one the host can hold:
// taking the default there would download weights the card cannot keep
// and spill them into system RAM, the incident waired#986 is about and
// waired-agent#1265 fixed. Decision 7 keeps those lighter builds in the
// recommendation for exactly that band.
//
// A manifest with no default for the engine, or a default the engine
// cannot load, is FamilyBestFit's answer unchanged.
func FamilyDefaultBuild(m catalog.Manifest, engine, engineVersion string, hw hardware.Profile) FamilyFit {
	best := FamilyBestFit(m, engine, engineVersion, hw)
	id := m.DefaultVariant[engine]
	if id == "" || !best.Fits || best.Variant.VariantID == id {
		return best
	}
	for _, v := range m.Variants {
		if v.VariantID != id {
			continue
		}
		if !engineSupports(v, engine) || !engineVersionSatisfies(v, engineVersion) || !hostFits(engine, m, v, hw) {
			return best
		}
		if !recommendedOnHost(m, v, engine, hw) {
			return best
		}
		return FamilyFit{Variant: v, Fits: true, Fit: familyPresentation(m, v, engine, hw)}
	}
	return best
}

// recommendedOnHost is preferRecommended's predicate for one build.
func recommendedOnHost(m catalog.Manifest, v catalog.Variant, engine string, hw hardware.Profile) bool {
	switch engine {
	case catalog.RuntimeOllama:
		return hostfit.OllamaRecommendModel(m, v, hw.HostFit()).Fits
	case catalog.RuntimeVLLM:
		return hostfit.VLLMRecommendModelOnHost(m, v, hw.HostFit(), hw.GPUSummaries()).Fits
	}
	return false
}

// FamilyBuildFit is the FamilyFit of one named build of m, for a surface
// that describes the build a host is actually serving rather than the one
// it would choose (decision 5 of
// docs/decisions/20260913/2355-catalog-variant-kv-and-residency-rulings.md:
// the running row is the served build).
func FamilyBuildFit(m catalog.Manifest, v catalog.Variant, engine, engineVersion string, hw hardware.Profile) FamilyFit {
	return FamilyFit{
		Variant: v,
		Fits:    VariantLoadable(v, engine, engineVersion) && hostFits(engine, m, v, hw),
		Fit:     familyPresentation(m, v, engine, hw),
	}
}
