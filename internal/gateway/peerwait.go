package gateway

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/waired-ai/waired-agent/internal/router"
)

// The peer leg's pre-first-byte wait (waired-agent#1040).
//
// #757 bounded it with a flat deadline, and that number turned out to be
// measuring the wrong thing. What a caller waits out before a peer's first
// byte is the peer PREFILLING the prompt, which is a property of that
// machine's speed and of how much context the client sent — not of whether
// the peer is reachable. A Claude Code first turn is ~30k tokens, and on the
// 0.0.3-rc4 review fleet three of four peers needed 164 s, 264 s and (on a
// card that could not hold it) forever, against a 60 s budget. So a peer that
// was working correctly and would have answered in about three minutes was
// abandoned, and the turn went to the real Anthropic API with the mesh idle.
//
// Owner ruling, 2026-08-28 (waired-ai/waired-agent#1040): cutting off a
// request that is being worked on, purely because it is slow, is the wrong
// question. Ask the peer instead.
//
// The peer can answer. Its /waired/v1/inference/healthz is a direct overlay
// call — not the signed network map — and a request this device dispatched
// holds one of that peer's admission slots for as long as it is being served,
// engine start and model load included (internal/inference/server.go, §8.2).
// So "engine ready and at least one slot in use" is the peer saying it is
// working, and it is the same probe the pre-dispatch selection already makes.
//
// What remains bounded is the peer's SILENCE, not its slowness: a peer that
// says it is working nothing, a peer that stops answering, and an absolute
// ceiling for the case both of those are wrong.

const (
	// peerLivenessInterval paces the health checks once the grace period
	// has passed. It is state.DefaultStaleAfter's figure — the age at
	// which this mesh stops believing a fact about another device — spelled
	// here rather than imported so this package keeps depending only on
	// router. A peer re-observes its own engine three times faster than
	// that (state.HeartbeatInterval, 5 s), so every check reads a fact no
	// older than one of its heartbeats.
	//
	// Each check is one overlay round trip against a handler that reads
	// cached state, so at this spacing a three-minute prefill costs about
	// a dozen of them.
	peerLivenessInterval = 15 * time.Second

	// peerLivenessMisses is how many consecutive unanswered checks end the
	// wait. More than one because a probe that does not come back is not
	// evidence about the peer: it can equally be a packet lost on the way,
	// a relay hiccup or a peer mid-handshake — the same reasoning that gave
	// the selection probe its retries (owner ruling on waired-agent#624).
	peerLivenessMisses = 2
)

// peerLiveness is the watch's plan for one peer leg. Nil on every leg that
// does not have one; see waitPolicyFor.
type peerLiveness struct {
	// PeerID is the mesh DeviceID to ask about. It is the functional
	// identifier, never a display one: it addresses the peer adapter.
	PeerID string
	// Grace is how long to wait before asking at all. It is the class's
	// configured TTFB budget, so an ordinary turn that answers inside the
	// old deadline is never probed and behaves exactly as it did.
	Grace time.Duration
	// Ceiling bounds the whole wait, including Grace. It exists for one
	// case only — a peer whose liveness claim is wrong — because the two
	// ways a wait legitimately ends are both caught by asking: the peer
	// saying it stopped, and the peer going quiet. That is why it is
	// longer than the local leg's figure rather than equal to it: that
	// one is a pure timeout with nothing behind it, and has to be
	// conservative. See agentconfig.ClaudePeerWaitCeilingMs.
	Ceiling time.Duration
	// Interval paces the checks after Grace; 0 means peerLivenessInterval.
	Interval time.Duration
}

// peerDeviceIDOf returns the mesh DeviceID a remote selection addresses,
// and false for a local or external one.
func peerDeviceIDOf(sel router.Selection) (string, bool) {
	id := strings.TrimPrefix(sel.Runtime, remoteRuntimePrefix)
	if id == sel.Runtime || id == "" {
		return "", false
	}
	return id, true
}

// peerHealth asks one peer what it is doing right now.
//
// Deps.PeerHealth is the test seam and is nil in production, where this
// composes the same two pieces the selection probe uses — so the code a test
// bypasses is one lookup, not the probe itself. A lookup failure is reported
// as a transport error rather than as a verdict about the peer: it is this
// device that could not ask.
func (h *HandlerSet) peerHealth(ctx context.Context, deviceID string) router.ProbeResult {
	if h.deps.PeerHealth != nil {
		return h.deps.PeerHealth(ctx, deviceID)
	}
	rt, baseURL, err := h.peerProbeLookup(deviceID)
	if err != nil {
		return router.ProbeResult{Outcome: router.ProbeTransportError, Err: err}
	}
	return router.ProbeHealth(ctx, rt, baseURL)
}

// peerFacts asks this device what it already knows about a peer. Nil dep
// — every listener but the Claude intercept — reports nothing known,
// which is what leaves every reader's behaviour unchanged.
func (h *HandlerSet) peerFacts(deviceID string) PeerFacts {
	if h.deps.PeerFacts == nil || deviceID == "" {
		return PeerFacts{}
	}
	return h.deps.PeerFacts(deviceID)
}

// peerHealthWithin bounds one check. Without a deadline of its own a check
// that never comes back holds the loop, and the loop is where every other
// deadline is evaluated — so a peer whose /healthz hangs would be waited on
// past the ceiling as well, which is the one failure the ceiling exists for.
// The bound is the polling interval: a check still outstanding when the next
// one is due has already told us what a late answer is worth.
func (h *HandlerSet) peerHealthWithin(ctx context.Context, deviceID string, d time.Duration) router.ProbeResult {
	if d <= 0 {
		return h.peerHealth(ctx, deviceID)
	}
	probeCtx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return h.peerHealth(probeCtx, deviceID)
}

// peerWorkVerdict is what one health check told us about the peer that is
// holding our request.
type peerWorkVerdict int

const (
	// peerWorking: the peer answered, its engine is up and answering, and
	// at least one admission slot is in use — ours, unless it lost the
	// request, and the two are indistinguishable from here by design:
	// either way that peer is doing inference work right now.
	peerWorking peerWorkVerdict = iota
	// peerIdle: the peer answered and is working on nothing, its engine is
	// down, or this device can see that its engine has stopped answering
	// while a slot stays occupied. Our request is not being served and no
	// further waiting will produce it.
	peerIdle
	// peerSilent: the check did not come back at all. One is not evidence.
	peerSilent
	// peerUnknowable: the question cannot be put to this peer — it has no
	// /healthz (an agent predating Phase 8), or it rejected the signature
	// envelope. Neither says anything about whether it is working, so the
	// flat budget stands.
	peerUnknowable
)

// classifyPeerWork reads one health check, plus what this device already
// knows about that peer's engine, and says what the wait learned.
//
// engineKnown/engineLive is Deps.PeerFacts's answer — the bit the peer
// publishes to the whole mesh from a LIVE call to its engine, and the one
// selection already refuses on. It is here because /healthz cannot answer
// the question this function asks. `engine_ready` is the serving adapter's
// cached state, and both adapters re-observe the engine only when its
// process exits (internal/runtime/{ollama,vllm}.go, superviseChild), so an
// engine that is running and answering nothing — frozen mid-prefill, a
// wedged runner — stays latched ready for as long as the daemon lives.
// A slot is occupied, `engine_ready` is true, and the peer is not working:
// that combination held a turn for the full 30-minute ceiling with zero
// bytes and zero log lines (waired-agent#1220).
//
// Occupancy is still all the SLOT says. What the extra bit adds is the
// other half of the conjunction the peerWorking doc claims — that there is
// an engine behind the slot — and it adds it from the observer that has
// been right about this all along.
//
// engineKnown false is not evidence: no dep, a peer absent from the
// snapshot, a stale frame. The verdict is then exactly what it was before
// this argument existed.
func classifyPeerWork(res router.ProbeResult, engineLive, engineKnown bool) peerWorkVerdict {
	switch res.Outcome {
	case router.ProbeOK:
		if engineKnown && !engineLive {
			// A slot is occupied and the peer says its engine is up, but
			// this device can see that engine is not answering. Occupancy
			// is not work.
			return peerIdle
		}
		if res.Status.EngineReady && res.Status.CapacityUsed > 0 {
			return peerWorking
		}
		return peerIdle
	case router.ProbeLegacyPeer:
		return peerUnknowable
	case router.ProbeAuthError:
		// The path is usually up and the envelope was refused — clock skew,
		// a replayed nonce, a missing identity (see router.ProbeOutcome).
		// That is this device failing to ask, not the peer failing to work,
		// and calling it "the peer vanished" would name the wrong machine in
		// the reroute notice.
		return peerUnknowable
	default:
		return peerSilent
	}
}

// watchPeerWhileItWorks runs the wait for one peer leg and reports how it
// ended: an empty reason means the watch was told to stop (the peer answered,
// or the caller went away), and anything else is a pre-commit abort reason
// with the time that had elapsed when it was decided.
//
// It is written as a plain loop over an injected clock-free interface — the
// probe function and the sleep are the only two things it does — so a test
// drives it with a millisecond Interval and a scripted peer rather than by
// waiting out real minutes.
func (h *HandlerSet) watchPeerWhileItWorks(ctx context.Context, lv peerLiveness, started time.Time) (reason string, waited time.Duration) {
	interval := lv.Interval
	if interval <= 0 {
		interval = peerLivenessInterval
	}
	if !sleepUntil(ctx, lv.Grace) {
		return "", 0
	}
	misses := 0
	for {
		if lv.Ceiling > 0 && time.Since(started) >= lv.Ceiling {
			return LocalErrorPeerTTFBTimeout, time.Since(started)
		}
		known := h.peerFacts(lv.PeerID)
		switch classifyPeerWork(h.peerHealthWithin(ctx, lv.PeerID, interval), known.EngineLive, known.Known) {
		case peerWorking:
			misses = 0
		case peerIdle:
			// The peer is up and is not working on anything. Whatever
			// happened to this request happened on that machine, and the
			// caller is waiting on a turn nobody is producing.
			return LocalErrorPeerStoppedServing, time.Since(started)
		case peerSilent:
			misses++
			if misses >= peerLivenessMisses {
				return LocalErrorPeerUnreachable, time.Since(started)
			}
		case peerUnknowable:
			// Nothing to learn from this peer, so the wait falls back to
			// exactly what it was before there was anything to ask: the
			// flat budget, which is the grace period that just elapsed.
			return LocalErrorPeerTTFBTimeout, time.Since(started)
		}
		if ctx.Err() != nil {
			return "", 0
		}
		nap := interval
		if lv.Ceiling > 0 {
			if left := lv.Ceiling - time.Since(started); left < nap {
				nap = left
			}
		}
		if !sleepUntil(ctx, nap) {
			return "", 0
		}
	}
}

// sleepUntil waits d, and reports false when ctx ended first (or d was
// non-positive, which is the ceiling already reached).
func sleepUntil(ctx context.Context, d time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// preCommitWatch is whatever is allowed to end a streaming leg before the
// engine's first byte: a flat timer, or the peer liveness watch.
//
// One type for both because the caller's obligations are the same either way
// — disarm exactly once, the instant response headers arrive, and read the
// verdict under the same lock that writes it — and because a leg that had two
// mechanisms would need a rule about which one won.
type preCommitWatch struct {
	mu       sync.Mutex
	reason   string
	waited   time.Duration
	disarmed bool
	stop     func()
}

// trip records why the wait ended and cancels the in-flight request, and
// reports whether it took. It is a no-op after disarm, so a decision reached
// at the same moment headers arrived loses to the headers — which is the safe
// direction: the response is real and the abort is a guess about a request
// that had not answered yet. The return value is what keeps a losing decision
// out of the log as well as out of the response.
func (p *preCommitWatch) trip(reason string, waited time.Duration, cancel context.CancelFunc) bool {
	if reason == "" {
		return false
	}
	p.mu.Lock()
	if p.disarmed || p.reason != "" {
		p.mu.Unlock()
		return false
	}
	p.reason = reason
	p.waited = waited
	p.mu.Unlock()
	cancel()
	return true
}

// disarm stops the watch and reports its verdict: an empty reason means the
// leg was not ended by it. Nil-safe, so a leg with no watch needs no guard.
func (p *preCommitWatch) disarm() (string, time.Duration) {
	if p == nil {
		return "", 0
	}
	p.mu.Lock()
	p.disarmed = true
	reason, waited := p.reason, p.waited
	p.mu.Unlock()
	if p.stop != nil {
		p.stop()
	}
	return reason, waited
}

// armPreCommitWatch starts whatever bounds this leg's pre-first-byte wait,
// or returns nil when nothing does.
//
// parent is the CALLER's context, not the request context the watch cancels:
// the watch has to outlive its own cancellation long enough to record why,
// and deriving it from the context it cancels would race that.
func (h *HandlerSet) armPreCommitWatch(parent context.Context, wait waitPolicy, cancel context.CancelFunc) *preCommitWatch {
	if wait.Budget <= 0 && wait.Liveness == nil {
		return nil
	}
	w := &preCommitWatch{}
	started := time.Now()
	if wait.Liveness == nil {
		t := time.AfterFunc(wait.Budget, func() { _ = w.trip(wait.Reason, wait.Budget, cancel) })
		w.stop = func() { t.Stop() }
		return w
	}
	ctx, stop := context.WithCancel(parent)
	lv := *wait.Liveness
	// The ceiling is armed here rather than left to the loop below. The loop
	// evaluates it between checks, which is the right place while checks are
	// coming back — and exactly the wrong place if one does not. A timer is
	// answerable to nothing the peer does.
	var ceiling *time.Timer
	if lv.Ceiling > 0 {
		ceiling = time.AfterFunc(lv.Ceiling, func() {
			_ = w.trip(LocalErrorPeerTTFBTimeout, lv.Ceiling, cancel)
			stop()
		})
	}
	w.stop = func() {
		if ceiling != nil {
			ceiling.Stop()
		}
		stop()
	}
	go func() {
		reason, waited := h.watchPeerWhileItWorks(ctx, lv, started)
		_ = w.trip(reason, waited, cancel)
	}()
	return w
}

// failedPeerLegReason names a peer leg that failed before it committed,
// and reports whether the turn ends here.
//
// Two things were wrong with the bare 502 this replaces (waired-agent#1171).
//
// The NAME. The reason recorded for a dispatch that never reached the peer
// was engine_request_failed — a sentence about an engine, for a failure in
// which no engine took part. This device dialled that peer's overlay
// endpoint and did not get there, which is what peer_unreachable says, and
// the watch already uses that word for the same fact observed a different
// way. Naming it where it happens is the rule waired-agent#1168 settled
// (docs/decisions/20260904/0215-a-hangup-is-not-the-engines-failure.md):
// name it at the source, and do not move the status to do it.
//
// The STATUS, but only on a pinned leg. A 5xx is retried by Claude Code,
// and each retry is a fresh request that re-runs selection — so for a leg
// with substitutes the retry is a recovery path, not ten wasted attempts,
// and it stays a 502 for the same reason ErrPeersDidNotAnswer stayed a 503
// through the fail-closed change. A PIN has no substitute
// (waired-agent#325), so retrying re-asks the one machine that just failed.
// That is the case waired-agent#1180 already ruled on for the pre-dispatch
// twin: the turn ends now, naming the computer, rather than after ten
// anonymous retries. The person's situation is identical on either side of
// the dispatch, so it gets the identical sentence.
//
// Never on a cancelled request: the client left, every call under its
// context fails with it, and the peer is not what went wrong.
func failedPeerLegReason(ctx context.Context, sel router.Selection, rr *requestRec) (reason string, endTurn bool) {
	if ctx != nil && ctx.Err() != nil {
		return LocalErrorClientDisconnected, false
	}
	if _, ok := peerDeviceIDOf(sel); !ok {
		// This device's own engine. engine_request_failed is already the
		// name of what happened, and a local leg is not answered here.
		return "", false
	}
	return LocalErrorPeerUnreachable, rr.isPinnedPeer()
}

// isPinnedPeer is nil-safe: a rec-less direct call (tests) is not a pin,
// which is the answer that leaves the leg on the status it has today.
func (rr *requestRec) isPinnedPeer() bool { return rr != nil && rr.pinnedPeer }

// preCommitAbortMessage is the client-facing sentence for each way the wait
// can end. They differ because they are different facts, and the one the
// #757 timeout produced ("produced no response within 1m0s") would be a
// false account of a peer that answered a health check by saying it had
// stopped.
func preCommitAbortMessage(who, reason string, waited time.Duration) string {
	// Rounded once, for every branch. Two of the three ways this is reached
	// carry a MEASURED wait rather than a configured one, and an unrounded
	// Duration prints every digit the clock had — "within 1m0.234567891s" in
	// a sentence addressed to a person. Rounding to the second leaves the
	// configured cases (the flat budget, the ceiling) reading exactly as they
	// did, because those are whole seconds already.
	waited = waited.Round(time.Second)
	switch reason {
	case LocalErrorPeerStoppedServing:
		return fmt.Sprintf("the %s stopped working on this request after %s", who, waited)
	case LocalErrorPeerUnreachable:
		return fmt.Sprintf("the %s stopped answering after %s", who, waited)
	default:
		return fmt.Sprintf("%s produced no response within %s", who, waited)
	}
}
