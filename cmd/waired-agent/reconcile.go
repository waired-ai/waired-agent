package main

import (
	"context"
	"encoding/base64"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/waired-ai/waired-agent/internal/controlclient"
	"github.com/waired-ai/waired-agent/internal/devicekeys"
	"github.com/waired-ai/waired-agent/internal/identity"
	disco "github.com/waired-ai/waired-agent/internal/network/disco"
	"github.com/waired-ai/waired-agent/internal/network/wgnet"
	wiredisco "github.com/waired-ai/waired-agent/proto/disco"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// reconciler is the single owner of wgnet.Engine.UpdatePeers calls. It
// keeps the latest NetworkMap and per-peer path state, and re-applies
// the peer set whenever either changes.
//
// Two trigger sources produce a reconcile:
//   - Apply(nm)       — fresh map from CP. currentPath is reset to
//     "direct" so the agent always tries direct after a
//     topology change. RTT EWMAs / observed addr / disco
//     hint are PRESERVED across maps because NAT
//     mappings don't expire just because CP republished.
//   - OnDiscoEvent(e) — RTT/miss/pong from the disco subsystem. The
//     Tailscale-style "asymmetric ratio + miss-streak"
//     evaluator runs on every event and may flip a
//     peer's currentPath (subject to minDwellTime).
//   - Tick()          — periodic safety net. Only fires when probe-driven
//     hasn't been able to act (no recent disco evidence)
//     and WireGuard handshake hasn't completed within
//     fallbackAfter; in that case the peer is forced to
//     relay regardless of EWMA state.
//
// peerEngine is the slice of wgnet.Engine that the reconciler depends
// on. Hoisted to an interface so tests can stub it.
type peerEngine interface {
	UpdatePeers([]wgnet.Peer) error
	PeerHandshakeTimes() (map[string]time.Time, error)
	// SetPeerNetworks replaces the deviceID → foreign-home-network table
	// used to stamp DstNetworkID on relay frames (public share spec §10).
	SetPeerNetworks(map[string]string)
}

// reconcilerConfig is the bag of probe-driven-fallback knobs. All
// thresholds are exposed as flags on cmd/waired-agent so the testnet
// scripts can dial them down to make tests deterministic without
// touching defaults that ship to users.
type reconcilerConfig struct {
	ForceRelay        bool
	FallbackAfter     time.Duration // safety net: WG handshake idle for this long ⇒ flip to relay
	DowngradeRTTRatio float64       // direct EWMA > N × relay EWMA ⇒ downgrade
	UpgradeRTTRatio   float64       // direct EWMA < M × relay EWMA ⇒ upgrade (M > 1: prefer direct at parity; M < N keeps a dead band)
	DowngradeMisses   int           // consecutive direct probe misses ⇒ downgrade
	UpgradePongStreak int           // consecutive direct pongs needed for upgrade
	EWMAAlpha         float64       // weight given to a fresh sample (0,1]
	MinRTTSamples     int           // minimum samples before RTT ratio is consulted
	MinDwellTime      time.Duration // no reverse switch within this window

	// CallMeMaybeInterval is the per-peer base cadence for emitting
	// call_me_maybe frames over the relay. CMM is emitted only when:
	//   - currentPath == relay (rescue mechanism), OR
	//   - currentPath == direct AND no direct sample has yet been
	//     observed AND time since the peer was added > 2 ×
	//     ProbeReprobeActive (direct-stuck bootstrap)
	// Defaults to 15s (= disco ProbeReprobeActive).
	CallMeMaybeInterval time.Duration

	// CallMeMaybeBackoffMax caps the effective cadence after a CMM
	// fail-streak. When a CMM is sent and no direct evidence arrives
	// within 2 × cadence, the streak increments; for streak ≥ 3 the
	// cadence scales linearly per streak step, capped here. Defaults
	// to 5 minutes — long enough to bound relay bandwidth on
	// symmetric-NAT pairs that CMM fundamentally cannot fix.
	CallMeMaybeBackoffMax time.Duration

	// CMMBootstrapDelay gates the direct-stuck-bootstrap CMM trigger:
	// we wait at least this long after a peer joins before emitting
	// CMM, so the first probeAllPeers cycle has a chance to succeed
	// without CMM assistance. Defaults to 2 × disco ProbeReprobeActive
	// (= 30s).
	CMMBootstrapDelay time.Duration
}

// defaults follow the Tailscale-style "dead-band ratio" model: a
// working direct path is preferred over relay even at RTT parity
// (fewer hops, no relay bandwidth), so the upgrade ratio sits ABOVE
// 1.0 — direct only stays vetoed when it is substantially slower than
// relay. The 1.5–2.0 gap against the downgrade ratio is the
// hysteresis dead band; MinDwellTime + UpgradePongStreak provide flap
// suppression on top. The pre-#349 value of 0.8 ("direct must be 20%
// faster") starved the relay→direct upgrade whenever host scheduling
// latency dominated both paths (direct ≈ relay), pinning peers to
// relay for minutes.
const (
	defaultDowngradeRTTRatio     = 2.0
	defaultUpgradeRTTRatio       = 1.5
	defaultDowngradeMisses       = 3
	defaultUpgradePongStreak     = 3
	defaultEWMAAlpha             = 0.2
	defaultMinRTTSamples         = 3
	defaultMinDwellTime          = 30 * time.Second
	defaultCallMeMaybeInterval   = 15 * time.Second
	defaultCallMeMaybeBackoffMax = 5 * time.Minute
	defaultCMMBootstrapDelay     = 30 * time.Second
)

func (c reconcilerConfig) withDefaults() reconcilerConfig {
	if c.DowngradeRTTRatio <= 0 {
		c.DowngradeRTTRatio = defaultDowngradeRTTRatio
	}
	if c.UpgradeRTTRatio <= 0 {
		c.UpgradeRTTRatio = defaultUpgradeRTTRatio
	}
	if c.DowngradeMisses <= 0 {
		c.DowngradeMisses = defaultDowngradeMisses
	}
	if c.UpgradePongStreak <= 0 {
		c.UpgradePongStreak = defaultUpgradePongStreak
	}
	if c.EWMAAlpha <= 0 || c.EWMAAlpha > 1 {
		c.EWMAAlpha = defaultEWMAAlpha
	}
	if c.MinRTTSamples <= 0 {
		c.MinRTTSamples = defaultMinRTTSamples
	}
	if c.MinDwellTime <= 0 {
		c.MinDwellTime = defaultMinDwellTime
	}
	if c.CallMeMaybeInterval <= 0 {
		c.CallMeMaybeInterval = defaultCallMeMaybeInterval
	}
	if c.CallMeMaybeBackoffMax <= 0 {
		c.CallMeMaybeBackoffMax = defaultCallMeMaybeBackoffMax
	}
	if c.CMMBootstrapDelay <= 0 {
		c.CMMBootstrapDelay = defaultCMMBootstrapDelay
	}
	return c
}

// pathDirect / pathRelay are the canonical currentPath values. They
// match the disco package's wire-level Path strings so logs and
// management-API output use the same vocabulary end-to-end.
const (
	pathDirect = "direct"
	pathRelay  = "relay"
)

// discoSubsystem is the slice of *disco.Service the reconciler depends
// on. Hoisted to an interface so tests can stub the CMM trigger and
// observability accessors. *disco.Service satisfies this interface
// directly — the production wiring is unchanged.
type discoSubsystem interface {
	UpdateRelays([]string)
	UpdatePeers(map[string]disco.PeerSnapshot)
	SendCallMeMaybe(peerNodePub, peerDeviceID, peerNodeKey, relayURL string, candidates []netip.AddrPort) error
	ObservedAddr() netip.AddrPort
	NATType() disco.NATType
}

type reconciler struct {
	engine   peerEngine
	provider *agentProvider
	logger   *slog.Logger
	cfg      reconcilerConfig
	id       *identity.Identity
	// disco is optional; when non-nil the reconciler pushes
	// (relays, peers) snapshots into the disco subsystem on Apply so
	// it knows which UDP STUN-echoes to poll and which peers to probe.
	// Also drives the call_me_maybe trigger out of Tick. Nil during
	// unit tests that exercise reconciler logic only.
	disco discoSubsystem

	mu    sync.Mutex
	nm    *signer.NetworkMap
	state map[string]*peerPathState // keyed by peer NodePublicKey (std-base64)
	// logNames maps peer NodePublicKey → log display name: the Public
	// Share grant pseudonym for foreign grant peers, the real DeviceID
	// otherwise (spec §8.5). Rebuilt on every Apply.
	logNames map[string]string
}

// peerLogName returns the identifier to print in logs for a map peer:
// the grant pseudonym for Public Share peers, the DeviceID otherwise.
func peerLogName(p signer.NetworkMapPeer) string {
	if p.Grant != nil && p.Grant.Pseudonym != "" {
		return p.Grant.Pseudonym
	}
	return p.DeviceID
}

// logNameLocked resolves a peer NodePublicKey to its log display name.
// Caller holds r.mu. fallback is used for peers not in the current map
// (e.g. a disco event racing a map update).
func (r *reconciler) logNameLocked(nodePub, fallback string) string {
	if n, ok := r.logNames[nodePub]; ok && n != "" {
		return n
	}
	return fallback
}

// peerPathState is the per-peer book-keeping the reconciler keeps for
// path selection. Combines:
//   - the disco-driven RTT EWMAs and miss counts that the Tailscale-style
//     asymmetric ratio evaluator consumes;
//   - dwell-time bookkeeping to suppress flapping;
//   - the legacy disco-pong-derived observed addr (the actual dst the
//     reconciler hands to wireguard-go when the path is direct);
//   - a "lastDirectEvidenceAt" timestamp so the safety-net Tick can
//     distinguish "we sent probes and they're failing" (let probe-driven
//     handle it) from "we sent probes and the disco subsystem is silent"
//     (force a relay flip after fallbackAfter).
type peerPathState struct {
	currentPath      string    // pathDirect / pathRelay; "" treated as direct
	lastSwitchAt     time.Time // last time currentPath flipped
	lastSwitchReason string    // human/structured reason recorded at the switch
	lastEvalAt       time.Time // last Apply (= "fresh chance for direct" anchor)

	// Direct path quality. RTT EWMA in nanoseconds (time.Duration).
	directRTTEWMA        time.Duration
	directSampleCount    int
	directMissStreak     int
	recentDirectPongs    []bool    // ring buffer of last K probe results (true=pong, false=miss)
	lastDirectEvidenceAt time.Time // most recent direct RTT sample OR miss event

	// lastUpgradeRejectReason records which evaluateSwitchLocked gate
	// silenced the most-recent relay→direct upgrade attempt. Empty
	// when currentPath==direct (no upgrade needed) or when the last
	// evaluation actually upgraded. Surfaced via PathSnapshot so the
	// testnet fallback-runner can attribute a stuck-on-relay state to
	// a specific gate ("samples","ewma_zero","ring_not_full","ratio",
	// "dwell","force_relay") rather than having to instrument reconciler
	// flow live.
	lastUpgradeRejectReason string

	// Relay path quality. Same shape, but no miss-streak / pong-ring is
	// tracked because relay loss isn't a downgrade trigger in this model
	// (relay is always available; if relay itself dies, WG handshake
	// stalls and the safety net + heartbeat reconnect handle it).
	relayRTTEWMA     time.Duration
	relaySampleCount int

	// disco-derived direct addr. Set when EventPongFromPeer (direct
	// path) verifies a peer; the reconciler uses observedAddr in place
	// of the peer's published Endpoints[0] when currentPath==direct.
	observedAddr   netip.AddrPort
	directHinted   bool
	directHintedAt time.Time

	// call_me_maybe sender-side bookkeeping.
	//   lastCallMeMaybeAt:    last time we emitted a CMM frame to this peer
	//   cmmFailureRecorded:   guards against double-counting one CMM
	//                         emission's failure into callMeMaybeFailStreak
	//   callMeMaybeFailStreak: incremented when a CMM was emitted and no
	//                         direct evidence followed within 2 ×
	//                         effective cadence. Used to scale the
	//                         effective cadence linearly (cap = config),
	//                         bounding relay bandwidth on symmetric NATs
	//                         CMM cannot fix. Reset on EventPongFromPeer.
	//   callMeMaybeSentCount: lifetime emit counter (observability)
	//   callMeMaybeRecvAt:    timestamp of most recent EventCallMeMaybeReceived
	//   callMeMaybeRecvCount: lifetime receive counter (observability)
	lastCallMeMaybeAt     time.Time
	cmmFailureRecorded    bool
	callMeMaybeFailStreak int
	callMeMaybeSentCount  int
	callMeMaybeRecvAt     time.Time
	callMeMaybeRecvCount  int
}

func newReconciler(engine peerEngine, provider *agentProvider, logger *slog.Logger, id *identity.Identity, cfg reconcilerConfig) *reconciler {
	return &reconciler{
		engine:   engine,
		provider: provider,
		logger:   logger,
		cfg:      cfg.withDefaults(),
		id:       id,
		state:    map[string]*peerPathState{},
	}
}

// AttachDisco wires the disco subsystem so Apply pushes (relays,
// peers) snapshots into it AND so Tick can drive the call_me_maybe
// trigger. Optional; the reconciler functions correctly with
// disco=nil (no NAT punching, no CMM, falls back to safety-net only).
//
// Production wires *disco.Service here. Tests use a stub.
func (r *reconciler) AttachDisco(d discoSubsystem) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.disco = d
}

// OnDiscoEvent is called by the goroutine draining disco.Service.Events().
// It updates per-peer state and, when the event implies a path change,
// re-applies the WG peer set.
func (r *reconciler) OnDiscoEvent(ev disco.Event) {
	switch e := ev.(type) {
	case disco.EventPongFromPeer:
		r.handlePongFromPeer(e)
	case disco.EventProbeRTTSampled:
		r.handleRTTSample(e)
	case disco.EventProbeMissed:
		r.handleProbeMissed(e)
	case disco.EventProbeRoundFinalized:
		r.handleRoundFinalized(e)
	case disco.EventCallMeMaybeReceived:
		r.handleCallMeMaybeReceived(e)
	default:
		// EventObservedAddr / EventNATTypeDetected are observability
		// signals; main.go handles them (e.g. forwarding observed addr
		// to CP via POST /v1/devices/self/endpoints).
	}
}

// handleCallMeMaybeReceived records the receive timestamp / counter for
// observability. The disco service has already merged the candidate
// list into its probe target set as cmmHints, so the reconciler does
// not need to feed anything into the WG peer set — the next regular
// probe cycle will exercise the new candidates and the existing
// probe-driven path-selection will pick up the resulting RTT samples.
func (r *reconciler) handleCallMeMaybeReceived(e disco.EventCallMeMaybeReceived) {
	r.mu.Lock()
	st, ok := r.state[e.PeerNodePub]
	if !ok {
		st = &peerPathState{lastEvalAt: e.At}
		r.state[e.PeerNodePub] = st
	}
	st.callMeMaybeRecvAt = e.At
	st.callMeMaybeRecvCount++
	r.mu.Unlock()
}

// handlePongFromPeer adopts the disco-confirmed direct addr for a
// peer. It does NOT decide path selection on its own — that's
// evaluateSwitch's job — but it DOES re-run recompute() so the freshly
// learned observedAddr is plumbed into the engine immediately. Without
// the recompute, currentPath==direct peers would keep routing to the
// stale Endpoints[0] from the network map until the next RTT/miss
// event triggered a recompute as a side effect.
func (r *reconciler) handlePongFromPeer(e disco.EventPongFromPeer) {
	r.mu.Lock()
	st, ok := r.state[e.PeerNodePub]
	if !ok {
		st = &peerPathState{lastEvalAt: e.ReceivedAt}
		r.state[e.PeerNodePub] = st
	}
	changed := st.observedAddr != e.DirectSrc || !st.directHinted
	st.observedAddr = e.DirectSrc
	st.directHinted = true
	st.directHintedAt = e.ReceivedAt
	// Direct evidence cancels the call_me_maybe fail-streak — the
	// path is alive (whether or not CMM was the reason); future CMM
	// cadences should reset to the base rate, not the backed-off rate.
	st.callMeMaybeFailStreak = 0
	st.cmmFailureRecorded = false
	r.mu.Unlock()
	if changed {
		if err := r.recompute(); err != nil {
			r.logger.Error("recompute after disco pong", "err", err)
		}
	}
}

// handleRTTSample updates the per-peer EWMA and sample count for the
// sample's path, and stamps direct evidence on a successful direct
// probe. Path-switch hysteresis (pongRing) and miss-streak bookkeeping
// have moved to handleRoundFinalized so multi-candidate fan-out can't
// distort the ring with per-probe outcomes — see disco's
// EventProbeRoundFinalized doc.
func (r *reconciler) handleRTTSample(e disco.EventProbeRTTSampled) {
	r.mu.Lock()
	st, ok := r.state[e.PeerNodePub]
	if !ok {
		st = &peerPathState{lastEvalAt: e.At}
		r.state[e.PeerNodePub] = st
	}
	if e.Path == pathRelay {
		st.relayRTTEWMA = applyEWMA(st.relayRTTEWMA, e.RTT, r.cfg.EWMAAlpha)
		st.relaySampleCount++
	} else {
		st.directRTTEWMA = applyEWMA(st.directRTTEWMA, e.RTT, r.cfg.EWMAAlpha)
		st.directSampleCount++
		st.lastDirectEvidenceAt = e.At
	}
	switched, reason := r.evaluateSwitchLocked(st, e.At, r.logNameLocked(e.PeerNodePub, e.PeerDeviceID))
	r.mu.Unlock()
	if switched {
		if err := r.recompute(); err != nil {
			r.logger.Error("recompute after switch", "err", err, "reason", reason)
		}
	}
}

// handleProbeMissed records the per-event miss for observability and
// re-evaluates the switch criteria. Miss-streak and pongRing updates
// have moved to handleRoundFinalized — a single round may emit N miss
// events (one per candidate) and we don't want to count those N times
// against the downgrade threshold.
//
// Note: lastDirectEvidenceAt is intentionally NOT updated on a miss.
// "Direct evidence" means a successful round-trip — a miss is evidence
// the path is broken, not that we recently reached the peer. Updating
// the timestamp on misses would suppress the safety-net Tick
// indefinitely whenever probes are firing but failing (a state both
// triggers should fire on, not a state to wait through).
func (r *reconciler) handleProbeMissed(e disco.EventProbeMissed) {
	r.mu.Lock()
	st, ok := r.state[e.PeerNodePub]
	if !ok {
		st = &peerPathState{lastEvalAt: e.At}
		r.state[e.PeerNodePub] = st
	}
	switched, reason := r.evaluateSwitchLocked(st, e.At, r.logNameLocked(e.PeerNodePub, e.PeerDeviceID))
	r.mu.Unlock()
	if switched {
		if err := r.recompute(); err != nil {
			r.logger.Error("recompute after switch", "err", err, "reason", reason)
		}
	}
}

// handleRoundFinalized is the round-aware counterpart to handleRTTSample /
// handleProbeMissed. The disco service aggregates per-candidate outcomes
// into one EventProbeRoundFinalized per direct probe round, so the
// reconciler can update its hysteresis state (pongRing + miss streak)
// without per-candidate noise corrupting either signal. A round counts as
// "success" iff at least one candidate pongged (AnySuccess=true).
//
// Relay-path events still flow through handleRTTSample (1 round = 1
// probe over a single HomeRelay, no fan-out); only direct-path round
// finalization is handled here.
func (r *reconciler) handleRoundFinalized(e disco.EventProbeRoundFinalized) {
	if e.Path != pathDirect {
		return
	}
	r.mu.Lock()
	st, ok := r.state[e.PeerNodePub]
	if !ok {
		st = &peerPathState{lastEvalAt: e.At}
		r.state[e.PeerNodePub] = st
	}
	appendPongRing(st, e.AnySuccess, r.cfg.UpgradePongStreak)
	if e.AnySuccess {
		st.directMissStreak = 0
	} else {
		st.directMissStreak++
	}
	switched, reason := r.evaluateSwitchLocked(st, e.At, r.logNameLocked(e.PeerNodePub, e.PeerDeviceID))
	r.mu.Unlock()
	if switched {
		if err := r.recompute(); err != nil {
			r.logger.Error("recompute after round finalized", "err", err, "reason", reason)
		}
	}
}

// evaluateSwitchLocked is the heart of the Tailscale-style asymmetric
// ratio evaluator. Caller holds r.mu. Returns (switched, reason) where
// switched is true iff currentPath flipped. reason is the structured
// log explanation.
//
// Downgrade triggers (currentPath==direct):
//   - directMissStreak >= cfg.DowngradeMisses, OR
//   - directRTTEWMA > cfg.DowngradeRTTRatio × relayRTTEWMA AND we have
//     enough samples on both sides
//
// Upgrade triggers (currentPath==relay):
//   - directRTTEWMA < cfg.UpgradeRTTRatio × relayRTTEWMA AND
//     last cfg.UpgradePongStreak direct probes all pongged AND
//     enough samples
//
// With the default UpgradeRTTRatio of 1.5 the RTT term is a veto, not
// a contest: a direct path proven alive by the pong streak upgrades
// even at RTT parity with relay, and is only held back when it is
// >1.5× slower. The gap up to DowngradeRTTRatio (2.0) is the dead
// band that prevents upgrade/downgrade ping-pong for marginal paths.
//
// All switches are blocked for cfg.MinDwellTime after the previous
// switch; this provides hysteresis on top of the ratio dead-band.
func (r *reconciler) evaluateSwitchLocked(st *peerPathState, now time.Time, peerDeviceID string) (bool, string) {
	if r.cfg.ForceRelay {
		if st.currentPath == pathRelay {
			st.lastUpgradeRejectReason = "force_relay"
		}
		return false, "" // force-relay short-circuits everything
	}
	curr := st.currentPath
	if curr == "" {
		curr = pathDirect
	}
	// minDwell: respect dwell after the FIRST switch; before any
	// switch, lastSwitchAt is zero and we let probes drive freely.
	if !st.lastSwitchAt.IsZero() && now.Sub(st.lastSwitchAt) < r.cfg.MinDwellTime {
		if curr == pathRelay {
			st.lastUpgradeRejectReason = "dwell"
		}
		return false, ""
	}

	switch curr {
	case pathDirect:
		// Downgrade?
		if st.directMissStreak >= r.cfg.DowngradeMisses {
			st.currentPath = pathRelay
			st.lastSwitchAt = now
			st.lastSwitchReason = "miss_streak"
			r.logger.Info("path: downgrade to relay (miss streak)",
				"device_id", peerDeviceID,
				"miss_streak", st.directMissStreak,
				"direct_rtt_ms", st.directRTTEWMA.Milliseconds(),
				"relay_rtt_ms", st.relayRTTEWMA.Milliseconds(),
			)
			return true, "miss_streak"
		}
		if r.haveEnoughSamples(st) && st.directRTTEWMA > durMul(st.relayRTTEWMA, r.cfg.DowngradeRTTRatio) {
			st.currentPath = pathRelay
			st.lastSwitchAt = now
			st.lastSwitchReason = "rtt_ratio"
			r.logger.Info("path: downgrade to relay (rtt ratio)",
				"device_id", peerDeviceID,
				"direct_rtt_ms", st.directRTTEWMA.Milliseconds(),
				"relay_rtt_ms", st.relayRTTEWMA.Milliseconds(),
				"ratio_threshold", r.cfg.DowngradeRTTRatio,
			)
			return true, "rtt_ratio"
		}
	case pathRelay:
		// Upgrade?
		if !r.haveEnoughSamples(st) {
			st.lastUpgradeRejectReason = "samples"
			return false, ""
		}
		if st.directRTTEWMA == 0 {
			st.lastUpgradeRejectReason = "ewma_zero"
			return false, "" // no direct RTT yet — can't compare
		}
		if !pongRingFull(st, r.cfg.UpgradePongStreak) {
			st.lastUpgradeRejectReason = "ring_not_full"
			return false, ""
		}
		if st.directRTTEWMA < durMul(st.relayRTTEWMA, r.cfg.UpgradeRTTRatio) {
			st.currentPath = pathDirect
			st.lastSwitchAt = now
			st.lastSwitchReason = "rtt_ratio_upgrade"
			st.lastUpgradeRejectReason = ""
			st.directMissStreak = 0
			st.lastDirectEvidenceAt = now
			r.logger.Info("path: upgrade to direct (rtt ratio)",
				"device_id", peerDeviceID,
				"direct_rtt_ms", st.directRTTEWMA.Milliseconds(),
				"relay_rtt_ms", st.relayRTTEWMA.Milliseconds(),
				"ratio_threshold", r.cfg.UpgradeRTTRatio,
			)
			return true, "rtt_ratio_upgrade"
		}
		st.lastUpgradeRejectReason = "ratio"
	}
	return false, ""
}

func (r *reconciler) haveEnoughSamples(st *peerPathState) bool {
	return st.directSampleCount >= r.cfg.MinRTTSamples && st.relaySampleCount >= r.cfg.MinRTTSamples
}

// applyEWMA returns the new EWMA after folding `sample` in with weight α.
// First sample (prev==0) is taken as-is so we don't have to seed.
func applyEWMA(prev, sample time.Duration, alpha float64) time.Duration {
	if prev <= 0 {
		return sample
	}
	return time.Duration(float64(sample)*alpha + float64(prev)*(1-alpha))
}

// durMul multiplies a duration by a float, avoiding overflow into
// negative for huge factors (caps at MaxInt64).
func durMul(d time.Duration, f float64) time.Duration {
	if d <= 0 {
		return 0
	}
	v := float64(d) * f
	if v > float64(time.Duration(1<<62)) {
		return time.Duration(1 << 62)
	}
	return time.Duration(v)
}

// appendPongRing appends a pong/miss outcome (true/false) to the
// per-peer ring buffer used for the upgrade pong-streak gate. Ring
// length is capped at cap; older entries are dropped.
func appendPongRing(st *peerPathState, ok bool, capN int) {
	if capN <= 0 {
		capN = 1
	}
	st.recentDirectPongs = append(st.recentDirectPongs, ok)
	if len(st.recentDirectPongs) > capN {
		st.recentDirectPongs = st.recentDirectPongs[len(st.recentDirectPongs)-capN:]
	}
}

// pongRingFull reports whether the most recent capN entries are all
// pongs (true). Used as the upgrade-trigger AND condition.
func pongRingFull(st *peerPathState, capN int) bool {
	if len(st.recentDirectPongs) < capN {
		return false
	}
	for _, ok := range st.recentDirectPongs[len(st.recentDirectPongs)-capN:] {
		if !ok {
			return false
		}
	}
	return true
}

// Apply ingests a new NetworkMap. Per-peer state is touched as follows:
//
//   - For peers that are NEW (no prior state): initialize at currentPath
//     == direct, anchor lastEvalAt / lastDirectEvidenceAt to now.
//   - For peers that ALREADY have state: preserve currentPath, EWMAs,
//     sample counts, miss streak, observedAddr, directHinted, and
//     timestamps. Apply must be idempotent w.r.t. path selection so
//     that a CP republish (which fires for ANY peer change in the
//     network, not just this one) does not overwrite a probe-driven
//     downgrade. The original behaviour — reset currentPath to direct
//     on every Apply — caused the testnet `fallback-basic` /
//     `flap-suppression` / `cold-start` scenarios to oscillate, since
//     each republish (~every few seconds during enrollment) flipped
//     the agent back to direct before the downgrade could take effect.
//   - Peers that disappeared from the map have their entire state
//     garbage-collected.
//
// The intent of the legacy "fresh chance for direct after topology
// changes" was to catch peers that genuinely became reachable on a
// new candidate. That case is now covered cleanly by the probe-driven
// upgrade trigger (asymmetric ratio + pong streak) — no Apply-level
// reset needed.
func (r *reconciler) Apply(nm *signer.NetworkMap) error {
	r.mu.Lock()
	r.nm = nm
	now := time.Now()
	live := make(map[string]struct{}, len(nm.Peers))
	r.logNames = make(map[string]string, len(nm.Peers))
	added := 0
	for _, p := range nm.Peers {
		live[p.NodePublicKey] = struct{}{}
		r.logNames[p.NodePublicKey] = peerLogName(p)
		if _, ok := r.state[p.NodePublicKey]; ok {
			continue // preserve existing per-peer path state
		}
		added++
		r.state[p.NodePublicKey] = &peerPathState{
			currentPath:          pathDirect,
			lastEvalAt:           now,
			lastDirectEvidenceAt: now,
		}
	}
	removed := 0
	for k := range r.state {
		if _, ok := live[k]; !ok {
			delete(r.state, k)
			removed++
		}
	}
	d := r.disco
	r.mu.Unlock()

	if r.logger != nil {
		r.logger.Debug("reconcile: applying network map",
			"network_id", nm.NetworkID,
			"peers", len(nm.Peers),
			"relays", len(nm.Relays),
			"peers_added", added,
			"peers_removed", removed,
		)
	}

	// Cross-network peer table (public share spec §10): CP-injected
	// foreign peers carry their home NetworkID; the relay bind stamps
	// it as DstNetworkID on frames to them. Fed BEFORE peers/disco so
	// the registry exists before anything can trigger a send.
	nets := map[string]string{}
	for _, p := range nm.Peers {
		if p.NetworkID != "" && p.NetworkID != nm.NetworkID {
			nets[p.DeviceID] = p.NetworkID
		}
	}
	r.engine.SetPeerNetworks(nets)

	r.provider.replacePeers(nm)
	if d != nil {
		r.pushDiscoSnapshot(d, nm)
	}
	return r.recompute()
}

// pushDiscoSnapshot translates the latest NetworkMap into the inputs
// the disco subsystem needs: relay base URLs (for STUN observation)
// and peer (machinePub, candidates, relay) tuples (for probe targeting
// over both direct UDP and the peer's HomeRelay).
//
// Every non-relay candidate is forwarded to disco — no receiver-side
// subnet filter. A peer's KindLocal RFC1918 address might look
// unreachable when it's on a different prefix than ours, but routed
// cross-subnet topologies (corporate WAN, SD-WAN, multi-VLAN home
// routers, overlay VPNs, k8s pod networks, VPC peering) make
// "different prefix" a poor proxy for "unreachable". Probe everything,
// let the cryptographically authenticated pong decide. This matches
// Tailscale's magicsock policy (see docs/knowledges/20260517/
// 0200-tailscale-style-candidate-probing.md).
func (r *reconciler) pushDiscoSnapshot(d discoSubsystem, nm *signer.NetworkMap) {
	urls := make([]string, 0, len(nm.Relays))
	relayByID := make(map[string]string, len(nm.Relays))
	for _, rel := range nm.Relays {
		if rel.URL != "" {
			relayByID[rel.RelayID] = rel.URL
		}
		// DiscoHosts is the authoritative STUN-target list when present;
		// v6 entries land first so the observer's bestObs selection
		// (first relay with ≥2 samples) reports a v6 observed_addr when
		// both families are reachable — what testnet-ipv6-verify.sh
		// asserts. Legacy v4-only relays leave DiscoHosts empty and we
		// fall back to extracting the host from rel.URL.
		if len(rel.DiscoHosts) > 0 {
			for _, host := range orderHostsV6First(rel.DiscoHosts) {
				urls = append(urls, discoProbeURL(host))
			}
		} else if rel.URL != "" {
			urls = append(urls, rel.URL)
		}
	}
	d.UpdateRelays(urls)

	peers := make(map[string]disco.PeerSnapshot, len(nm.Peers))
	for _, p := range nm.Peers {
		np, err := base64.StdEncoding.DecodeString(p.NodePublicKey)
		if err != nil || len(np) != wiredisco.NodeKeySize {
			// Without the NodeKey the disco sealed-frame layer can't
			// encrypt to this peer — skip it. (MachinePub was used for
			// Ed25519 sig verify on the prior plaintext disco path;
			// AEAD authenticates via NodeKey-derived ECDH instead.)
			continue
		}
		var cands []string
		for _, ep := range p.Endpoints {
			if ep.Kind == signer.KindRelay {
				continue
			}
			if ep.Addr != "" {
				cands = append(cands, ep.Addr)
			}
		}
		var nodePub [wiredisco.NodeKeySize]byte
		copy(nodePub[:], np)
		peers[p.NodePublicKey] = disco.PeerSnapshot{
			DeviceID:   p.DeviceID,
			LogName:    peerLogName(p),
			NodePub:    nodePub,
			Candidates: cands,
			RelayURL:   relayByID[p.HomeRelay],
		}
	}
	d.UpdatePeers(peers)
	if r.logger != nil {
		r.logger.Debug("reconcile: disco snapshot pushed",
			"relay_urls", len(urls), "peers", len(peers))
	}
}

// Tick is the safety-net path. Probe-driven downgrade (RTT ratio /
// miss streak) handles the common case; this Tick exists for the
// corner case where the disco subsystem is silent (no probes
// completing, no misses firing) AND WireGuard hasn't completed a
// handshake within fallbackAfter — in that case we conservatively flip
// to relay so connectivity isn't held hostage by a stuck disco.
//
// Conditions to fire safety net (per peer):
//   - currentPath == direct
//   - HomeRelay configured
//   - now-lastEvalAt >= fallbackAfter (gives direct a chance after Apply)
//   - now-lastDirectEvidenceAt >= fallbackAfter (probe-driven IS silent)
//   - WG hasn't handshaken since lastEvalAt (data path is broken)
//   - dwell time has elapsed since any prior switch
func (r *reconciler) Tick(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	if r.cfg.ForceRelay {
		return
	}
	hs, err := r.engine.PeerHandshakeTimes()
	if err != nil {
		r.logger.Warn("peer handshake times", "err", err)
		return
	}

	r.mu.Lock()
	if r.nm == nil {
		r.mu.Unlock()
		return
	}
	now := time.Now()
	changed := false
	for _, p := range r.nm.Peers {
		st := r.state[p.NodePublicKey]
		if st == nil {
			continue
		}
		if st.currentPath == pathRelay {
			continue
		}
		if p.HomeRelay == "" {
			continue
		}
		if hasOnlyRelayEndpoint(p) {
			continue
		}
		if now.Sub(st.lastEvalAt) < r.cfg.FallbackAfter {
			continue
		}
		if !st.lastSwitchAt.IsZero() && now.Sub(st.lastSwitchAt) < r.cfg.MinDwellTime {
			continue
		}
		if !st.lastDirectEvidenceAt.IsZero() && now.Sub(st.lastDirectEvidenceAt) < r.cfg.FallbackAfter {
			// Probe-driven layer is awake (recent RTT or miss event);
			// let it handle the decision instead of forcing a flip.
			continue
		}
		hsTime := hs[p.NodePublicKey]
		if hsTime.After(st.lastEvalAt) {
			// We did handshake on direct; data path is alive.
			continue
		}
		st.currentPath = pathRelay
		st.lastSwitchAt = now
		st.lastSwitchReason = "safety_net"
		st.lastEvalAt = now
		changed = true
		r.logger.Info("path: downgrade to relay (safety net)",
			"device_id", peerLogName(p),
			"home_relay", p.HomeRelay,
			"stale_for", now.Sub(hsTime).Truncate(time.Second).String(),
		)
	}

	// CMM emit pass — separate from the safety-net loop so the dwell
	// gating differs (CMM does not flip currentPath; it just nudges
	// the peer to also probe us). Runs under r.mu but enqueues actual
	// emissions on goroutines so a slow relay-WSS write can't stall
	// Tick.
	cmmEmits := r.collectCMMEmissionsLocked(now)
	r.mu.Unlock()

	for _, em := range cmmEmits {
		em := em
		go r.emitCallMeMaybe(em)
	}

	if changed {
		if err := r.recompute(); err != nil {
			r.logger.Error("safety-net recompute", "err", err)
		}
	}
}

// cmmEmission is the data captured from peer state under r.mu, then
// passed to emitCallMeMaybe so the relay-WSS write can happen outside
// the lock.
type cmmEmission struct {
	peerNodePub  string
	peerDeviceID string
	peerLog      string // display name for logs (grant pseudonym for public peers)
	peerNodeKey  string
	relayURL     string
	candidate    netip.AddrPort
}

// collectCMMEmissionsLocked builds the list of CMM frames Tick should
// emit this round. Caller holds r.mu. Updates st.lastCallMeMaybeAt /
// st.cmmFailureRecorded / st.callMeMaybeFailStreak in place — emit is
// committed-to here to keep the cadence honest even if the goroutine
// fails the actual send.
func (r *reconciler) collectCMMEmissionsLocked(now time.Time) []cmmEmission {
	if r.disco == nil {
		return nil
	}
	natType := r.disco.NATType()
	if natType == disco.NATTypeSymmetric {
		// CMM relies on EIM behaviour: the addr we tell the peer is
		// the same addr it will see on direct probes. Symmetric NAT
		// invalidates that assumption; any CMM emission would just be
		// wasted relay bandwidth.
		return nil
	}
	observed := r.disco.ObservedAddr()
	if !observed.IsValid() {
		// We don't yet know our public addr (STUN observation hasn't
		// completed); CMM has no useful payload yet.
		return nil
	}
	relayByID := make(map[string]string, len(r.nm.Relays))
	for _, rel := range r.nm.Relays {
		relayByID[rel.RelayID] = rel.URL
	}

	var out []cmmEmission
	for _, p := range r.nm.Peers {
		st := r.state[p.NodePublicKey]
		if st == nil {
			continue
		}
		if p.HomeRelay == "" {
			continue
		}
		relayURL := relayByID[p.HomeRelay]
		if relayURL == "" {
			continue
		}
		if !cmmTriggerActive(st, r.cfg, now) {
			continue
		}
		// Cadence gate: respect the effective cadence for this peer's
		// current fail streak.
		cadence := effectiveCMMCadence(r.cfg.CallMeMaybeInterval, r.cfg.CallMeMaybeBackoffMax, st.callMeMaybeFailStreak)
		// Failure check: if the previous CMM was emitted more than
		// 2× cadence ago AND no direct evidence has arrived since, count
		// it as a failure (once per emission via cmmFailureRecorded).
		if !st.lastCallMeMaybeAt.IsZero() &&
			!st.cmmFailureRecorded &&
			now.Sub(st.lastCallMeMaybeAt) >= 2*cadence &&
			st.lastDirectEvidenceAt.Before(st.lastCallMeMaybeAt) {
			st.callMeMaybeFailStreak++
			st.cmmFailureRecorded = true
			r.logger.Debug("call_me_maybe fail streak ++",
				"device_id", peerLogName(p), "streak", st.callMeMaybeFailStreak)
			cadence = effectiveCMMCadence(r.cfg.CallMeMaybeInterval, r.cfg.CallMeMaybeBackoffMax, st.callMeMaybeFailStreak)
		}
		if !st.lastCallMeMaybeAt.IsZero() && now.Sub(st.lastCallMeMaybeAt) < cadence {
			continue
		}
		st.lastCallMeMaybeAt = now
		st.cmmFailureRecorded = false
		st.callMeMaybeSentCount++
		out = append(out, cmmEmission{
			peerNodePub:  p.NodePublicKey,
			peerDeviceID: p.DeviceID,
			peerLog:      peerLogName(p),
			peerNodeKey:  p.NodePublicKey,
			relayURL:     relayURL,
			candidate:    observed,
		})
	}
	return out
}

// cmmTriggerActive reports whether either CMM trigger condition is
// currently true for this peer:
//
//  1. Relay-state rescue: currentPath == relay (we already gave up on
//     direct via probe-driven; CMM is the rescue mechanism).
//  2. Direct-stuck bootstrap: currentPath == direct AND no direct
//     sample has been recorded AND directHinted is false AND time
//     since the peer was added > CMMBootstrapDelay (gives the regular
//     two-side-initiator probe race a chance to succeed without CMM
//     assistance, falling back to CMM when it doesn't).
func cmmTriggerActive(st *peerPathState, cfg reconcilerConfig, now time.Time) bool {
	curr := st.currentPath
	if curr == "" {
		curr = pathDirect
	}
	if curr == pathRelay {
		return true
	}
	if st.directSampleCount > 0 || st.directHinted {
		return false
	}
	if st.lastEvalAt.IsZero() {
		return false
	}
	return now.Sub(st.lastEvalAt) >= cfg.CMMBootstrapDelay
}

// effectiveCMMCadence applies the linear-with-cap backoff to the base
// cadence. Streak below 3 returns base; streak ≥ 3 multiplies by
// (streak - 1), capped at backoffMax.
func effectiveCMMCadence(base, backoffMax time.Duration, failStreak int) time.Duration {
	if failStreak < 3 {
		return base
	}
	scale := time.Duration(failStreak - 1)
	d := base * scale
	if backoffMax > 0 && d > backoffMax {
		d = backoffMax
	}
	return d
}

// emitCallMeMaybe issues the relay-tunnelled SendCallMeMaybe. Runs in
// a goroutine so a slow relay-WSS write doesn't stall Tick. The
// counter / timestamp updates that gate cadence already happened under
// r.mu in collectCMMEmissionsLocked — this function only logs.
func (r *reconciler) emitCallMeMaybe(em cmmEmission) {
	d := r.disco
	if d == nil {
		return
	}
	err := d.SendCallMeMaybe(em.peerNodePub, em.peerDeviceID, em.peerNodeKey, em.relayURL, []netip.AddrPort{em.candidate})
	if err != nil {
		r.logger.Debug("call_me_maybe send",
			"device_id", em.peerLog, "url", em.relayURL, "err", err)
		return
	}
	r.logger.Info("call_me_maybe sent",
		"device_id", em.peerLog,
		"url", em.relayURL,
		"observed", em.candidate.String(),
	)
}

func (r *reconciler) recompute() error {
	r.mu.Lock()
	nm := r.nm
	type snap struct {
		currentPath  string
		directHinted bool
		observedAddr netip.AddrPort
	}
	state := make(map[string]snap, len(r.state))
	for k, st := range r.state {
		state[k] = snap{currentPath: st.currentPath, directHinted: st.directHinted, observedAddr: st.observedAddr}
	}
	r.mu.Unlock()

	if nm == nil {
		return nil
	}
	if r.id != nil && r.id.ControlSigningPublicKey != "" {
		if err := controlclient.VerifyMap(r.id.ControlSigningPublicKey, nm); err != nil {
			r.logger.Warn("network map signature verification failed (warning only)", "err", err)
		}
	}

	peers := make([]wgnet.Peer, 0, len(nm.Peers))
	for _, p := range nm.Peers {
		ipAddr, err := netip.ParseAddr(p.OverlayIP)
		if err != nil {
			r.logger.Warn("skipping peer with bad overlay_ip", "device_id", peerLogName(p), "overlay_ip", p.OverlayIP, "err", err)
			continue
		}
		pubKey, err := devicekeys.DecodeX25519PublicKey(p.NodePublicKey)
		if err != nil {
			r.logger.Warn("skipping peer with bad node_public_key", "device_id", peerLogName(p), "err", err)
			continue
		}
		st := state[p.NodePublicKey]
		useRelay := r.cfg.ForceRelay || st.currentPath == pathRelay
		endpoint := pickEndpointWithHint(p, nm.Relays, useRelay, st.directHinted, st.observedAddr)
		if endpoint == "" {
			r.logger.Warn("skipping peer with no usable endpoint", "device_id", peerLogName(p), "force_relay", r.cfg.ForceRelay, "current_path", st.currentPath)
			continue
		}
		peers = append(peers, wgnet.Peer{
			DeviceName:          p.DeviceName,
			OverlayIP:           ipAddr,
			WireGuardPublicKey:  pubKey,
			Endpoint:            endpoint,
			PersistentKeepalive: 25,
		})
	}
	if r.logger != nil {
		r.logger.Debug("reconcile: pushing peer set to engine",
			"peers", len(peers), "map_peers", len(nm.Peers), "force_relay", r.cfg.ForceRelay)
	}
	return r.engine.UpdatePeers(peers)
}

// PathSnapshot is one peer's path-quality view, surfaced via the
// management API so testnet-fallback-* scripts can assert on it.
type PathSnapshot struct {
	DeviceID              string
	CurrentPath           string
	LastSwitchAt          time.Time
	LastSwitchReason      string
	DirectRTTMS           float64
	RelayRTTMS            float64
	DirectSampleCount     int
	RelaySampleCount      int
	DirectMissStreak      int
	LastDirectEvidence    time.Time
	HasDiscoHint          bool
	ObservedAddr          string
	CallMeMaybeSentAt     time.Time
	CallMeMaybeSentCount  int
	CallMeMaybeRecvAt     time.Time
	CallMeMaybeRecvCount  int
	CallMeMaybeFailStreak int
	// LastUpgradeRejectReason / RecentDirectPongs surface the reconciler's
	// upgrade-gate decision so the testnet fallback-runner can attribute a
	// stuck-on-relay state to a specific gate without live instrumentation.
	// Empty / nil when currentPath==direct.
	LastUpgradeRejectReason string
	RecentDirectPongs       []bool
}

// Snapshot returns a copy of the per-peer path state for observability.
// The map is keyed by NodePublicKey (std-base64), matching how the
// reconciler indexes internally.
func (r *reconciler) Snapshot() map[string]PathSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]PathSnapshot, len(r.state))
	for nodePub, st := range r.state {
		ps := PathSnapshot{
			CurrentPath:             st.currentPath,
			LastSwitchAt:            st.lastSwitchAt,
			LastSwitchReason:        st.lastSwitchReason,
			DirectRTTMS:             float64(st.directRTTEWMA.Microseconds()) / 1000.0,
			RelayRTTMS:              float64(st.relayRTTEWMA.Microseconds()) / 1000.0,
			DirectSampleCount:       st.directSampleCount,
			RelaySampleCount:        st.relaySampleCount,
			DirectMissStreak:        st.directMissStreak,
			LastDirectEvidence:      st.lastDirectEvidenceAt,
			HasDiscoHint:            st.directHinted,
			CallMeMaybeSentAt:       st.lastCallMeMaybeAt,
			CallMeMaybeSentCount:    st.callMeMaybeSentCount,
			CallMeMaybeRecvAt:       st.callMeMaybeRecvAt,
			CallMeMaybeRecvCount:    st.callMeMaybeRecvCount,
			CallMeMaybeFailStreak:   st.callMeMaybeFailStreak,
			LastUpgradeRejectReason: st.lastUpgradeRejectReason,
		}
		if len(st.recentDirectPongs) > 0 {
			ps.RecentDirectPongs = append([]bool(nil), st.recentDirectPongs...)
		}
		if st.directHinted && st.observedAddr.IsValid() {
			ps.ObservedAddr = st.observedAddr.String()
		}
		if r.nm != nil {
			for _, p := range r.nm.Peers {
				if p.NodePublicKey == nodePub {
					ps.DeviceID = p.DeviceID
					break
				}
			}
		}
		if ps.CurrentPath == "" {
			ps.CurrentPath = pathDirect
		}
		out[nodePub] = ps
	}
	return out
}

// pickEndpointWithHint chooses the wgnet endpoint string for one peer.
//
// Order of preference, highest to lowest:
//  1. forceRelay or currentPath==relay + HomeRelay set → relay
//  2. directHinted (disco-confirmed direct path) → observedAddr
//  3. peer's first non-relay candidate from NetworkMap (LAN listen,
//     IPv6, etc. — preserves direct-first behaviour for same-LAN peers
//     even before disco completes a punch)
//  4. (last resort) any remaining candidate, even if relay-shaped — so
//     a peer whose only published endpoint is a relay-form addr stays
//     reachable rather than getting silently dropped from UpdatePeers.
func pickEndpointWithHint(p signer.NetworkMapPeer, relays []signer.NetworkMapRelay, useRelay, directHinted bool, observed netip.AddrPort) string {
	if useRelay && p.HomeRelay != "" {
		for _, r := range relays {
			if r.RelayID == p.HomeRelay {
				return "relay:" + r.URL + "#dst=" + p.DeviceID + "&nk=" + p.NodePublicKey
			}
		}
	}
	if directHinted && observed.IsValid() && !useRelay {
		if observed.Addr().Is4() {
			return "udp4:" + observed.String()
		}
		return "udp6:" + observed.String()
	}
	for _, ep := range p.Endpoints {
		if ep.Kind == signer.KindRelay {
			continue
		}
		if ep.Addr != "" {
			return ep.Addr
		}
	}
	for _, ep := range p.Endpoints {
		if ep.Addr != "" {
			return ep.Addr
		}
	}
	return ""
}

// hasOnlyRelayEndpoint reports true when every advertised endpoint is
// already a relay form (Kind = "relay" or addr starts with "relay:").
// In that case the safety-net loop has nothing to do (the peer is on
// relay regardless).
func hasOnlyRelayEndpoint(p signer.NetworkMapPeer) bool {
	if len(p.Endpoints) == 0 {
		return false
	}
	for _, ep := range p.Endpoints {
		if ep.Kind != signer.KindRelay && !startsWith(ep.Addr, "relay:") {
			return false
		}
	}
	return true
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// discoProbeURL wraps a bare literal v4/v6 host in a synthetic wss URL
// so the existing disco observer (extractHostFromRelayURL → makeUDPDst)
// extracts the host and chooses the right UDP family without further
// changes. v6 literals get bracket wrapping, v4 stays bare.
func discoProbeURL(host string) string {
	if strings.Contains(host, ":") {
		return "wss://[" + host + "]/relay/v1/disco"
	}
	return "wss://" + host + "/relay/v1/disco"
}

// orderHostsV6First copies the input with all v6 literals (containing a
// colon) ahead of v4 literals, preserving relative order within each
// family. The observer's bestObs selection (first relay with ≥2 samples)
// reports the first family's observation, so putting v6 first means a
// dual-stack agent reports a v6 observed_addr when v6 is reachable.
func orderHostsV6First(hosts []string) []string {
	if len(hosts) <= 1 {
		return hosts
	}
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		if strings.Contains(h, ":") {
			out = append(out, h)
		}
	}
	for _, h := range hosts {
		if !strings.Contains(h, ":") {
			out = append(out, h)
		}
	}
	return out
}
