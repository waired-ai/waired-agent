package router

import (
	"errors"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// TestSelectK_PinnedAndSaturatedNamesTheOneComputer is waired-agent#1303's
// wording half, at the selection layer.
//
// Product contract, ratifying source waired-agent#1303: a request pinned to
// one computer is never refused with a sentence about the mesh. Measured on
// the 0.0.3-rc6 fleet (S3): a pinned turn came back after 61.6 s with
// "router: every matching mesh peer is at capacity" while two other
// computers sat idle and were never candidates for it — a pin is not
// substituted (tryProbeAndCommit breaks on the pin index).
//
// This is the selection-layer half, reached when the pin is the only
// candidate the mesh offers. The other half — the pin is full and other
// peers are ready, so the candidate list is not empty — is answered after
// the probe round, in internal/gateway.
func TestSelectK_PinnedAndSaturatedNamesTheOneComputer(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			mkPeerWithCap("peer-pin", "qwen3:8b-q4_K_M", 1),
		},
	}
	tracker := NewInFlightTracker()
	release, _ := tracker.Acquire("peer-pin", 1)
	defer release()

	s := NewSelector(Inputs{
		Manifests:          []catalog.Manifest{qwen()},
		LocalState:         emptyState(),
		Hardware:           goodHardware(),
		Runtimes:           registryWithOllama(),
		MeshSnapshotFn:     func() inferencemesh.Snapshot { return snap },
		LocalInFlight:      tracker,
		RoutingMode:        state.RoutingModePinned,
		PinnedPeerDeviceID: "peer-pin",
	})
	_, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 3)
	if err == nil {
		t.Fatal("a pinned request to a full peer must not succeed")
	}
	// The status mapping, the Retry-After sizing and every existing
	// errors.Is in the gateway hang off this.
	if !errors.Is(err, ErrAllPeersOverloaded) {
		t.Fatalf("err = %v, want it to unwrap to ErrAllPeersOverloaded", err)
	}
	busy, ok := PinnedPeerBusy(err)
	if !ok {
		t.Fatalf("err = %v, want a *PinnedPeerBusyError", err)
	}
	if busy.PeerDisplayID != "peer-pin" {
		t.Errorf("PeerDisplayID = %q, want peer-pin", busy.PeerDisplayID)
	}
	if strings.Contains(err.Error(), "every matching mesh peer") {
		t.Errorf("the pinned refusal still names the whole mesh: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "peer-pin") {
		t.Errorf("the pinned refusal does not name the computer: %q", err.Error())
	}
}

// TestSelectK_UnpinnedAndSaturatedStaysGeneric is the other side of the
// same boundary: with no pin, "every matching mesh peer is at capacity" is
// a true description and must survive.
func TestSelectK_UnpinnedAndSaturatedStaysGeneric(t *testing.T) {
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
		t.Fatalf("err = %v, want ErrAllPeersOverloaded", err)
	}
	if _, ok := PinnedPeerBusy(err); ok {
		t.Errorf("an unpinned request named a pin: %v", err)
	}
}

// TestPinnedPeerBusyError_Wording pins the two shapes the message takes.
// Record of today's behaviour: the owner has ruled on the STATUS
// (2026-09-12: keep the 503 and the retry, fix the sentence), not on these
// words. The user-facing copy is quoted in docs-site and moves with it.
func TestPinnedPeerBusyError_Wording(t *testing.T) {
	withSlots := &PinnedPeerBusyError{PeerName: "m4-mac-mini", PeerDisplayID: "dev_x", CapacityUsed: 1, CapacityTotal: 1}
	if got := withSlots.Error(); !strings.Contains(got, "m4-mac-mini") || !strings.Contains(got, "1 of 1") {
		t.Errorf("Error() = %q, want the name and the slot count", got)
	}
	// A wait that ended before any probe reported figures says what it
	// knows and no more, rather than printing "0 of 0".
	noFigures := &PinnedPeerBusyError{PeerName: "m4-mac-mini", PeerDisplayID: "dev_x"}
	if got := noFigures.Error(); strings.Contains(got, "0 of 0") {
		t.Errorf("Error() = %q, want no slot count when none was read", got)
	}
	// A Public Share peer has no device name this host may show, so the
	// pseudonym is what the sentence carries (spec §8.5).
	pseudonym := &PinnedPeerBusyError{PeerDisplayID: "guest-7"}
	if got := pseudonym.Error(); !strings.Contains(got, "guest-7") {
		t.Errorf("Error() = %q, want the display id when there is no name", got)
	}
}

// TestSelectK_PinFullByThisRequesterIsNotSubstituted is waired-agent#1365.
//
// Product contract, ratifying source waired-agent#325 (a pin is not
// substituted) and waired-agent#1303 (a busy pin is refused by name). The
// pin is full by this requester's own outbound count and another peer is
// idle. Measured on real hardware with OpenCode on a per-computer row: the
// admission pre-filter dropped the pin, the idle peer was left as the only
// candidate, the gateway's pin guard saw no Pinned candidate, and the turn
// ran on the other computer for 47 s with no fallback header.
func TestSelectK_PinFullByThisRequesterIsNotSubstituted(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			mkPeerWithCap("peer-A", "qwen3:8b-q4_K_M", 1),
			mkPeerWithCap("peer-pin", "qwen3:8b-q4_K_M", 1),
		},
	}
	tracker := NewInFlightTracker()
	release, _ := tracker.Acquire("peer-pin", 1)
	defer release()

	s := NewSelector(Inputs{
		Manifests:          []catalog.Manifest{qwen()},
		LocalState:         emptyState(),
		Hardware:           goodHardware(),
		Runtimes:           registryWithOllama(),
		MeshSnapshotFn:     func() inferencemesh.Snapshot { return snap },
		LocalInFlight:      tracker,
		RoutingMode:        state.RoutingModePinned,
		PinnedPeerDeviceID: "peer-pin",
	})
	cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 3)
	for _, c := range cands {
		if c.PeerID != "peer-pin" {
			t.Errorf("a pinned request was offered %q while the pin was full", c.PeerID)
		}
	}
	if _, ok := PinnedPeerBusy(err); !ok {
		t.Fatalf("err = %v, want a *PinnedPeerBusyError naming the pin", err)
	}
}

// TestSelectK_PinnedListHoldsOnlyThePin: the other peers are not kept behind
// a reachable pin, because nothing may serve from there (waired-agent#1365).
// The gateway's guard against walking past a pin only works on a pin that is
// inside the probed set, and the tail gave the Selector ways to hand it a
// set without one. Product contract, same sources as above.
func TestSelectK_PinnedListHoldsOnlyThePin(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			mkPeerWithCap("peer-A", "qwen3:8b-q4_K_M", 2),
			mkPeerWithCap("peer-B", "qwen3:8b-q4_K_M", 2),
			mkPeerWithCap("peer-C", "qwen3:8b-q4_K_M", 2),
			mkPeerWithCap("peer-pin", "qwen3:8b-q4_K_M", 2),
		},
	}
	s := NewSelector(Inputs{
		Manifests:          []catalog.Manifest{qwen()},
		LocalState:         emptyState(),
		Hardware:           goodHardware(),
		Runtimes:           registryWithOllama(),
		MeshSnapshotFn:     func() inferencemesh.Snapshot { return snap },
		LocalInFlight:      NewInFlightTracker(),
		RoutingMode:        state.RoutingModePinned,
		PinnedPeerDeviceID: "peer-pin",
	})
	cands, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 3)
	if err != nil {
		t.Fatalf("SelectK: %v", err)
	}
	if len(cands) != 1 || cands[0].PeerID != "peer-pin" || !cands[0].Pinned {
		ids := make([]string, 0, len(cands))
		for _, c := range cands {
			ids = append(ids, c.PeerID)
		}
		t.Fatalf("candidates = %v, want only the pinned peer-pin", ids)
	}
}
