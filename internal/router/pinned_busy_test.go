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
	withSlots := &PinnedPeerBusyError{PeerName: "sv-macmini", PeerDisplayID: "dev_x", CapacityUsed: 1, CapacityTotal: 1}
	if got := withSlots.Error(); !strings.Contains(got, "sv-macmini") || !strings.Contains(got, "1 of 1") {
		t.Errorf("Error() = %q, want the name and the slot count", got)
	}
	// A wait that ended before any probe reported figures says what it
	// knows and no more, rather than printing "0 of 0".
	noFigures := &PinnedPeerBusyError{PeerName: "sv-macmini", PeerDisplayID: "dev_x"}
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
