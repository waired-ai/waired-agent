package router

import (
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
)

// ResolveModelForPeer maps "whatever model peer deviceID is serving"
// back to a catalog manifest, for surfaces that must route an
// unresolvable client model id (e.g. Claude Code's claude-* ids) to a
// specific mesh node (#647): the request can only succeed if it is
// resolved to a model that node actually advertises.
//
// The peer's advertised engine identifiers (InferenceState.Models) are
// intersected with each manifest's engine-native want set
// (variantWantSets — ollama Source.Tag / vllm Source.RepoID, keyed by
// InferenceState.Type like buildMeshCandidates). When several catalog
// models match, the strongest wins — score ParamCount×QuantizationTier,
// the same scale mesh candidate ordering uses — with ModelID as the
// deterministic tie-break, because the tiering use case is "main loop
// on the strong model of that node".
//
// Returns ok=false when the peer is absent, stale, unreachable, engine
// "none"/unknown, or advertises nothing in the catalog. Reachability
// here is snapshot-level only; the selection layer still applies the
// disco-prober and admission checks per request.
func ResolveModelForPeer(manifests []catalog.Manifest, snap inferencemesh.Snapshot, deviceID string) (catalog.Manifest, bool) {
	var peer *inferencemesh.PeerView
	for i := range snap.Peers {
		if snap.Peers[i].DeviceID == deviceID {
			peer = &snap.Peers[i]
			break
		}
	}
	if peer == nil || peer.InferenceState == nil || !peer.InferenceState.Reachable || peer.Stale {
		return catalog.Manifest{}, false
	}
	kind := peer.InferenceState.Type
	if kind == "" {
		kind = catalog.RuntimeOllama
	}
	if kind != catalog.RuntimeOllama && kind != catalog.RuntimeVLLM {
		return catalog.Manifest{}, false
	}

	var (
		best      catalog.Manifest
		bestScore int64 = -1
		found     bool
	)
	for _, m := range manifests {
		wantOllama, wantVLLM := variantWantSets(m)
		want := wantOllama
		if kind == catalog.RuntimeVLLM {
			want = wantVLLM
		}
		for _, adv := range peer.InferenceState.Models {
			v, ok := want[adv]
			if !ok {
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

// lookupByEngineModel resolves a single ENGINE-NATIVE model identifier
// — an ollama tag ("qwen3:8b-q4_K_M") or a vLLM repo id
// ("Qwen/Qwen3-Coder-30B-A3B-Instruct-AWQ") — back to its manifest.
//
// Where ResolveModelForPeer answers "what is that node serving?", this
// answers "what did this name come from?", which is the question the
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
