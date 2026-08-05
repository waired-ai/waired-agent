package router

import (
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// spreadFixture is a Selector whose local model is NOT ready — the only
// path mesh candidates appear on — wired with the three stores the
// concurrent-sub spread reads and writes.
type spreadFixture struct {
	sel      *Selector
	sticky   *StickyStore
	inflight *StickyInFlight
}

func newSpreadFixture(t *testing.T, peers ...inferencemesh.PeerView) *spreadFixture {
	t.Helper()
	return newSpreadFixtureWithPolicy(t, nil, PublicPolicy{}, peers...)
}

func newSpreadFixtureWithPolicy(t *testing.T, tune func(*Inputs), policy PublicPolicy, peers ...inferencemesh.PeerView) *spreadFixture {
	t.Helper()
	snap := inferencemesh.Snapshot{Peers: peers}
	f := &spreadFixture{
		sticky:   NewStickyStore(time.Minute, time.Now),
		inflight: NewStickyInFlight(),
	}
	in := Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
		Sticky:         f.sticky,
		LocalInFlight:  NewInFlightTracker(),
		StickyInFlight: f.inflight,
	}
	if policy.Mode != PublicModeOff {
		in.PublicPolicyFn = func() PublicPolicy { return policy }
	}
	if tune != nil {
		tune(&in)
	}
	f.sel = NewSelector(in)
	return f
}

// peerOrder is the candidate slice as device ids, which is the only
// thing these tests care about.
func (f *spreadFixture) peerOrder(t *testing.T, stickyID string, k int) []string {
	t.Helper()
	cands, err := f.sel.SelectK(t.Context(), Request{Model: "waired/default", StickyID: stickyID}, k)
	if err != nil {
		t.Fatalf("SelectK: %v", err)
	}
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.PeerID)
	}
	return out
}

func threePeers() []inferencemesh.PeerView {
	return []inferencemesh.PeerView{
		mkPeerWithCap("peer-A", "qwen3:8b-q4_K_M", 8),
		mkPeerWithCap("peer-B", "qwen3:8b-q4_K_M", 8),
		mkPeerWithCap("peer-C", "qwen3:8b-q4_K_M", 8),
	}
}

// TestSpread_SequentialRequestsKeepTheBinding is the regression test
// waired-ai/waired#828 asks for by name. Sub-agents that run one after
// another must keep their KV prefix on one peer; only overlap is a
// reason to move.
func TestSpread_SequentialRequestsKeepTheBinding(t *testing.T) {
	f := newSpreadFixture(t, threePeers()...)
	f.sticky.Touch("conv-1:sub", "peer-C")

	for turn := 0; turn < 5; turn++ {
		// Each turn commits and finishes before the next one starts,
		// which is what "sequential" means: the in-flight count is back
		// to 0 when the next SelectK runs.
		cands, err := f.sel.SelectK(t.Context(), Request{Model: "waired/default", StickyID: "conv-1:sub"}, 3)
		if err != nil {
			t.Fatalf("turn %d: SelectK: %v", turn, err)
		}
		if cands[0].PeerID != "peer-C" {
			t.Fatalf("turn %d: head = %q, want peer-C (sticky affinity regressed)", turn, cands[0].PeerID)
		}
		sel, ok := cands[0].Commit()
		if !ok {
			t.Fatalf("turn %d: Commit refused", turn)
		}
		if got := f.inflight.InFlight("conv-1:sub", "peer-C"); got != 1 {
			t.Errorf("turn %d: in-flight during the request = %d, want 1", turn, got)
		}
		sel.Release()
		if got := f.inflight.InFlight("conv-1:sub", "peer-C"); got != 0 {
			t.Errorf("turn %d: in-flight after Release = %d, want 0", turn, got)
		}
	}
}

// TestSpread_ConcurrentRequestDemotesTheBoundPeer is the behaviour
// #828 exists for: the second overlapping sub of one conversation must
// not queue behind the first.
func TestSpread_ConcurrentRequestDemotesTheBoundPeer(t *testing.T) {
	f := newSpreadFixture(t, threePeers()...)
	f.sticky.Touch("conv-1:sub", "peer-C")

	first, err := f.sel.SelectK(t.Context(), Request{Model: "waired/default", StickyID: "conv-1:sub"}, 3)
	if err != nil {
		t.Fatalf("first SelectK: %v", err)
	}
	if first[0].PeerID != "peer-C" {
		t.Fatalf("first head = %q, want peer-C", first[0].PeerID)
	}
	held, ok := first[0].Commit()
	if !ok {
		t.Fatal("first Commit refused")
	}
	defer held.Release()

	// Second request arrives while the first is still running.
	order := f.peerOrder(t, "conv-1:sub", 3)
	if order[0] == "peer-C" {
		t.Fatalf("order = %v, want the bound peer off the head", order)
	}
	if last := order[len(order)-1]; last != "peer-C" {
		t.Errorf("order = %v, want peer-C demoted to the last probed slot", order)
	}
	// Still a candidate, not excluded: if the other two fail their
	// probes the request must land on the peer that can serve it.
	if len(order) != 3 {
		t.Errorf("order = %v, want all three peers still offered", order)
	}
}

// TestSpread_DemotionStaysInsideTheProbeWindow: k is what the gateway
// actually probes. Demoting past it would drop a healthy peer from the
// round, and a round where every probed peer fails would then 503 with
// the bound peer sitting there able to serve.
func TestSpread_DemotionStaysInsideTheProbeWindow(t *testing.T) {
	peers := append(threePeers(),
		mkPeerWithCap("peer-D", "qwen3:8b-q4_K_M", 8),
		mkPeerWithCap("peer-E", "qwen3:8b-q4_K_M", 8),
	)
	f := newSpreadFixture(t, peers...)
	f.sticky.Touch("conv-1:sub", "peer-E")
	rel := f.inflight.Acquire("conv-1:sub", "peer-E")
	defer rel()

	order := f.peerOrder(t, "conv-1:sub", 3)
	if len(order) != 3 {
		t.Fatalf("order = %v, want 3 candidates", order)
	}
	if order[2] != "peer-E" {
		t.Errorf("order = %v, want peer-E demoted to index 2, not out of the probe window", order)
	}
}

// TestSpread_SoleCandidateIsNotDemoted: with nowhere else to go, the
// bound peer stays. Spreading is a preference between peers, never a
// refusal to serve.
func TestSpread_SoleCandidateIsNotDemoted(t *testing.T) {
	f := newSpreadFixture(t, mkPeerWithCap("peer-A", "qwen3:8b-q4_K_M", 8))
	f.sticky.Touch("conv-1:sub", "peer-A")
	rel := f.inflight.Acquire("conv-1:sub", "peer-A")
	defer rel()

	if order := f.peerOrder(t, "conv-1:sub", 3); len(order) != 1 || order[0] != "peer-A" {
		t.Errorf("order = %v, want [peer-A]", order)
	}
}

// TestSpread_NeverCrossesTheOwnPublicBoundary: waired#827's partition.
// Two of an operator's own sub-agents overlapping is a much smaller
// event than moving their traffic onto a stranger's machine.
func TestSpread_NeverCrossesTheOwnPublicBoundary(t *testing.T) {
	f := newSpreadFixtureWithPolicy(t, nil, allowAll(),
		mkPeerWithCap("peer-A", "qwen3:8b-q4_K_M", 8),
		mkPublicPeer(publicPeerDeviceID, publicPeerAlias, "qwen3:8b-q4_K_M"),
	)
	f.sticky.Touch("conv-1:sub", "peer-A")
	rel := f.inflight.Acquire("conv-1:sub", "peer-A")
	defer rel()

	order := f.peerOrder(t, "conv-1:sub", 3)
	if len(order) == 0 || order[0] != "peer-A" {
		t.Errorf("order = %v, want the own peer still first (public must not be spread onto)", order)
	}
}

// TestSpread_PinnedPeerIsNotDemoted: "use this machine" is an explicit
// operator action and outranks a balancing preference.
func TestSpread_PinnedPeerIsNotDemoted(t *testing.T) {
	f := newSpreadFixtureWithPolicy(t, func(in *Inputs) {
		in.RoutingMode = state.RoutingModePinned
		in.PinnedPeerDeviceID = "peer-C"
	}, PublicPolicy{}, threePeers()...)
	f.sticky.Touch("conv-1:sub", "peer-C")
	rel := f.inflight.Acquire("conv-1:sub", "peer-C")
	defer rel()

	if order := f.peerOrder(t, "conv-1:sub", 3); order[0] != "peer-C" {
		t.Errorf("order = %v, want the pinned peer to stay at the head", order)
	}
}

// TestSpread_SpreadCommitDoesNotRebindTheConversation: the binding says
// where the conversation's prefix lives. A sub sent elsewhere because
// that peer was busy is a one-request detour; letting it move the
// binding would walk the whole conversation onto whichever peer took
// the last overlapping sub.
func TestSpread_SpreadCommitDoesNotRebindTheConversation(t *testing.T) {
	f := newSpreadFixture(t, threePeers()...)
	f.sticky.Touch("conv-1:sub", "peer-C")
	rel := f.inflight.Acquire("conv-1:sub", "peer-C")
	defer rel()

	cands, err := f.sel.SelectK(t.Context(), Request{Model: "waired/default", StickyID: "conv-1:sub"}, 3)
	if err != nil {
		t.Fatalf("SelectK: %v", err)
	}
	sel, ok := cands[0].Commit()
	if !ok {
		t.Fatal("Commit refused")
	}
	defer sel.Release()

	if bound, _ := f.sticky.Lookup("conv-1:sub"); bound != "peer-C" {
		t.Errorf("binding = %q after a spread commit, want peer-C (unchanged)", bound)
	}
}

// TestSpread_BoundPeerWinningAsLastResortStillRefreshesTheBinding: the
// complement. When every better-ranked peer fails and the demoted peer
// serves the request after all, that is the conversation running on its
// own node, so the TTL should move.
func TestSpread_BoundPeerWinningAsLastResortStillRefreshesTheBinding(t *testing.T) {
	f := newSpreadFixture(t, threePeers()...)
	f.sticky.Touch("conv-1:sub", "peer-C")
	rel := f.inflight.Acquire("conv-1:sub", "peer-C")
	defer rel()

	cands, err := f.sel.SelectK(t.Context(), Request{Model: "waired/default", StickyID: "conv-1:sub"}, 3)
	if err != nil {
		t.Fatalf("SelectK: %v", err)
	}
	demoted := cands[len(cands)-1]
	if demoted.PeerID != "peer-C" {
		t.Fatalf("last candidate = %q, want the demoted peer-C", demoted.PeerID)
	}
	sel, ok := demoted.Commit()
	if !ok {
		t.Fatal("Commit refused")
	}
	defer sel.Release()

	if bound, _ := f.sticky.Lookup("conv-1:sub"); bound != "peer-C" {
		t.Errorf("binding = %q, want peer-C", bound)
	}
}

// TestSpread_NormalCommitStillBinds guards the demotion from swallowing
// the ordinary case: with no overlap there is no spread, and the peer
// that served becomes the conversation's peer.
func TestSpread_NormalCommitStillBinds(t *testing.T) {
	f := newSpreadFixture(t, threePeers()...)

	cands, err := f.sel.SelectK(t.Context(), Request{Model: "waired/default", StickyID: "conv-new"}, 3)
	if err != nil {
		t.Fatalf("SelectK: %v", err)
	}
	sel, ok := cands[0].Commit()
	if !ok {
		t.Fatal("Commit refused")
	}
	defer sel.Release()

	bound, found := f.sticky.Lookup("conv-new")
	if !found || bound != cands[0].PeerID {
		t.Errorf("binding = (%q, %v), want (%q, true)", bound, found, cands[0].PeerID)
	}
}

// TestSpread_DecisionNamesTheDemotion: "why am I not on my usual node"
// has to be answerable from `waired infer --explain`.
func TestSpread_DecisionNamesTheDemotion(t *testing.T) {
	f := newSpreadFixture(t, threePeers()...)
	f.sticky.Touch("conv-1:sub", "peer-C")
	rel := f.inflight.Acquire("conv-1:sub", "peer-C")
	defer rel()

	cands, err := f.sel.SelectK(t.Context(), Request{Model: "waired/default", StickyID: "conv-1:sub"}, 3)
	if err != nil {
		t.Fatalf("SelectK: %v", err)
	}
	joined := strings.Join(cands[0].Decision.Reason, "\n")
	if !strings.Contains(joined, "demoted it for this request") {
		t.Errorf("winner reasons do not mention the demotion:\n%s", joined)
	}
}

// TestSpread_UnwiredTrackerKeepsStickyFirst is the nil-degrade contract
// the other Phase 7 inputs carry: no StickyInFlight, no spread, and the
// pre-#828 behaviour is unchanged.
func TestSpread_UnwiredTrackerKeepsStickyFirst(t *testing.T) {
	snap := inferencemesh.Snapshot{Peers: threePeers()}
	sticky := NewStickyStore(time.Minute, time.Now)
	sticky.Touch("conv-1:sub", "peer-C")
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     emptyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
		Sticky:         sticky,
		LocalInFlight:  NewInFlightTracker(),
		// StickyInFlight deliberately nil.
	})
	cands, err := s.SelectK(t.Context(), Request{Model: "waired/default", StickyID: "conv-1:sub"}, 3)
	if err != nil {
		t.Fatalf("SelectK: %v", err)
	}
	if cands[0].PeerID != "peer-C" {
		t.Errorf("head = %q, want peer-C", cands[0].PeerID)
	}
	sel, ok := cands[0].Commit()
	if !ok {
		t.Fatal("Commit refused")
	}
	sel.Release() // must not panic on the noop sticky release
}
