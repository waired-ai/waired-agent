package router

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
)

// TestSelectK_CandidateCarriesMeasuredRTT is the router half of the
// waired-agent#624 fix. The gateway sizes each readiness-probe round from
// how far away the peers are, and the Selector is the only layer holding
// that measurement — so it has to reach the Candidate.
//
// Record of today's behaviour: the wire shape of Candidate.RTTMS is not a
// published contract, only an internal hand-off between the two layers.
func TestSelectK_CandidateCarriesMeasuredRTT(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			mkPeer("peer-measured", "qwen3:8b-q4_K_M", true, false),
			mkPeer("peer-unmeasured", "qwen3:8b-q4_K_M", true, false),
		},
	}
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
		// Only one of the two peers has ever produced a matched pong,
		// which is what a relay-only peer also looks like.
		LocalRTT: func() map[string]uint32 { return map[string]uint32{"peer-measured": 52} },
	})

	cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 3)
	if err != nil {
		t.Fatalf("SelectK: %v", err)
	}
	got := map[string]uint32{}
	for _, c := range cands {
		got[c.PeerID] = c.RTTMS
	}
	if got["peer-measured"] != 52 {
		t.Errorf("measured peer RTTMS = %d, want 52", got["peer-measured"])
	}
	// Not 0: a consumer scaling a wait by RTT must be able to tell "one
	// millisecond away" from "never measured", and 0 would read as the
	// closest possible peer.
	if got["peer-unmeasured"] != RTTUnknown {
		t.Errorf("unmeasured peer RTTMS = %d, want RTTUnknown (%d)", got["peer-unmeasured"], uint32(RTTUnknown))
	}
}
