package main

import (
	"sync"
	"testing"
	"time"
)

func TestGrantUsage_MarkLastUsedForget(t *testing.T) {
	u := newGrantUsage()
	t0 := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)

	// Unknown grant reads as the zero time (== infinitely idle).
	if got := u.LastUsed("g1"); !got.IsZero() {
		t.Fatalf("unknown grant LastUsed = %v, want zero", got)
	}

	u.Mark("g1", t0)
	if got := u.LastUsed("g1"); !got.Equal(t0) {
		t.Fatalf("LastUsed after Mark = %v, want %v", got, t0)
	}

	// A later Mark advances the timestamp.
	t1 := t0.Add(3 * time.Minute)
	u.Mark("g1", t1)
	if got := u.LastUsed("g1"); !got.Equal(t1) {
		t.Fatalf("LastUsed after 2nd Mark = %v, want %v", got, t1)
	}

	// Empty grant IDs are ignored (the router never reports one, but be safe).
	u.Mark("", t1)
	if got := u.LastUsed(""); !got.IsZero() {
		t.Fatalf(`LastUsed("") = %v, want zero`, got)
	}

	u.Forget("g1")
	if got := u.LastUsed("g1"); !got.IsZero() {
		t.Fatalf("LastUsed after Forget = %v, want zero", got)
	}
}

// TestGrantUsage_ConcurrentMarkAndRead exercises the router-writes /
// loop-reads split under the race detector: many goroutines Mark while
// others LastUsed/Forget.
func TestGrantUsage_ConcurrentMarkAndRead(t *testing.T) {
	u := newGrantUsage()
	base := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id := "g" + string(rune('a'+n))
			for j := 0; j < 500; j++ {
				u.Mark(id, base.Add(time.Duration(j)*time.Second))
				_ = u.LastUsed(id)
				if j%50 == 0 {
					u.Forget(id)
				}
			}
		}(i)
	}
	wg.Wait()
}
