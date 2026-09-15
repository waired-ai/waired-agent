package router

import (
	"sync"
	"time"

	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// PrefillWindow is this requester's per-peer record of what a request costs
// on each peer, in seconds per request — the figure the speed key ranks on
// (waired-agent#1127; decision 9 of
// docs/decisions/20260913/2245-speed-is-one-request-at-32768-tokens.md).
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
//   - PUBLISHED, from the peer's own measurement (its `speed`): what a
//     32,768-token request costs there. It answers the first turn to a peer
//     this device has never used.
//   - OBSERVED, from turns this device actually sent: prompt tokens over
//     the time to first token. It replaces the prefill term of the
//     published figure with what this requester really experienced,
//     including the network leg the peer cannot see — only when the turn's
//     depth resembles 32,768 and the peer published a decode rate to finish
//     the sum with.
//
// The NEWER reading replaces the older one, whichever is faster: a
// re-measurement is the peer's current answer, and a rule that kept the best
// reading would leave a peer that had become slower ranked on its old figure
// until the window expired.
type PrefillWindow struct {
	now func() time.Time

	mu    sync.Mutex
	peers map[string]*peerSpeedEntry // deviceID → entry
}

type peerSpeedEntry struct {
	variantID string
	// capacityUsed is how many requests the peer said were running, at
	// lastProbe. It is the congestion multiplier, and it is a snapshot: one
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

const (
	// prefillWindowTTL is how long a reading stands. Long compared with
	// ErrorWindow's 60 s because these arrive far less often — a published
	// figure refreshes on every probe round, but an observed one needs a
	// real turn to have happened — and short enough that a peer which was
	// re-tuned, switched model or acquired a neighbour ages out rather
	// than being believed indefinitely.
	prefillWindowTTL = 15 * time.Minute

	// prefillObservationBandLow / High bound which turns may stand in for
	// the measurement's prefill term: the prompt has to be within this
	// ratio of hostfit.SpeedMeasurementDepthTokens. Prefill throughput falls
	// as the prompt grows, so a 4k-token turn says nothing about a
	// 32,768-token one — a reading taken at a depth the figure does not
	// describe measures the depth, not the host.
	prefillObservationBandLow  = 0.7
	prefillObservationBandHigh = 1.5
)

// PeerSpeed is what the Selector reads back for one peer.
type PeerSpeed struct {
	// VariantID is the model the readings describe. A peer that switched
	// model has a different one, and its old readings are dropped rather
	// than carried forward.
	VariantID string
	// CapacityUsed is the peer's own in-flight count at the last probe —
	// the congestion multiplier. It counts the peer owner's own work as
	// well as mesh traffic, which is exactly right: a machine busy with its
	// owner's turn is busy.
	CapacityUsed int
	// Turn is the peer's cost per request in seconds, nil when this
	// requester has no such reading for it — a peer that has measured
	// nothing yet, or one nobody has probed.
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
		e = &peerSpeedEntry{}
		w.peers[deviceID] = e
	}
	if variantID != "" && e.variantID != variantID {
		// The readings described a different model. Nothing carries over:
		// a figure is meaningless against another variant.
		e.variantID = variantID
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
	if s.Speed != nil {
		variant = s.Speed.VariantID
	}
	e := w.entryLocked(deviceID, variant)
	e.capacityUsed = s.CapacityUsed
	e.lastProbe = now
	if !s.Speed.usable() {
		return
	}
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

// RecordObserved folds one real turn into the window: prompt tokens over
// the time to first token, this requester's own experience of the peer.
//
// The turn replaces the prefill term of the peer's seconds-per-request
// figure — only when its depth resembles hostfit.SpeedMeasurementDepthTokens
// (prefillObservationBandLow..High) and the peer has published a decode rate
// to complete the sum:
//
//	turn = TurnSecondsAt(32768, observed prefill, published decode)
//
// dated now, so it stands until the peer publishes a newer measurement. A
// turn outside the band is dropped: an uncomparable reading is worse than no
// reading, because the ranking cannot tell it apart from a comparable one.
func (w *PrefillWindow) RecordObserved(deviceID, variantID string, promptTokens int, ttft time.Duration) {
	if deviceID == "" || promptTokens <= 0 || ttft <= 0 {
		return
	}
	tokps := float64(promptTokens) / ttft.Seconds()
	if tokps <= 0 {
		return
	}
	ratio := float64(promptTokens) / float64(hostfit.SpeedMeasurementDepthTokens)
	if ratio < prefillObservationBandLow || ratio > prefillObservationBandHigh {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	e := w.entryLocked(deviceID, variantID)
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
		staleProbe := now.Sub(e.lastProbe) > prefillWindowTTL
		if turn == nil && staleProbe {
			delete(w.peers, deviceID)
			continue
		}
		if out == nil {
			out = make(map[string]PeerSpeed)
		}
		ps := PeerSpeed{VariantID: e.variantID, Turn: turn}
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
