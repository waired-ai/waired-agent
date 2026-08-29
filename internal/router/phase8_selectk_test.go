package router

import (
	"errors"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// TestSelectK_LocalReadyReturnsOneCandidate exercises the local-fast-
// path: when the catalog state reports the model is local-ready, the
// Selector skips mesh fallback entirely and returns a single candidate
// the gateway can commit without probing.
func TestSelectK_LocalReadyReturnsOneCandidate(t *testing.T) {
	s := NewSelector(Inputs{
		Manifests:  []catalog.Manifest{qwen()},
		LocalState: readyState(),
		Hardware:   goodHardware(),
		Runtimes:   registryWithOllama(),
	})
	cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 3)
	if err != nil {
		t.Fatalf("SelectK: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("local-ready must return exactly 1 candidate; got %d", len(cands))
	}
	if cands[0].ExecutionMode != "local" {
		t.Errorf("ExecutionMode = %q, want %q", cands[0].ExecutionMode, "local")
	}
	if cands[0].PeerID != "" {
		t.Errorf("local candidate must have empty PeerID; got %q", cands[0].PeerID)
	}
	sel, ok := cands[0].Commit()
	if !ok {
		t.Fatal("local Candidate.Commit returned ok=false; local should always commit")
	}
	if sel.Runtime != catalog.RuntimeOllama {
		t.Errorf("Selection.Runtime = %q, want %q", sel.Runtime, catalog.RuntimeOllama)
	}
	if sel.Release == nil {
		t.Error("Selection.Release must be non-nil even for local (no-op)")
	}
}

// TestSelectK_RemoteOnlyReturnsKRankedCandidates verifies the multi-
// candidate path. With three peers carrying the same model and no
// admission constraint, SelectK returns all three ordered by score-
// desc / deviceID-asc. The gateway will probe them in parallel.
func TestSelectK_RemoteOnlyReturnsKRankedCandidates(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			mkPeer("peer-C", "qwen3:8b-q4_K_M", true, false),
			mkPeer("peer-A", "qwen3:8b-q4_K_M", true, false),
			mkPeer("peer-B", "qwen3:8b-q4_K_M", true, false),
		},
	}
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
	})
	cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 3)
	if err != nil {
		t.Fatalf("SelectK: %v", err)
	}
	if len(cands) != 3 {
		t.Fatalf("3 eligible peers → want 3 candidates, got %d", len(cands))
	}
	wantOrder := []string{"peer-A", "peer-B", "peer-C"}
	for i, want := range wantOrder {
		if cands[i].PeerID != want {
			t.Errorf("cands[%d].PeerID = %q, want %q (deviceID-asc tie-break)", i, cands[i].PeerID, want)
		}
		if cands[i].ExecutionMode != "remote" {
			t.Errorf("cands[%d].ExecutionMode = %q, want %q", i, cands[i].ExecutionMode, "remote")
		}
	}
}

// TestSelectK_KLargerThanEligibleReturnsAllAvailable confirms K is a
// ceiling, not a floor — asking for K=10 when only 2 peers match
// returns 2 candidates. The Phase 8 probe coordinator handles fewer-
// than-K probes naturally.
func TestSelectK_KLargerThanEligibleReturnsAllAvailable(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			mkPeer("peer-A", "qwen3:8b-q4_K_M", true, false),
			mkPeer("peer-B", "qwen3:8b-q4_K_M", true, false),
		},
	}
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
	})
	cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 10)
	if err != nil {
		t.Fatalf("SelectK: %v", err)
	}
	if len(cands) != 2 {
		t.Errorf("len(cands) = %d, want 2 (K=10 but only 2 eligible)", len(cands))
	}
}

// TestSelectK_StickyBoundPeerLeadsCandidates exercises the affinity-
// first ordering. With sticky binding to peer-Z, that peer must appear
// at position 0 even though score-sort + deviceID-asc would put peer-A
// first.
func TestSelectK_StickyBoundPeerLeadsCandidates(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			mkPeer("peer-A", "qwen3:8b-q4_K_M", true, false),
			mkPeer("peer-Z", "qwen3:8b-q4_K_M", true, false),
		},
	}
	sticky := NewStickyStore(time.Minute, time.Now)
	sticky.Touch("conv-1", "peer-Z")

	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
		Sticky:         sticky,
	})
	cands, err := s.SelectK(t.Context(), Request{Model: "waired/default", StickyID: "conv-1"}, 3)
	if err != nil {
		t.Fatalf("SelectK: %v", err)
	}
	if len(cands) < 1 || cands[0].PeerID != "peer-Z" {
		t.Errorf("cands[0].PeerID = %q, want %q (sticky-first)", first(cands), "peer-Z")
	}
}

// TestSelectK_SilentPeerDemotedNotExcluded inverts the original Phase 8
// contract (this test used to be TestSelectK_LocalReachableHardExcludes
// and asserted the opposite). PRODUCT CONTRACT, waired#729.
//
// A disco pong travels raw UDP or the relay's WSS control session; an
// inference request travels the WireGuard data plane. Silence on the
// former does not prove the latter is down, and the cost of being
// wrong is asymmetric: a wrong exclusion fails the request with no
// in-request recovery, a wrong inclusion costs one ~50 ms /healthz
// probe that runs in parallel and spins no GPU. So silence orders the
// candidate last; the probe layer is what decides.
func TestSelectK_SilentPeerDemotedNotExcluded(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			mkSilentPeer("peer-B", "qwen3:8b-q4_K_M"), // disco heard nothing recently
			mkPeer("peer-A", "qwen3:8b-q4_K_M", true, false),
			mkPeer("peer-C", "qwen3:8b-q4_K_M", true, false),
		},
	}
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
	})
	cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 5)
	if err != nil {
		t.Fatalf("SelectK: %v", err)
	}
	if !containsPeer(cands, "peer-B") {
		t.Errorf("silent peer-B was dropped from cands; it must be demoted, not excluded: %+v", cands)
	}
	if !containsPeer(cands, "peer-A") || !containsPeer(cands, "peer-C") {
		t.Errorf("healthy peers missing from cands: %+v", cands)
	}
	if got := cands[len(cands)-1].PeerID; got != "peer-B" {
		t.Errorf("last candidate = %q, want peer-B (silence sorts last)", got)
	}
}

// TestSelectK_AllSilentStillReturnsCandidates is the behavioural core
// of waired#729: the reported topology had exactly one peer, reachable
// over relay, and disco silence made it invisible — worker --pin and
// the Claude per-class pin both came back "routing=pinned, no mesh
// candidate" (ErrModelNotReady) against a data plane that was carrying
// traffic fine. With silence advisory there is nothing left to veto
// the whole mesh. PRODUCT CONTRACT.
func TestSelectK_AllSilentStillReturnsCandidates(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			mkSilentPeer("peer-A", "qwen3:8b-q4_K_M"),
			mkSilentPeer("peer-B", "qwen3:8b-q4_K_M"),
		},
	}
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
	})
	cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 5)
	if err != nil {
		t.Fatalf("SelectK with an all-silent mesh: %v", err)
	}
	if len(cands) != 2 {
		t.Errorf("an all-silent mesh must still yield candidates; got %d", len(cands))
	}
}

// TestSelectK_SilentPinStillHoistedAndReturned pins the operator-pin
// half of waired#729. A pin is an explicit "use this machine"; disco
// silence is a guess. The pin still reaches the head of the candidate
// list so the probe layer gets to try it, and if the probe fails the
// gateway surfaces the named PinnedPeerUnreachableError rather than a
// generic "no mesh candidate". PRODUCT CONTRACT.
func TestSelectK_SilentPinStillHoistedAndReturned(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			mkPeer("peer-A", "qwen3:8b-q4_K_M", true, false),
			mkSilentPeer("peer-pin", "qwen3:8b-q4_K_M"),
		},
	}
	s := NewSelector(Inputs{
		Manifests:          []catalog.Manifest{qwen()},
		LocalState:         emptyState(),
		Hardware:           goodHardware(),
		Runtimes:           registryWithOllama(),
		MeshSnapshotFn:     func() inferencemesh.Snapshot { return snap },
		RoutingMode:        state.RoutingModePinned,
		PinnedPeerDeviceID: "peer-pin",
	})
	cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 5)
	if err != nil {
		t.Fatalf("SelectK with a silent pin: %v", err)
	}
	if len(cands) == 0 || cands[0].PeerID != "peer-pin" {
		t.Fatalf("cands[0].PeerID = %q, want peer-pin (pin hoist beats the silence demotion)", first(cands))
	}
	if !cands[0].Pinned {
		t.Errorf("cands[0].Pinned = false; the gateway needs this flag to name the peer when its probe fails")
	}
}

// TestSortMeshCandidates_SilentOrdering pins where the advisory key
// sits in the comparison chain. PRODUCT CONTRACT.
//
// Above priority: silence predicts probe failure, and there are only
// probeFanoutK slots — spending one on a silent High-priority peer
// instead of a healthy Middle one lowers the expected first-round hit
// rate for nothing.
//
// Below the public/grant tier: a silent OWN peer must still outrank a
// healthy PUBLIC one, or a 45-second gap in pongs would route the
// owner's traffic onto a stranger's machine.
func TestSortMeshCandidates_SilentOrdering(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cands []meshCandidate
		want  []string
	}{
		{
			name: "healthy beats silent within a tier",
			cands: []meshCandidate{
				{deviceID: "silent", silent: true},
				{deviceID: "healthy"},
			},
			want: []string{"healthy", "silent"},
		},
		{
			name: "silence outranks admin priority",
			cands: []meshCandidate{
				{deviceID: "silent-high", silent: true, priority: 2},
				{deviceID: "healthy-mid", priority: 1},
			},
			want: []string{"healthy-mid", "silent-high"},
		},
		{
			name: "own-but-silent still beats healthy-public",
			cands: []meshCandidate{
				{deviceID: "public-healthy", public: true},
				{deviceID: "own-silent", silent: true},
			},
			want: []string{"own-silent", "public-healthy"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sortMeshCandidates(tc.cands, "")
			for i, want := range tc.want {
				if tc.cands[i].deviceID != want {
					t.Errorf("cands[%d] = %q, want %q (full order: %+v)", i, tc.cands[i].deviceID, want, tc.cands)
				}
			}
		})
	}
}

// TestSelectK_AllSaturatedReturnsErrAllPeersOverloaded preserves the
// Phase 7 invariant: when every matching peer is at its InFlight
// capacity, SelectK returns ErrAllPeersOverloaded rather than an
// empty candidate list (the gateway distinguishes "no model" from
// "all peers full" via the typed error).
func TestSelectK_AllSaturatedReturnsErrAllPeersOverloaded(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			mkPeerWithCap("peer-A", "qwen3:8b-q4_K_M", 1),
			mkPeerWithCap("peer-B", "qwen3:8b-q4_K_M", 1),
		},
	}
	tracker := NewInFlightTracker()
	rA, _ := tracker.Acquire("peer-A", 1)
	rB, _ := tracker.Acquire("peer-B", 1)
	defer rA()
	defer rB()

	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
		LocalInFlight:  tracker,
	})
	_, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 3)
	if !errors.Is(err, ErrAllPeersOverloaded) {
		t.Errorf("err = %v, want ErrAllPeersOverloaded", err)
	}
}

// TestCandidateCommit_AcquiresInFlightSlot verifies the two-phase
// admission contract: SelectK returns a candidate without consuming
// the slot, Commit consumes it. The InFlight counter only moves on
// Commit, not on SelectK.
func TestCandidateCommit_AcquiresInFlightSlot(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			mkPeerWithCap("peer-A", "qwen3:8b-q4_K_M", 2),
		},
	}
	tracker := NewInFlightTracker()
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
		LocalInFlight:  tracker,
	})
	cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 3)
	if err != nil {
		t.Fatalf("SelectK: %v", err)
	}
	// Pre-commit: counter is still 0.
	if got := tracker.InFlight("peer-A"); got != 0 {
		t.Errorf("InFlight before Commit = %d, want 0 (SelectK must not Acquire)", got)
	}
	sel, ok := cands[0].Commit()
	if !ok {
		t.Fatal("Commit returned ok=false on a fresh peer")
	}
	if got := tracker.InFlight("peer-A"); got != 1 {
		t.Errorf("InFlight after Commit = %d, want 1", got)
	}
	sel.Release()
	if got := tracker.InFlight("peer-A"); got != 0 {
		t.Errorf("InFlight after Release = %d, want 0", got)
	}
}

// TestCandidateCommit_FailsWhenSaturatedBetweenSelectAndCommit
// exercises the race the two-phase pattern is designed to handle:
// SelectK builds a candidate while peer-A has 1 slot free, but
// another goroutine fills it before Commit. Commit must return
// ok=false so the gateway can fall back to the next candidate.
func TestCandidateCommit_FailsWhenSaturatedBetweenSelectAndCommit(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			mkPeerWithCap("peer-A", "qwen3:8b-q4_K_M", 1),
		},
	}
	tracker := NewInFlightTracker()
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
		LocalInFlight:  tracker,
	})
	cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 3)
	if err != nil {
		t.Fatalf("SelectK: %v", err)
	}
	if len(cands) == 0 {
		t.Fatalf("expected at least 1 candidate")
	}
	// Simulate a concurrent request stealing the slot.
	r, ok := tracker.Acquire("peer-A", 1)
	if !ok {
		t.Fatal("setup: concurrent Acquire failed unexpectedly")
	}
	defer r()
	// Now Commit. Capacity=1, slot is held by the simulated concurrent
	// request → Commit's internal Acquire fails.
	_, ok = cands[0].Commit()
	if ok {
		t.Error("Commit returned ok=true despite peer being saturated at commit-time")
	}
}

// TestCandidateCommit_TouchesStickyBinding confirms that committing a
// remote candidate updates the sticky store. This is what makes
// Phase 8's fallback-on-probe-failure useful: a request that probes
// past a dead peer A to land on peer B updates the binding so the
// next request in the same conversation skips A entirely.
func TestCandidateCommit_TouchesStickyBinding(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			mkPeer("peer-A", "qwen3:8b-q4_K_M", true, false),
			mkPeer("peer-B", "qwen3:8b-q4_K_M", true, false),
		},
	}
	sticky := NewStickyStore(time.Minute, time.Now)
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
		Sticky:         sticky,
	})
	cands, err := s.SelectK(t.Context(), Request{Model: "waired/default", StickyID: "conv-1"}, 2)
	if err != nil {
		t.Fatalf("SelectK: %v", err)
	}
	// Sticky binding is created only at Commit-time (Phase 8: the
	// gateway probes first and commits the winning candidate).
	if _, found := sticky.Lookup("conv-1"); found {
		t.Error("sticky binding leaked at SelectK time; should defer to Commit")
	}
	// Commit cands[1] (= peer-B per deviceID-asc), simulating "peer-A
	// failed probe so gateway commits peer-B".
	sel, ok := cands[1].Commit()
	if !ok {
		t.Fatal("Commit returned ok=false")
	}
	got, found := sticky.Lookup("conv-1")
	if !found {
		t.Fatal("Commit didn't Touch sticky")
	}
	if got != sel.Runtime[len("remote:"):] {
		t.Errorf("sticky bound to %q, want %q", got, sel.Runtime)
	}
}

// TestSelect_PreservesPhase7Contract confirms the existing Select API
// keeps the same behaviour after the SelectK refactor. Phase 7
// fixtures and the gateway's current Select call site (replaced in
// Phase 8 commit 6) must continue to work in mixed-version mesh
// rollouts where Phase 7 callers haven't yet upgraded.
func TestSelect_PreservesPhase7Contract(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			mkPeer("peer-A", "qwen3:8b-q4_K_M", true, false),
		},
	}
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
	})
	sel, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.Runtime != "remote:peer-A" {
		t.Errorf("Select.Runtime = %q, want %q (Phase 7 contract)", sel.Runtime, "remote:peer-A")
	}
	if sel.Release == nil {
		t.Error("Selection.Release must remain non-nil (Phase 7 contract)")
	}
	sel.Release()
}

func first(cands []Candidate) string {
	if len(cands) == 0 {
		return ""
	}
	return cands[0].PeerID
}

func containsPeer(cands []Candidate, deviceID string) bool {
	for _, c := range cands {
		if c.PeerID == deviceID {
			return true
		}
	}
	return false
}
