package router

import (
	"github.com/waired-ai/waired-agent/internal/catalog"
)

// lookupByEngineModel resolves a single ENGINE-NATIVE model identifier
// — an ollama tag ("qwen3:8b-q4_K_M") or a vLLM repo id
// ("Qwen/Qwen3-Coder-30B-A3B-Instruct-AWQ") — back to its manifest.
//
// It answers "what did this name come from?", which is the question the
// SERVING side of a peer hop has to answer: the consumer rewrote the
// proxied body's `model` field to Selection.EngineModel, and that is an
// engine identifier, not a catalog alias (#107).
//
// Both engine namespaces are searched because a bare model name carries
// no engine hint; they do not collide in practice (an ollama tag has a
// ":", a repo id has a "/"). Addressability mirrors variantWantSets
// exactly — Source.Tag names an ollama-capable variant, Source.RepoID a
// vLLM-capable one — so this resolves precisely the set of names a peer
// can advertise and nothing more.
//
// Two manifests claiming the same identifier is a catalog defect;
// resolution stays deterministic regardless of slice order by taking
// the strongest variant (ParamCount x QuantizationTier, the scale mesh
// candidate ordering uses) with ModelID as the tie-break.
func lookupByEngineModel(name string, manifests []catalog.Manifest) (catalog.Manifest, bool) {
	if name == "" {
		return catalog.Manifest{}, false
	}
	var (
		best      catalog.Manifest
		bestScore int64 = -1
		found     bool
	)
	for _, m := range manifests {
		for _, v := range m.Variants {
			if !variantAddressableAs(v, name) {
				continue
			}
			score := int64(v.ParamCount) * int64(v.QuantizationTier)
			if !found || score > bestScore || (score == bestScore && m.ModelID < best.ModelID) {
				best, bestScore, found = m, score, true
			}
		}
	}
	return best, found
}

// variantAddressableAs reports whether a peer running v can advertise
// name, applying the same rule variantWantSets builds its maps from.
// Callers must reject an empty name first: an unset Source field would
// otherwise match it.
func variantAddressableAs(v catalog.Variant, name string) bool {
	if supports(v.RuntimeSupport, catalog.RuntimeOllama) && v.Source.Tag == name {
		return true
	}
	return supports(v.RuntimeSupport, catalog.RuntimeVLLM) && v.Source.RepoID == name
}
