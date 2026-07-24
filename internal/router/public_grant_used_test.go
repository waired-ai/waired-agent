package router

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
)

// selectorReportingGrants builds a mesh-only Selector (local model not
// ready, so every Select falls through to the mesh) that records every
// grant ID reported used via OnPublicGrantUsed. tracker may be nil.
func selectorReportingGrants(t *testing.T, tracker *InFlightTracker, peers ...inferencemesh.PeerView) (*Selector, *[]string) {
	t.Helper()
	var used []string
	snap := inferencemesh.Snapshot{Peers: peers}
	s := NewSelector(Inputs{
		Manifests:         []catalog.Manifest{qwenTier(50)},
		LocalState:        emptyState(),
		Hardware:          goodHardware(),
		Runtimes:          registryWithOllama(),
		LocalInFlight:     tracker,
		MeshSnapshotFn:    func() inferencemesh.Snapshot { return snap },
		PublicPolicyFn:    func() PublicPolicy { return allowAll() },
		OnPublicGrantUsed: func(grantID string) { used = append(used, grantID) },
	})
	return s, &used
}

// TestCommit_PublicCandidateReportsGrantUsed: committing a request to a
// Public Share provider reports the grant behind it, which is the only
// signal the acquirer has that the grant is carrying traffic (waired#898).
func TestCommit_PublicCandidateReportsGrantUsed(t *testing.T) {
	peer := mkPublicPeer(publicPeerDeviceID, publicPeerAlias, "qwen3:8b-q4_K_M") // Grant.ID = grant_test0001
	s, used := selectorReportingGrants(t, nil, peer)

	cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 3)
	if err != nil || len(cands) == 0 {
		t.Fatalf("SelectK: err=%v cands=%d", err, len(cands))
	}
	if _, ok := cands[0].Commit(); !ok {
		t.Fatal("Commit on an uncontended public candidate should succeed")
	}
	if len(*used) != 1 || (*used)[0] != "grant_test0001" {
		t.Fatalf("OnPublicGrantUsed = %v, want [grant_test0001]", *used)
	}
}

// TestCommit_OwnNetworkCandidateDoesNotReport: an own-network peer carries
// no grant, so committing to it must never call OnPublicGrantUsed.
func TestCommit_OwnNetworkCandidateDoesNotReport(t *testing.T) {
	peer := mkPeer("peer-own", "qwen3:8b-q4_K_M", true, false) // Grant == nil
	s, used := selectorReportingGrants(t, nil, peer)

	cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 3)
	if err != nil || len(cands) == 0 {
		t.Fatalf("SelectK: err=%v cands=%d", err, len(cands))
	}
	if _, ok := cands[0].Commit(); !ok {
		t.Fatal("Commit on an own-network candidate should succeed")
	}
	if len(*used) != 0 {
		t.Fatalf("own-network commit reported grants %v, want none", *used)
	}
}

// TestCommit_PublicCandidateAtCapacityDoesNotReport: the grant is "used"
// only when admission actually succeeds. A candidate that loses its slot
// between SelectK and Commit returns ok=false and must not be counted —
// otherwise a rejected request would keep a grant alive it never used.
func TestCommit_PublicCandidateAtCapacityDoesNotReport(t *testing.T) {
	peer := mkPublicPeer(publicPeerDeviceID, publicPeerAlias, "qwen3:8b-q4_K_M")
	peer.InferenceState.Capacity = 1
	tracker := NewInFlightTracker()
	s, used := selectorReportingGrants(t, tracker, peer)

	// SelectK while the peer still has its one slot free.
	cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 3)
	if err != nil || len(cands) == 0 {
		t.Fatalf("SelectK: err=%v cands=%d", err, len(cands))
	}
	// Saturate the peer AFTER SelectK, so the admission pre-filter didn't
	// drop it — the loss happens at Commit, the path under test.
	if _, ok := tracker.Acquire(publicPeerDeviceID, 1); !ok {
		t.Fatal("filling the single slot should succeed")
	}
	if _, ok := cands[0].Commit(); ok {
		t.Fatal("Commit should fail once the peer is at capacity")
	}
	if len(*used) != 0 {
		t.Fatalf("a failed Commit reported grants %v, want none", *used)
	}
}
