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

// TestStickyStore_TTLExpires verifies the sliding-TTL behavior.
// After ttl passes, Lookup misses and the entry is reaped lazily.
func TestStickyStore_TTLExpires(t *testing.T) {
	clk := newMockClock(time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC))
	s := NewStickyStore(time.Minute, clk.Now)
	s.Touch("conv-1", "peer-A")

	clk.advance(30 * time.Second)
	if _, ok := s.Lookup("conv-1"); !ok {
		t.Error("Lookup before TTL should hit")
	}

	clk.advance(31 * time.Second) // total 61 s > 60 s ttl
	if id, ok := s.Lookup("conv-1"); ok {
		t.Errorf("Lookup after TTL should miss; got (%q, %v)", id, ok)
	}
	// Lazy reap: the failed Lookup deletes the entry.
	if s.Size() != 0 {
		t.Errorf("expired entry should be reaped by Lookup; Size=%d", s.Size())
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

// TestStickyStore_GCReapsExpiredEntries confirms the explicit GC
// pass clears stale entries even when no Lookup hits them.
func TestStickyStore_GCReapsExpiredEntries(t *testing.T) {
	clk := newMockClock(time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC))
	s := NewStickyStore(time.Minute, clk.Now)
	s.Touch("a", "peer-A")
	s.Touch("b", "peer-B")
	s.Touch("c", "peer-C")

	clk.advance(90 * time.Second)
	s.Touch("d", "peer-D") // still fresh

	s.GC()
	if s.Size() != 1 {
		t.Errorf("Size after GC = %d, want 1 (only peer-D should remain)", s.Size())
	}
	if got, ok := s.Lookup("d"); !ok || got != "peer-D" {
		t.Errorf("survivor lookup wrong: (%q, %v)", got, ok)
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
