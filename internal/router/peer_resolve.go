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
