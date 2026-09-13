package router

import (
	"context"
	"sync"
	"sync/atomic"
)

// Assignments makes "rank this request, then count it against the computer
// it ranked first" one step, for requests that arrive together
// (waired-agent#1354).
//
// Without it, every request of a burst ranks on the same numbers. Claude Code
// launches its subagents within a few milliseconds of each other, and all six
// of a measured burst had been ranked before the first of them committed
// anywhere: the congestion term of assignSpeedRanks, (capacity_used + 1), did
// not include one sibling. Four of the six landed on the requester's own
// one-slot engine and ran one after another for up to 371 s, while two peers
// that ranked just below it sat idle. The same six requests sent 50 ms apart
// spread 1/1/2/2.
//
// Ranking and decision are this requester's own (the control plane hands out
// the network map, and nothing per request), so the fix is local: selections
// that hold the lock run one after another, and each one's first choice is
// counted before the lock is let go. The ranking itself stays short — it
// reads snapshots and sorts — and the readiness probes run outside the lock,
// in parallel, as docs/decisions/20260805/1703 has them. The owner chose this
// shape on 2026-09-14 (docs/decisions/20260914/0420-assignment-is-counted-when-it-is-made.md).
//
// What is counted:
//
//   - A peer, in LocalInFlight, the same count admission has always read —
//     from the moment the request is assigned rather than from its Commit.
//   - This device's own engine, here. It is never refused: the local leg
//     waits for a slot and has no ceiling (docs/decisions/20260912/2130 §3).
//     The count only enters the ranking.
//
// The zero value is ready to use. A nil *Assignments is the pre-#1354
// behaviour: no serialisation, and this device's own engine is ranked on its
// served count alone.
type Assignments struct {
	mu    sync.Mutex
	local atomic.Int32
}

// NewAssignments returns an empty set. Safe for concurrent use.
func NewAssignments() *Assignments { return &Assignments{} }

// Local reports how many requests this device has assigned to its own
// engine and not yet released: waiting for a slot and holding one alike.
func (a *Assignments) Local() int {
	if a == nil {
		return 0
	}
	return int(a.local.Load())
}

// addLocal counts one request against this device's own engine and returns
// the closure that gives it back. The closure is idempotent: it is composed
// into Selection.Release, which the gateway defers once, and into
// Candidate.Abandon, and a double release would understate the count for as
// long as the process runs.
func (a *Assignments) addLocal() func() {
	a.local.Add(1)
	var once sync.Once
	return func() { once.Do(func() { a.local.Add(-1) }) }
}

// SelectKAssigned is SelectK for a request that is about to be dispatched:
// the ranking runs under Inputs.Assignments' lock, and the first candidate
// is assigned — its slot counted — before the lock is released, so the next
// request's ranking already sees it.
//
// The caller commits one candidate and must call Abandon on the others (a
// candidate that was not assigned, or has been committed, ignores it).
// Without Inputs.Assignments this is SelectK.
//
// The first candidate is assigned even though it has not been probed. If its
// probe comes back not ready, the caller commits a later ready candidate and
// abandons this one; the count it held is given back then. Holding it for the
// length of a probe round is what keeps a concurrent request from ranking on
// a count that is about to be wrong.
func (s *Selector) SelectKAssigned(ctx context.Context, req Request, k int) ([]Candidate, error) {
	a := s.in.Assignments
	if a == nil {
		return s.SelectK(ctx, req, k)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	cands, err := s.SelectK(ctx, req, k)
	if err != nil || len(cands) == 0 {
		return cands, err
	}
	if held, ok := cands[0].assign(); ok {
		cands[0] = held
	}
	return cands, nil
}

// withAssigned raises each candidate's capacity_used, as the congestion term
// of assignSpeedRanks reads it, to at least what this requester has assigned
// there and not released.
//
// A peer's figure is its own /healthz count from the last probe, and a probe
// happens before this round's commits: a request assigned since then is not
// in it. This device's figure is the engine's slot holders, and a request
// assigned here but still waiting for its slot is not in that either. Both
// are raised to the count this requester keeps, and not added to it: the two
// overlap whenever the probe, or the slot, has already seen the request, and
// the larger of two lower bounds is still a lower bound.
//
// Only readings that exist are touched. A candidate with no reading is ranked
// by the nil rule, not by a count.
//
// A copy: roundSpeeds returns PeerSpeeds' own map when this device has no id.
func (s *Selector) withAssigned(speeds map[string]PeerSpeed, local LocalNode) map[string]PeerSpeed {
	if s.in.Assignments == nil || len(speeds) == 0 {
		return speeds
	}
	out := make(map[string]PeerSpeed, len(speeds))
	for id, sp := range speeds {
		n := 0
		switch {
		case local.DeviceID != "" && id == local.DeviceID:
			n = s.in.Assignments.Local()
		case s.in.LocalInFlight != nil:
			n = int(s.in.LocalInFlight.InFlight(id))
		}
		if n > sp.CapacityUsed {
			sp.CapacityUsed = n
		}
		out[id] = sp
	}
	return out
}

// heldSlot is a slot taken at assignment, waiting for Commit to use it or
// Abandon to give it back. Exactly one of the two gets the release.
type heldSlot struct {
	mu      sync.Mutex
	release func()
	taken   bool
}

func (h *heldSlot) take() (func(), bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.taken {
		return nil, false
	}
	h.taken = true
	return h.release, true
}
