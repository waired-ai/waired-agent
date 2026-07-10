package router

import (
	"sync"
	"time"
)

// ErrorWindow tracks a sliding 60-second per-peer success/failure
// count split across four 15-second buckets. Phase 7's Selector
// reads the snapshot to break ties between same-score candidates —
// healthier peers (lower error rate) win first, then RTT, then
// deviceID.
//
// Bucket rotation is lazy: Record advances the ring head based on
// the elapsed wall-clock time, so an idle peer's stale samples age
// out the next time anyone touches the window without needing a
// background ticker. Snapshot does the same drain so a Selector
// query for a peer with no recent records reports the correct rate
// (= 0 if all buckets aged out).
//
// Concurrency: a single sync.Mutex serialises both Record and
// Snapshot. The contention window is tiny — three map lookups and
// up to four counter increments — so a finer-grained lock isn't
// worth the cognitive overhead.
type ErrorWindow struct {
	now func() time.Time

	mu      sync.Mutex
	entries map[string]*errorEntry
}

type errorEntry struct {
	// buckets is a fixed-size ring of (success, failure) counts;
	// bucket[head] is the most recent. Rotated lazily by drainLocked
	// when time has advanced past the current bucket's edge.
	buckets [errorWindowBuckets]struct {
		successes uint32
		failures  uint32
	}
	head int       // index of the current (most recent) bucket
	tip  time.Time // start-of-bucket timestamp for buckets[head]
}

const (
	// errorWindowBuckets × errorWindowBucketWidth defines the total
	// window. 4 × 15 s = 60 s — matches the staleness window already
	// used by the inferencemesh aggregator so a peer that drops out
	// of routing also drops out of the error history at the same
	// pace.
	errorWindowBuckets     = 4
	errorWindowBucketWidth = 15 * time.Second
)

// NewErrorWindow returns an empty window. now defaults to time.Now
// when nil; tests inject a mocked clock to drive bucket rotation
// deterministically.
func NewErrorWindow(now func() time.Time) *ErrorWindow {
	if now == nil {
		now = time.Now
	}
	return &ErrorWindow{
		now:     now,
		entries: map[string]*errorEntry{},
	}
}

// Record folds one outcome into the per-peer window. ok=true is a
// success (200 / 2xx from the overlay request), ok=false is a
// failure (5xx, dial error, timeout). deviceID="" is a no-op
// (Selector should never produce an empty target, but guard anyway).
func (w *ErrorWindow) Record(deviceID string, ok bool) {
	if deviceID == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	e, exists := w.entries[deviceID]
	if !exists {
		e = &errorEntry{tip: bucketStart(now)}
		w.entries[deviceID] = e
	}
	w.drainLocked(e, now)
	b := &e.buckets[e.head]
	if ok {
		b.successes++
	} else {
		b.failures++
	}
}

// Snapshot returns the current failure fraction per peer. Empty map
// (returned as nil to keep the result compact) when no peer has
// any observations within the live window.
//
// A peer with zero total observations in the window is omitted —
// the Selector treats "no data" identically to "no errors observed"
// for the tie-break, and excluding zero entries keeps the in-process
// LocalErrors snapshot compact.
func (w *ErrorWindow) Snapshot() map[string]float32 {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	var out map[string]float32
	for deviceID, e := range w.entries {
		w.drainLocked(e, now)
		var s, f uint64
		for _, b := range e.buckets {
			s += uint64(b.successes)
			f += uint64(b.failures)
		}
		total := s + f
		if total == 0 {
			continue
		}
		if out == nil {
			out = make(map[string]float32)
		}
		out[deviceID] = float32(f) / float32(total)
	}
	return out
}

// drainLocked rotates aged buckets to zero based on elapsed time
// since e.tip. Skipping more than errorWindowBuckets steps zeros
// the whole window — that peer had no activity for a full minute,
// so all prior samples are gone. Caller must hold w.mu.
func (w *ErrorWindow) drainLocked(e *errorEntry, now time.Time) {
	currentStart := bucketStart(now)
	if !currentStart.After(e.tip) {
		return
	}
	steps := int(currentStart.Sub(e.tip) / errorWindowBucketWidth)
	if steps <= 0 {
		return
	}
	if steps >= errorWindowBuckets {
		// Entire window has aged out; reset.
		for i := range e.buckets {
			e.buckets[i].successes = 0
			e.buckets[i].failures = 0
		}
		e.head = 0
		e.tip = currentStart
		return
	}
	for i := 0; i < steps; i++ {
		e.head = (e.head + 1) % errorWindowBuckets
		e.buckets[e.head].successes = 0
		e.buckets[e.head].failures = 0
	}
	e.tip = currentStart
}

// bucketStart rounds t down to the nearest 15-second boundary
// (UTC). Aligning the boundary independently of e.tip means two
// peers updated in the same Record call land in the same bucket
// even if their first sample arrived at slightly different times.
func bucketStart(t time.Time) time.Time {
	return t.UTC().Truncate(errorWindowBucketWidth)
}
