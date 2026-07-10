package router

import (
	"sync"
	"sync/atomic"
)

// InFlightTracker counts the number of overlay-inference requests this
// agent currently has outstanding to each mesh peer. The Phase 7
// Selector consults it during admission: if a peer's count is at or
// above its advertised Capacity, the candidate is skipped and the
// next-best scored peer is tried instead.
//
// Implementation notes:
//
//   - sync.Map avoids a global mutex on the hot lookup path (one
//     Acquire per request × N peers). Per-key state is a
//     *atomic.Int32 so increments/decrements are lock-free.
//   - Acquire returns a release closure: the caller defers it so a
//     panic in the downstream proxy still decrements the counter.
//   - capacity == 0 is the "unlimited" path; Acquire still increments
//     so Snapshot reflects real concurrent usage (used by the future
//     metrics surface, not by the Selector).
type InFlightTracker struct {
	m sync.Map // deviceID → *atomic.Int32
}

// NewInFlightTracker returns an empty tracker. Safe for concurrent
// use immediately.
func NewInFlightTracker() *InFlightTracker {
	return &InFlightTracker{}
}

// counter materialises (or returns) the per-deviceID atomic counter.
// Always returns a non-nil pointer; the caller can safely Load /
// CompareAndSwap on it.
func (t *InFlightTracker) counter(deviceID string) *atomic.Int32 {
	if v, ok := t.m.Load(deviceID); ok {
		return v.(*atomic.Int32)
	}
	// LoadOrStore so concurrent first-time-Acquires don't race a fresh
	// counter into existence and drop one increment.
	fresh := new(atomic.Int32)
	actual, _ := t.m.LoadOrStore(deviceID, fresh)
	return actual.(*atomic.Int32)
}

// Acquire atomically tries to admit one request for deviceID. It
// returns a release closure (always non-nil) plus an ok flag:
//
//   - ok == true and capacity > 0 ⇒ the new in-flight count is ≤ capacity.
//   - ok == true and capacity == 0 ⇒ unlimited; the counter still
//     advances so callers can observe real load.
//   - ok == false ⇒ a peer in-flight ≥ capacity; the counter is NOT
//     modified and the returned release is a no-op (safe to call).
//
// The release closure must be called exactly once per successful
// Acquire — typically `defer release()`. Calling it after a
// no-op (ok == false) is harmless.
func (t *InFlightTracker) Acquire(deviceID string, capacity int) (release func(), ok bool) {
	c := t.counter(deviceID)
	if capacity <= 0 {
		c.Add(1)
		return func() { c.Add(-1) }, true
	}
	for {
		cur := c.Load()
		if int(cur) >= capacity {
			return func() {}, false
		}
		if c.CompareAndSwap(cur, cur+1) {
			return func() { c.Add(-1) }, true
		}
	}
}

// Snapshot returns the current per-deviceID in-flight counts.
// Allocates a fresh map; safe to read without further locking.
// Returns nil when no peer has had an Acquire call yet (keeps the
// downstream JSON shape compact).
func (t *InFlightTracker) Snapshot() map[string]int32 {
	var out map[string]int32
	t.m.Range(func(k, v any) bool {
		n := v.(*atomic.Int32).Load()
		if n == 0 {
			return true
		}
		if out == nil {
			out = make(map[string]int32)
		}
		out[k.(string)] = n
		return true
	})
	return out
}

// InFlight reports the current count for a single deviceID. 0 for
// unknown peers.
func (t *InFlightTracker) InFlight(deviceID string) int32 {
	if v, ok := t.m.Load(deviceID); ok {
		return v.(*atomic.Int32).Load()
	}
	return 0
}
