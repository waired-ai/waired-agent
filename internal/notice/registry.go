package notice

import (
	"cmp"
	"slices"
	"sync"
	"time"
)

// DefaultTTL is how long a published set stays live without being
// republished. It tolerates three missed heartbeats at the 15 s cadence
// cmd/waired-agent republishes on, the same shape and the same reasoning
// as the setup executor's lease (cmd/waired-agent/setup_desired.go).
const DefaultTTL = 60 * time.Second

// Registry holds the notices the daemon is currently publishing.
//
// A producer publishes its WHOLE set at once and keeps republishing it;
// a set nobody republishes lapses on its own. That is the owner's design
// (2026-09-05): every producer's notices are shown, no producer
// overwrites another's, and a notice disappears when it stops being
// produced rather than when someone remembers to clear it.
//
// The unit is a set per producer rather than one notice per kind for two
// reasons that only show up once a producer repeats itself. Replacing a
// single notice re-stamps FirstSeen on every republish, which reshuffles
// the very order FirstSeen exists to keep steady; and when a producer's
// answer moves to a different Target, a per-kind entry cannot say the
// old one is gone, so two contradictory notices sit side by side for a
// whole TTL. Whether two notices are the same thing is knowledge the
// producer has and the registry does not.
//
// The zero value is not usable; call NewRegistry.
type Registry struct {
	ttl time.Duration
	now func() time.Time

	mu      sync.RWMutex
	sources map[string]source
}

// source is one producer's published set and its lease.
type source struct {
	notices []Notice
	expires time.Time
}

// NewRegistry returns a registry whose leases last ttl. A zero ttl means
// DefaultTTL; a nil clock means time.Now (the clock is injected so the
// expiry policy is testable without sleeping).
func NewRegistry(ttl time.Duration, now func() time.Time) *Registry {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if now == nil {
		now = time.Now
	}
	return &Registry{ttl: ttl, now: now, sources: make(map[string]source)}
}

// Publish replaces everything published under name with ns and renews
// its lease. Publishing nil or an empty slice clears that producer at
// once, which is what lets an event that resolves a notice — a person
// declining a suggestion — take effect without waiting for the lease to
// lapse.
//
// FirstSeen is carried forward from the previous set for any notice that
// matches on (Kind, Target), so republishing an unchanged notice does
// not make it look new.
func (r *Registry) Publish(name string, ns []Notice) {
	now := r.now()

	r.mu.Lock()
	defer r.mu.Unlock()

	if len(ns) == 0 {
		delete(r.sources, name)
		return
	}

	prev := make(map[[2]string]time.Time, len(r.sources[name].notices))
	for _, p := range r.sources[name].notices {
		prev[[2]string{string(p.Kind), p.Target}] = p.FirstSeen
	}

	out := make([]Notice, 0, len(ns))
	for _, n := range ns {
		n.FirstSeen = now
		if seen, ok := prev[[2]string{string(n.Kind), n.Target}]; ok && !seen.IsZero() {
			n.FirstSeen = seen
		}
		n.ExpiresAt = now.Add(r.ttl)
		out = append(out, n)
	}
	r.sources[name] = source{notices: out, expires: now.Add(r.ttl)}
}

// Active returns the notices to show now, ordered and clamped.
//
// Expiry is applied here rather than on a sweep: a lapsed set is simply
// not returned, and it is dropped from the map as a side effect so a
// producer that stops for good does not leak. Order is severity
// descending, then first appearance, then Kind — deterministic, and
// stable across republishes, so rows do not swap under the cursor.
func (r *Registry) Active() []Notice {
	now := r.now()

	r.mu.Lock()
	defer r.mu.Unlock()

	var out []Notice
	for name, s := range r.sources {
		if !s.expires.After(now) {
			delete(r.sources, name)
			continue
		}
		out = append(out, s.notices...)
	}
	slices.SortStableFunc(out, func(a, b Notice) int {
		if c := cmp.Compare(b.Severity, a.Severity); c != 0 {
			return c
		}
		if c := a.FirstSeen.Compare(b.FirstSeen); c != 0 {
			return c
		}
		return cmp.Compare(string(a.Kind), string(b.Kind))
	})
	return Clamp(out)
}

// Clamp cuts a list to MaxActive. Every surface calls it on what it
// receives as well as trusting the daemon to have done it, so a daemon
// newer than the CLI cannot make a renderer overrun the rows it has.
func Clamp(ns []Notice) []Notice {
	if len(ns) > MaxActive {
		return ns[:MaxActive]
	}
	return ns
}
