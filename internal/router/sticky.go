package router

import (
	"sync"
	"time"
)

// StickyStore maps a conversation ID to the peer device that last
// served a request with that ID. The Phase 7 Selector looks it up
// first — if the same conversation already routed to a reachable,
// under-capacity peer, sticking with that peer maximises KV-cache
// reuse (llm-d reports 87.4% cache hit at scale with this pattern).
//
// A binding is honoured while it is FRESH (inside DefaultStickyTTL),
// and past that only while the store has no reason to believe the
// peer's cache is gone — see Lookup.
type StickyStore struct {
	ttl time.Duration
	now func() time.Time

	mu sync.Mutex
	m  map[string]stickyEntry // conversationID → entry
	// lastBound is the most recent moment ANY conversation was bound to
	// a device. It is what answers "has this peer been handed to someone
	// else since we were here", which is the whole of the second-chance
	// rule below. Keyed by deviceID, so it is bounded by mesh size.
	lastBound map[string]time.Time
}

type stickyEntry struct {
	deviceID  string
	boundAt   time.Time
	expiresAt time.Time
}

// DefaultStickyTTL is how long a binding is honoured unconditionally.
//
// 10 minutes, raised from the Phase 7 baseline of 5 (owner ruling,
// 2026-08-29, waired-agent#1129). The old figure was set against "the
// operator paused to read the diff"; what the mesh actually serves is a
// coding agent whose operator reads a plan, runs a build, or takes a
// call, and 5 minutes put the boundary inside ordinary human pauses. A
// rebuild is not free: same host, same model, same engine, an appended
// turn answers in 2.57 s against 35.38 s for one whose prefix was lost,
// which is the 33.85 s cold value.
const DefaultStickyTTL = 10 * time.Minute

// StickyHardCap bounds the second chance below. Past this, a binding is
// dropped whatever the evidence says: an engine that has been up for an
// hour has almost certainly reloaded, restarted or been re-tuned, and an
// entry nobody has come back for is just a map row.
//
// Exported because it, not the TTL, is what bounds how long an entry can
// occupy the map — so it is the figure the daemon's sweep is measured
// against.
const StickyHardCap = 60 * time.Minute

// NewStickyStore returns a fresh store. ttl ≤ 0 falls back to
// DefaultStickyTTL; now defaults to time.Now.
func NewStickyStore(ttl time.Duration, now func() time.Time) *StickyStore {
	if ttl <= 0 {
		ttl = DefaultStickyTTL
	}
	if now == nil {
		now = time.Now
	}
	return &StickyStore{
		ttl:       ttl,
		now:       now,
		m:         map[string]stickyEntry{},
		lastBound: map[string]time.Time{},
	}
}

// Lookup returns the peer the given conversation last routed to, if the
// binding may still be honoured. Misses (no entry, empty ID, a binding
// this store has given up on) return ("", false). Callers MUST still
// verify the returned peer is reachable, non-stale, and under its
// Capacity before committing to it — Sticky is a hint, not an authority.
//
// # The second chance (owner ruling, 2026-08-29, waired-agent#1129)
//
// Past DefaultStickyTTL the binding is still honoured while no OTHER
// conversation has been bound to that peer since, up to StickyHardCap:
//
//	特例として、10分経過後でもまだ他のセッションがそのノードに新たに
//	割り当てられておらず kv キャッシュが維持されている可能性がたかければ、
//	そこに優先スケジュールされるように
//
// The condition is answerable here and only here. applyStickyFirst runs
// BEFORE the probe round (SelectK orders candidates, then the gateway
// probes the top k), so no live fact about the peer — residency,
// capacity_used, engine health — is in scope at the moment the binding
// is consulted. What IS in scope is this store's own record of who else
// it has sent to that peer, and on the engine this mesh mostly runs a
// slot IS the unit of retention: OLLAMA_NUM_PARALLEL is 1, or 2 where
// doubling KV costs no context. So "nobody else was sent there" is the
// strongest available evidence that the prefix survived, and "somebody
// was" is near-conclusive that it did not.
//
// A rebound peer's entry is deleted rather than kept: lastBound only
// moves forward, so the entry can never become honourable again.
func (s *StickyStore) Lookup(conversationID string) (deviceID string, ok bool) {
	if conversationID == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, exists := s.m[conversationID]
	if !exists {
		return "", false
	}
	now := s.now()
	if !s.honourableLocked(e, now) {
		delete(s.m, conversationID)
		return "", false
	}
	return e.deviceID, true
}

// honourableLocked is the one rule Lookup and GC share. Caller holds mu.
func (s *StickyStore) honourableLocked(e stickyEntry, now time.Time) bool {
	if now.After(e.boundAt.Add(StickyHardCap)) {
		return false
	}
	if !now.After(e.expiresAt) {
		return true
	}
	// Stale, so it survives only on the second chance: this store has
	// sent nobody else to that peer since. Touch records our own binding
	// in lastBound, so an equal timestamp is us.
	return !s.lastBound[e.deviceID].After(e.boundAt)
}

// Touch records (or refreshes) a sticky binding. Empty conversationID
// or empty deviceID is a no-op.
func (s *StickyStore) Touch(conversationID, deviceID string) {
	if conversationID == "" || deviceID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.m[conversationID] = stickyEntry{
		deviceID:  deviceID,
		boundAt:   now,
		expiresAt: now.Add(s.ttl),
	}
	s.lastBound[deviceID] = now
}

// GC removes bindings that can no longer be honoured. Not strictly
// necessary — Lookup drops the entry it is asked about — but trimming
// the map keeps the memory bound predictable when conversations are
// short-lived.
func (s *StickyStore) GC() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, e := range s.m {
		if !s.honourableLocked(e, now) {
			delete(s.m, k)
		}
	}
	// A device row is only ever read against an entry's boundAt, and no
	// entry outlives StickyHardCap, so a row older than that can answer
	// no remaining question. Dropping it early would grant a second
	// chance the peer has not earned, which is why the cutoff is the cap
	// and not the TTL.
	for dev, at := range s.lastBound {
		if now.After(at.Add(StickyHardCap)) {
			delete(s.lastBound, dev)
		}
	}
}

// Size reports the current entry count. Exposed for tests and a
// future metrics surface.
func (s *StickyStore) Size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}
