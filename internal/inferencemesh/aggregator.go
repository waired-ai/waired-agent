package inferencemesh

import (
	"sync"
	"time"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// Freshness policy defaults. See Policy for what each one governs and
// why it is set where it is.
const (
	// DefaultSelfStaleness is the maximum age of the LOCAL probe's
	// InferenceState.LastCheck. The local probe loop refreshes it every
	// state.HeartbeatInterval (5 s) unconditionally, so 3× the cadence
	// is the right bound here and always was.
	DefaultSelfStaleness = 15 * time.Second

	// DefaultFrameStaleness is the maximum age of the most recent
	// network map frame before every peer entry derived from it is
	// treated as unusable. The Control Plane re-emits a map frame on
	// any content change and, failing that, on its long-poll backstop
	// (MapPollMaxAge, 5 min by default), so a frame older than that
	// means the map stream itself is not delivering — not that the
	// peers went away. 1 min of margin absorbs long-poll turnaround
	// and stream reconnects.
	DefaultFrameStaleness = 6 * time.Minute

	// DefaultAdvertisedLiveness is the maximum age of a peer's
	// InferenceState.LastCheck **measured at the moment its frame
	// arrived**, not continuously afterwards. Peers push their probe
	// result every 5 s and the CP validates last_check within ±60 s of
	// its own clock at intake, so a live peer's advertisement is a few
	// seconds old when the frame is assembled; 90 s leaves room for
	// push jitter and modest clock skew while still catching a peer
	// that stopped pushing minutes ago and whose CP-stored state is
	// therefore frozen.
	DefaultAdvertisedLiveness = 90 * time.Second
)

// Policy is the freshness contract the aggregator applies. It exists
// because "is this peer usable?" has three independent failure modes
// that used to be collapsed into one 15 s LastCheck comparison
// (waired-agent#323):
//
//   - the map stream stopped delivering       → FrameStaleness
//   - the peer stopped pushing to the CP      → AdvertisedLiveness
//   - the peer's host went off the wire       → PeerLiveness
//
// The collapsed version assumed the CP would redeliver a frame
// whenever LastCheck advanced. It does not: the content-change gate
// deliberately ignores last_check (waking every peer 12×/min for a
// no-op push would be all cost), so a frame's LastCheck went stale
// 15 s after arrival and stayed stale until the next real content
// change or the 5-minute backstop — marking healthy peers unreachable
// for ~95% of steady-state time.
type Policy struct {
	// SelfStaleness bounds the age of Self.InferenceState.LastCheck.
	// Self is fed by the local probe loop, which does tick on a fixed
	// cadence, so this axis keeps its original cadence×3 meaning.
	SelfStaleness time.Duration

	// FrameStaleness bounds the age of the last network map frame.
	FrameStaleness time.Duration

	// AdvertisedLiveness bounds a peer's LastCheck age at frame
	// receipt. The verdict is frozen at receipt and held until the
	// next frame replaces it — between frames, "unchanged" IS the
	// truth the CP is asserting.
	AdvertisedLiveness time.Duration

	// PeerLiveness, when non-nil, returns the disco prober's
	// deviceID → recent-pong map. Three-valued exactly as
	// disco.Service.ReachableSnapshot defines it: present+true = a
	// recent pong, present+false = once seen, now silent (exclude),
	// absent = never probed (trust). It restores the fast
	// host-death signal that receipt-based freshness alone would
	// stretch out to the CP backstop interval; the router already
	// hard-excludes on this same map, so wiring it here only makes
	// the tray label and the peer adapter's health check agree with
	// the routing decision.
	PeerLiveness func() map[string]bool
}

func (p Policy) withDefaults() Policy {
	if p.SelfStaleness <= 0 {
		p.SelfStaleness = DefaultSelfStaleness
	}
	if p.FrameStaleness <= 0 {
		p.FrameStaleness = DefaultFrameStaleness
	}
	if p.AdvertisedLiveness <= 0 {
		p.AdvertisedLiveness = DefaultAdvertisedLiveness
	}
	return p
}

// peerEntry is one peer's map-frame record plus the freshness verdict
// computed when that frame arrived. Keeping freshAtReceipt out of the
// exported PeerView keeps the wire shape honest — it is a derived
// input to Stale, not a field the API returns.
type peerEntry struct {
	view PeerView
	// freshAtReceipt is whether the peer's advertised LastCheck was
	// within Policy.AdvertisedLiveness of the instant this frame was
	// consumed. Frozen here on purpose: re-evaluating it against
	// wall-clock later is exactly the bug this replaces.
	freshAtReceipt bool
}

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
// Snapshot is safe to call from any reader.
//
// The aggregator does NOT push anything to CP — that's the probe
// loop's job, via internal/controlclient.PushInferenceStatus. The
// aggregator is purely the consumer-side fan-in.
type Aggregator struct {
	policy Policy
	now    func() time.Time

	mu              sync.RWMutex
	selfDeviceID    string
	selfPlaceholder PeerView
	selfState       *signer.InferenceState
	peers           map[string]peerEntry // by DeviceID, never includes self
	// framesAt is when the most recent network map frame was consumed.
	// Zero means no frame has arrived yet.
	framesAt time.Time
}

// New builds an aggregator for selfDeviceID under the supplied
// freshness policy. Zero-valued Policy durations fall back to the
// Default* constants; PeerLiveness stays optional and can also be
// attached later with SetPeerLiveness.
func New(selfDeviceID string, p Policy, now func() time.Time) *Aggregator {
	if now == nil {
		now = time.Now
	}
	return &Aggregator{
		policy:          p.withDefaults(),
		now:             now,
		selfDeviceID:    selfDeviceID,
		selfPlaceholder: PeerView{DeviceID: selfDeviceID},
		peers:           map[string]peerEntry{},
	}
}

// SetPeerLiveness attaches (or replaces) the disco reachability
// oracle. Separate from New because the disco service is constructed
// after the aggregator in the agent's boot order; nil clears it.
func (a *Aggregator) SetPeerLiveness(fn func() map[string]bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.policy.PeerLiveness = fn
}

// Update consumes a fresh network map. It refreshes every peer's
// pushed InferenceState (replacing whatever was there), stamps the
// frame's receipt time, and updates the self placeholder's DeviceName
// / OverlayIP from nm.Self. The self entry's InferenceState is NOT
// touched here — that's owned by the local probe via UpdateLocal —
// because Phase 3's content-change optimisation on the CP side means a
// network map redelivery does NOT necessarily contain a fresh
// self.InferenceState (the CP just echoes whatever it has stored,
// which might be stale relative to the agent's just-completed probe).
func (a *Aggregator) Update(nm *signer.NetworkMap) {
	if nm == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	now := a.now()
	a.framesAt = now

	a.selfPlaceholder.DeviceID = nm.Self.DeviceID
	a.selfPlaceholder.DeviceName = nm.Self.DeviceName
	a.selfPlaceholder.OverlayIP = nm.Self.OverlayIP
	if nm.Self.DeviceID != "" {
		a.selfDeviceID = nm.Self.DeviceID
	}

	// Replace the peer set wholesale — devices that were removed from
	// the network (revoked, deleted) should drop out, not linger.
	a.peers = make(map[string]peerEntry, len(nm.Peers))
	for _, p := range nm.Peers {
		if p.DeviceID == a.selfDeviceID {
			continue
		}
		a.peers[p.DeviceID] = peerEntry{
			view: PeerView{
				DeviceID:       p.DeviceID,
				DeviceName:     p.DeviceName,
				OverlayIP:      p.OverlayIP,
				InferenceState: p.InferenceState,
				Grant:          p.Grant,
			},
			freshAtReceipt: p.InferenceState != nil &&
				!isStale(p.InferenceState.LastCheck, now, a.policy.AdvertisedLiveness),
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
	// Resolve the liveness oracle before taking the read lock: it
	// reaches into the disco service's own mutex, and there is no
	// reason to hold ours across that call.
	a.mu.RLock()
	livenessFn := a.policy.PeerLiveness
	a.mu.RUnlock()
	var liveness map[string]bool
	if livenessFn != nil {
		liveness = livenessFn()
	}

	a.mu.RLock()
	defer a.mu.RUnlock()

	now := a.now()
	out := Snapshot{
		GeneratedAt:          now.UTC().Format(time.RFC3339Nano),
		SelfDeviceID:         a.selfDeviceID,
		StalenessThresholdMS: a.policy.AdvertisedLiveness.Milliseconds(),
		FrameStalenessMS:     a.policy.FrameStaleness.Milliseconds(),
		Self:                 a.selfPlaceholder,
		Peers:                make([]PeerView, 0, len(a.peers)),
	}
	if a.selfState != nil {
		s := *a.selfState
		out.Self.InferenceState = &s
		out.Self.Stale = isStale(a.selfState.LastCheck, now, a.policy.SelfStaleness)
	}

	// A frame we never received, or one older than the CP's redelivery
	// contract allows, invalidates every peer entry derived from it.
	mapDead := a.framesAt.IsZero() || now.Sub(a.framesAt) > a.policy.FrameStaleness
	if !a.framesAt.IsZero() {
		out.MapReceivedAt = a.framesAt.UTC().Format(time.RFC3339Nano)
		out.MapAgeMS = now.Sub(a.framesAt).Milliseconds()
	}

	reachable := false
	for _, pe := range a.peers {
		pv := pe.view
		stale := false
		if pv.InferenceState != nil {
			// present+false is the disco prober saying "this peer used
			// to answer and has now gone silent"; absent means we have
			// no signal and default to trust.
			live, known := liveness[pv.DeviceID]
			stale = mapDead || !pe.freshAtReceipt || (known && !live)
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
