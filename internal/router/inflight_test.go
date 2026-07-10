package router

import (
	"sync"
	"testing"
)

func TestInFlightTracker_AcquireRelease(t *testing.T) {
	tr := NewInFlightTracker()

	r1, ok := tr.Acquire("peer-A", 2)
	if !ok {
		t.Fatal("first Acquire should succeed")
	}
	if got := tr.InFlight("peer-A"); got != 1 {
		t.Errorf("after 1st Acquire: InFlight=%d, want 1", got)
	}

	r2, ok := tr.Acquire("peer-A", 2)
	if !ok {
		t.Fatal("second Acquire at cap should succeed (= reaches the limit, not over)")
	}
	if got := tr.InFlight("peer-A"); got != 2 {
		t.Errorf("after 2nd Acquire: InFlight=%d, want 2", got)
	}

	// At cap: 3rd Acquire must fail and NOT increment.
	r3, ok := tr.Acquire("peer-A", 2)
	if ok {
		t.Error("3rd Acquire above cap should fail")
	}
	if got := tr.InFlight("peer-A"); got != 2 {
		t.Errorf("rejected Acquire must not mutate counter; InFlight=%d, want 2", got)
	}
	// Failed-acquire release is a safe no-op.
	r3()
	if got := tr.InFlight("peer-A"); got != 2 {
		t.Errorf("no-op release should not touch counter; InFlight=%d, want 2", got)
	}

	r1()
	r2()
	if got := tr.InFlight("peer-A"); got != 0 {
		t.Errorf("after both releases: InFlight=%d, want 0", got)
	}
}

func TestInFlightTracker_PerDeviceIndependence(t *testing.T) {
	tr := NewInFlightTracker()

	// Fill peer-A to cap.
	releases := make([]func(), 0, 4)
	for i := 0; i < 4; i++ {
		r, ok := tr.Acquire("peer-A", 4)
		if !ok {
			t.Fatalf("filling peer-A: Acquire #%d failed", i)
		}
		releases = append(releases, r)
	}
	if _, ok := tr.Acquire("peer-A", 4); ok {
		t.Error("peer-A 5th Acquire should fail at cap")
	}

	// peer-B should be unaffected.
	rB, ok := tr.Acquire("peer-B", 1)
	if !ok {
		t.Error("peer-B Acquire must not be blocked by peer-A saturation")
	}
	if got := tr.InFlight("peer-B"); got != 1 {
		t.Errorf("peer-B InFlight=%d, want 1", got)
	}

	rB()
	for _, r := range releases {
		r()
	}
}

func TestInFlightTracker_UnlimitedCapacityStillCounts(t *testing.T) {
	tr := NewInFlightTracker()
	releases := make([]func(), 0, 100)
	for i := 0; i < 100; i++ {
		r, ok := tr.Acquire("peer-A", 0) // 0 == unlimited
		if !ok {
			t.Fatalf("Acquire #%d failed under unlimited capacity", i)
		}
		releases = append(releases, r)
	}
	if got := tr.InFlight("peer-A"); got != 100 {
		t.Errorf("InFlight=%d after 100 unlimited Acquires, want 100", got)
	}
	for _, r := range releases {
		r()
	}
	if got := tr.InFlight("peer-A"); got != 0 {
		t.Errorf("InFlight=%d after balanced releases, want 0", got)
	}
}

func TestInFlightTracker_SnapshotOmitsZero(t *testing.T) {
	tr := NewInFlightTracker()

	rA, _ := tr.Acquire("peer-A", 4)
	rB, _ := tr.Acquire("peer-B", 4)
	rC, _ := tr.Acquire("peer-C", 4)
	rC() // peer-C back to 0 before snapshot

	got := tr.Snapshot()
	if _, exists := got["peer-C"]; exists {
		t.Errorf("Snapshot should drop zero entries; peer-C present: %+v", got)
	}
	if got["peer-A"] != 1 || got["peer-B"] != 1 {
		t.Errorf("Snapshot=%+v, want peer-A:1 and peer-B:1", got)
	}

	rA()
	rB()
	if got := tr.Snapshot(); got != nil {
		t.Errorf("empty-state Snapshot=%+v, want nil for compact JSON", got)
	}
}

// TestInFlightTracker_ConcurrentAcquireRespectsLimit runs many
// goroutines hammering the same deviceID under a tight cap; the
// counter must never exceed the cap. Run with -race.
func TestInFlightTracker_ConcurrentAcquireRespectsLimit(t *testing.T) {
	tr := NewInFlightTracker()
	const cap = 4
	const workers = 32
	const iter = 200

	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for i := 0; i < iter; i++ {
				release, ok := tr.Acquire("peer-A", cap)
				if !ok {
					continue
				}
				// Sample 10% of admissions; if InFlight ever exceeds
				// the cap, the CAS loop is broken.
				if i%10 == 0 {
					if got := tr.InFlight("peer-A"); int(got) > cap {
						t.Errorf("InFlight %d > cap %d during race", got, cap)
					}
				}
				release()
			}
		}()
	}
	wg.Wait()
	if got := tr.InFlight("peer-A"); got != 0 {
		t.Errorf("after balanced concurrent Acquire/Release: InFlight=%d, want 0", got)
	}
}
