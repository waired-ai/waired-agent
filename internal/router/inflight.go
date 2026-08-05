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

// stickyPeer is StickyInFlight's composite key. stickyID is whatever
// the gateway put in Request.StickyID — on the Claude surface that
// already carries the ":<class>" suffix (internal/gateway/anthropic.go),
// so the main and subagent legs of one conversation count separately
// without this type knowing what a class is.
type stickyPeer struct{ stickyID, deviceID string }

// StickyInFlight counts the requests this agent currently has
// outstanding to each peer FOR ONE STICKY KEY. InFlightTracker answers
// "is this peer full"; this answers "is this peer already busy with
// this conversation", which is what the concurrent-sub spread needs
// (waired-ai/waired#828): when a conversation's sticky-bound peer is
// already serving one of its requests and another candidate could take
// the next, the Selector demotes the bound peer instead of piling the
// whole sub-agent fan-out onto it.
//
// It does NOT mirror InFlightTracker's sync.Map. That type is keyed by
// deviceID, so its key space is bounded by the mesh and a permanent
// entry per peer costs nothing. This one is keyed by conversation,
// which is unbounded, so an entry is deleted the moment its count
// returns to zero — a map that only ever grows would need the periodic
// sweep StickyStore needed (#531), and the count is the only thing
// worth keeping.
type StickyInFlight struct {
	mu sync.Mutex
	m  map[stickyPeer]int
}

// NewStickyInFlight returns an empty tracker. Safe for concurrent use
// immediately.
func NewStickyInFlight() *StickyInFlight {
	return &StickyInFlight{m: map[stickyPeer]int{}}
}

// Acquire counts one outstanding request for (stickyID, deviceID) and
// returns the closure that gives it back. Unlike InFlightTracker.Acquire
// it never refuses: this is a routing signal, not an admission gate —
// admission stays with the peer's advertised Capacity.
//
// An empty stickyID or deviceID counts nothing and returns a no-op
// release, so a request with no affinity hint (Request.StickyID empty,
// the whole non-coding-agent case) never allocates a key.
//
// The release closure is idempotent. InFlightTracker's is not, but this
// one is composed with it into the single Selection.Release the gateway
// defers, and a double release there would corrupt a map rather than
// just a counter.
func (t *StickyInFlight) Acquire(stickyID, deviceID string) (release func()) {
	if stickyID == "" || deviceID == "" {
		return func() {}
	}
	k := stickyPeer{stickyID: stickyID, deviceID: deviceID}
	t.mu.Lock()
	t.m[k]++
	t.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			if n := t.m[k]; n > 1 {
				t.m[k] = n - 1
				return
			}
			delete(t.m, k)
		})
	}
}

// InFlight reports how many requests for stickyID are outstanding to
// deviceID right now. 0 for any pair that has never been acquired, and
// for empty ids.
func (t *StickyInFlight) InFlight(stickyID, deviceID string) int {
	if stickyID == "" || deviceID == "" {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.m[stickyPeer{stickyID: stickyID, deviceID: deviceID}]
}

// Size reports the number of (sticky key, peer) pairs with at least one
// request outstanding. Exposed for tests — it is what pins the
// delete-at-zero behaviour — and for a future metrics surface, matching
// StickyStore.Size.
func (t *StickyInFlight) Size() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.m)
}
