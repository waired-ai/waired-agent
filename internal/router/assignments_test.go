package router

import (
	"sync"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// burstMesh is the four-computer mesh waired-agent#1354 was measured on, as
// this requester held it: every peer's last probe saying it had nothing in
// flight. One model tag everywhere so only the speeds and slots differ. The
// seconds per request are the prefill rates measured at 8,192 tokens then
// (1,443 / 588 / 354 / 293 tok/s) put on one scale, so the buckets keep the
// spacing the measurement had.
//
//	candidate  slots  s/request  bucket at 0 / 1 / 2 in use
//	peer-A       1       48.6    17 / 20
//	peer-B       1      119.2    21 / 24
//	this device  1      197.9    23 / 26 / 28
//	peer-C       2      239.1    24 / 27
func burstMesh(t *testing.T, withAssignments bool) (*Selector, *InFlightTracker, *Assignments) {
	t.Helper()
	tag := "qwen3:8b-q4_K_M"
	snap := inferencemesh.Snapshot{
		SelfDeviceID: "self",
		Peers: []inferencemesh.PeerView{
			mkPeerWithCap("peer-A", tag, 1),
			mkPeerWithCap("peer-B", tag, 1),
			mkPeerWithCap("peer-C", tag, 2),
		},
	}
	turn := func(seconds float64) *PeerTurn { return &PeerTurn{TurnSeconds: seconds} }
	speeds := map[string]PeerSpeed{
		"peer-A": {VariantID: "q4-gguf", Turn: turn(48.6)},
		"peer-B": {VariantID: "q4-gguf", Turn: turn(119.2)},
		"peer-C": {VariantID: "q4-gguf", Turn: turn(239.1)},
	}
	ln := localFor(tag)
	ln.Capacity = 1
	ln.Speed = &PeerSpeedReading{VariantID: "q4-gguf", TurnSeconds: 197.9}

	tracker := NewInFlightTracker()
	var a *Assignments
	if withAssignments {
		a = NewAssignments()
	}
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen()},
		LocalState:     readyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
		LocalNode:      func() LocalNode { return ln },
		PeerSpeeds:     func() map[string]PeerSpeed { return speeds },
		LocalInFlight:  tracker,
		Assignments:    a,
		RoutingMode:    state.RoutingModeAuto,
	})
	return s, tracker, a
}

// where names the computer a committed Selection runs on.
func where(sel Selection) string {
	if sel.ExecutionMode == "local" {
		return "self"
	}
	return sel.Runtime[len("remote:"):]
}

// commitLikeTheGateway commits the first candidate that takes, in order, and
// gives back the rest: the probe round of internal/gateway with every probe
// answering ready.
func commitLikeTheGateway(t *testing.T, cands []Candidate) Selection {
	t.Helper()
	defer func() {
		for _, c := range cands {
			c.Abandon()
		}
	}()
	for _, c := range cands {
		if sel, ok := c.Commit(); ok {
			return sel
		}
	}
	t.Fatalf("no candidate committed out of %d", len(cands))
	return Selection{}
}

// TestSelectKAssigned_ABurstSpreadsAsItWouldInSequence is waired-agent#1354.
//
// Six requests are ranked at the same moment — every ranking finishes before
// any of them commits, which is what Claude Code's subagent launch looked
// like on the wire (six rankings within 6 ms) — and then each commits.
//
// Measured before the fix: 1 on peer-A, 1 on peer-B, 4 on this device's
// one-slot engine, 0 on peer-C. The same six sent 50 ms apart went 1/1/2/2,
// which is what the ranking gives when each request sees the previous one.
// Product contract, ratifying source: owner decision 2026-09-14 that a
// request is counted when it is assigned
// (docs/decisions/20260914/0420-assignment-is-counted-when-it-is-made.md).
func TestSelectKAssigned_ABurstSpreadsAsItWouldInSequence(t *testing.T) {
	s, _, a := burstMesh(t, true)

	const n = 6
	ranked := make([][]Candidate, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			cands, err := s.SelectKAssigned(t.Context(), Request{Model: "waired/default"}, 3)
			if err != nil {
				t.Errorf("request %d: %v", i, err)
				return
			}
			ranked[i] = cands
		}(i)
	}
	close(start)
	wg.Wait()

	got := map[string]int{}
	var held []Selection
	for i := 0; i < n; i++ {
		if ranked[i] == nil {
			t.Fatalf("request %d has no candidates", i)
		}
		sel := commitLikeTheGateway(t, ranked[i])
		held = append(held, sel)
		got[where(sel)]++
	}
	want := map[string]int{"peer-A": 1, "peer-B": 1, "self": 2, "peer-C": 2}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("served by %v, want %v", got, want)
			break
		}
	}
	if a.Local() != 2 {
		t.Errorf("Assignments.Local() = %d with two requests on this device, want 2", a.Local())
	}
	for _, sel := range held {
		sel.Release()
	}
	if a.Local() != 0 {
		t.Errorf("Assignments.Local() = %d after every release, want 0", a.Local())
	}
}

// TestSelectKAssigned_WithoutAssignmentsIsSelectK: a Selector with no
// Assignments ranks as it always did, so the burst of #1354 still piles up.
// Record of today's behaviour for the unwired case — the agent wires it; this
// pins that the nil value is inert, and that the burst test above is not
// green for some other reason.
func TestSelectKAssigned_WithoutAssignmentsIsSelectK(t *testing.T) {
	s, _, _ := burstMesh(t, false)
	var ranked [][]Candidate
	for i := 0; i < 6; i++ {
		cands, err := s.SelectKAssigned(t.Context(), Request{Model: "waired/default"}, 3)
		if err != nil {
			t.Fatalf("SelectKAssigned: %v", err)
		}
		ranked = append(ranked, cands)
	}
	got := map[string]int{}
	for _, cands := range ranked {
		got[where(commitLikeTheGateway(t, cands))]++
	}
	if got["self"] != 4 || got["peer-C"] != 0 {
		t.Errorf("served by %v, want the measured pile-up: 4 on self and none on peer-C", got)
	}
}

// TestSelectKAssigned_AbandonGivesTheCountBack: a first choice whose probe
// did not come back ready is abandoned, and the next request must not rank
// on a count that request never used.
func TestSelectKAssigned_AbandonGivesTheCountBack(t *testing.T) {
	s, tracker, a := burstMesh(t, true)

	cands, err := s.SelectKAssigned(t.Context(), Request{Model: "waired/default"}, 3)
	if err != nil {
		t.Fatalf("SelectKAssigned: %v", err)
	}
	if cands[0].PeerID != "peer-A" {
		t.Fatalf("first choice = %q, want peer-A", cands[0].PeerID)
	}
	if got := tracker.InFlight("peer-A"); got != 1 {
		t.Fatalf("peer-A in flight after assignment = %d, want 1", got)
	}
	for _, c := range cands {
		c.Abandon()
	}
	if got := tracker.InFlight("peer-A"); got != 0 {
		t.Errorf("peer-A in flight after abandon = %d, want 0", got)
	}
	// Abandoning twice gives back nothing more.
	cands[0].Abandon()
	if got := tracker.InFlight("peer-A"); got != 0 {
		t.Errorf("peer-A in flight after a second abandon = %d, want 0", got)
	}

	// A committed assignment is not given back by Abandon: Release does it.
	cands, _ = s.SelectKAssigned(t.Context(), Request{Model: "waired/default"}, 3)
	sel, ok := cands[0].Commit()
	if !ok {
		t.Fatal("an assigned first choice refused to commit")
	}
	cands[0].Abandon()
	if got := tracker.InFlight("peer-A"); got != 1 {
		t.Errorf("peer-A in flight after commit then abandon = %d, want 1", got)
	}
	sel.Release()
	if got := tracker.InFlight("peer-A"); got != 0 {
		t.Errorf("peer-A in flight after release = %d, want 0", got)
	}
	if a.Local() != 0 {
		t.Errorf("Assignments.Local() = %d, want 0", a.Local())
	}
}

// TestSelectKAssigned_ThisDeviceIsNeverRefused: the local leg waits for a
// slot rather than being refused (docs/decisions/20260912/2130 §3), so a
// count above this device's slots moves it down the ranking and never out of
// the list. Product contract, same source.
func TestSelectKAssigned_ThisDeviceIsNeverRefused(t *testing.T) {
	s, tracker, a := burstMesh(t, true)
	// Every peer full by this requester's own count.
	for _, id := range []string{"peer-A", "peer-B", "peer-C", "peer-C"} {
		if _, ok := tracker.Acquire(id, 2); !ok {
			t.Fatalf("could not fill %s", id)
		}
	}
	for i := 0; i < 3; i++ {
		cands, err := s.SelectKAssigned(t.Context(), Request{Model: "waired/default"}, 3)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if len(cands) != 1 || cands[0].ExecutionMode != "local" {
			t.Fatalf("request %d: candidates %+v, want this device alone", i, cands)
		}
		if _, ok := cands[0].Commit(); !ok {
			t.Fatalf("request %d: this device refused a request", i)
		}
	}
	if a.Local() != 3 {
		t.Errorf("Assignments.Local() = %d, want 3 on a one-slot engine", a.Local())
	}
}
