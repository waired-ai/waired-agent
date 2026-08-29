package main

import (
	"context"
	"time"

	"github.com/waired-ai/waired-agent/internal/router"
)

// stickyGCInterval is how often unhonourable sticky bindings are swept.
//
// A fifth of the TTL. Nothing depends on the exact figure: Lookup
// applies the same rule to the entry it is asked about regardless, so
// this only decides how long a conversation that never returns keeps its
// slot in the map. Note that a binding past its TTL is not necessarily
// unhonourable — see StickyStore.Lookup's second chance — so the sweep
// removes rather less than the interval's name suggests.
const stickyGCInterval = router.DefaultStickyTTL / 5

// runStickyGC drops expired sticky bindings on a fixed cadence until
// ctx is done.
//
// StickyStore.Lookup already drops the entry it is asked about when the
// binding can no longer be honoured, so a conversation that comes back
// always gets the right answer. What is left behind is the entry for a
// conversation that never comes back —
// and those are the majority, since Touch runs on every mesh commit and
// short conversations dominate. Reclaiming them keeps the map's size a
// function of recent traffic rather than of daemon uptime.
//
// A ticker, not a call from the gateway's request path: the sweep walks
// every entry under the store's mutex, so paying it per request would
// make request cost grow with the number of IDLE conversations, which
// is the opposite of the point. It would also need a handle on a router
// primitive that the gateway is deliberately not given.
func runStickyGC(ctx context.Context, s *router.StickyStore, interval time.Duration) {
	if s == nil {
		return
	}
	if interval <= 0 {
		interval = stickyGCInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.GC()
		}
	}
}
