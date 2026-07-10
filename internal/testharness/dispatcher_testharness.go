//go:build testharness

package testharness

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// applyTimeout bounds a single Apply or Revert call. Real iptables
// invocations are sub-second; the budget is generous to absorb a slow
// fork+exec under contention without flaking.
const applyTimeout = 30 * time.Second

// DiscoEndpointSource is the testharness-only subset of disco state the
// dispatcher consults at Apply time + subscribes to for CMM-driven
// re-Apply. The agent's *disco.Service satisfies it; tests pass nil to
// keep the NetworkMap-only behaviour.
//
// KnownAndHintedFor returns the union of NetworkMap-published peer
// candidates and live call_me_maybe-learned hints — i.e., every UDP
// addr the prober might try this cycle for the given peer.
// ClearHintsFor drops the peer's CMM hint list, so a subsequent probe
// cycle won't probe a hint addr the dispatcher's iptables block hasn't
// yet caught. OnCallMeMaybe registers a hook fired after each verified
// CMM frame so the dispatcher can incrementally extend the block when
// new endpoints arrive after Apply.
type DiscoEndpointSource interface {
	KnownAndHintedFor(peerNodePub string) []netip.AddrPort
	ClearHintsFor(peerNodePub string)
	OnCallMeMaybe(fn func(peerNodePub, peerDeviceID string))
}

// activeDispatcher is the testharness-build Dispatcher. It holds at
// most one in-flight scenario per agent at a time — CP-side enforces
// network-wide "one active scenario" via /v1/test/scenario, so per-
// agent uniqueness is sufficient.
type activeDispatcher struct {
	log      Logger
	rep      Reporter
	selfID   string
	registry map[string]Scenario
	disco    DiscoEndpointSource // optional; nil falls back to NM-only

	mu      sync.Mutex // guards current; held by the worker during iptables ops
	current *currentApplication

	// latest holds the most recent NetworkMap handed to Apply. It is a
	// lock-free slot (Apply stores, the worker loads) so Apply never
	// blocks on a.mu while the worker is mid-iptables. Latest-wins:
	// intermediate maps coalesce; the worker always acts on the newest.
	// nil until the first Apply.
	latest atomic.Pointer[signer.NetworkMap]

	// Unified worker plumbing. mapTrigger and cmmTrigger are 1-buffered
	// coalescing signal channels: Apply (resp. the disco call_me_maybe
	// callback) drops a token non-blockingly; the single worker goroutine
	// consumes either and runs the corresponding iptables work under
	// a.mu. Decoupling Apply from the agent's network-map stream loop
	// this way keeps a slow iptables op from back-pressuring the stream
	// reader (issue #303), and putting both map-apply and CMM-reapply on
	// ONE goroutine removes their a.mu contention. stop is closed by Stop
	// to retire the worker; workerDone is closed by the worker on exit so
	// Stop can wait for an in-flight op to finish before reverting.
	// cmmTrigger only ever receives when disco != nil (the callback is
	// registered only then).
	mapTrigger chan struct{}
	cmmTrigger chan struct{}
	stop       chan struct{}
	workerDone chan struct{}
	stopOnce   sync.Once
}

type currentApplication struct {
	scenario    Scenario
	peer        string // DeviceID — matches against CMM callback's peerDeviceID
	peerNodePub string // std-base64 NodePublicKey — for disco.ClearHintsFor / KnownAndHintedFor
	nonce       int64
	direction   string
	overlayIP   string
	appliedIPs  []string // sorted, deduped — captured at Apply time + grown on CMM re-Apply
}

// NewActive constructs the testharness-only dispatcher. registry may be
// nil — unknown scenario_id values are reported as
// StateUnknownScenario and otherwise no-op. disco may be nil — when
// passed the dispatcher snapshots the peer's disco-known endpoint
// union at Apply time and re-Applies the scenario on each subsequent
// call_me_maybe frame so the iptables block stays ahead of the prober.
func NewActive(log Logger, rep Reporter, selfDeviceID string, reg map[string]Scenario, disco DiscoEndpointSource) Dispatcher {
	if reg == nil {
		reg = map[string]Scenario{}
	}
	a := &activeDispatcher{
		log:        log,
		rep:        rep,
		selfID:     selfDeviceID,
		registry:   reg,
		disco:      disco,
		mapTrigger: make(chan struct{}, 1),
		cmmTrigger: make(chan struct{}, 1),
		stop:       make(chan struct{}),
		workerDone: make(chan struct{}),
	}
	if disco != nil {
		// Register exactly once for the dispatcher's lifetime. The
		// callback is a tiny no-allocation filter: if no scenario is
		// active, or the CMM is for a different peer, return without
		// signalling. Production agents never run with the testharness
		// build tag, so this hook is exercised only on testnet VMs.
		disco.OnCallMeMaybe(func(_, peerDeviceID string) {
			a.mu.Lock()
			active := a.current != nil && a.current.peer == peerDeviceID
			a.mu.Unlock()
			if !active {
				return
			}
			select {
			case a.cmmTrigger <- struct{}{}:
			default:
				// worker has a pending token; coalesce.
			}
		})
	}
	go a.worker()
	return a
}

// Apply records the latest NetworkMap and wakes the worker. It is
// deliberately non-blocking: it never takes a.mu (which the worker may
// hold for up to applyTimeout during iptables) and performs no I/O, so
// the agent's network-map stream consumer is never back-pressured by a
// slow scenario apply (issue #303). The decision logic + iptables work
// runs on the worker against the latest map. ctx is unused — kept for
// the Dispatcher interface; the worker uses its own background context
// so a stream reconnect can't abort an in-flight op.
//
// Callers must not mutate nm after passing it here — the worker reads it
// concurrently. The CP emits a fresh frame per change, so this holds.
func (a *activeDispatcher) Apply(_ context.Context, nm *signer.NetworkMap) error {
	a.latest.Store(nm)
	select {
	case a.mapTrigger <- struct{}{}:
	default:
		// worker has a pending token; it will read the latest map.
	}
	return nil
}

// applyMap runs the scenario state machine for one map. It executes only
// on the worker goroutine; ctx is the worker's background context,
// bounded per scenario op by applyTimeout inside applyLocked /
// revertLocked.
func (a *activeDispatcher) applyMap(ctx context.Context, nm *signer.NetworkMap) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	want := nm.ActiveTestScenario

	if want == nil {
		if a.current == nil {
			a.log.Debug("dispatcher: no-op, no scenario active and none wanted")
			return nil
		}
		return a.revertLocked(ctx)
	}

	a.log.Debug("dispatcher: worker applying map",
		"scenario", want.ScenarioID, "peer", want.PeerDeviceID, "nonce", want.ExpectedNonce)

	params, peerNodePub := a.resolveWithDisco(nm, want)
	if a.current != nil &&
		a.current.scenario.ID() == want.ScenarioID &&
		a.current.peer == want.PeerDeviceID &&
		a.current.nonce == want.ExpectedNonce &&
		ipSetEqual(a.current.appliedIPs, params.PeerEndpoints) {
		a.log.Debug("dispatcher: no-op, scenario already applied (idempotent)",
			"scenario", want.ScenarioID, "peer", want.PeerDeviceID, "nonce", want.ExpectedNonce)
		return nil
	}

	if a.current != nil {
		if err := a.revertLocked(ctx); err != nil {
			a.log.Warn("dispatcher: revert before reapply failed", "err", err)
		}
	}
	return a.applyLocked(ctx, want, params, peerNodePub)
}

func (a *activeDispatcher) applyLocked(ctx context.Context, want *signer.ActiveTestScenario, params ScenarioParams, peerNodePub string) error {
	sc, ok := a.registry[want.ScenarioID]
	if !ok {
		a.rep.ReportScenario(StateUnknownScenario, want.ScenarioID, want.PeerDeviceID, want.ExpectedNonce, "")
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, applyTimeout)
	defer cancel()
	if err := sc.Apply(cctx, params); err != nil {
		a.rep.ReportScenario(StateApplyError, want.ScenarioID, want.PeerDeviceID, want.ExpectedNonce, err.Error())
		return fmt.Errorf("apply %s: %w", want.ScenarioID, err)
	}
	a.current = &currentApplication{
		scenario:    sc,
		peer:        want.PeerDeviceID,
		peerNodePub: peerNodePub,
		nonce:       want.ExpectedNonce,
		direction:   params.Direction,
		overlayIP:   params.PeerOverlayIP,
		appliedIPs:  append([]string(nil), params.PeerEndpoints...),
	}
	// Drop any disco-learned CMM hints for this peer so the prober
	// doesn't keep probing stale hint addrs the iptables block just
	// caught. Fresh CMM frames will refill via the OnCallMeMaybe
	// callback and trigger an incremental re-Apply.
	if a.disco != nil && peerNodePub != "" {
		a.disco.ClearHintsFor(peerNodePub)
	}
	a.rep.ReportScenario(StateApplied, want.ScenarioID, want.PeerDeviceID, want.ExpectedNonce, "")
	return nil
}

// reapplyForCMM is invoked off the drain goroutine each time a
// call_me_maybe frame for the active scenario's peer arrives. It
// recomputes the peer's known endpoint union (NetworkMap snapshot ∪
// fresh disco hints) and re-invokes scenario.Apply with the new set.
// scenario.Apply is idempotent at the iptables layer (BlockUDPDirect
// uses -C precheck and InstallChain is structurally idempotent), so
// the re-Apply only emits -A appends for genuinely new addrs.
//
// The worker goroutine guarantees this never runs concurrently with
// itself or with applyMap; a.mu serialises it against Stop.
func (a *activeDispatcher) reapplyForCMM(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	cur := a.current
	if cur == nil || a.disco == nil || cur.peerNodePub == "" {
		return
	}
	hinted := a.disco.KnownAndHintedFor(cur.peerNodePub)
	if len(hinted) == 0 {
		return
	}
	seen := make(map[string]bool, len(cur.appliedIPs)+len(hinted))
	for _, ip := range cur.appliedIPs {
		seen[ip] = true
	}
	var grew bool
	updated := append([]string(nil), cur.appliedIPs...)
	for _, ap := range hinted {
		ip := ap.Addr().String()
		if seen[ip] {
			continue
		}
		// BlockUDPDirect's IPv6 skip is silent at the iptables layer;
		// here we still record the addr in appliedIPs so future CMM
		// frames covering the same v6 don't keep triggering re-Apply.
		seen[ip] = true
		updated = append(updated, ip)
		grew = true
	}
	if !grew {
		return
	}
	sort.Strings(updated)
	params := ScenarioParams{
		PeerDeviceID:  cur.peer,
		PeerOverlayIP: cur.overlayIP,
		PeerEndpoints: updated,
		Direction:     cur.direction,
		Nonce:         cur.nonce,
	}
	cctx, cancel := context.WithTimeout(ctx, applyTimeout)
	defer cancel()
	if err := cur.scenario.Apply(cctx, params); err != nil {
		a.log.Warn("dispatcher: CMM-driven re-Apply failed", "scenario", cur.scenario.ID(), "peer", cur.peer, "err", err)
		return
	}
	cur.appliedIPs = updated
}

// worker is the single goroutine that performs all iptables work for
// this dispatcher. It serialises map-apply (mapTrigger) and CMM-driven
// re-apply (cmmTrigger) so they never contend a.mu, and runs under a
// background context so a network-map stream reconnect can't abort an op
// mid-flight. Exits when stop is closed (from Stop()), closing
// workerDone so Stop can wait for any in-flight op to finish.
func (a *activeDispatcher) worker() {
	defer close(a.workerDone)
	bg := context.Background()
	for {
		select {
		case <-a.stop:
			return
		case <-a.mapTrigger:
			if nm := a.latest.Load(); nm != nil {
				if err := a.applyMap(bg, nm); err != nil {
					a.log.Warn("dispatcher: applyMap failed", "err", err)
				}
			}
		case <-a.cmmTrigger:
			a.reapplyForCMM(bg)
		}
	}
}

func (a *activeDispatcher) revertLocked(ctx context.Context) error {
	cur := a.current
	// Clear current BEFORE calling scenario.Revert so any in-flight
	// CMM callback that lands during the Revert -F/-X iptables window
	// no-ops (drain checks a.current under the same lock). Without
	// this re-ordering a late CMM token could rebuild the chain after
	// FlushChain.
	a.current = nil
	cctx, cancel := context.WithTimeout(ctx, applyTimeout)
	defer cancel()
	err := cur.scenario.Revert(cctx)
	state := StateReverted
	msg := ""
	if err != nil {
		state = StateRevertError
		msg = err.Error()
	}
	a.rep.ReportScenario(state, cur.scenario.ID(), cur.peer, cur.nonce, msg)
	return err
}

func (a *activeDispatcher) Stop(ctx context.Context) error {
	// Retire the worker. stopOnce makes this safe under repeated Stop.
	a.stopOnce.Do(func() { close(a.stop) })
	// Wait for the worker to exit WITHOUT holding a.mu: a worker that is
	// mid-applyMap / reapplyForCMM holds a.mu and needs it to finish, so
	// taking the lock here would deadlock. Once workerDone is closed the
	// worker has provably exited and we are the only goroutine that can
	// touch a.current.
	<-a.workerDone
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.current == nil {
		return nil
	}
	return a.revertLocked(ctx)
}

// resolveWithDisco builds the ScenarioParams the scenario will receive
// at Apply time. base.PeerEndpoints starts as the resolved NetworkMap
// snapshot; when disco is wired the dispatcher additionally merges the
// peer's current disco-known + CMM-hinted UDP endpoints.
//
// Returns (params, peerNodePub) so applyLocked can stash the node
// public key on current for later disco lookups without re-walking
// nm.Peers.
func (a *activeDispatcher) resolveWithDisco(nm *signer.NetworkMap, want *signer.ActiveTestScenario) (ScenarioParams, string) {
	base := resolvePeer(nm, want)
	p := findPeerByDeviceID(nm.Peers, want.PeerDeviceID)
	if p == nil {
		return base, ""
	}
	if a.disco == nil {
		return base, p.NodePublicKey
	}
	hinted := a.disco.KnownAndHintedFor(p.NodePublicKey)
	if len(hinted) == 0 {
		return base, p.NodePublicKey
	}
	seen := map[string]bool{}
	for _, ip := range base.PeerEndpoints {
		seen[ip] = true
	}
	for _, ap := range hinted {
		ip := ap.Addr().String()
		if seen[ip] {
			continue
		}
		seen[ip] = true
		base.PeerEndpoints = append(base.PeerEndpoints, ip)
	}
	sort.Strings(base.PeerEndpoints)
	return base, p.NodePublicKey
}

// resolvePeer extracts the peer's address set from the Network Map
// for the dispatcher's idempotency check + the scenario's iptables
// targets. relay: candidates are excluded — they are not
// data-plane UDP endpoints.
func resolvePeer(nm *signer.NetworkMap, want *signer.ActiveTestScenario) ScenarioParams {
	base := ScenarioParams{
		PeerDeviceID: want.PeerDeviceID,
		Direction:    want.Direction,
		Nonce:        want.ExpectedNonce,
	}
	if nm == nil {
		return base
	}
	p := findPeerByDeviceID(nm.Peers, want.PeerDeviceID)
	if p == nil {
		return base
	}
	base.PeerOverlayIP = p.OverlayIP
	seen := map[string]bool{}
	var ips []string
	for _, e := range p.Endpoints {
		host := parseHostFromAddr(e.Addr)
		if host == "" || seen[host] {
			continue
		}
		seen[host] = true
		ips = append(ips, host)
	}
	sort.Strings(ips)
	base.PeerEndpoints = ips
	return base
}

func findPeerByDeviceID(peers []signer.NetworkMapPeer, id string) *signer.NetworkMapPeer {
	for i := range peers {
		if peers[i].DeviceID == id {
			return &peers[i]
		}
	}
	return nil
}

// parseHostFromAddr extracts the host portion of EndpointCandidate.Addr.
// Returns "" for relay: entries or unparseable inputs.
//
//	"udp4:1.2.3.4:51820"        -> "1.2.3.4"
//	"udp6:[2001:db8::1]:51820"  -> "2001:db8::1"
//	"relay:..."                 -> ""
//	""                          -> ""
func parseHostFromAddr(addr string) string {
	var rest string
	switch {
	case strings.HasPrefix(addr, "udp4:"):
		rest = addr[len("udp4:"):]
	case strings.HasPrefix(addr, "udp6:"):
		rest = addr[len("udp6:"):]
	default:
		return ""
	}
	host, _, err := net.SplitHostPort(rest)
	if err != nil {
		return ""
	}
	return host
}

func ipSetEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
