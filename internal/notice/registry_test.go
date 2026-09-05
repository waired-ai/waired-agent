package notice

import (
	"testing"
	"time"
)

// clock is a hand-wound test clock. Expiry is policy, not timing, so it
// is exercised by moving the clock rather than by sleeping.
type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func newTestRegistry(ttl time.Duration) (*Registry, *clock) {
	c := &clock{t: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)}
	return NewRegistry(ttl, c.now), c
}

func titles(ns []Notice) []string {
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, n.Title)
	}
	return out
}

// TestActiveDropsASourceNobodyRepublishes
//
// PRODUCT CONTRACT (owner ruling, 2026-09-05: "消えるかは、しばらく通知が
// なければ消す"). This is the whole reason the registry holds leases
// rather than values: nothing has to remember to clear a notice, so a
// producer that dies cannot leave one on screen forever.
func TestActiveDropsASourceNobodyRepublishes(t *testing.T) {
	r, c := newTestRegistry(60 * time.Second)
	r.Publish("inference", []Notice{LighterModel("a", "b", 1, 2)})

	c.add(59 * time.Second)
	if got := r.Active(); len(got) != 1 {
		t.Fatalf("inside the lease: got %d notices, want 1", len(got))
	}
	c.add(2 * time.Second)
	if got := r.Active(); len(got) != 0 {
		t.Fatalf("past the lease: got %v, want none", titles(got))
	}
}

// TestRepublishRenewsTheLease
//
// PRODUCT CONTRACT (owner ruling above: producers keep pushing what they
// want shown).
func TestRepublishRenewsTheLease(t *testing.T) {
	r, c := newTestRegistry(60 * time.Second)
	for range 6 {
		r.Publish("inference", []Notice{LighterModel("a", "b", 1, 2)})
		c.add(15 * time.Second)
	}
	if got := r.Active(); len(got) != 1 {
		t.Fatalf("a republished notice lapsed: got %v", titles(got))
	}
}

// TestRepublishCarriesFirstSeenForward
//
// PRODUCT CONTRACT (owner ruling above). Active() orders by first
// appearance so rows stay put; a republish that re-stamped FirstSeen
// would reshuffle the list on every heartbeat, which is the opposite of
// what the ordering is for.
func TestRepublishCarriesFirstSeenForward(t *testing.T) {
	r, c := newTestRegistry(60 * time.Second)
	r.Publish("inference", []Notice{LighterModel("a", "b", 1, 2)})
	first := r.Active()[0].FirstSeen

	c.add(15 * time.Second)
	r.Publish("inference", []Notice{LighterModel("a", "b", 1, 2)})

	if got := r.Active()[0].FirstSeen; !got.Equal(first) {
		t.Fatalf("FirstSeen moved on republish: %v then %v", first, got)
	}
}

// TestPublishReplacesTheProducersWholeSet
//
// PRODUCT CONTRACT (owner ruling above). A producer whose answer moves
// to a different target must be able to say the old one is gone. Keyed
// per notice instead of per producer, both would stand side by side for
// a whole lease — two suggestions naming different models at once.
func TestPublishReplacesTheProducersWholeSet(t *testing.T) {
	r, _ := newTestRegistry(60 * time.Second)
	r.Publish("inference", []Notice{LighterModel("a", "old-target", 1, 2)})
	r.Publish("inference", []Notice{LighterModel("a", "new-target", 1, 2)})

	got := r.Active()
	if len(got) != 1 {
		t.Fatalf("got %v, want only the newer suggestion", titles(got))
	}
	if got[0].Target != "new-target" {
		t.Fatalf("target = %q, want new-target", got[0].Target)
	}
}

// TestPublishEmptyClearsAtOnce
//
// PRODUCT CONTRACT (owner ruling above). This is what lets an event that
// resolves a notice — someone declining a suggestion — take effect
// immediately instead of leaving the row up until the lease lapses.
func TestPublishEmptyClearsAtOnce(t *testing.T) {
	r, _ := newTestRegistry(60 * time.Second)
	r.Publish("inference", []Notice{LighterModel("a", "b", 1, 2)})
	r.Publish("inference", nil)
	if got := r.Active(); len(got) != 0 {
		t.Fatalf("got %v, want none", titles(got))
	}
}

// TestProducersDoNotOverwriteEachOther
//
// PRODUCT CONTRACT (owner ruling, 2026-09-05: "こういう通知を全部出す
// （つまり上書きされない）"). The field carries every producer's notices;
// none of them is a single slot the last writer wins.
func TestProducersDoNotOverwriteEachOther(t *testing.T) {
	r, _ := newTestRegistry(60 * time.Second)
	r.Publish("one", []Notice{LighterModel("a", "b", 1, 2)})
	r.Publish("two", []Notice{BetterModel("a", "c", 1, 2)})

	if got := r.Active(); len(got) != 2 {
		t.Fatalf("got %v, want both", titles(got))
	}
}

// TestActiveOrdersWarningsFirstThenByFirstAppearance records today's
// ordering: severity descending, then first appearance, then kind.
func TestActiveOrdersWarningsFirstThenByFirstAppearance(t *testing.T) {
	r, c := newTestRegistry(60 * time.Second)
	r.Publish("early-info", []Notice{BetterModel("a", "b", 1, 2)})
	c.add(time.Second)
	r.Publish("late-warn", []Notice{LighterModel("a", "c", 1, 2)})

	got := r.Active()
	if len(got) != 2 || got[0].Severity != SeverityWarn {
		t.Fatalf("got %v, want the warning first", titles(got))
	}
}

// TestActiveClampsToMaxActive records today's behaviour. A surface
// pre-allocates MaxActive rows and cannot add more later, so the
// registry never hands one a list it has nowhere to put.
func TestActiveClampsToMaxActive(t *testing.T) {
	r, _ := newTestRegistry(60 * time.Second)
	for i := range MaxActive + 3 {
		r.Publish(string(rune('a'+i)), []Notice{LighterModel("a", string(rune('a'+i)), 1, 2)})
	}
	if got := r.Active(); len(got) != MaxActive {
		t.Fatalf("got %d notices, want %d", len(got), MaxActive)
	}
}

// TestActiveIsSafeForConcurrentUse records today's behaviour: the
// producer publishes on its own timer while three surfaces read.
func TestActiveIsSafeForConcurrentUse(t *testing.T) {
	r := NewRegistry(time.Minute, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 200 {
			r.Publish("inference", []Notice{LighterModel("a", "b", 1, 2)})
		}
	}()
	for range 200 {
		_ = r.Active()
	}
	<-done
}
