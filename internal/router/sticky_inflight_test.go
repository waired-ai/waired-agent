package router

import (
	"sync"
	"testing"
)

func TestStickyInFlight_CountsPerKeyAndPeer(t *testing.T) {
	tr := NewStickyInFlight()

	if got := tr.InFlight("conv-1", "peer-A"); got != 0 {
		t.Fatalf("fresh tracker: InFlight = %d, want 0", got)
	}

	relA1 := tr.Acquire("conv-1", "peer-A")
	relA2 := tr.Acquire("conv-1", "peer-A")
	relB := tr.Acquire("conv-1", "peer-B")
	relOther := tr.Acquire("conv-2", "peer-A")

	if got := tr.InFlight("conv-1", "peer-A"); got != 2 {
		t.Errorf("conv-1/peer-A = %d, want 2", got)
	}
	// The three assertions below are the whole point of the composite
	// key: a different peer and a different conversation must not read
	// as load on (conv-1, peer-A).
	if got := tr.InFlight("conv-1", "peer-B"); got != 1 {
		t.Errorf("conv-1/peer-B = %d, want 1", got)
	}
	if got := tr.InFlight("conv-2", "peer-A"); got != 1 {
		t.Errorf("conv-2/peer-A = %d, want 1", got)
	}
	if got := tr.InFlight("conv-2", "peer-B"); got != 0 {
		t.Errorf("conv-2/peer-B = %d, want 0", got)
	}

	relA1()
	if got := tr.InFlight("conv-1", "peer-A"); got != 1 {
		t.Errorf("after one release: conv-1/peer-A = %d, want 1", got)
	}
	relA2()
	relB()
	relOther()
	if got := tr.InFlight("conv-1", "peer-A"); got != 0 {
		t.Errorf("after all releases: conv-1/peer-A = %d, want 0", got)
	}
}

// TestStickyInFlight_ReleaseDeletesTheEntry is the reason this type
// does not reuse InFlightTracker's sync.Map: the key space is one entry
// per conversation, so anything that only ever grows needs the periodic
// sweep StickyStore needed (#531). Nothing sweeps this one — reaching
// zero has to remove the key.
func TestStickyInFlight_ReleaseDeletesTheEntry(t *testing.T) {
	tr := NewStickyInFlight()
	for i := 0; i < 100; i++ {
		rel := tr.Acquire("conv-"+string(rune('a'+i%26))+string(rune('a'+i/26)), "peer-A")
		rel()
	}
	if got := tr.Size(); got != 0 {
		t.Fatalf("Size = %d after every request finished, want 0 (entries leak)", got)
	}

	rel1 := tr.Acquire("conv-1", "peer-A")
	rel2 := tr.Acquire("conv-1", "peer-A")
	if got := tr.Size(); got != 1 {
		t.Errorf("two requests on one key: Size = %d, want 1", got)
	}
	rel1()
	if got := tr.Size(); got != 1 {
		t.Errorf("one of two released: Size = %d, want 1 (entry still in use)", got)
	}
	rel2()
	if got := tr.Size(); got != 0 {
		t.Errorf("both released: Size = %d, want 0", got)
	}
}

// TestStickyInFlight_ReleaseIsIdempotent pins the guarantee the
// composed Selection.Release depends on. Without it a double release
// would delete a key another in-flight request still owns, and the
// Selector would then read that peer as free and decline to spread.
func TestStickyInFlight_ReleaseIsIdempotent(t *testing.T) {
	tr := NewStickyInFlight()
	relFirst := tr.Acquire("conv-1", "peer-A")
	tr.Acquire("conv-1", "peer-A")

	relFirst()
	relFirst()
	relFirst()

	if got := tr.InFlight("conv-1", "peer-A"); got != 1 {
		t.Errorf("after three calls to one release: InFlight = %d, want 1", got)
	}
}

func TestStickyInFlight_EmptyIDsCountNothing(t *testing.T) {
	tr := NewStickyInFlight()
	// A request with no affinity hint (Request.StickyID empty) is the
	// whole non-coding-agent case; it must not allocate a key.
	tr.Acquire("", "peer-A")()
	tr.Acquire("conv-1", "")()
	rel := tr.Acquire("", "")
	rel()

	if got := tr.Size(); got != 0 {
		t.Errorf("Size = %d, want 0 (empty ids must not allocate)", got)
	}
	if got := tr.InFlight("", "peer-A"); got != 0 {
		t.Errorf("InFlight with empty sticky id = %d, want 0", got)
	}
	if got := tr.InFlight("conv-1", ""); got != 0 {
		t.Errorf("InFlight with empty device id = %d, want 0", got)
	}
}

// TestStickyInFlight_ConcurrentAcquireRelease is the -race case: the
// gateway acquires and releases from one goroutine per request.
func TestStickyInFlight_ConcurrentAcquireRelease(t *testing.T) {
	tr := NewStickyInFlight()
	const goroutines, iterations = 16, 200

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			peer := "peer-" + string(rune('A'+g%4))
			for i := 0; i < iterations; i++ {
				rel := tr.Acquire("conv-1", peer)
				_ = tr.InFlight("conv-1", peer)
				rel()
			}
		}(g)
	}
	wg.Wait()

	if got := tr.Size(); got != 0 {
		t.Errorf("Size = %d after every goroutine finished, want 0", got)
	}
}
