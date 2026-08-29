package router

import (
	"sync"
	"time"
)

// PrefillWindow is this requester's per-peer record of how fast each peer
// prefills — the term that decides what a coding agent's first turn costs
// (waired-agent#1127).
//
// It exists because the two halves of that answer arrive on opposite sides
// of a seam. A peer publishes its own measurement on /healthz, which is
// read at PROBE time; the Selector ranks candidates at SNAPSHOT time, and
// as assignRankTiers puts it, "the two never meet". So the requester keeps
// what it learned and reads it back when it next ranks — the same shape
// ErrorWindow already has for caller-observed failure rates.
//
// Two sources compose:
//
//   - PUBLISHED, from the peer's own measurement. It answers the first
//     turn to a peer this device has never used.
//   - OBSERVED, from turns this device actually sent: prompt tokens over
//     the time to first token. It corrects the published figure with what
//     this requester really experienced, including the network leg the
//     peer cannot see.
//
// Both are keyed by DEPTH, and an observed turn is only folded into a rung
// whose depth it is actually close to. Prefill throughput falls as the
// prompt grows, so a 30k-token turn says nothing about a 4k rung; the
// acceptance band is the same 0.7-1.5 the measurement itself uses.
//
// The BEST reading in the window wins rather than the mean: a cold sample
// includes a model load and only ever understates, so keeping the best is
// what stops one unlucky turn from re-ranking a peer for a quarter of an
// hour.
type PrefillWindow struct {
	now func() time.Time

	mu    sync.Mutex
	peers map[string]*peerSpeedEntry // deviceID → entry
}

type peerSpeedEntry struct {
	variantID string
	rungs     map[int]speedReading // depth → best reading
	// capacityUsed is how many requests the peer said were running, at
	// lastProbe. It is the congestion divisor, and it is a snapshot: one
	// probe round old by the time the next ranking reads it.
	capacityUsed int
	lastProbe    time.Time
}

type speedReading struct {
	tokps float64
	bound bool
	at    time.Time
}

const (
	// prefillWindowTTL is how long a reading stands. Long compared with
	// ErrorWindow's 60 s because these arrive far less often — a published
	// figure refreshes on every probe round, but an observed one needs a
	// real turn to have happened — and short enough that a peer which was
	// re-tuned, switched model or acquired a neighbour ages out rather
	// than being believed indefinitely.
	prefillWindowTTL = 15 * time.Minute

	// prefillObservationBandLow / High bound which rung an observed turn
	// may be folded into. Same band, for the same reason, as the
	// measurement's own read-back guard: a reading taken at a depth the
	// rung does not describe measures the depth, not the host.
	prefillObservationBandLow  = 0.7
	prefillObservationBandHigh = 1.5
)

// PeerSpeed is what the Selector reads back for one peer.
type PeerSpeed struct {
	// VariantID is the model the readings describe. A peer that switched
	// model has a different one, and its old readings are dropped rather
	// than carried forward.
	VariantID string
	// Rungs are the depths this requester has a reading for, with the best
	// rate seen at each.
	Rungs map[int]PrefillRung
	// CapacityUsed is the peer's own in-flight count at the last probe —
	// the congestion divisor. It counts the peer owner's own work as well
	// as mesh traffic, which is exactly right: a machine busy with its
	// owner's turn is busy.
	CapacityUsed int
}

// NewPrefillWindow returns an empty window. now defaults to time.Now.
func NewPrefillWindow(now func() time.Time) *PrefillWindow {
	if now == nil {
		now = time.Now
	}
	return &PrefillWindow{now: now, peers: map[string]*peerSpeedEntry{}}
}

// entryLocked returns the peer's entry, resetting it when the peer has
// switched model. Caller holds mu.
func (w *PrefillWindow) entryLocked(deviceID, variantID string) *peerSpeedEntry {
	e, ok := w.peers[deviceID]
	if !ok {
		e = &peerSpeedEntry{rungs: map[int]speedReading{}}
		w.peers[deviceID] = e
	}
	if variantID != "" && e.variantID != variantID {
		// The readings described a different model. Nothing carries over:
		// a rate is meaningless against another variant.
		e.variantID = variantID
		e.rungs = map[int]speedReading{}
	}
	return e
}

// RecordProbe folds one probe response into the window: the peer's own
// published measurement, and its in-flight count. Called for every remote
// candidate a probe round touched, so the figures track the live mesh
// without a channel of their own.
func (w *PrefillWindow) RecordProbe(deviceID string, s HealthStatus) {
	if deviceID == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	variant := ""
	if s.PrefillRate != nil {
		variant = s.PrefillRate.VariantID
	}
	e := w.entryLocked(deviceID, variant)
	e.capacityUsed = s.CapacityUsed
	e.lastProbe = now
	if s.PrefillRate == nil {
		return
	}
	for _, r := range s.PrefillRate.Rungs {
		if r.Depth <= 0 || r.Tokps <= 0 {
			continue
		}
		w.keepBestLocked(e, r.Depth, r.Tokps, r.Bound, now)
	}
}

// RecordObserved folds one real turn into the window: prompt tokens over
// the time to first token, this requester's own experience of the peer.
//
// It lands on the rung whose depth the turn actually resembles, and on no
// other. A turn that resembles none is dropped — an uncomparable reading
// is worse than no reading, because the ranking cannot tell it apart from
// a comparable one.
func (w *PrefillWindow) RecordObserved(deviceID, variantID string, promptTokens int, ttft time.Duration) {
	if deviceID == "" || promptTokens <= 0 || ttft <= 0 {
		return
	}
	depth, ok := prefillRungForDepth(promptTokens)
	if !ok {
		return
	}
	tokps := float64(promptTokens) / ttft.Seconds()
	if tokps <= 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	e := w.entryLocked(deviceID, variantID)
	w.keepBestLocked(e, depth, tokps, false, w.now())
}

// keepBestLocked keeps the faster of the stored and the new reading, and
// prefers a measurement to a bound at the same rung — a bound is what a
// host publishes when it could not finish, so any real reading supersedes
// it. Caller holds mu.
func (w *PrefillWindow) keepBestLocked(e *peerSpeedEntry, depth int, tokps float64, bound bool, now time.Time) {
	cur, ok := e.rungs[depth]
	fresh := ok && now.Sub(cur.at) <= prefillWindowTTL
	switch {
	case !fresh:
	case cur.bound && !bound:
		// A measurement always beats a bound.
	case !cur.bound && bound:
		return // never let a bound displace a measurement
	case tokps <= cur.tokps:
		return
	}
	e.rungs[depth] = speedReading{tokps: tokps, bound: bound, at: now}
}

// prefillRungForDepth maps an observed prompt length onto the rung it is
// close enough to describe, if any.
func prefillRungForDepth(promptTokens int) (int, bool) {
	best, bestDist := 0, 0.0
	for _, d := range PrefillRungDepths {
		ratio := float64(promptTokens) / float64(d)
		if ratio < prefillObservationBandLow || ratio > prefillObservationBandHigh {
			continue
		}
		dist := ratio
		if dist < 1 {
			dist = 1 / dist
		}
		if best == 0 || dist < bestDist {
			best, bestDist = d, dist
		}
	}
	return best, best != 0
}

// PrefillRungDepths are the fixed depths every host measures at. They are
// declared here as well as in the agent because BOTH sides need them: the
// host to climb, the requester to know which rung an observed turn belongs
// to. Changing one without the other silently stops observations from
// merging with published readings.
var PrefillRungDepths = []int{4096, 8192, 32768}

// Snapshot returns the live per-peer view, dropping readings past the
// window. Peers with nothing left are omitted, so an empty map means "this
// requester knows nothing about anyone's speed" — which the ranking must
// read as no information, never as slow.
func (w *PrefillWindow) Snapshot() map[string]PeerSpeed {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	var out map[string]PeerSpeed
	for deviceID, e := range w.peers {
		var rungs map[int]PrefillRung
		for depth, r := range e.rungs {
			if now.Sub(r.at) > prefillWindowTTL {
				delete(e.rungs, depth)
				continue
			}
			if rungs == nil {
				rungs = make(map[int]PrefillRung, len(e.rungs))
			}
			rungs[depth] = PrefillRung{Depth: depth, Tokps: r.tokps, Bound: r.bound}
		}
		staleProbe := now.Sub(e.lastProbe) > prefillWindowTTL
		if rungs == nil && staleProbe {
			delete(w.peers, deviceID)
			continue
		}
		if out == nil {
			out = make(map[string]PeerSpeed)
		}
		ps := PeerSpeed{VariantID: e.variantID, Rungs: rungs}
		if !staleProbe {
			ps.CapacityUsed = e.capacityUsed
		}
		out[deviceID] = ps
	}
	return out
}

// RoundRung is the depth a whole selection round may be compared at: the
// deepest rung every candidate that has any reading at all reached.
//
// One depth for the round, not one per pair. Comparing each pair at its
// own deepest common rung would not be a total order — A could beat B on
// one depth and lose to C on another — and sort.SliceStable given a
// non-transitive comparison produces an arbitrary answer.
//
// A peer that dragged the round down to the shallowest rung has dragged it
// down for everyone, which is the honest reading: that is the depth at
// which this field is comparable at all. ok=false means no depth is shared
// and speed cannot order this round.
func RoundRung(speeds []PeerSpeed) (depth int, ok bool) {
	var withReadings []PeerSpeed
	for _, s := range speeds {
		if len(s.Rungs) > 0 {
			withReadings = append(withReadings, s)
		}
	}
	if len(withReadings) == 0 {
		return 0, false
	}
	for i := len(PrefillRungDepths) - 1; i >= 0; i-- {
		d := PrefillRungDepths[i]
		all := true
		for _, s := range withReadings {
			if _, has := s.Rungs[d]; !has {
				all = false
				break
			}
		}
		if all {
			return d, true
		}
	}
	return 0, false
}
