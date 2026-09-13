package router

import (
	"sync"
	"time"

	"github.com/waired-ai/waired-agent/proto/hostfit"
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
// It holds two kinds of reading, because the mesh runs two generations of
// agent while waired-agent#1341 rolls out.
//
// SECONDS PER REQUEST (the peer's `speed`, decision 9 of
// docs/decisions/20260913/2245-speed-is-one-request-at-32768-tokens.md): one
// figure per peer, what a 32,768-token request costs there. The NEWER
// reading replaces the older one, whichever is faster — a re-measurement is
// the peer's current answer, and the rule that used to keep the best reading
// is what left a peer that had become slower ranked on its old figure until
// the window expired. An observed turn replaces only the prefill term, and
// only when its depth resembles 32,768 and the peer published a decode rate
// to finish the sum with.
//
// PREFILL RUNGS (the peer's `prefill_rate`, every agent before #1341): keyed
// by DEPTH, and an observed turn is only folded into a rung whose depth it is
// actually close to. Prefill throughput falls as the prompt grows, so a
// 30k-token turn says nothing about a 4k rung; the acceptance band is the
// same 0.7-1.5 the measurement itself uses. Here the BEST reading in the
// window still wins rather than the mean: a cold sample includes a model load
// and only ever understates. This path is kept unchanged for one release so a
// round that mixes old and new agents still orders on something comparable
// (assignSpeedRanks).
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

	// published is the peer's own seconds-per-request reading, observed
	// the one this requester computed from a real turn. The newer of the
	// two (by measuredAt) is what Snapshot reports.
	published *turnReading
	observed  *turnReading
}

type turnReading struct {
	turn, floor float64
	decode      float64
	measuredAt  time.Time
	recordedAt  time.Time
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
	// Turn is the peer's cost per request in seconds, nil when this
	// requester has no such reading for it — an agent predating
	// waired-agent#1341, or a peer nobody has probed.
	Turn *PeerTurn
}

// PeerTurn is one peer's seconds-per-request reading as the ranking reads it.
type PeerTurn struct {
	// TurnSeconds is the finished figure; TurnFloorSeconds, set only when
	// TurnSeconds is not, a lower bound from a measurement still running
	// past the line.
	TurnSeconds      float64
	TurnFloorSeconds float64
	MeasuredAt       time.Time
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
		e.published, e.observed = nil, nil
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
	switch {
	case s.Speed != nil && s.Speed.VariantID != "":
		variant = s.Speed.VariantID
	case s.PrefillRate != nil:
		variant = s.PrefillRate.VariantID
	}
	e := w.entryLocked(deviceID, variant)
	e.capacityUsed = s.CapacityUsed
	e.lastProbe = now
	if s.Speed.usable() {
		measuredAt := parseMeasuredAt(s.Speed.MeasuredAt, now)
		r := &turnReading{
			turn: s.Speed.TurnSeconds, floor: s.Speed.TurnFloorSeconds,
			decode: s.Speed.DecodeTokps, measuredAt: measuredAt, recordedAt: now,
		}
		if r.turn > 0 {
			r.floor = 0
		}
		cur := e.published
		// Newer replaces older, faster or not. The same measurement arriving
		// again refreshes it; an OLDER one — a probe answered out of order —
		// does not overturn it unless what is stored has already aged out.
		if cur == nil || !measuredAt.Before(cur.measuredAt) || now.Sub(cur.recordedAt) > prefillWindowTTL {
			e.published = r
		}
	}
	if s.PrefillRate == nil {
		return
	}
	for _, r := range s.PrefillRate.Rungs {
		if r.Depth <= 0 || r.Tokps <= 0 {
			continue
		}
		depth := r.Depth
		if !isPrefillRungDepth(depth) {
			// A #1341 peer publishes its one reading at the depth it
			// measured, which is 32,768 except on a host whose window
			// cannot hold that prompt. Filed under the rung it resembles,
			// so an older round can still compare it; dropped if it
			// resembles none.
			var ok bool
			if depth, ok = prefillRungForDepth(depth); !ok {
				continue
			}
		}
		w.keepBestLocked(e, depth, r.Tokps, r.Bound, now)
	}
}

// parseMeasuredAt reads a published RFC3339Nano instant, falling back to the
// moment it was received: a reading with no date is at least as new as the
// probe that carried it.
func parseMeasuredAt(v string, received time.Time) time.Time {
	if v == "" {
		return received
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return received
	}
	return t
}

func isPrefillRungDepth(depth int) bool {
	for _, d := range PrefillRungDepths {
		if d == depth {
			return true
		}
	}
	return false
}

// RecordObserved folds one real turn into the window: prompt tokens over
// the time to first token, this requester's own experience of the peer.
//
// It lands on the rung whose depth the turn actually resembles, and on no
// other. A turn that resembles none is dropped — an uncomparable reading
// is worse than no reading, because the ranking cannot tell it apart from
// a comparable one.
//
// The same turn may also replace the prefill term of the peer's
// seconds-per-request figure — when its depth resembles
// hostfit.SpeedMeasurementDepthTokens (the same band) and the peer has
// published a decode rate to complete the sum:
//
//	turn = TurnSecondsAt(32768, observed prefill, published decode)
//
// dated now, so it stands until the peer publishes a newer measurement.
func (w *PrefillWindow) RecordObserved(deviceID, variantID string, promptTokens int, ttft time.Duration) {
	if deviceID == "" || promptTokens <= 0 || ttft <= 0 {
		return
	}
	tokps := float64(promptTokens) / ttft.Seconds()
	if tokps <= 0 {
		return
	}
	depth, rungOK := prefillRungForDepth(promptTokens)
	ratio := float64(promptTokens) / float64(hostfit.SpeedMeasurementDepthTokens)
	turnOK := ratio >= prefillObservationBandLow && ratio <= prefillObservationBandHigh
	if !rungOK && !turnOK {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	e := w.entryLocked(deviceID, variantID)
	if rungOK {
		w.keepBestLocked(e, depth, tokps, false, now)
	}
	if !turnOK {
		return
	}
	pub := e.published
	if pub == nil || pub.decode <= 0 || now.Sub(pub.recordedAt) > prefillWindowTTL {
		return
	}
	e.observed = &turnReading{
		turn:       hostfit.TurnSecondsAt(hostfit.SpeedMeasurementDepthTokens, tokps, pub.decode),
		decode:     pub.decode,
		measuredAt: now,
		recordedAt: now,
	}
}

// keepBestLocked is the legacy rung rule: it keeps the faster of the stored
// and the new reading, and
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
//
// Kept for one release after waired-agent#1341, whose agents measure one
// depth and publish seconds per request: a round that still contains an
// older agent orders on these rungs (assignSpeedRanks).
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
		turn := e.turnLocked(now)
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
		if rungs == nil && turn == nil && staleProbe {
			delete(w.peers, deviceID)
			continue
		}
		if out == nil {
			out = make(map[string]PeerSpeed)
		}
		ps := PeerSpeed{VariantID: e.variantID, Rungs: rungs, Turn: turn}
		if !staleProbe {
			ps.CapacityUsed = e.capacityUsed
		}
		out[deviceID] = ps
	}
	return out
}

// turnLocked is the reading Snapshot reports: the newer of the published and
// the observed figure, among those still inside the window. Aged-out
// readings are dropped. Caller holds mu.
func (e *peerSpeedEntry) turnLocked(now time.Time) *PeerTurn {
	if e.published != nil && now.Sub(e.published.recordedAt) > prefillWindowTTL {
		e.published = nil
	}
	if e.observed != nil && now.Sub(e.observed.recordedAt) > prefillWindowTTL {
		e.observed = nil
	}
	r := e.published
	if o := e.observed; o != nil && (r == nil || o.measuredAt.After(r.measuredAt)) {
		r = o
	}
	if r == nil {
		return nil
	}
	return &PeerTurn{TurnSeconds: r.turn, TurnFloorSeconds: r.floor, MeasuredAt: r.measuredAt}
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
