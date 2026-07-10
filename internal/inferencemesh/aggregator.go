package inferencemesh

import (
	"sync"
	"time"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// Aggregator owns the agent's in-memory view of every peer's
// InferenceState. It is fed:
//
//   - on every network map frame (Update) — refreshes peer entries
//     and the self placeholder fields (DeviceName / OverlayIP)
//   - on every local probe tick (UpdateLocal) — refreshes Self.InferenceState
//     so consumers see the same rich payload they get from peers
//
// Concurrent-safe. Single goroutine per writer (the network map loop
// and the probe loop respectively) is the expected pattern, but
// SnapshotWith is safe to call from any reader.
//
// The aggregator does NOT push anything to CP — that's the probe
// loop's job, via internal/controlclient.PushInferenceStatus. The
// aggregator is purely the consumer-side fan-in.
type Aggregator struct {
	staleness time.Duration
	now       func() time.Time

	mu              sync.RWMutex
	selfDeviceID    string
	selfPlaceholder PeerView
	selfState       *signer.InferenceState
	peers           map[string]PeerView // by DeviceID, never includes self
}

// New builds an aggregator for selfDeviceID. staleness is the maximum
// age of an InferenceState.LastCheck before that peer is treated as
// unreachable for aggregation; 15 s is the Phase 3 default (3× the
// 5 s probe / push cadence).
func New(selfDeviceID string, staleness time.Duration, now func() time.Time) *Aggregator {
	if now == nil {
		now = time.Now
	}
	return &Aggregator{
		staleness:       staleness,
		now:             now,
		selfDeviceID:    selfDeviceID,
		selfPlaceholder: PeerView{DeviceID: selfDeviceID},
		peers:           map[string]PeerView{},
	}
}

// Update consumes a fresh network map. It refreshes every peer's
// pushed InferenceState (replacing whatever was there) and updates
// the self placeholder's DeviceName / OverlayIP from nm.Self. The
// self entry's InferenceState is NOT touched here — that's owned by
// the local probe via UpdateLocal — because Phase 3's content-change
// optimisation on the CP side means a network map redelivery does
// NOT necessarily contain a fresh self.InferenceState (the CP just
// echoes whatever it has stored, which might be stale relative to
// the agent's just-completed probe).
func (a *Aggregator) Update(nm *signer.NetworkMap) {
	if nm == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	a.selfPlaceholder.DeviceID = nm.Self.DeviceID
	a.selfPlaceholder.DeviceName = nm.Self.DeviceName
	a.selfPlaceholder.OverlayIP = nm.Self.OverlayIP
	if nm.Self.DeviceID != "" {
		a.selfDeviceID = nm.Self.DeviceID
	}

	// Replace the peer set wholesale — devices that were removed from
	// the network (revoked, deleted) should drop out, not linger.
	a.peers = make(map[string]PeerView, len(nm.Peers))
	for _, p := range nm.Peers {
		if p.DeviceID == a.selfDeviceID {
			continue
		}
		a.peers[p.DeviceID] = PeerView{
			DeviceID:       p.DeviceID,
			DeviceName:     p.DeviceName,
			OverlayIP:      p.OverlayIP,
			InferenceState: p.InferenceState,
		}
	}
}

// UpdateLocal records the agent's own latest probe result. The state
// argument is the same payload the agent will (or did) push to CP —
// keeping a copy here avoids a CP→network-map round-trip just for the
// diagnose UI to render self's state.
func (a *Aggregator) UpdateLocal(state *signer.InferenceState) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if state == nil {
		a.selfState = nil
		return
	}
	cp := *state
	a.selfState = &cp
}

// Snapshot returns the current aggregated view. Reachable is the
// peers-only OR (self deliberately excluded — see types.go).
func (a *Aggregator) Snapshot() Snapshot {
	a.mu.RLock()
	defer a.mu.RUnlock()

	now := a.now()
	out := Snapshot{
		GeneratedAt:          now.UTC().Format(time.RFC3339Nano),
		SelfDeviceID:         a.selfDeviceID,
		StalenessThresholdMS: a.staleness.Milliseconds(),
		Self:                 a.selfPlaceholder,
		Peers:                make([]PeerView, 0, len(a.peers)),
	}
	if a.selfState != nil {
		s := *a.selfState
		out.Self.InferenceState = &s
		out.Self.Stale = isStale(a.selfState.LastCheck, now, a.staleness)
	}

	reachable := false
	for _, pv := range a.peers {
		stale := false
		if pv.InferenceState != nil {
			stale = isStale(pv.InferenceState.LastCheck, now, a.staleness)
		}
		pv.Stale = stale
		out.Peers = append(out.Peers, pv)
		if !reachable && pv.InferenceState != nil &&
			pv.InferenceState.Reachable && !stale {
			reachable = true
		}
	}
	out.Reachable = reachable
	return out
}

// isStale reports whether the supplied RFC3339(Nano) timestamp is
// older than threshold relative to now. An empty / unparseable
// timestamp counts as stale (= "we have no idea when this was
// observed, so we can't trust it"). Accepts both RFC3339Nano and
// RFC3339 second-precision formats.
func isStale(ts string, now time.Time, threshold time.Duration) bool {
	if ts == "" || threshold <= 0 {
		return true
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t, err = time.Parse(time.RFC3339, ts)
		if err != nil {
			return true
		}
	}
	return now.Sub(t) > threshold
}
