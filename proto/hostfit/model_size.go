package hostfit

import (
	"sync"

	"github.com/waired-ai/waired-agent/proto/catalog"
)

// Model size classes — which class of graphics card runs this model.
//
// They exist because quality_tier stopped being showable. #518 redefined
// it as the parameter ordering of the generations we carry, corrected by
// a handful of cited overrides: 10·log10(params) − 5·log10(footprint_mb).
// That is arithmetic over two catalog fields, not a measurement, and a
// two-digit number labelled "quality" invites a reading the data does
// not support (#537).
//
// The obvious replacement — bucketing the tier itself — is incoherent
// for the same reason: a threshold on a number we just called
// unfounded inherits its footing. So the classes rest on nothing the
// tier touches. They are a fact about hardware, and the surfaces that
// already answer hardware questions per-host keep doing so; this one
// answers the question those cannot, which is what a machine you cannot
// see is running.
//
// The earlier ruling was the opposite — "the raw number rather than a
// coarse scale (owner decision): any re-bucketing would have to be kept
// in step with the catalog forever", recorded on the setup wizard's
// qualityNote and on the tray's catalogSpecSuffix. That objection holds
// against a class somebody AUTHORS. It does not hold here: nothing below
// is written down per model. The classes are computed from the weight
// annotation the manifest already carries, so there is nothing to keep
// in step.
//
// small / medium / large rather than low / medium / high: the claim is
// size, and low/high would smuggle back the good-or-bad reading the
// number was removed for. It is also the established triple for model
// scale.
const (
	ModelSizeSmall  = "small"
	ModelSizeMedium = "medium"
	ModelSizeLarge  = "large"
)

// The two reference machines. Ordinary consumer graphics cards, one at
// each end of what people actually buy — and the boundaries land in
// gaps rather than through a crowd: as of 2026-08 the catalog holds
// nothing between 24,873 MiB and 48,721 MiB, so the 32 GB line has 96 %
// of clear air around it, and the 8 GB line has 24 %.
//
// A card, not a host. Waired's own capacity rule counts RAM and
// dedicated VRAM together, which is the right question for "will this
// run on YOUR machine" and the wrong one here: a class has to mean the
// same thing on every machine, or a filter written on one host means
// something else on the next.
const (
	ModelSizeSmallCardMB  = 8 * 1024
	ModelSizeMediumCardMB = 32 * 1024
)

// VariantSize classifies one variant by what its WEIGHTS need in
// GPU-addressable memory — OllamaWeightsResidentMB, the term the
// recommendation gate itself compares.
//
// Reusing that function rather than composing a fresh formula is what
// gives the boundaries a meaning inside the product: a variant is
// "small" exactly when the recommendation gate would pass it on an 8 GB
// card. Weights and not the window, for the reason that function
// documents — KV that does not fit is a window the tuning clamps, while
// weights that do not fit are re-read from system RAM on every decode
// step, and that is the one an operator feels.
//
// Discrete overhead on both reference machines: they are graphics
// cards. The unified-memory arm would price the same card lower and
// make the class depend on a host the class is defined not to have.
//
// Empty for a variant with no weight annotation. That is "unknown", and
// no caller may read it as small — SizeRank keeps it below every real
// class so a floor excludes it, matching what BestTier's zero already
// means for the tier.
func VariantSize(v catalog.Variant) string {
	mb := OllamaWeightsResidentMB(v, false)
	switch {
	case mb <= 0:
		return ""
	case mb <= ModelSizeSmallCardMB:
		return ModelSizeSmall
	case mb <= ModelSizeMediumCardMB:
		return ModelSizeMedium
	default:
		return ModelSizeLarge
	}
}

// ModelSize is the class a catalog ROW carries: the smallest card that
// can run this model at all, i.e. the class of its lightest variant.
//
// The row is a family and the variant is ours to pick — every picker
// renders one line per manifest and resolves the variant behind it from
// the host. Classifying by the heaviest build would tell a reader a
// model is out of reach when the build we would actually give them
// fits, which is the direction that costs someone a model they could
// have run.
//
// Empty when no variant carries a weight annotation; see VariantSize.
func ModelSize(m catalog.Manifest) string {
	best := ""
	for _, v := range m.Variants {
		s := VariantSize(v)
		if s == "" {
			continue
		}
		if best == "" || SizeRank(s) < SizeRank(best) {
			best = s
		}
	}
	return best
}

// SizeRank orders the classes so callers can compare without re-deriving
// the vocabulary. Unknown is 0 and sorts below every real class, which
// makes `SizeRank(got) >= SizeRank(floor)` fail closed on a model whose
// footprint we cannot price.
func SizeRank(size string) int {
	switch size {
	case ModelSizeSmall:
		return 1
	case ModelSizeMedium:
		return 2
	case ModelSizeLarge:
		return 3
	default:
		return 0
	}
}

// BestSize resolves the largest size class an inference endpoint
// advertises, given its engine type and the raw model names it reports.
// The sibling of catalog.BestTier, with the same matching convention
// (ollama names match Variant.Source.Tag, vLLM names match
// Variant.Source.RepoID) and the same degradation: unresolvable input
// returns "" rather than an error.
//
// It resolves the VARIANT, not the family. This answers what a peer is
// running right now, and a model shipping both a light and a heavy
// build would otherwise be reported at the light one on a machine
// serving the heavy one.
//
// It lives here rather than beside BestTier because the classification
// does: proto/catalog cannot import proto/hostfit.
func BestSize(engineType string, models []string) string {
	return BestSizeIn(cachedBundled(), engineType, models)
}

// BestSizeIn is BestSize over an explicit manifest set (tests, or a
// caller that already loaded its own manifests — the agent's router
// holds the set it was configured with and must not silently answer
// from a different one).
func BestSizeIn(manifests []catalog.Manifest, engineType string, models []string) string {
	if len(models) == 0 || (engineType != catalog.RuntimeOllama && engineType != catalog.RuntimeVLLM) {
		return ""
	}
	want := make(map[string]bool, len(models))
	for _, m := range models {
		if m != "" {
			want[m] = true
		}
	}
	best := ""
	for _, mf := range manifests {
		for _, v := range mf.Variants {
			id := v.Source.Tag
			if engineType == catalog.RuntimeVLLM {
				id = v.Source.RepoID
			}
			if id == "" || !want[id] {
				continue
			}
			if !variantSupportsRuntime(v, engineType) {
				continue
			}
			if s := VariantSize(v); SizeRank(s) > SizeRank(best) {
				best = s
			}
		}
	}
	return best
}

func variantSupportsRuntime(v catalog.Variant, runtime string) bool {
	for _, s := range v.RuntimeSupport {
		if s == runtime {
			return true
		}
	}
	return false
}

// cachedBundled mirrors the catalog package's own once-loader, for the
// same reasons: a decode failure of the embedded catalog would be a
// build defect, so degrade to "unknown" rather than panicking in a
// server hot path, and include internal models because this resolves
// names a device is ALREADY serving.
var cachedBundled = sync.OnceValue(func() []catalog.Manifest {
	ms, err := catalog.BundledManifestsIncludingInternal()
	if err != nil {
		return nil
	}
	return ms
})
