package router

import (
	"sync"
	"time"
)

// StickyStore maps a conversation ID to the peer device that last
// served a request with that ID, with a TTL. The Phase 7 Selector
// looks it up first — if the same conversation already routed to a
// reachable, under-capacity peer, sticking with that peer maximises
// KV-cache reuse (llm-d reports 87.4% cache hit at scale with this
// pattern).
//
// TTL is intentionally short (5 min) so a long-idle conversation
// doesn't pin the operator to a peer that has since become slower
// or has restarted (losing the cache anyway).
type StickyStore struct {
	ttl time.Duration
	now func() time.Time

	mu sync.Mutex
	m  map[string]stickyEntry // conversationID → entry
}

type stickyEntry struct {
	deviceID  string
	expiresAt time.Time
}

// DefaultStickyTTL is the Phase 7 baseline. 5 min matches the typical
// "operator paused to read the diff" gap in a coding agent session;
// keeping it under the ~15 s NetworkMap heartbeat × scaling factor
// would force unnecessary cache rebuilds.
const DefaultStickyTTL = 5 * time.Minute

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
		ttl: ttl,
		now: now,
		m:   map[string]stickyEntry{},
	}
}

// Lookup returns the peer the given conversation last routed to, if
// the entry is still within TTL. Misses (no entry, expired entry,
// empty ID) return ("", false). Callers MUST still verify the
// returned peer is reachable, non-stale, and under its Capacity
// before committing to it — Sticky is a hint, not an authority.
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
	if s.now().After(e.expiresAt) {
		delete(s.m, conversationID)
		return "", false
	}
	return e.deviceID, true
}

// Touch records (or refreshes) a sticky binding. Empty conversationID
// or empty deviceID is a no-op.
func (s *StickyStore) Touch(conversationID, deviceID string) {
	if conversationID == "" || deviceID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[conversationID] = stickyEntry{
		deviceID:  deviceID,
		expiresAt: s.now().Add(s.ttl),
	}
}

// GC removes expired entries. Cheap to call opportunistically from
// the hot path (gateway request completion). Not strictly necessary
// — Lookup already lazily expires — but trimming the map keeps the
// memory bound predictable when conversations are short-lived.
func (s *StickyStore) GC() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, e := range s.m {
		if now.After(e.expiresAt) {
			delete(s.m, k)
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
