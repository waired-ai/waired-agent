package observability

import (
	"strconv"
	"testing"
)

// TestRing_RecentRequestsNewestFirst pins the order, which is the whole
// reason a caller reaches for this rather than Since: the first element
// is "what just happened" and everything after it is what to judge it
// against.
func TestRing_RecentRequestsNewestFirst(t *testing.T) {
	r := NewRing(16)
	for i := 0; i < 5; i++ {
		r.Append(Event{Kind: KindRequest, Request: &RequestEvent{Model: strconv.Itoa(i)}})
	}
	got := r.RecentRequests(5)
	if len(got) != 5 {
		t.Fatalf("got %d events, want 5", len(got))
	}
	for i, want := range []string{"4", "3", "2", "1", "0"} {
		if got[i].Request.Model != want {
			t.Errorf("events[%d].Model = %q, want %q (newest first)", i, got[i].Request.Model, want)
		}
	}
}

// TestRing_RecentRequestsHonoursLimit checks the cap, and that it counts
// the events RETURNED rather than the entries walked — a caller asking
// for 2 gets the 2 newest, not whatever the 2 newest slots happened to
// hold.
func TestRing_RecentRequestsHonoursLimit(t *testing.T) {
	r := NewRing(16)
	for i := 0; i < 5; i++ {
		r.Append(Event{Kind: KindRequest, Request: &RequestEvent{Model: strconv.Itoa(i)}})
	}
	got := r.RecentRequests(2)
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
	if got[0].Request.Model != "4" || got[1].Request.Model != "3" {
		t.Errorf("got %q,%q, want 4,3", got[0].Request.Model, got[1].Request.Model)
	}
	if r.RecentRequests(0) != nil || r.RecentRequests(-1) != nil {
		t.Error("a non-positive limit must return nothing rather than everything")
	}
}

// TestRing_RecentRequestsSkipsOtherKinds is the case that would silently
// break a caller: a ring busy with fallback events must still yield the
// request events buried in it, rather than stopping at the first entry
// that is not one.
func TestRing_RecentRequestsSkipsOtherKinds(t *testing.T) {
	r := NewRing(32)
	r.Append(Event{Kind: KindRequest, Request: &RequestEvent{Model: "old"}})
	for i := 0; i < 10; i++ {
		r.Append(Event{Kind: KindFallback, Fallback: &FallbackEvent{}})
	}
	r.Append(Event{Kind: KindRequest, Request: &RequestEvent{Model: "new"}})

	got := r.RecentRequests(8)
	if len(got) != 2 {
		t.Fatalf("got %d request events, want 2 — non-request kinds must be skipped, not terminate the walk", len(got))
	}
	if got[0].Request.Model != "new" || got[1].Request.Model != "old" {
		t.Errorf("got %q,%q, want new,old", got[0].Request.Model, got[1].Request.Model)
	}
}

// TestRing_RecentRequestsEmpty covers both empty answers, which callers
// must be able to tell apart from a zero-valued reading.
func TestRing_RecentRequestsEmpty(t *testing.T) {
	if got := NewRing(8).RecentRequests(4); got != nil {
		t.Errorf("an empty ring returned %d events, want nil", len(got))
	}
	r := NewRing(8)
	for i := 0; i < 4; i++ {
		r.Append(Event{Kind: KindFallback, Fallback: &FallbackEvent{}})
	}
	if got := r.RecentRequests(4); got != nil {
		t.Errorf("a ring with no request events returned %d, want nil", len(got))
	}
}

// TestRing_RecentRequestsAfterOverflow checks the wrapped case. The
// index arithmetic is the part most likely to be wrong, and a ring that
// has wrapped is the normal state on a busy host, not an edge case.
func TestRing_RecentRequestsAfterOverflow(t *testing.T) {
	r := NewRing(4)
	for i := 0; i < 10; i++ {
		r.Append(Event{Kind: KindRequest, Request: &RequestEvent{Model: strconv.Itoa(i)}})
	}
	got := r.RecentRequests(10)
	if len(got) != 4 {
		t.Fatalf("got %d events, want 4 (the ring capacity)", len(got))
	}
	for i, want := range []string{"9", "8", "7", "6"} {
		if got[i].Request.Model != want {
			t.Errorf("events[%d].Model = %q, want %q", i, got[i].Request.Model, want)
		}
	}
}
