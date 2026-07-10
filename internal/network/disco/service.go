package disco

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	wireframe "github.com/waired-ai/waired-agent/proto/disco"
)

// Bind is the slice of *wgnet.MultiplexBind the disco service depends
// on. Hoisted to an interface so tests can stub UDP I/O.
//
// SendDisco sends a frame over direct UDP. SendDiscoViaRelay tunnels a
// frame through an active relay session (relay forwards opaquely;
// receiver classifies by disco magic prefix). Returning an error from
// either is non-fatal — the prober just records a probe miss for that
// path.
type Bind interface {
	SendDisco(payload []byte, dst string) error
	SendDiscoViaRelay(payload []byte, dstDeviceID, dstNodeKey, relayURL string) error
	DiscoInbound() <-chan wireframe.Inbound
}

// Config configures the agent disco subsystem.
type Config struct {
	// SelfDeviceID is the agent's CP-assigned device_id. Stamped into
	// every outbound frame's TagSrcDeviceID.
	SelfDeviceID string
	// SelfNodeKeyPriv is the agent's curve25519 (NodeKey) private scalar.
	// Used as the local half of ECDH key agreement when sealing /
	// opening peer↔peer disco frames (probe / pong / call_me_maybe).
	// Stays internal to the agent — only the corresponding public is
	// shared via CP NetworkMap.
	SelfNodeKeyPriv [wireframe.NodeKeySize]byte
	// SelfNodeKeyPub is the matching X25519 public half. Cached so we
	// don't recompute the scalar-mult per outbound frame and don't have
	// to re-derive when the sealed-frame HKDF info needs it.
	SelfNodeKeyPub [wireframe.NodeKeySize]byte
	// RelaySharedSecret is the optional relay-shared-secret used to
	// HMAC the disco_stun_request / disco_stun_response frames
	// against the relay's UDP STUN-echo service. v0 deployments leave
	// it empty because agents don't hold the relay-shared-secret;
	// the relay's STUN listener treats empty-HMAC requests as valid
	// in that mode and rate-limits to bound abuse. (STUN AEAD is
	// deferred — see GitHub issue follow-up; it would require the CP
	// to publish each relay's X25519 public via NetworkMap.)
	RelaySharedSecret []byte
	// Bind is the UDP plumbing. Required.
	Bind Bind
	// Logger receives operational logs.
	Logger *slog.Logger
	// Now is the clock (overridable for tests).
	Now func() time.Time

	// STUNObserveLearning is the relay-STUN poll interval while the
	// agent has not yet successfully observed any STUN response
	// (= bestObs is still zero). The shorter cadence shrinks the v6
	// first-observation tail on testnet (where per-VM /96 route
	// convergence and agent-side v6 NIC init can each add seconds),
	// since the unconditional STUNObserveActive=60s otherwise pays a
	// full sweep delay per missed exchange. Defaults to 10s; bounded
	// above by STUNObserveActive (the loop switches to active once any
	// observed addr lands).
	STUNObserveLearning time.Duration
	// STUNObserveActive is the relay-STUN poll interval while the
	// agent is actively trying to punch (= has any peer in
	// direct_probing). Defaults to 60s (spec §15.3 active path).
	STUNObserveActive time.Duration
	// STUNObserveIdle is the poll interval when no peer is probing
	// (relay path is fine). Defaults to 5min.
	STUNObserveIdle time.Duration
	// STUNTimeout caps how long we wait for a stun_response before
	// declaring the relay unreachable for STUN.
	STUNTimeout time.Duration
	// ProbeReprobeActive is the per-peer probe re-emit interval while
	// the peer is in direct_probing. Defaults to 15s.
	ProbeReprobeActive time.Duration
	// ProbeWindow is the timestamp window for replay-rejecting
	// inbound disco frames. Defaults to 60s.
	ProbeWindow time.Duration
	// CMMHintTTL bounds how long a call_me_maybe-learned candidate
	// addr stays in the per-peer probe target set. The receiver-side
	// hint mechanism keeps the NAT mapping refreshed across multiple
	// probe cycles (a single one-shot probe would only hold the
	// conntrack window open for ~30s before a regular probe to the
	// CP-published candidate would close it). Defaults to
	// 2 × ProbeReprobeActive (= 30s).
	CMMHintTTL time.Duration

	// DiscoUDPPorts is the list of UDP ports the relay's STUN-echo
	// service listens on. Two ports (3478 + 3479 by default) let the
	// detector compare observed src_ports for symmetric-NAT detection.
	DiscoUDPPorts []int
}

// Defaults filled in by New().
const (
	defaultSTUNObserveLearning = 10 * time.Second
	defaultSTUNObserveActive   = 60 * time.Second
	defaultSTUNObserveIdle     = 5 * time.Minute
	defaultSTUNTimeout         = 3 * time.Second
	defaultProbeReprobeActive  = 15 * time.Second
	defaultProbeWindow         = 60 * time.Second
)

var defaultDiscoUDPPorts = []int{3478, 3479}

// Service runs the disco subsystem.
type Service struct {
	cfg    Config
	logger *slog.Logger
	now    func() time.Time

	events chan Event

	// relayUpdate wakes runObserveLoop the instant UpdateRelays learns a
	// new/changed relay set, so the first STUN sweep fires immediately
	// instead of waiting up to one full observe interval. This removes the
	// up-to-one-cadence dead wait between "agent received the relay in its
	// NetworkMap" and "agent's next sweep" — the shareable (v4+v6) tail of
	// testnet bring-up. Cap-1 + non-blocking send: coalesces bursts and
	// never blocks UpdateRelays. Discovery + the STUN round-trip are
	// unchanged — this is pure latency removal.
	relayUpdate chan struct{}

	mu        sync.Mutex
	relayURLs []string             // agent-known relay base URLs (wss://host:port/...)
	peers     map[string]peerState // keyed by peer NodePublicKey (std-base64)
	natType   NATType
	observed  netip.AddrPort
	// lastObservedV6 records the most recent v6 sample the STUN observer
	// has seen (zero when never observed). Separate from `observed`
	// because the current round may have picked v4 (timing, transient
	// drop). Downstream consumers that want to confirm "v6 substrate
	// reachable" should consult this — it's monotonic in the sense that
	// once we have a v6 GUA, it stays until clearly invalidated.
	lastObservedV6 netip.AddrPort
	// firstObservedV6At stamps the wall-clock time of the very first v6
	// sample that ever flowed through observeOnce. Set once and never
	// rewritten; downstream (Status.FirstObservedV6Unix) consumes it to
	// surface "how long after agent start did v6 reach steady state"
	// per-agent. Zero until the first v6 sample.
	firstObservedV6At time.Time
	// stunAttempts{V4,V6} count outbound STUN echo sends in observeOne,
	// split by destination address family. Lockless atomic counters —
	// reads via STUNCounters() should not stall the observer hot path.
	// Paired with stunResponses{V4,V6} (counted on inbound nonce match
	// in handleSTUNResponse) the verifier can attribute "1/7 misses v6"
	// to either "agent never sent v6 STUN" or "agent sent but never got
	// a reply" — the two have distinct fixes.
	stunAttemptsV4  atomic.Uint64
	stunAttemptsV6  atomic.Uint64
	stunResponsesV4 atomic.Uint64
	stunResponsesV6 atomic.Uint64
	pendingProbes   map[string]pendingProbe  // probe nonce hex -> sent metadata, for pong correlation + miss detection
	pendingObserve  map[string]observeWaiter // outstanding STUN nonce -> waiter
	nonceCache      map[string]time.Time     // peer↔peer pong nonce dedup

	// roundIDCounter is bumped once per direct-path probe round (per
	// peer per probeAllPeers iteration). Atomic so test fixtures can
	// inject probes off-loop without taking s.mu.
	roundIDCounter atomic.Uint64
	// directRounds tracks in-flight direct-path probe rounds keyed by
	// roundID. Entries are created lazily on first send, populated by
	// pong / miss outcomes, and deleted once succeeded+missed >=
	// expected (and finalized=true). gc() also age-sweeps any entry
	// older than 2× ProbeReprobeActive as a theoretical leak guard.
	directRounds map[uint64]*directRoundState

	// rttEMA holds a per-peer exponentially-weighted moving average of
	// observed pong round-trip times, in milliseconds. Updated by
	// handlePong with α = rttEMAAlpha after the existing probe miss /
	// reconciler bookkeeping runs. Exposed via RTTSnapshot so this
	// agent's local Phase 7 Selector can use it as a tie-break after
	// catalog score and error rate (`router.Selector.LocalRTT`).
	//
	// Keyed by peer DeviceID so the consumer (Selector) reads it
	// without further translation. Direct-path samples and relay-path
	// samples are merged into the same EMA — the Phase 7 router scoring
	// treats RTT as a coarse signal where path attribution would not
	// change a routing decision.
	rttEMA map[string]float64

	// lastPongAt stamps the wall-clock time of the most recent
	// signed-and-nonce-matched pong from each peer. Set by
	// recordPongReceived, called inside handlePong's mu region only
	// when the pong matched an outstanding probe nonce (i.e. genuine
	// two-way reachability, not a replay). Exposed as a freshness map
	// via ReachableSnapshot — the Phase 8 Selector hard-excludes peers
	// whose entry is stale relative to the configured window.
	//
	// Peers we have NEVER received a pong from are absent. The
	// snapshot consumer treats absence as "unknown, default trust" so
	// freshly enrolled peers aren't excluded before their first probe
	// round completes.
	lastPongAt map[string]time.Time

	// cmmCallbacks are invoked after each verified call_me_maybe
	// frame is processed. Registered by OnCallMeMaybe. Production
	// code does not register here; the slice exists for the
	// testharness scenario dispatcher's CMM-aware iptables refresh.
	cmmCallbacks []func(peerNodePub, peerDeviceID string)
}

// rttEMAAlpha is the EMA smoothing factor for per-peer RTT samples.
// 0.3 weights the newest sample at 30% — fast enough to catch a peer
// migrating off Wi-Fi onto Ethernet (~3 samples to converge) but slow
// enough that a single pathological outlier doesn't flip the Selector
// preference for one routing decision.
const rttEMAAlpha = 0.3

// pendingProbe is one outstanding probe waiting for a pong. Tracked
// per-nonce so handlePong can compute RTT, attribute it to the correct
// transport (direct vs relay), and feed the reconciler a typed event.
// Probes that age out without a matching pong become EventProbeMissed
// in the GC loop.
type pendingProbe struct {
	sentAt       time.Time
	path         string // "direct" | "relay"
	peerNodePub  string
	peerDeviceID string
	// roundID is the per-probe-round identifier for direct-path probes.
	// Multiple candidate probes sent in the same probeAllPeers iteration
	// share a roundID so handlePong / gc can aggregate their outcomes
	// into one EventProbeRoundFinalized. Zero for relay-path probes
	// (1 round = 1 probe so the bookkeeping is trivial).
	roundID uint64
}

// directRoundState is the per-(peer, roundID) bookkeeping for one
// direct-path probe round. probeAllPeers initializes it lazily (the
// first send creates the entry; subsequent sends bump expected). After
// the last candidate is sent, finalizeRoundExpected sets finalized=true
// so handlePong / gc know they can emit EventProbeRoundFinalized as
// soon as succeeded+missed >= expected. Without that finalized flag a
// race-fast pong could trigger finalize before more candidates were
// sent in the same round.
type directRoundState struct {
	peerNodePub  string
	peerDeviceID string
	startedAt    time.Time // for age-based leak sweep
	expected     uint16
	succeeded    uint16
	missed       uint16
	finalized    bool // set by finalizeRoundExpected once all sends complete
}

// peerState is the disco-side per-peer view. Mirrors a subset of the
// reconciler's peerPathState — they're separate concerns, kept loosely
// coupled via the Event channel.
type peerState struct {
	deviceID string
	nodeKey  string // peer's std-base64 node public key (used in relay frame headers)
	// nodePub is the same NodeKey decoded to raw 32-byte form. Used
	// as the peer half of ECDH key agreement for sealed (AEAD)
	// disco frames (probe / pong / call_me_maybe). The base64 string
	// form is kept alongside because the relay session frame header
	// still routes by base64-NodeKeyID.
	nodePub     [wireframe.NodeKeySize]byte
	candidates  []string // wgnet endpoint syntax: "udp4:host:port"
	relayURL    string   // peer's HomeRelay URL; empty disables relay-path probing for this peer
	lastProbeAt time.Time
	// cmmHints are call_me_maybe-learned candidate addrs the peer
	// asked us to also probe. Each carries an expiresAt set by
	// handleCallMeMaybe to now+CMMHintTTL; probeAllPeers iterates
	// candidates ∪ liveCmmHints and lazily prunes expired entries.
	cmmHints []cmmHint
}

// cmmHint is one peer-supplied candidate addr with a TTL.
type cmmHint struct {
	addr      string // wgnet endpoint syntax: "udp4:host:port" / "udp6:[host]:port"
	expiresAt time.Time
}

type observeWaiter struct {
	port     int
	deadline time.Time
	resultCh chan netip.AddrPort // sends the observed_addr or zero on timeout
}

// New validates Config and prepares a Service. Run must be called to
// start the goroutines.
func New(cfg Config) (*Service, error) {
	if cfg.SelfDeviceID == "" {
		return nil, errors.New("disco: SelfDeviceID is required")
	}
	var zeroKey [wireframe.NodeKeySize]byte
	if cfg.SelfNodeKeyPriv == zeroKey || cfg.SelfNodeKeyPub == zeroKey {
		return nil, errors.New("disco: SelfNodeKeyPriv/Pub required")
	}
	if cfg.Bind == nil {
		return nil, errors.New("disco: Bind required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.STUNObserveLearning == 0 {
		cfg.STUNObserveLearning = defaultSTUNObserveLearning
	}
	if cfg.STUNObserveActive == 0 {
		cfg.STUNObserveActive = defaultSTUNObserveActive
	}
	if cfg.STUNObserveIdle == 0 {
		cfg.STUNObserveIdle = defaultSTUNObserveIdle
	}
	// Defensively clamp learning ≤ active so misconfiguration cannot
	// silently slow the warm-up below the active baseline.
	if cfg.STUNObserveLearning > cfg.STUNObserveActive {
		cfg.STUNObserveLearning = cfg.STUNObserveActive
	}
	if cfg.STUNTimeout == 0 {
		cfg.STUNTimeout = defaultSTUNTimeout
	}
	if cfg.ProbeReprobeActive == 0 {
		cfg.ProbeReprobeActive = defaultProbeReprobeActive
	}
	if cfg.ProbeWindow == 0 {
		cfg.ProbeWindow = defaultProbeWindow
	}
	if cfg.CMMHintTTL == 0 {
		cfg.CMMHintTTL = 2 * cfg.ProbeReprobeActive
	}
	if len(cfg.DiscoUDPPorts) == 0 {
		cfg.DiscoUDPPorts = defaultDiscoUDPPorts
	}
	return &Service{
		cfg:    cfg,
		logger: cfg.Logger,
		now:    cfg.Now,
		// 1024 chosen to absorb ~30 peers × ~8 events/cycle (RTT sample
		// + miss + pong-from-peer × 2 paths) over several probe cycles
		// without dropping. The original 64 caused the testnet
		// cold-start scenario to silently lose miss events for the
		// target peer when stale Spanner enrollments inflated peer
		// count, defeating both the miss-streak downgrade and the
		// safety-net Tick (which gets reset on every miss event).
		events:         make(chan Event, 1024),
		relayUpdate:    make(chan struct{}, 1),
		peers:          map[string]peerState{},
		pendingProbes:  map[string]pendingProbe{},
		pendingObserve: map[string]observeWaiter{},
		nonceCache:     map[string]time.Time{},
		rttEMA:         map[string]float64{},
		lastPongAt:     map[string]time.Time{},
		directRounds:   map[uint64]*directRoundState{},
	}, nil
}

// RTTSnapshot returns a deviceID → EMA-smoothed RTT (ms, rounded)
// snapshot of all peers this Service has received at least one pong
// from. Used by the Phase 7 router Selector as the LocalRTT
// tie-break — the agent's own observation of overlay round-trip to
// each peer. Returns nil when no samples have been recorded yet so
// the caller (router) can short-circuit the tie-break cheaply.
//
// Allocates a fresh map; safe to read without further locking.
func (s *Service) RTTSnapshot() map[string]uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.rttEMA) == 0 {
		return nil
	}
	out := make(map[string]uint32, len(s.rttEMA))
	for k, v := range s.rttEMA {
		if v < 0 {
			v = 0
		}
		out[k] = uint32(v + 0.5) // round to nearest ms
	}
	return out
}

// recordRTTSample folds one RTT measurement into the per-peer EMA.
// Caller must hold s.mu. Defined as a method so handlePong (which
// already holds mu around pendingProbes mutation) can update the EMA
// in the same critical section.
func (s *Service) recordRTTSample(deviceID string, rtt time.Duration) {
	if deviceID == "" || rtt <= 0 {
		return
	}
	rttMS := float64(rtt.Microseconds()) / 1000.0
	if prev, ok := s.rttEMA[deviceID]; ok {
		s.rttEMA[deviceID] = rttEMAAlpha*rttMS + (1-rttEMAAlpha)*prev
	} else {
		s.rttEMA[deviceID] = rttMS
	}
}

// recordPongReceived stamps lastPongAt[deviceID] to s.now(). Caller
// must hold s.mu. Called from handlePong alongside recordRTTSample
// only when the inbound pong matched an outstanding probe nonce —
// i.e. confirms two-way reachability over a path we initiated, not a
// replayed or out-of-band frame. Empty deviceID is a no-op.
func (s *Service) recordPongReceived(deviceID string) {
	if deviceID == "" {
		return
	}
	s.lastPongAt[deviceID] = s.now()
}

// ReachableSnapshot returns a deviceID → recent-pong-presence map for
// the Phase 8 Selector to consult as a hard-exclusion signal.
//
//   - A deviceID present with value=true has produced a signed,
//     nonce-matched pong within [now-freshness, now].
//   - A deviceID present with value=false has pong'd at some point in
//     the past but its most recent observation is older than the
//     window — the Selector treats this as a hard exclusion (peer is
//     no longer reachable over the WG/relay path we last saw).
//   - A deviceID we have NEVER received a pong from is absent from
//     the result. The Selector treats absence as "unknown, default
//     trust" so freshly enrolled peers aren't excluded before their
//     first probe round completes.
//
// Returns nil when no peer has ever pong'd, so a freshly started
// agent's Selector sees zero exclusions (= legacy behaviour).
//
// Allocates a fresh map; safe to read without further locking.
func (s *Service) ReachableSnapshot(now time.Time, freshness time.Duration) map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.lastPongAt) == 0 {
		return nil
	}
	cutoff := now.Add(-freshness)
	out := make(map[string]bool, len(s.lastPongAt))
	for k, t := range s.lastPongAt {
		out[k] = !t.Before(cutoff)
	}
	return out
}

// Events returns the read-only event stream. The reconciler subscribes
// here to decide when to flip a peer's endpoint.
func (s *Service) Events() <-chan Event { return s.events }

// UpdateRelays replaces the list of known relay URLs the STUN observer
// targets. Called by the agent main loop on every NetworkMap update.
//
// Each URL is the WSS base (e.g., "wss://relay.example.com:443/relay/v1/connect").
// The observer extracts the host and probes (host, port) for each port
// in cfg.DiscoUDPPorts.
func (s *Service) UpdateRelays(urls []string) {
	next := append([]string(nil), urls...)
	s.mu.Lock()
	changed := !slices.Equal(s.relayURLs, next)
	s.relayURLs = next
	s.mu.Unlock()
	if changed {
		// Eager first-probe: wake runObserveLoop now rather than letting
		// it sleep out the current observe interval before noticing the
		// new relay set. Non-blocking send onto the cap-1 channel — if a
		// wake is already pending, this coalesces into it.
		select {
		case s.relayUpdate <- struct{}{}:
		default:
		}
	}
}

// UpdatePeers replaces the per-peer state. Called by the agent main
// loop on every NetworkMap update. peers is a map from
// peerNodePublicKey (std-base64) to its (deviceID, NodePub raw bytes,
// candidates, relay) tuple.
func (s *Service) UpdatePeers(peers map[string]PeerSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]peerState, len(peers))
	for k, p := range peers {
		prev := s.peers[k]
		out[k] = peerState{
			deviceID:    p.DeviceID,
			nodeKey:     k,
			nodePub:     p.NodePub,
			candidates:  append([]string(nil), p.Candidates...),
			relayURL:    p.RelayURL,
			lastProbeAt: prev.lastProbeAt,                         // preserve probe schedule across updates
			cmmHints:    append([]cmmHint(nil), prev.cmmHints...), // preserve receiver-side CMM hints
		}
	}
	s.peers = out
}

// PeerSnapshot is the per-peer input to UpdatePeers.
type PeerSnapshot struct {
	DeviceID string
	// NodePub is the peer's curve25519 public key (32 B raw bytes).
	// Used as the receiver half of ECDH when sealing outbound peer↔peer
	// frames and as the cross-check key on inbound (sealed) frames —
	// AEAD authentication proves the sender owns the matching private,
	// and we verify that key belongs to the peer claiming SrcDeviceID.
	NodePub    [wireframe.NodeKeySize]byte
	Candidates []string // wgnet endpoint syntax: "udp4:host:port" / "udp6:[host]:port"
	// RelayURL is the peer's HomeRelay URL. When non-empty, the prober
	// also sends one probe per cycle through that relay so the agent
	// can measure end-to-end relay RTT (used by the reconciler's
	// asymmetric ratio for direct/relay path selection). Empty
	// disables relay probing for this peer.
	RelayURL string
}

// ObservedAddr returns the most recent self observed addr, or the zero
// value if none has been received yet.
func (s *Service) ObservedAddr() netip.AddrPort {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.observed
}

// LastObservedV6 returns the most recent v6 sample the STUN observer
// has seen since process start, zero if none. Use when "the v6
// substrate is reachable" needs to be asserted independently of the
// most recent observation (which may be v4 due to v6 round-trip
// transient timeouts or v4 winning the bestObs race in a given round).
func (s *Service) LastObservedV6() netip.AddrPort {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastObservedV6
}

// FirstObservedV6At returns the wall-clock time of the agent's first
// v6 STUN observation since process start, zero if none. Stamped once
// by observeOnce and never rewritten. Used by Status to surface a
// per-agent v6-convergence latency to Cloud Logging, so the testnet
// verifier can build the convergence-time histogram across runs.
func (s *Service) FirstObservedV6At() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.firstObservedV6At
}

// STUNCounters returns lifetime totals of STUN echo sends and
// responses, split by destination / source family. Lock-free read of
// the atomic counters. The verifier compares attempts vs responses
// per-family to attribute a missing v6 observed_addr to either
// "agent never sent v6 STUN" (low attempts) or "agent sent but never
// received reply" (attempts ≫ responses, i.e. GCE return-path /
// BGP-asymmetric flake).
func (s *Service) STUNCounters() (attemptsV4, attemptsV6, responsesV4, responsesV6 uint64) {
	return s.stunAttemptsV4.Load(),
		s.stunAttemptsV6.Load(),
		s.stunResponsesV4.Load(),
		s.stunResponsesV6.Load()
}

// NATType returns the most recently detected NAT type.
func (s *Service) NATType() NATType {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.natType
}

// Run starts the inbound dispatcher and the STUN observer / probe
// goroutines. Returns nil on graceful ctx cancellation.
func (s *Service) Run(ctx context.Context) error {
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		s.runInbound(ctx)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		s.runObserveLoop(ctx)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		s.runProbeLoop(ctx)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		s.runGCLoop(ctx)
	}()

	<-ctx.Done()
	wg.Wait()
	return nil
}

// emit sends an event non-blockingly. Drops on overflow rather than
// stalling the disco hot path.
func (s *Service) emit(ev Event) {
	select {
	case s.events <- ev:
	default:
		s.logger.Warn("disco: event channel full; dropping", "event_type", fmt.Sprintf("%T", ev))
	}
}

func freshNonce() [wireframe.NonceSize]byte {
	var n [wireframe.NonceSize]byte
	_, _ = rand.Read(n[:])
	return n
}

// extractHostFromRelayURL returns the host part of a WSS URL, stripping
// scheme and path so the disco UDP listener can be addressed at
// (host, discoPort).
func extractHostFromRelayURL(u string) (string, error) {
	parsed, err := url.Parse(u)
	if err != nil {
		return "", err
	}
	h := parsed.Hostname()
	if h == "" {
		return "", fmt.Errorf("relay url has no hostname: %q", u)
	}
	return h, nil
}

// makeUDPDst formats an "udp4:host:port" or "udp6:[host]:port" string
// suitable for wgnet's MultiplexBind.SendDisco. IPv6 addresses get
// bracket wrapping.
func makeUDPDst(host string, port int) string {
	if strings.Contains(host, ":") {
		// IPv6 literal
		return "udp6:[" + host + "]:" + strconv.Itoa(port)
	}
	return "udp4:" + host + ":" + strconv.Itoa(port)
}

// parseUDPEndpoint reverses makeUDPDst / makeUDPDstFromAddrPort,
// turning "udp4:host:port" / "udp6:[host]:port" back into a
// netip.AddrPort. Returns ok=false for unparseable inputs (relay:
// candidates, malformed strings, etc.).
func parseUDPEndpoint(s string) (netip.AddrPort, bool) {
	var rest string
	switch {
	case strings.HasPrefix(s, "udp4:"):
		rest = s[len("udp4:"):]
	case strings.HasPrefix(s, "udp6:"):
		rest = s[len("udp6:"):]
	default:
		return netip.AddrPort{}, false
	}
	ap, err := netip.ParseAddrPort(rest)
	if err != nil {
		return netip.AddrPort{}, false
	}
	return ap, true
}

// KnownAndHintedFor returns the union of the peer's NetworkMap-
// published candidates and live (non-expired) call_me_maybe-learned
// hints, parsed back into netip.AddrPort form. Returns nil when the
// peer is unknown or has no endpoints. Caller must NOT hold s.mu.
//
// Defined as a testharness-oriented accessor: the scenario dispatcher
// snapshots a peer's full endpoint set at Apply time so its iptables
// block covers every addr the prober might try, not only the
// candidates surfaced via NetworkMap. Production paths consume the
// same data structures directly under s.mu and do not call this.
func (s *Service) KnownAndHintedFor(peerNodePub string) []netip.AddrPort {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.peers[peerNodePub]
	if !ok {
		return nil
	}
	now := s.now()
	// liveCmmHints rewrites p.cmmHints in place to drop expired
	// entries — write the trimmed state back to the map.
	live := liveCmmHints(&p, now)
	s.peers[peerNodePub] = p
	seen := make(map[netip.AddrPort]bool, len(p.candidates)+len(live))
	out := make([]netip.AddrPort, 0, len(p.candidates)+len(live))
	add := func(raw string) {
		ap, ok := parseUDPEndpoint(raw)
		if !ok || seen[ap] {
			return
		}
		seen[ap] = true
		out = append(out, ap)
	}
	for _, c := range p.candidates {
		add(c)
	}
	for _, h := range live {
		add(h)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ClearHintsFor drops every call_me_maybe-learned hint for the given
// peer. Idempotent; unknown peers are a no-op.
//
// The testharness scenario dispatcher calls this on Apply so the
// prober stops probing stale hint addrs that the upcoming iptables
// block hasn't yet caught. Production code does not call this.
func (s *Service) ClearHintsFor(peerNodePub string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.peers[peerNodePub]
	if !ok {
		return
	}
	p.cmmHints = nil
	s.peers[peerNodePub] = p
}

// OnCallMeMaybe registers a callback fired after each verified
// call_me_maybe frame from any peer is processed. The callback runs
// off the inbound goroutine but outside s.mu, so callees may safely
// re-enter Service methods. Callbacks must not block — signal a
// separate goroutine and return promptly.
//
// Used by the testharness scenario dispatcher to incrementally
// refresh iptables rules when new endpoint candidates arrive after
// the scenario was Applied. Production code does not register here.
// nil fn is silently dropped.
func (s *Service) OnCallMeMaybe(fn func(peerNodePub, peerDeviceID string)) {
	if fn == nil {
		return
	}
	s.mu.Lock()
	s.cmmCallbacks = append(s.cmmCallbacks, fn)
	s.mu.Unlock()
}
