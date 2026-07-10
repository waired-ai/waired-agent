package router

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
)

// weightedSelector builds a mesh-fallback Selector wired with the given
// in-flight tracker (and optional RTT snapshot) over the supplied
// peers. Local state is empty so every request takes the mesh path.
func weightedSelector(tracker *InFlightTracker, rtt func() map[string]uint32, peers ...inferencemesh.PeerView) *Selector {
	snap := inferencemesh.Snapshot{Peers: peers}
	return NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
		LocalInFlight:  tracker,
		LocalRTT:       rtt,
	})
}

// TestSelector_MeshFallback_LeastLoadedAmongEqual is the core Scenario A
// fix: two equal-capacity peers tie on score/error/RTT-band, so the
// load-fraction axis must route the next request to the idle peer
// instead of always landing on the lowest deviceID.
func TestSelector_MeshFallback_LeastLoadedAmongEqual(t *testing.T) {
	tracker := NewInFlightTracker()
	// peer-A carries one outstanding request; peer-B is idle.
	rA, _ := tracker.Acquire("peer-A", 4)
	defer rA()

	s := weightedSelector(tracker, nil,
		mkPeerWithCap("peer-A", "qwen3:8b-q4_K_M", 4),
		mkPeerWithCap("peer-B", "qwen3:8b-q4_K_M", 4),
	)
	sel, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.Runtime != "remote:peer-B" {
		t.Errorf("least-loaded must pick idle peer-B; got %q", sel.Runtime)
	}
	sel.Release()
}

// TestSelector_MeshFallback_WeightedByCapacity is Scenario C: a
// higher-capacity peer absorbs proportionally more load. peer-Z (cap 8,
// 3 in-flight → 0.375) beats peer-A (cap 2, 1 in-flight → 0.50) on
// load-fraction even though it carries more absolute requests and
// deviceID-asc would have picked peer-A.
func TestSelector_MeshFallback_WeightedByCapacity(t *testing.T) {
	tracker := NewInFlightTracker()
	rA, _ := tracker.Acquire("peer-A", 2)
	defer rA()
	for range 3 {
		r, _ := tracker.Acquire("peer-Z", 8)
		defer r()
	}

	s := weightedSelector(tracker, nil,
		mkPeerWithCap("peer-A", "qwen3:8b-q4_K_M", 2),
		mkPeerWithCap("peer-Z", "qwen3:8b-q4_K_M", 8),
	)
	sel, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.Runtime != "remote:peer-Z" {
		t.Errorf("weighted least-loaded must prefer higher-capacity peer-Z (0.375 < 0.50); got %q", sel.Runtime)
	}
	sel.Release()
}

// TestSelector_MeshFallback_EqualFractionFallsToDeviceID confirms the
// deterministic deviceID-asc suffix still breaks a load-fraction tie:
// peer-A (cap 8, 4 in-flight = 0.50) and peer-B (cap 2, 1 in-flight =
// 0.50) tie exactly, so the lower deviceID wins.
func TestSelector_MeshFallback_EqualFractionFallsToDeviceID(t *testing.T) {
	tracker := NewInFlightTracker()
	for range 4 {
		r, _ := tracker.Acquire("peer-A", 8)
		defer r()
	}
	rB, _ := tracker.Acquire("peer-B", 2)
	defer rB()

	s := weightedSelector(tracker, nil,
		mkPeerWithCap("peer-A", "qwen3:8b-q4_K_M", 8),
		mkPeerWithCap("peer-B", "qwen3:8b-q4_K_M", 2),
	)
	sel, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.Runtime != "remote:peer-A" {
		t.Errorf("equal load-fraction must fall to deviceID-asc (peer-A); got %q", sel.Runtime)
	}
	sel.Release()
}

// TestSelector_MeshFallback_UnlimitedCapacityWeightedAsOne pins the
// Capacity==0 ("unlimited" admission) balancing rule: it is weighted as
// effectiveCapacity=1, so one outstanding request drives its
// load-fraction to 1.0 (no divide-by-zero). The idle unlimited peer-Z
// therefore wins over the loaded unlimited peer-A despite deviceID-asc.
func TestSelector_MeshFallback_UnlimitedCapacityWeightedAsOne(t *testing.T) {
	tracker := NewInFlightTracker()
	rA, _ := tracker.Acquire("peer-A", 0) // unlimited admission, still increments
	defer rA()

	s := weightedSelector(tracker, nil,
		mkPeerWithCap("peer-A", "qwen3:8b-q4_K_M", 0),
		mkPeerWithCap("peer-Z", "qwen3:8b-q4_K_M", 0),
	)
	sel, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.Runtime != "remote:peer-Z" {
		t.Errorf("idle unlimited peer-Z should win over loaded peer-A (load 1.0); got %q", sel.Runtime)
	}
	sel.Release()
}

// TestSelector_MeshFallback_LoadBreaksTieWithinRTTBand verifies the
// coarse RTT bucketing: peer-A (10 ms) and peer-Z (20 ms) fall in the
// same 25 ms band, so the single-digit-ms difference is treated as
// noise and the idle peer-Z wins over the loaded-but-marginally-faster
// peer-A. (Precise RTT-asc would have kept peer-A.)
func TestSelector_MeshFallback_LoadBreaksTieWithinRTTBand(t *testing.T) {
	tracker := NewInFlightTracker()
	rA, _ := tracker.Acquire("peer-A", 4)
	defer rA()
	rtt := func() map[string]uint32 { return map[string]uint32{"peer-A": 10, "peer-Z": 20} }

	s := weightedSelector(tracker, rtt,
		mkPeerWithCap("peer-A", "qwen3:8b-q4_K_M", 4),
		mkPeerWithCap("peer-Z", "qwen3:8b-q4_K_M", 4),
	)
	sel, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.Runtime != "remote:peer-Z" {
		t.Errorf("within one RTT band, load must break the tie to idle peer-Z; got %q", sel.Runtime)
	}
	sel.Release()
}

// TestSelector_MeshFallback_RTTBandDominatesLoad is the converse: peer-A
// (10 ms, band 0) and peer-Z (60 ms, band 2) sit in different RTT
// bands, so the band axis decides ahead of load — peer-A wins even
// though it is loaded and peer-Z is idle.
func TestSelector_MeshFallback_RTTBandDominatesLoad(t *testing.T) {
	tracker := NewInFlightTracker()
	r1, _ := tracker.Acquire("peer-A", 4)
	defer r1()
	r2, _ := tracker.Acquire("peer-A", 4)
	defer r2()
	rtt := func() map[string]uint32 { return map[string]uint32{"peer-A": 10, "peer-Z": 60} }

	s := weightedSelector(tracker, rtt,
		mkPeerWithCap("peer-A", "qwen3:8b-q4_K_M", 4),
		mkPeerWithCap("peer-Z", "qwen3:8b-q4_K_M", 4),
	)
	sel, err := s.Select(t.Context(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.Runtime != "remote:peer-A" {
		t.Errorf("closer RTT band must win over load; got %q", sel.Runtime)
	}
	sel.Release()
}

// TestSelector_MeshFallback_DistributesProportionalToCapacity is the
// end-to-end balancing check: six sequential Select calls (each commits
// and holds its slot) across peer-A (cap 4) and peer-B (cap 2) fill in
// proportion to capacity — 4 land on A, 2 on B. The pre-balancing
// deviceID-only tie-break would have piled all six onto peer-A until it
// saturated.
func TestSelector_MeshFallback_DistributesProportionalToCapacity(t *testing.T) {
	tracker := NewInFlightTracker()
	s := weightedSelector(tracker, nil,
		mkPeerWithCap("peer-A", "qwen3:8b-q4_K_M", 4),
		mkPeerWithCap("peer-B", "qwen3:8b-q4_K_M", 2),
	)
	for i := range 6 {
		if _, err := s.Select(t.Context(), Request{Model: "waired/default"}); err != nil {
			t.Fatalf("Select #%d: %v", i, err)
		}
	}
	if got := tracker.InFlight("peer-A"); got != 4 {
		t.Errorf("peer-A in-flight = %d, want 4 (capacity-proportional)", got)
	}
	if got := tracker.InFlight("peer-B"); got != 2 {
		t.Errorf("peer-B in-flight = %d, want 2 (capacity-proportional)", got)
	}
}
