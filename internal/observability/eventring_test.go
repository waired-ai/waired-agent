package observability

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestRing_AppendAssignsMonotonicSeq(t *testing.T) {
	r := NewRing(8)
	for i := 0; i < 5; i++ {
		got := r.Append(Event{Kind: KindRequest, Request: &RequestEvent{Model: strconv.Itoa(i)}})
		if got != uint64(i+1) {
			t.Fatalf("append %d: seq = %d, want %d", i, got, i+1)
		}
	}
}

func TestRing_AppendStampsTSWhenZero(t *testing.T) {
	r := NewRing(4)
	before := time.Now()
	r.Append(Event{Kind: KindRequest, Request: &RequestEvent{}})
	after := time.Now()

	events, _, _ := r.Since(0, nil, 0)
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	ts := events[0].TS
	if ts.Before(before) || ts.After(after) {
		t.Fatalf("auto-stamped TS %v not in [%v, %v]", ts, before, after)
	}
}

func TestRing_AppendKeepsExplicitTS(t *testing.T) {
	r := NewRing(4)
	explicit := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	r.Append(Event{Kind: KindRequest, TS: explicit, Request: &RequestEvent{}})

	events, _, _ := r.Since(0, nil, 0)
	if !events[0].TS.Equal(explicit) {
		t.Fatalf("explicit TS overwritten: got %v want %v", events[0].TS, explicit)
	}
}

func TestRing_SinceEmpty(t *testing.T) {
	r := NewRing(8)
	events, oldest, gap := r.Since(0, nil, 0)
	if len(events) != 0 || oldest != 0 || gap {
		t.Fatalf("empty ring: got events=%d oldest=%d gap=%v", len(events), oldest, gap)
	}
}

func TestRing_SinceReturnsAllWhenSinceZero(t *testing.T) {
	r := NewRing(8)
	for i := 0; i < 3; i++ {
		r.Append(Event{Kind: KindRequest, Request: &RequestEvent{Model: strconv.Itoa(i)}})
	}
	events, oldest, gap := r.Since(0, nil, 0)
	if len(events) != 3 || oldest != 1 || gap {
		t.Fatalf("since=0: got events=%d oldest=%d gap=%v", len(events), oldest, gap)
	}
	for i, e := range events {
		if e.Seq != uint64(i+1) {
			t.Fatalf("event %d: seq=%d want %d", i, e.Seq, i+1)
		}
	}
}

func TestRing_SinceCursor(t *testing.T) {
	r := NewRing(8)
	for i := 0; i < 5; i++ {
		r.Append(Event{Kind: KindRequest, Request: &RequestEvent{Model: strconv.Itoa(i)}})
	}
	events, oldest, gap := r.Since(3, nil, 0)
	if len(events) != 2 || oldest != 1 || gap {
		t.Fatalf("since=3 in [1..5]: got events=%d oldest=%d gap=%v", len(events), oldest, gap)
	}
	if events[0].Seq != 4 || events[1].Seq != 5 {
		t.Fatalf("since=3: seqs=%d,%d want 4,5", events[0].Seq, events[1].Seq)
	}
}

func TestRing_SinceCursorAtTip(t *testing.T) {
	r := NewRing(8)
	for i := 0; i < 3; i++ {
		r.Append(Event{Kind: KindRequest, Request: &RequestEvent{}})
	}
	events, _, gap := r.Since(3, nil, 0)
	if len(events) != 0 || gap {
		t.Fatalf("since=tip: events=%d gap=%v", len(events), gap)
	}
}

func TestRing_OverflowEvictsOldest(t *testing.T) {
	r := NewRing(4)
	for i := 0; i < 6; i++ {
		r.Append(Event{Kind: KindRequest, Request: &RequestEvent{Model: strconv.Itoa(i)}})
	}
	events, oldest, gap := r.Since(0, nil, 0)
	if len(events) != 4 {
		t.Fatalf("ring full: want 4 events, got %d", len(events))
	}
	if oldest != 3 {
		t.Fatalf("oldestSeq after overflow: got %d want 3", oldest)
	}
	if gap {
		t.Fatalf("since=0 against fully-populated ring should not gap")
	}
	if events[0].Seq != 3 || events[3].Seq != 6 {
		t.Fatalf("evicted-ring seqs: got %d..%d want 3..6", events[0].Seq, events[3].Seq)
	}
}

func TestRing_GapDetected(t *testing.T) {
	r := NewRing(4)
	for i := 0; i < 6; i++ {
		r.Append(Event{Kind: KindRequest, Request: &RequestEvent{}})
	}
	// Ring now holds seq 3..6. since=1 is below oldest-1 (which is 2).
	_, oldest, gap := r.Since(1, nil, 0)
	if oldest != 3 {
		t.Fatalf("oldest: got %d want 3", oldest)
	}
	if !gap {
		t.Fatalf("since=1 with oldest=3 should signal gap")
	}
}

func TestRing_GapBoundaryNotTriggered(t *testing.T) {
	r := NewRing(4)
	for i := 0; i < 6; i++ {
		r.Append(Event{Kind: KindRequest, Request: &RequestEvent{}})
	}
	// since == oldest - 1 means consumer is caught up to one before
	// oldest; the next event in the ring is exactly oldest. No gap.
	events, _, gap := r.Since(2, nil, 0)
	if gap {
		t.Fatalf("since=oldest-1 must not signal gap")
	}
	if len(events) != 4 {
		t.Fatalf("since=oldest-1: want 4 events, got %d", len(events))
	}
}

func TestRing_KindFilter(t *testing.T) {
	r := NewRing(8)
	r.Append(Event{Kind: KindRequest, Request: &RequestEvent{}})
	r.Append(Event{Kind: KindFallback, Fallback: &FallbackEvent{}})
	r.Append(Event{Kind: KindRequest, Request: &RequestEvent{}})
	r.Append(Event{Kind: KindSelection, Selection: &SelectionEvent{}})
	r.Append(Event{Kind: KindRequest, Request: &RequestEvent{}})

	events, _, _ := r.Since(0, []Kind{KindRequest}, 0)
	if len(events) != 3 {
		t.Fatalf("kind=request filter: got %d want 3", len(events))
	}
	for _, e := range events {
		if e.Kind != KindRequest {
			t.Fatalf("unexpected kind in filtered slice: %s", e.Kind)
		}
	}

	events, _, _ = r.Since(0, []Kind{KindFallback, KindSelection}, 0)
	if len(events) != 2 {
		t.Fatalf("kind={fallback,selection} filter: got %d want 2", len(events))
	}
}

func TestRing_Limit(t *testing.T) {
	r := NewRing(8)
	for i := 0; i < 6; i++ {
		r.Append(Event{Kind: KindRequest, Request: &RequestEvent{}})
	}
	events, _, _ := r.Since(0, nil, 3)
	if len(events) != 3 {
		t.Fatalf("limit=3: got %d", len(events))
	}
	for i, e := range events {
		if e.Seq != uint64(i+1) {
			t.Fatalf("limit slice should preserve chronological order: idx=%d seq=%d", i, e.Seq)
		}
	}
}

func TestRing_LatestRequest(t *testing.T) {
	r := NewRing(8)
	if got := r.LatestRequest(); got != nil {
		t.Fatalf("empty ring: LatestRequest should be nil, got %+v", got)
	}
	r.Append(Event{Kind: KindRequest, Request: &RequestEvent{Model: "first"}})
	r.Append(Event{Kind: KindFallback, Fallback: &FallbackEvent{}})
	r.Append(Event{Kind: KindRequest, Request: &RequestEvent{Model: "second"}})
	r.Append(Event{Kind: KindSelection, Selection: &SelectionEvent{}})

	got := r.LatestRequest()
	if got == nil {
		t.Fatalf("LatestRequest returned nil with 2 requests in ring")
	}
	if got.Request == nil || got.Request.Model != "second" {
		t.Fatalf("LatestRequest model: got %+v want 'second'", got.Request)
	}
}

func TestRing_LatestRequestSkipsNonRequest(t *testing.T) {
	r := NewRing(4)
	r.Append(Event{Kind: KindFallback, Fallback: &FallbackEvent{}})
	r.Append(Event{Kind: KindSelection, Selection: &SelectionEvent{}})
	if got := r.LatestRequest(); got != nil {
		t.Fatalf("no request events: should return nil, got %+v", got)
	}
}

func TestRing_ConcurrentAppendIsRaceFree(t *testing.T) {
	r := NewRing(64)
	var wg sync.WaitGroup
	const workers = 8
	const perWorker = 100
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				r.Append(Event{Kind: KindRequest, Request: &RequestEvent{}})
			}
		}()
	}
	wg.Wait()
	// All seqs must be unique and span 1..workers*perWorker.
	events, _, _ := r.Since(0, nil, 0)
	seen := make(map[uint64]struct{}, len(events))
	for _, e := range events {
		if _, dup := seen[e.Seq]; dup {
			t.Fatalf("duplicate seq %d", e.Seq)
		}
		seen[e.Seq] = struct{}{}
	}
	// 64 ring entries should have evicted earlier ones; the final
	// nextSeq must equal workers*perWorker + 1.
	last := uint64(0)
	for _, e := range events {
		if e.Seq > last {
			last = e.Seq
		}
	}
	if last != workers*perWorker {
		t.Fatalf("last seq: got %d want %d", last, workers*perWorker)
	}
}

func TestRing_DefaultCapacity(t *testing.T) {
	r := NewRing(0)
	if r.cap != DefaultRingCapacity {
		t.Fatalf("cap on NewRing(0): got %d want %d", r.cap, DefaultRingCapacity)
	}
	r = NewRing(-5)
	if r.cap != DefaultRingCapacity {
		t.Fatalf("cap on NewRing(-5): got %d want %d", r.cap, DefaultRingCapacity)
	}
}
