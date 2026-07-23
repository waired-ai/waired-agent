package main

import (
	"sync"
	"time"
)

// grantUsage records, per Public Share grant ID, the wall-clock time the
// router last committed a request to that grant's provider. It is the
// bridge between two goroutines that never share a lock otherwise:
//
//   - the router's request goroutines write via Mark (from the Selector's
//     OnPublicGrantUsed callback, on every committed public route);
//   - the single background acquirer goroutine reads via LastUsed at renew
//     time and prunes via Forget when it drops a grant.
//
// A held Public Share grant is a live WG peering to a stranger's machine.
// Keying renewal on real traffic is what lets an idle consumer's peerings
// lapse instead of being renewed forever (waired#898, spec §6 renew row /
// §7.3 idle teardown). The map is mutex-guarded; writes are a single map
// insert, so the routing hot path pays almost nothing.
type grantUsage struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func newGrantUsage() *grantUsage {
	return &grantUsage{last: map[string]time.Time{}}
}

// Mark stamps grantID as used at time t. Called off the routing hot path
// with time.Now(); t is a parameter so tests can drive a fake clock.
func (u *grantUsage) Mark(grantID string, t time.Time) {
	if grantID == "" {
		return
	}
	u.mu.Lock()
	u.last[grantID] = t
	u.mu.Unlock()
}

// LastUsed returns the last time a request was committed to grantID, or
// the zero time if it was never used (which reads as "infinitely idle" to
// the acquirer's IdleTTL comparison, so a never-used grant lapses).
func (u *grantUsage) LastUsed(grantID string) time.Time {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.last[grantID]
}

// Forget drops grantID's usage record. The acquirer calls it whenever it
// stops tracking a grant so the map cannot grow without bound across a
// long-lived agent's churn of grants.
func (u *grantUsage) Forget(grantID string) {
	u.mu.Lock()
	delete(u.last, grantID)
	u.mu.Unlock()
}
