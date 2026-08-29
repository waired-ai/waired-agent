package router

import (
	"sync"
	"testing"
	"time"
)

// TestStickyStore_LookupMissOnEmpty covers the documented no-op
// branches: empty conversation ID and a never-touched ID both miss
// quietly.
func TestStickyStore_LookupMissOnEmpty(t *testing.T) {
	s := NewStickyStore(time.Minute, time.Now)
	if id, ok := s.Lookup(""); ok || id != "" {
		t.Errorf("empty conversation ID should miss; got (%q, %v)", id, ok)
	}
	if id, ok := s.Lookup("never-touched"); ok || id != "" {
		t.Errorf("unseen ID should miss; got (%q, %v)", id, ok)
	}
}

// TestStickyStore_TouchThenLookupHits is the happy path: write then
// read returns the recorded peer.
func TestStickyStore_TouchThenLookupHits(t *testing.T) {
	clk := newMockClock(time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC))
	s := NewStickyStore(time.Minute, clk.Now)
	s.Touch("conv-1", "peer-A")
	got, ok := s.Lookup("conv-1")
	if !ok || got != "peer-A" {
		t.Errorf("Lookup after Touch: (%q, %v), want (peer-A, true)", got, ok)
	}
	if s.Size() != 1 {
		t.Errorf("Size after one Touch = %d, want 1", s.Size())
	}
}

// TestStickyStore_WithinTTLHitsUnconditionally pins the first half of
// the rule: inside the TTL a binding is honoured with no further
// question asked.
//
// Product contract — owner ruling, 2026-08-29, waired-agent#1129.
func TestStickyStore_WithinTTLHitsUnconditionally(t *testing.T) {
	clk := newMockClock(time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC))
	s := NewStickyStore(time.Minute, clk.Now)
	s.Touch("conv-1", "peer-A")
	s.Touch("conv-2", "peer-A") // someone else on the same peer

	clk.advance(30 * time.Second)
	if got, ok := s.Lookup("conv-1"); !ok || got != "peer-A" {
		t.Errorf("inside the TTL the binding is honoured whatever else happened; got (%q, %v)", got, ok)
	}
}

// TestStickyStore_SecondChanceWhileNobodyElseWasSentThere INVERTS the
// pre-#1129 TestStickyStore_TTLExpires, which asserted that a binding
// past its TTL always misses.
//
// Product contract — owner ruling, 2026-08-29, waired-agent#1129:
//
//	特例として、10分経過後でもまだ他のセッションがそのノードに新たに
//	割り当てられておらず kv キャッシュが維持されている可能性がたかければ、
//	そこに優先スケジュールされるように
func TestStickyStore_SecondChanceWhileNobodyElseWasSentThere(t *testing.T) {
	clk := newMockClock(time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC))
	s := NewStickyStore(time.Minute, clk.Now)
	s.Touch("conv-1", "peer-A")

	clk.advance(61 * time.Second) // past the TTL, well inside the hard cap
	if got, ok := s.Lookup("conv-1"); !ok || got != "peer-A" {
		t.Errorf("a stale binding whose peer took nobody else must still be honoured; got (%q, %v)", got, ok)
	}
	if s.Size() != 1 {
		t.Errorf("an honourable binding must not be reaped; Size=%d", s.Size())
	}
}

// TestStickyStore_StaleBindingDropsWhenThePeerTookSomeoneElse is the
// other side of the same rule. On the engine this mesh mostly runs, a
// slot IS the unit of KV retention (OLLAMA_NUM_PARALLEL is 1, or 2), so
// a second conversation arriving at that peer is near-conclusive that
// the first one's prefix is gone.
func TestStickyStore_StaleBindingDropsWhenThePeerTookSomeoneElse(t *testing.T) {
	clk := newMockClock(time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC))
	s := NewStickyStore(time.Minute, clk.Now)
	s.Touch("conv-1", "peer-A")

	clk.advance(10 * time.Second)
	s.Touch("conv-2", "peer-A") // somebody else, after us

	clk.advance(61 * time.Second) // conv-1 is now stale
	if got, ok := s.Lookup("conv-1"); ok {
		t.Errorf("a stale binding whose peer took another conversation must miss; got (%q, %v)", got, ok)
	}
	// lastBound only moves forward, so this entry can never become
	// honourable again — the failed Lookup reaps it.
	if _, ok := s.m["conv-1"]; ok {
		t.Error("an unhonourable binding should be reaped by the failed Lookup")
	}
	// conv-2 is stale too, but IT was the last one sent there.
	if got, ok := s.Lookup("conv-2"); !ok || got != "peer-A" {
		t.Errorf("the most recent conversation on that peer keeps its second chance; got (%q, %v)", got, ok)
	}
}

// TestStickyStore_OwnRefreshIsNotSomeoneElse guards the equality edge:
// Touch writes lastBound for the peer as well as the entry, so a
// conversation that refreshed its own binding must not read its own
// timestamp as evidence that somebody else arrived.
func TestStickyStore_OwnRefreshIsNotSomeoneElse(t *testing.T) {
	clk := newMockClock(time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC))
	s := NewStickyStore(time.Minute, clk.Now)
	s.Touch("conv-1", "peer-A")
	clk.advance(10 * time.Second)
	s.Touch("conv-1", "peer-A") // same conversation, same peer

	clk.advance(61 * time.Second)
	if got, ok := s.Lookup("conv-1"); !ok || got != "peer-A" {
		t.Errorf("a conversation's own refresh is not another session; got (%q, %v)", got, ok)
	}
}

// TestStickyStore_HardCapEndsTheSecondChance bounds it. An engine that
// has been up for an hour has almost certainly reloaded, restarted or
// been re-tuned, so past StickyHardCap the binding goes whatever the
// evidence says.
func TestStickyStore_HardCapEndsTheSecondChance(t *testing.T) {
	clk := newMockClock(time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC))
	s := NewStickyStore(time.Minute, clk.Now)
	s.Touch("conv-1", "peer-A")

	clk.advance(StickyHardCap - time.Second)
	if _, ok := s.Lookup("conv-1"); !ok {
		t.Error("just inside the hard cap the second chance still holds")
	}
	clk.advance(2 * time.Second)
	if got, ok := s.Lookup("conv-1"); ok {
		t.Errorf("past the hard cap the binding is gone; got (%q, %v)", got, ok)
	}
	if s.Size() != 0 {
		t.Errorf("a capped-out entry should be reaped by Lookup; Size=%d", s.Size())
	}
}

// TestStickyStore_TouchRefreshesTTL ensures repeated Touches extend
// the lifetime. Coding-agent sessions emit a Touch on every reply, so
// an ongoing session must keep the binding alive indefinitely.
func TestStickyStore_TouchRefreshesTTL(t *testing.T) {
	clk := newMockClock(time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC))
	s := NewStickyStore(time.Minute, clk.Now)

	// Touch every 30 s for 5 minutes — entry must always be live.
	s.Touch("conv-1", "peer-A")
	for i := 0; i < 10; i++ {
		clk.advance(30 * time.Second)
		s.Touch("conv-1", "peer-A")
		if _, ok := s.Lookup("conv-1"); !ok {
			t.Fatalf("Lookup after refresh #%d should hit", i)
		}
	}
}

// TestStickyStore_TouchChangesBinding covers the "peer migration"
// path: if a conversation gets re-routed (e.g. original peer went
// stale), the next Touch updates the binding.
func TestStickyStore_TouchChangesBinding(t *testing.T) {
	s := NewStickyStore(time.Minute, time.Now)
	s.Touch("conv-1", "peer-A")
	s.Touch("conv-1", "peer-B")
	if got, _ := s.Lookup("conv-1"); got != "peer-B" {
		t.Errorf("Touch should overwrite; got peer=%q, want peer-B", got)
	}
}

// TestStickyStore_GCReapsWhatCanNoLongerBeHonoured confirms the
// explicit GC pass applies the SAME rule Lookup does, without being
// asked about a particular conversation.
//
// It INVERTS the pre-#1129 TestStickyStore_GCReapsExpiredEntries, which
// asserted that every entry past its TTL is swept.
func TestStickyStore_GCReapsWhatCanNoLongerBeHonoured(t *testing.T) {
	clk := newMockClock(time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC))
	s := NewStickyStore(time.Minute, clk.Now)
	s.Touch("a", "peer-A")
	s.Touch("b", "peer-B")
	s.Touch("displaced", "peer-C")

	clk.advance(30 * time.Second)
	s.Touch("took-peer-c", "peer-C") // displaces "displaced"

	clk.advance(90 * time.Second) // everything above is now stale
	s.Touch("d", "peer-D")        // fresh

	s.GC()
	// a and b: stale, but their peers took nobody else — second chance.
	// displaced: stale AND its peer took another conversation — gone.
	// took-peer-c: stale, last one on peer-C — second chance.
	// d: fresh.
	for _, id := range []string{"a", "b", "took-peer-c", "d"} {
		if _, ok := s.Lookup(id); !ok {
			t.Errorf("%q should have survived GC", id)
		}
	}
	if _, ok := s.Lookup("displaced"); ok {
		t.Error("a binding whose peer took another conversation should be swept")
	}
	if s.Size() != 4 {
		t.Errorf("Size after GC = %d, want 4", s.Size())
	}
}

// TestStickyStore_GCPrunesTheDeviceIndexOnlyPastTheHardCap keeps the
// second-chance evidence from being thrown away early. A device row
// dropped while a stale entry still points at it would read as "nobody
// else was sent there" — granting a second chance the peer has not
// earned.
func TestStickyStore_GCPrunesTheDeviceIndexOnlyPastTheHardCap(t *testing.T) {
	clk := newMockClock(time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC))
	s := NewStickyStore(time.Minute, clk.Now)
	s.Touch("conv-1", "peer-A")
	clk.advance(10 * time.Second)
	s.Touch("conv-2", "peer-A")

	clk.advance(90 * time.Second)
	s.GC()
	s.mu.Lock()
	_, kept := s.lastBound["peer-A"]
	s.mu.Unlock()
	if !kept {
		t.Error("the device row must outlive the TTL; it is what answers the second chance")
	}
	if _, ok := s.Lookup("conv-1"); ok {
		t.Error("conv-1 lost its peer to conv-2 and must stay gone after a GC")
	}

	clk.advance(StickyHardCap)
	s.GC()
	s.mu.Lock()
	_, stillThere := s.lastBound["peer-A"]
	n := len(s.m)
	s.mu.Unlock()
	if stillThere {
		t.Error("past the hard cap the device row answers no remaining question and should go")
	}
	if n != 0 {
		t.Errorf("every entry is past the hard cap; %d left", n)
	}
}

// TestStickyStore_TouchEmptyArgsIgnored guards the defensive no-ops.
func TestStickyStore_TouchEmptyArgsIgnored(t *testing.T) {
	s := NewStickyStore(time.Minute, time.Now)
	s.Touch("", "peer-A")
	s.Touch("conv-1", "")
	if s.Size() != 0 {
		t.Errorf("empty args should be no-op; Size=%d", s.Size())
	}
}

// TestStickyStore_ConcurrentTouchAndLookup runs concurrent writers
// and readers; Run with -race.
func TestStickyStore_ConcurrentTouchAndLookup(t *testing.T) {
	s := NewStickyStore(time.Minute, time.Now)
	const workers = 16
	var wg sync.WaitGroup
	wg.Add(workers * 2)
	for i := 0; i < workers; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				s.Touch("conv", "peer")
			}
		}(i)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				_, _ = s.Lookup("conv")
			}
		}(i)
	}
	wg.Wait()
}
