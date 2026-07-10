package disco

import (
	"testing"
	"time"
)

// TestRound_FinalizeOnLastReport: a round with 2 candidates that pongs
// and 1 that misses emits exactly one EventProbeRoundFinalized with
// AnySuccess=true after the last candidate reports.
func TestRound_FinalizeOnLastReport(t *testing.T) {
	bind := newFakeBind()
	s, _, _ := newService(t, nil, bind)
	now := time.Now()

	// Simulate probeAllPeers having sent 3 candidates in round 7.
	ev, done := s.finalizeRoundExpected(7, "node_pub_p", "dev_p", 3, now)
	if done {
		t.Fatalf("finalized before any reports, got %+v", ev)
	}

	// First two candidates pong.
	s.mu.Lock()
	_, done1 := s.recordRoundResultLocked(7, true, "node_pub_p", "dev_p", now)
	_, done2 := s.recordRoundResultLocked(7, true, "node_pub_p", "dev_p", now)
	s.mu.Unlock()
	if done1 || done2 {
		t.Fatalf("finalized after only 2/3 reports: done1=%v done2=%v", done1, done2)
	}

	// Third candidate ages out into a miss.
	s.mu.Lock()
	ev3, done3 := s.recordRoundResultLocked(7, false, "node_pub_p", "dev_p", now)
	s.mu.Unlock()
	if !done3 {
		t.Fatalf("expected finalize after 3/3 reports, got !done")
	}
	if !ev3.AnySuccess {
		t.Errorf("AnySuccess = false, want true (2 of 3 ponged)")
	}
	if ev3.RoundID != 7 || ev3.PeerNodePub != "node_pub_p" {
		t.Errorf("event identity wrong: %+v", ev3)
	}

	// State cleaned up.
	s.mu.Lock()
	_, present := s.directRounds[7]
	s.mu.Unlock()
	if present {
		t.Errorf("directRounds[7] not deleted after finalize")
	}
}

// TestRound_AllMissEmitsAnySuccessFalse: a round where every candidate
// times out emits AnySuccess=false on the last miss.
func TestRound_AllMissEmitsAnySuccessFalse(t *testing.T) {
	bind := newFakeBind()
	s, _, _ := newService(t, nil, bind)
	now := time.Now()

	s.finalizeRoundExpected(11, "node_pub_p", "dev_p", 3, now)
	s.mu.Lock()
	s.recordRoundResultLocked(11, false, "node_pub_p", "dev_p", now)
	s.recordRoundResultLocked(11, false, "node_pub_p", "dev_p", now)
	ev, done := s.recordRoundResultLocked(11, false, "node_pub_p", "dev_p", now)
	s.mu.Unlock()
	if !done {
		t.Fatalf("expected finalize after 3/3 reports")
	}
	if ev.AnySuccess {
		t.Errorf("AnySuccess = true, want false (all 3 missed)")
	}
}

// TestRound_FastPongDoesNotFinalizeEarly: a pong arriving before
// finalizeRoundExpected sets the expected count must not finalize the
// round prematurely. Race guard: probeAllPeers calls finalize AFTER all
// sends complete; an in-flight handlePong on the first candidate could
// otherwise see succeeded == expected (=0) and emit before remaining
// candidates were even sent.
func TestRound_FastPongDoesNotFinalizeEarly(t *testing.T) {
	bind := newFakeBind()
	s, _, _ := newService(t, nil, bind)
	now := time.Now()

	// Pong arrives first — finalized=false guard must prevent emit.
	s.mu.Lock()
	_, done := s.recordRoundResultLocked(9, true, "node_pub_p", "dev_p", now)
	s.mu.Unlock()
	if done {
		t.Fatalf("emitted before finalize: race guard broken")
	}

	// Now finalize with expected=1 (the one that already ponged).
	ev, done := s.finalizeRoundExpected(9, "node_pub_p", "dev_p", 1, now)
	if !done {
		t.Fatalf("expected synchronous finalize: succeeded already == expected")
	}
	if !ev.AnySuccess {
		t.Errorf("AnySuccess = false, want true")
	}
}

// TestRound_ZeroRoundIDIsIgnored: relay-path probes carry roundID=0 and
// must not feed into directRounds at all.
func TestRound_ZeroRoundIDIsIgnored(t *testing.T) {
	bind := newFakeBind()
	s, _, _ := newService(t, nil, bind)
	s.mu.Lock()
	_, done := s.recordRoundResultLocked(0, true, "node_pub_p", "dev_p", time.Now())
	count := len(s.directRounds)
	s.mu.Unlock()
	if done {
		t.Errorf("roundID=0 must not finalize anything")
	}
	if count != 0 {
		t.Errorf("roundID=0 created directRounds entry: count=%d", count)
	}
}
