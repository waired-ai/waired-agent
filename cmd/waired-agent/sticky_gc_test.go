package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/router"
)

// testClock lets the store's expiry be driven without sleeping for it.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// TestRunStickyGC_ReclaimsWhatLookupNeverRevisits: StickyStore.Lookup
// expires the entry it is asked about, so a conversation that comes
// back is always answered correctly. The entries that need this sweep
// are the ones nothing ever asks about again — which, since Touch runs
// on every mesh commit, is most of them.
func TestRunStickyGC_ReclaimsWhatLookupNeverRevisits(t *testing.T) {
	clk := &testClock{t: time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)}
	s := router.NewStickyStore(time.Minute, clk.Now)
	s.Touch("conv-1", "peer-A")
	s.Touch("conv-2", "peer-B")
	if s.Size() != 2 {
		t.Fatalf("Size before = %d, want 2", s.Size())
	}
	clk.advance(90 * time.Second) // both bindings are now past their TTL

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runStickyGC(ctx, s, time.Millisecond)
	}()

	deadline := time.After(waitBackstop)
	for s.Size() != 0 {
		select {
		case <-deadline:
			t.Fatalf("Size = %d after %s, want 0: the sweep never ran", s.Size(), waitBackstop)
		case <-time.After(time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(waitBackstop):
		t.Fatal("runStickyGC did not return on cancel; the daemon would not shut down")
	}
}

// TestRunStickyGC_NilStoreReturns: main() only builds the store when
// inference is enabled, so the loop must tolerate not having one rather
// than panicking a daemon that is otherwise fine.
func TestRunStickyGC_NilStoreReturns(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		runStickyGC(t.Context(), nil, time.Millisecond)
	}()
	select {
	case <-done:
	case <-time.After(waitBackstop):
		t.Fatal("runStickyGC did not return for a nil store")
	}
}
