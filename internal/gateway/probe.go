package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/waired-ai/waired-agent/internal/observability"
	"github.com/waired-ai/waired-agent/internal/router"
)

// probeReasonCapacityFull is router.ProbeResult.FailureReason()'s tag
// for "the peer answered, it is just full". Named here because
// pinnedProbeFailure has to treat it as overloaded rather than
// unreachable.
const probeReasonCapacityFull = "capacity_full"

const (
	probeFanoutK    = 3
	briefQueueDelay = 250 * time.Millisecond

	// probeAttempts is how many times the SelectK + probe + commit chain
	// runs before the gateway gives up, with briefQueueDelay between
	// attempts. More than one because a probe that does not come back is
	// not evidence about the peer: it can equally be a packet lost on the
	// way, a relay hiccup, or a peer mid-handshake, and a network fault
	// should not be reported as a verdict on the mesh (owner ruling on
	// waired-agent#624, 20260812).
	probeAttempts = 3

	// probeBudgetFloor and probeBudgetCeiling bound the per-attempt
	// readiness-probe budget that probeBudgetFor derives from peer RTT.
	//
	// The floor is not 2×RTT: even a peer one millisecond away has to
	// accept a connection, run its /healthz handler and answer, and the
	// point of the floor is that a LAN peer is never rejected for being
	// microseconds late.
	//
	// The ceiling bounds the whole failure path. Worst case before an
	// unreachable mesh reports back is
	// probeAttempts×ceiling + (probeAttempts-1)×briefQueueDelay, which
	// TestSelectAndProbe_WorstCaseWaitStaysBounded pins.
	probeBudgetFloor   = 250 * time.Millisecond
	probeBudgetCeiling = 1000 * time.Millisecond

	// probeRTTMargin multiplies the peer's measured RTT to get its
	// budget. It is deliberately generous rather than tight: the peer
	// adapter sets DisableKeepAlives, so every probe pays a fresh overlay
	// TCP handshake plus the request itself — two round trips before the
	// peer's own handler time is counted at all. The budget this replaced
	// was a flat 50 ms, which is under ONE round trip on a mesh measuring
	// 52 ms, so no probe could ever finish and every mesh request
	// returned 503 (waired-agent#624).
	probeRTTMargin = 6
)

// probeBudgetFor sizes one probe round for the peers it is about to
// probe. The probes run concurrently under a single deadline, so the
// budget follows the FARTHEST candidate — a nearer peer answering sooner
// costs nothing, while sizing to the nearest would cut off the others.
//
// A candidate whose RTT was never measured (router.RTTUnknown — no disco
// pong, which is also every relay-only peer) contributes the ceiling:
// there is no distance to scale by, and guessing small is what makes a
// far peer permanently unroutable.
func probeBudgetFor(cands []router.Candidate) time.Duration {
	budget := probeBudgetFloor
	for _, c := range cands {
		if c.ExecutionMode != "remote" {
			continue
		}
		want := probeBudgetCeiling
		if c.RTTMS != router.RTTUnknown {
			want = time.Duration(c.RTTMS) * time.Millisecond * probeRTTMargin
		}
		if want > budget {
			budget = want
		}
	}
	if budget > probeBudgetCeiling {
		budget = probeBudgetCeiling
	}
	return budget
}

// probesWentUnanswered reports whether no probe in the round produced a
// verdict about its peer's load. An empty round is not "unanswered";
// there was nothing to ask.
//
// The bar is a peer that served the /healthz contract — a 200 with a
// body, or the 404 a pre-Phase-8 peer answers with. Anything else told
// us nothing about capacity: a transport error (which is also what a
// budget overrun becomes, see router.ProbeHealth), and equally a 401 or
// 403, where the path is usually up and the signature envelope was
// rejected. The old bar was "every result is a transport error", so a
// single auth rejection turned a mesh nobody could use into
// waired_all_peers_overloaded — the reader sent to the capacity filter,
// which never ran. That is the waired-agent#624 mis-diagnosis under a
// different outcome value, and #849 is where it was found again.
func probesWentUnanswered(results []router.ProbeResult) bool {
	if len(results) == 0 {
		return false
	}
	for _, r := range results {
		switch r.Outcome {
		case router.ProbeOK, router.ProbeLegacyPeer:
			return false
		}
	}
	return true
}

// Wire header names the Phase 8 gateway sets on responses so the
// caller (claude-code, codex, custom waired plugin) can surface
// fallback events without parsing logs. Promoted as constants so
// downstream consumers (tray UI / plugin code) can import them.
const (
	HeaderInferencePeer  = "X-Waired-Inference-Peer"
	HeaderFallbackFrom   = "X-Waired-Fallback-From"
	HeaderFallbackReason = "X-Waired-Fallback-Reason"
	// HeaderLocalModel names the catalog model id of the selection that
	// answered an Anthropic messages request — router.Selection.ModelID,
	// which is the Deps.ResolveUnknownModel target on the mapped path
	// (#600) and the id the client asked for otherwise. Set whenever
	// selection succeeded, on a local leg and a mesh leg alike; the
	// serving peer, when there was one, is on HeaderInferencePeer.
	// "Local" here reads as "served by Waired", not "served by this
	// device" — the name predates the mesh-capable Claude surface
	// (#601) and stays because downstream consumers import it.
	//
	// It used to be set only on the mapped path, so a request naming a
	// catalog model id directly reached the Claude intercept's
	// commit-time observer with nothing to report, and
	// `waired claude route` printed no `last served:` line for a turn
	// Waired had served — locally or on a peer (#755).
	HeaderLocalModel = "X-Waired-Local-Model"
	// HeaderLocalError carries a machine-readable local error reason
	// ("no_model"). The Claude intercept prefixes it with "local_" for
	// the fallback reason (=> local_no_model), reading it off the staged
	// (uncommitted) response to emit a distinguishable fallback reason;
	// the literal is duplicated in internal/proxy/intercept (stdlib-only
	// package) — keep them in sync.
	HeaderLocalError = "X-Waired-Local-Error"
	// LocalErrorContextOverflow is the HeaderLocalError value the Anthropic
	// messages handler stages on a #623 context-window 400. Unlike other
	// local errors it must NOT trigger the intercept's auto-mode fallback
	// to the real Anthropic API — the 400 has to reach Claude Code so it
	// auto-compacts and keeps serving locally. The intercept recognises
	// this exact value as "surface, don't fall back" (the literal is
	// duplicated there, stdlib-only package — keep them in sync).
	LocalErrorContextOverflow = "context_overflow"
	// LocalErrorPeerTTFBTimeout is the HeaderLocalError value staged when a
	// peer inference leg produced no response headers within the class's
	// TTFB budget (#757). Unlike LocalErrorContextOverflow it IS a normal
	// fallback reason — the abort is pre-commit, so the intercept's auto
	// mode reroutes the turn. The literal is duplicated in
	// internal/proxy/intercept (stdlib-only package) — keep them in sync.
	LocalErrorPeerTTFBTimeout = "peer_ttfb_timeout"
	// LocalErrorEngineRequestShape is the HeaderLocalError value staged
	// when the engine refused the shape of the body this gateway built for
	// it (waired-agent#1035). Like LocalErrorPeerTTFBTimeout it IS a normal
	// fallback reason — nothing was committed and the turn is not the
	// client's fault — so auto mode reroutes it and the journal names the
	// cause instead of a bare local_status_400. The literal is duplicated
	// in internal/proxy/intercept (stdlib-only package) — keep them in
	// sync.
	LocalErrorEngineRequestShape = "engine_request_shape"
	// LocalErrorPeerStoppedServing is the HeaderLocalError value staged
	// when a watched peer leg ended because the peer itself said it is not
	// working on anything: its /healthz answered with the engine down, or
	// with no admission slot in use while this request should have been
	// holding one (waired-agent#1040). Like LocalErrorPeerTTFBTimeout it IS
	// a normal fallback reason — nothing was committed — so auto mode
	// reroutes the turn. It is a DIFFERENT reason from that one on purpose:
	// a timeout says only that we stopped waiting, and this says the peer
	// told us there was nothing left to wait for. The literal is duplicated
	// in internal/proxy/intercept (stdlib-only package) — keep in sync.
	LocalErrorPeerStoppedServing = "peer_stopped_serving"
	// LocalErrorPeerUnreachable is its sibling for the other way a watched
	// peer leg ends: consecutive health checks that did not come back, so
	// the peer is gone rather than idle (waired-agent#1040). Distinct from
	// LocalErrorPinnedPeerUnreachable, which is about SELECTION — the
	// operator's pin could not be probed before the turn was dispatched —
	// where this is about a peer that accepted work and then vanished. Also
	// duplicated in internal/proxy/intercept.
	LocalErrorPeerUnreachable = "peer_unreachable"
	// LocalErrorPinnedPeerUnreachable is the HeaderLocalError value staged
	// when the operator's pinned peer cannot serve the request. Like
	// LocalErrorPeerTTFBTimeout it IS a normal fallback reason — nothing was
	// committed — so the intercept's auto mode reroutes the turn to the real
	// Anthropic API and names the pin in the reroute notice. On the "waired"
	// route there is no fallback and the 503 reaches the client. The literal
	// is duplicated in internal/proxy/intercept (stdlib-only package) — keep
	// them in sync.
	LocalErrorPinnedPeerUnreachable = "pinned_peer_unreachable"
	// LocalErrorModelNotServed is the HeaderLocalError value staged when
	// no host serves the requested model and none is fetching it
	// (waired-agent#788). Like the two above it IS a normal fallback
	// reason — nothing was committed — so auto mode reroutes the turn and
	// the journal names the cause instead of a bare local_status_404. On
	// the waired route there is no fallback and the 404 reaches the
	// client, which is the point: a retryable 503 there was answered by
	// the Claude CLI with silent, unbounded backoff.
	LocalErrorModelNotServed = "model_not_served"
	// LocalErrorInferenceDisabled is the HeaderLocalError value staged
	// when this host's local inference is off and the mesh had nothing to
	// take the request either (waired-agent#829). A normal fallback
	// reason like the two above — nothing was committed — so auto mode
	// reroutes the turn and the journal names the toggle instead of a
	// bare local_status_503.
	LocalErrorInferenceDisabled = "inference_disabled"

	// LocalErrorEngineTTFBTimeout is the HeaderLocalError value staged when
	// THIS device's own engine produced no response headers within
	// Deps.LocalTTFBBudget (waired-agent#837). Nothing was committed, so
	// auto mode reroutes the turn to the Anthropic API and the notice can
	// say the engine here did not answer — as opposed to
	// LocalErrorPeerTTFBTimeout, which is the same shape about someone
	// else's machine. Deliberately not "local_ttfb_timeout": the intercept
	// renders the reason as "local_" + this value.
	LocalErrorEngineTTFBTimeout = "engine_ttfb_timeout"

	// HeaderTTFBBudgetMs is a response header staged alongside
	// LocalErrorPeerTTFBTimeout or LocalErrorEngineTTFBTimeout carrying the
	// budget (milliseconds) that elapsed, so the intercept can name it in
	// the user-facing reroute notice (#757, waired-agent#837). Duplicated in
	// internal/proxy/intercept — keep in sync.
	HeaderTTFBBudgetMs = "X-Waired-TTFB-Budget-Ms"

	// HeaderFallbackAllowed is a REQUEST header the Claude intercept sets
	// on its auto-dispatch leg to authorize the gateway's pre-commit TTFB
	// abort (#757). It is absent on waired/anthropic (pinned) legs, so a
	// stalled peer under a pinned route is never aborted into a surfaced
	// 502 — the operator's routing lock stands. Value "1" = armed. The
	// literal is duplicated in internal/proxy/intercept (stdlib-only
	// package) — keep them in sync.
	HeaderFallbackAllowed = "X-Waired-Fallback-Allowed"
)

// probedSelection bundles a committed Selection with the Phase 8
// fallback metadata the gateway surfaces on the response.
//
//   - FallbackFrom is the DeviceID of the original first-choice peer
//     when the gateway ended up routing to a different peer (probe
//     failure on cands[0]). Empty when no fallback happened.
//   - Reason is the X-Waired-Fallback-Reason tag (engine_not_ready /
//     paused / share_off / capacity_full / transport_error / etc.).
//     Empty when FallbackFrom is empty.
type probedSelection struct {
	Sel          router.Selection
	FallbackFrom string
	Reason       string

	// probeResults is populated on the not-ok return paths so the
	// caller can inspect what every probe saw (used to surface a
	// uniform typed error like ErrPeerRoutingDisabled directly
	// instead of degenerating to ErrAllPeersOverloaded).
	probeResults []router.ProbeResult
	// cands is the candidate slice probeResults indexes into, also
	// only on the not-ok paths. Needed to tell a failed OPERATOR PIN
	// apart from a busy mesh — see pinnedProbeFailure.
	cands []router.Candidate
	// queuedFor is how long selectAndProbe held the request waiting for
	// an admission slot before giving up (#786). Set only on the
	// capacity return path; the handler turns it into the client's
	// Retry-After, so a caller told "at capacity" after a real wait is
	// not sent back in five seconds to find the same busy peer.
	queuedFor time.Duration
}

// pinnedProbeFailure returns the error for "the operator's pinned peer
// was in the candidate set and its own probe did not come back ready".
// nil when no pin was probed, or when the pin probed ready.
//
// Before waired#729 a pin whose host had gone quiet on disco was
// filtered out of the snapshot, and tryMeshFallbackK produced the
// named ErrPinnedPeerUnreachable from that. Now that disco silence is
// only advisory the pin reaches the probe layer instead, so without
// this the same failure would collapse into
// waired_all_peers_overloaded and lose the peer's name, the
// X-Waired-Local-Error staging and the #391 observability events.
//
// Deliberately scans by the Pinned flag rather than assuming index 0:
// partitionOwnFirst can move a public pin out of the head slot.
//
// "capacity_full" is deliberately NOT an unreachable verdict, and
// neither is a pin that probed ready and lost the admission race: a
// busy box is exactly what ErrAllPeersOverloaded means, and telling
// its operator it is unreachable would send them hunting a network
// fault that isn't there. That split also keeps this path behaviour-
// identical to the pre-waired#729 one, where a capacity-full pin
// passed the snapshot check and fell through to overloaded.
func (h *HandlerSet) pinnedProbeFailure(g probedSelection) error {
	for i, c := range g.cands {
		if !c.Pinned || i >= len(g.probeResults) {
			continue
		}
		r := g.probeResults[i]
		if r.IsReady() || r.FailureReason() == probeReasonCapacityFull {
			return nil
		}
		// One value for the event and the error, as the Selector's own
		// pinUnreachable does. The event reaches the ring the management
		// API serves and a debug log line, and neither may carry a public
		// machine's real device id (#739, spec §8.5). For an own-network
		// candidate this is the same string c.PeerID was.
		display := candidateDisplayID(c)
		if h.deps.Recorder != nil {
			h.deps.Recorder.RecordPinnedPeerUnreachable(display, c.ModelID, "probe_failed")
		}
		// c.ModelID, not the request alias: this mirrors the
		// manifest.ModelID the Selector's own pinUnreachable reports,
		// so both paths name the same thing.
		return &router.PinnedPeerUnreachableError{
			PeerDisplayID: display,
			ModelID:       c.ModelID,
		}
	}
	return nil
}

// uniformProbeErr scans probe results for a typed error every probe
// shares. Returns the error when every result carries it (via
// errors.Is); the handler then surfaces it directly instead of
// collapsing into ErrAllPeersOverloaded. ErrPeerRoutingDisabled is
// the canonical case: the overlay-side gateway has PeerAdapterFactory
// nil for loop prevention, and a "remote:" candidate must surface
// runtime_unavailable rather than blame "every mesh peer is
// overloaded" — those are two different operator problems.
func uniformProbeErr(results []router.ProbeResult, target error) error {
	if len(results) == 0 {
		return nil
	}
	for _, r := range results {
		if r.Err == nil || !errors.Is(r.Err, target) {
			return nil
		}
	}
	return target
}

// unansweredMeshError names the peers this computer asked and what came
// back, wrapping the sentinel so nothing downstream has to change.
//
// The sentinel alone says a probe round went unanswered and stops there.
// A reader then has no way to tell "the peers are off" from "this
// machine reaches nothing", and on a host whose overlay is dead the
// answer is always the second one — which is the whole of #849. Every
// production consumer matches with errors.Is (the gateway's status and
// reason tables, the management explain mapping), so wrapping is
// invisible to them.
//
// Never chained with ErrAllPeersOverloaded: selectionErrorReason and
// selectionStatus test that one first, and an error that satisfied both
// would be reported as the wrong one of the two.
//
// Peer names go through candidateDisplayID because this string is
// written verbatim into the 503 body the client reads. A Public Share
// peer's real device identifier may not appear there (spec §8.5, #739).
func unansweredMeshError(g probedSelection) error {
	tried := make([]string, 0, len(g.probeResults))
	for i, r := range g.probeResults {
		if i >= len(g.cands) {
			break
		}
		tried = append(tried, fmt.Sprintf("%q: %s",
			candidateDisplayID(g.cands[i]), probeMiss(r)))
	}
	if len(tried) == 0 {
		return router.ErrPeersDidNotAnswer
	}
	return fmt.Errorf("%w from this computer (tried %s)",
		router.ErrPeersDidNotAnswer, strings.Join(tried, ", "))
}

// probeMiss phrases one probe's failure for a person reading a 503.
//
// Kept apart from ProbeResult.FailureReason, which returns the
// wire-stable header tag: these words are read by whoever ran the
// request, and "auth_error" does not tell them their computer's
// identity was turned away.
func probeMiss(r router.ProbeResult) string {
	switch r.Outcome {
	case router.ProbeAuthError:
		return "rejected our identity"
	case router.ProbeTransportError:
		return "no answer"
	default:
		return "not ready"
	}
}

// logUnansweredRound is the only place a probe's error text survives.
//
// ParallelProbe records an outcome tag and a latency per probe, and the
// failure return drops the results entirely, so the reason a peer could
// not be reached existed nowhere at all. One line per round rather than
// one per probe: a five-peer mesh that is off would otherwise write five
// lines for every request that arrives.
func logUnansweredRound(g probedSelection) {
	attrs := make([]any, 0, 2*len(g.probeResults)+1)
	attrs = append(attrs, "candidates", len(g.cands))
	for i, r := range g.probeResults {
		if i >= len(g.cands) || r.Err == nil {
			continue
		}
		attrs = append(attrs, candidateDisplayID(g.cands[i]), r.Err.Error())
	}
	slog.Warn("no mesh peer answered its readiness probe", attrs...)
}

// selectAndProbe is the Phase 8 probe-then-commit pipeline shared
// between the OpenAI and Anthropic handlers. It:
//
//  1. SelectK(k=3) ranked candidates.
//  2. ParallelProbe under probeBudgetFor(candidates) — first ready wins.
//  3. Commit the winner. If Commit fails (capacity hit between probe
//     and commit), walk forward through the remaining ready candidates.
//  4. If no candidate ever committed, brief-queue 250 ms and run the
//     whole chain again, up to probeAttempts rounds — or, when the round
//     failed for capacity and the caller passed a capacityWait budget,
//     for as long as that budget allows (#786). A config verdict
//     (ErrPeerRoutingDisabled) or a failed operator pin short-circuits
//     instead: retrying those only delays the same answer.
//  5. When every round fails, report which failure it was —
//     ErrPeersDidNotAnswer when no probe came back at all,
//     ErrAllPeersOverloaded when peers answered and were full.
//
// capacityWait is the ceiling from capacityQueueBudget: 0 keeps the
// historical bounded shape (probeAttempts rounds and no more).
//
// The handler caller defers Selection.Release; selectAndProbe never
// holds an admission slot itself after returning.
func (h *HandlerSet) selectAndProbe(ctx context.Context, req router.Request, capacityWait time.Duration) (probedSelection, error) {
	var got probedSelection
	// selectErr carries a capacity rejection the SELECTOR made, so the
	// final answer keeps that error's identity and message instead of
	// being re-derived from probe results that never ran.
	var selectErr error
	start := time.Now()
	for attempt := 1; ; attempt++ {
		var ok bool
		var err error
		selectErr = nil
		got, ok, err = h.tryProbeAndCommit(ctx, req)
		switch {
		case err != nil && !errors.Is(err, router.ErrAllPeersOverloaded):
			if attempt > 1 {
				h.recordRetryOutcome("failed")
			}
			return probedSelection{}, err
		case err != nil:
			// waired-agent#786: the router's own admission filter rejects
			// before any probe runs when every candidate is at its
			// concurrent-request cap. That is the same transient state
			// step 4 exists for, but it used to return from here — so
			// each of Claude Code's concurrent sub-requests got an
			// instant 503 while the peer's single slot was busy, and the
			// journal recorded latency_ms 0 for every one of them.
			selectErr = err
		case ok:
			if attempt > 1 {
				h.recordRetryOutcome("succeeded")
			}
			return got, nil
		default:
			// Two verdicts must not be retried, because retrying them only
			// adds delay before the same answer.
			//
			// Every probe failing with the same typed error
			// (ErrPeerRoutingDisabled is the load-bearing case — overlay-side
			// listeners pin PeerAdapterFactory=nil for loop prevention) is a
			// config problem, so operators see "runtime_unavailable" rather
			// than "all peers overloaded".
			if e := uniformProbeErr(got.probeResults, ErrPeerRoutingDisabled); e != nil {
				return probedSelection{}, e
			}
			// An operator pin that failed its own probe is a different
			// operator problem from "the mesh is busy": the brief queue
			// exists to catch a peer finishing a request, not to wait out an
			// unreachable one.
			if e := h.pinnedProbeFailure(got); e != nil {
				return probedSelection{}, e
			}
		}
		full := selectErr != nil || roundWasCapacityFull(got.probeResults)
		elapsed := time.Since(start)
		if !queueAgain(attempt, elapsed, capacityWait, full) {
			if attempt > 1 {
				h.recordRetryOutcome("failed")
			}
			if selectErr != nil {
				return probedSelection{queuedFor: elapsed}, selectErr
			}
			// Say which of the two happened. "At capacity" is a claim
			// about peers that answered; when none of them answered, the
			// mesh went unmeasured and reporting load sends the reader to
			// the capacity filter, which never ran (waired-agent#624).
			if probesWentUnanswered(got.probeResults) {
				logUnansweredRound(got)
				return probedSelection{}, unansweredMeshError(got)
			}
			return probedSelection{queuedFor: elapsed}, router.ErrAllPeersOverloaded
		}
		// Brief queue: a short sleep that often coincides with another
		// request completing on a peer (in-flight count drops below
		// capacity), and gives a lost probe packet a second chance.
		// Cheaper than asking the client to retry.
		slog.Debug("gateway brief-queue retry",
			"attempt", attempt, "delay_ms", briefQueueDelay.Milliseconds(),
			"capacity_full", full, "elapsed_ms", elapsed.Milliseconds(),
			"capacity_wait_ms", capacityWait.Milliseconds())
		select {
		case <-time.After(briefQueueDelay):
		case <-ctx.Done():
			return probedSelection{}, ctx.Err()
		}
	}
}

// roundWasCapacityFull reports whether a probe round ended with peers
// that answered and were busy, as opposed to peers that never answered
// (or a round with nothing to ask). Only the busy case is worth waiting
// out: a slot frees when the request holding it ends, while an
// unreachable mesh has nothing to wait for.
func roundWasCapacityFull(results []router.ProbeResult) bool {
	return len(results) > 0 && !probesWentUnanswered(results)
}

// queueAgain reports whether the selection pipeline should brief-queue
// and run another round.
//
// The first probeAttempts rounds are the waired-agent#624 retry: a probe
// that does not come back is not evidence about the peer. Past them the
// only condition worth waiting out is capacity, and only when the caller
// supplied a budget — capacityQueueBudget arms one solely for a leg with
// no fallback to take. An unreachable mesh therefore still reports back
// inside the bound TestSelectAndProbe_WorstCaseWaitStaysBounded pins.
//
// elapsed is the whole time spent since the first round started, not the
// sum of the sleeps: the probe rounds between them are time the client
// is held open too, and the budget is a promise about that total.
func queueAgain(attempt int, elapsed, capacityWait time.Duration, capacityFull bool) bool {
	if attempt < probeAttempts {
		return true
	}
	if !capacityFull || capacityWait <= 0 {
		return false
	}
	return elapsed+briefQueueDelay < capacityWait
}

// capacityQueueBudget bounds how long selectAndProbe may keep queueing
// for an admission slot before answering "at capacity" (waired-agent#786).
//
// The ceiling is the class's configured pre-first-byte window
// (agentconfig.ClaudeTTFBBudgetMainMs / ClaudeTTFBBudgetSubMs, reached
// through Deps.TTFBBudget). That number already states how long this
// deployment accepts waiting before the first byte of a turn arrives, so
// spending part of it on a peer's busy slot costs the caller nothing it
// was not already prepared to spend — and no fresh constant has to be
// justified. Subagent legs get the tighter budget, so Claude Code's
// concurrent helper requests give up before its main conversation does.
//
// Armed only for a leg with NO fallback: the Claude intercept sets
// HeaderFallbackAllowed on its auto-route dispatch, and there a fast 503
// is the better answer — the intercept reroutes the turn to the real
// Anthropic API on any status >= 400, so queueing would only delay a
// turn that has somewhere else to go. The waired (and pinned) routes
// have nowhere else, which is what makes waiting worth something.
func capacityQueueBudget(deps Deps, r *http.Request, class string) time.Duration {
	if deps.TTFBBudget == nil || r.Header.Get(HeaderFallbackAllowed) == "1" {
		return 0
	}
	if b := deps.TTFBBudget(class); b > 0 {
		return b
	}
	return 0
}

// recordRetryOutcome reports how a request that needed more than its
// first probe round ended up. Kept as one helper so the attempt loop
// cannot record a retry it did not make.
func (h *HandlerSet) recordRetryOutcome(outcome string) {
	if h.deps.Recorder != nil {
		h.deps.Recorder.RecordBriefQueueRetry(outcome)
	}
}

// tryProbeAndCommit is one round of SelectK + ParallelProbe + Commit.
// Returns ok=false when no candidate committed (either no ready
// probe or every ready candidate lost the admission race). The
// not-ok return value carries probeResults so the caller can surface
// uniform typed errors (ErrPeerRoutingDisabled) directly.
func (h *HandlerSet) tryProbeAndCommit(ctx context.Context, req router.Request) (probedSelection, bool, error) {
	cands, err := h.deps.Selector.SelectK(ctx, req, probeFanoutK)
	if err != nil {
		return probedSelection{}, false, err
	}
	slog.Debug("probe candidates", "model", req.Model, "count", len(cands))
	if len(cands) == 0 {
		return probedSelection{}, false, nil
	}
	budget := probeBudgetFor(cands)
	winnerIdx, results := ParallelProbe(ctx, cands, h.peerProbeLookup, budget)
	slog.Debug("probe complete",
		"candidates", len(cands), "winner_idx", winnerIdx, "budget_ms", budget.Milliseconds())
	// Emit per-probe telemetry for every candidate that actually
	// reached out over the WG mesh. Fast-path (local / external)
	// slots carry a synthetic ProbeOK with zero latency and are
	// suppressed; the gateway's request-level event covers them.
	if h.deps.Recorder != nil {
		for i, c := range cands {
			if c.ExecutionMode != "remote" {
				continue
			}
			r := results[i]
			h.deps.Recorder.RecordProbe(r.Outcome.String(), r.LatencyMs)
		}
	}
	if winnerIdx < 0 {
		return probedSelection{probeResults: results, cands: cands}, false, nil
	}
	// Try the winner first, then walk forward through the remaining
	// ready candidates if commit fails (capacity hit between probe
	// and commit). Walking forward is walking down the Selector's
	// ranking, because winnerIdx is the best-ranked ready candidate:
	// every index before it already answered, and answered not-ready.
	for i := winnerIdx; i < len(cands); i++ {
		if i != winnerIdx && !results[i].IsReady() {
			continue
		}
		sel, ok := cands[i].Commit()
		if !ok {
			continue
		}
		got := probedSelection{Sel: sel}
		if i > 0 && cands[0].PeerID != "" {
			// Display identifier, never the raw DeviceID: FallbackFrom
			// reaches the X-Waired-Fallback-From response header, the
			// observability FallbackEvent (ring + slog + tray) and
			// RequestEvent.FallbackFrom. Nothing consumes it
			// functionally. Reachable with a public cands[0] whenever
			// the own-candidate set is empty (spec §8.5).
			got.FallbackFrom = candidateDisplayID(cands[0])
			got.Reason = firstFailureReason(results)
		}
		return got, true, nil
	}
	return probedSelection{probeResults: results, cands: cands}, false, nil
}

// firstFailureReason scans the probe results and returns the first
// non-empty FailureReason. The Phase 8 plan uses the original first-
// choice peer's reason as the surface signal — operators see "why did
// my preferred peer get skipped" not "what's wrong with the eventual
// winner".
func firstFailureReason(results []router.ProbeResult) string {
	for _, r := range results {
		if reason := r.FailureReason(); reason != "" {
			return reason
		}
	}
	return "unknown"
}

// setSelectionHeaders surfaces the Phase 8 inference / fallback
// metadata as response headers. The X-Waired-Inference-Peer header is
// set on every remote response so claude-code / codex / waired-plugin
// can show "this request was served by peer-A"; the fallback headers
// only appear when the winning peer wasn't the Selector's first
// choice.
//
// When rec is non-nil, fallback emission is routed through
// Recorder.RecordFallback (which fans the same event to slog + Prom +
// ring); when rec is nil, the Phase 8 direct slog.Warn line is
// preserved for backwards-compatible test fixtures.
func setSelectionHeaders(w http.ResponseWriter, sel router.Selection, fallbackFrom, reason string, rec Recorder) {
	if peerID := peerDisplayID(sel); peerID != "" {
		w.Header().Set(HeaderInferencePeer, peerID)
	}
	if fallbackFrom == "" {
		return
	}
	w.Header().Set(HeaderFallbackFrom, fallbackFrom)
	if reason == "" {
		reason = "unknown"
	}
	w.Header().Set(HeaderFallbackReason, reason)
	to := peerDisplayID(sel)
	if to == "" {
		to = sel.Runtime
	}
	if rec != nil {
		rec.RecordFallback(observability.FallbackEvent{
			From:   fallbackFrom,
			To:     to,
			Reason: reason,
			Model:  sel.ModelID,
		})
		return
	}
	slog.Warn("inference fallback",
		"from", fallbackFrom,
		"to", to,
		"reason", reason,
		"model", sel.ModelID,
	)
}

// PeerProbeLookup resolves a mesh peer DeviceID to the components
// ProbeHealth needs: the signing RoundTripper and the peer-overlay
// base URL. Implemented by the gateway's lookupAdapter / clientFor
// path; passed as a function so probe.go stays free of runtime
// imports and can be unit-tested with stub transports.
//
// Returns an error when the peer cannot be reached (factory failure,
// unknown deviceID); the coordinator treats the candidate as a
// ProbeTransportError result.
type PeerProbeLookup func(peerID string) (transport http.RoundTripper, baseURL string, err error)

// ProbeOutcome is a per-candidate result the coordinator collects.
// Carries the index back into the input slice so the call site can
// stitch results to candidates without sharing pointers.
type ProbeOutcome struct {
	Index  int
	Result router.ProbeResult
}

// ParallelProbe fans probes out to up to N candidates in parallel and
// returns the index of the BEST-RANKED one whose probe completed with
// IsReady() == true — the lowest index, not the first arrival — plus
// the full slice of per-candidate results.
//
// It used to return the first arrival, which quietly discarded the
// Selector's ranking: every candidate is probed at once, so on a mesh
// of probeFanoutK peers or fewer the winner was simply whoever answered
// /healthz first. Admin routing priority, the catalog score, the
// per-peer failure window and the sticky binding all sort candidates
// (sortMeshCandidates) and none of them could change the outcome. See
// docs/decisions/20260805/1703-probe-honours-the-selector-ranking.md.
//
// Deciding on rank costs at most the budget and usually far less: index
// i wins as soon as every candidate ahead of it has resolved as
// not-ready, so a failed or refusing peer releases the decision
// immediately rather than at the deadline. A peer that hangs holds the
// decision only until the budget expires, which is what the budget is
// for.
//
// Fast path: when candidates[0] has ExecutionMode != "remote" (local
// or external), no probing happens — the coordinator returns
// winnerIdx=0 with a synthetic ready result. The probe-then-commit
// pattern is meaningful only for the mesh path.
//
// Cancellation: derives a child context with the given budget and
// cancels it as soon as the winner is settled so the losers drop their
// RoundTrip immediately. If no candidate becomes ready within the
// budget, returns winnerIdx=-1 with all the not-ready / failed results
// populated; the coordinator caller (gateway/openai or anthropic) then
// runs the brief-queue retry.
//
// The returned slice has len(cands) entries, one per input candidate,
// in original order. Slots for candidates that never ran (e.g. if
// fast-path returned for index 0) carry the zero ProbeResult value.
func ParallelProbe(ctx context.Context, cands []router.Candidate, lookup PeerProbeLookup, budget time.Duration) (winnerIdx int, results []router.ProbeResult) {
	n := len(cands)
	if n == 0 {
		return -1, nil
	}
	results = make([]router.ProbeResult, n)
	// settled[i] means results[i] is final — either a probe answered
	// for it or it never had one to run. Ranking needs to know the
	// difference between "not ready" and "not answered yet".
	settled := make([]bool, n)

	// Fast path: SelectK returned a local / external candidate. No
	// probing needed; the candidate commits immediately.
	if cands[0].ExecutionMode != "remote" {
		results[0] = router.ProbeResult{Outcome: router.ProbeOK, Status: router.HealthStatus{
			EngineReady:  true,
			ShareEnabled: true,
		}}
		return 0, results
	}

	pctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	type signal struct {
		idx int
		r   router.ProbeResult
	}
	sigs := make(chan signal, n)
	var wg sync.WaitGroup

	for i, c := range cands {
		if c.ExecutionMode != "remote" {
			// Shouldn't normally happen — mesh batches are uniform —
			// but defensively pre-fill with the "skip probe, treat
			// as ready" outcome so a hypothetical mixed batch doesn't
			// dead-lock the fan-in loop.
			results[i] = router.ProbeResult{Outcome: router.ProbeOK, Status: router.HealthStatus{
				EngineReady: true, ShareEnabled: true,
			}}
			settled[i] = true
			continue
		}
		if lookup == nil {
			results[i] = router.ProbeResult{
				Outcome: router.ProbeTransportError,
				Err:     errors.New("gateway: probe lookup nil"),
			}
			settled[i] = true
			continue
		}
		wg.Add(1)
		go func(idx int, peerID string) {
			defer wg.Done()
			transport, baseURL, err := lookup(peerID)
			if err != nil {
				sigs <- signal{idx, router.ProbeResult{Outcome: router.ProbeTransportError, Err: err}}
				return
			}
			sigs <- signal{idx, router.ProbeHealth(pctx, transport, baseURL)}
		}(i, c.PeerID)
	}

	// Closer drains wg without blocking the consumer.
	go func() {
		wg.Wait()
		close(sigs)
	}()

	winnerIdx = -1
	// Covers the batch where no goroutine ran at all (nil lookup, or a
	// batch of pre-filled slots): the drain loop below sees a closed
	// channel and never gets to ask.
	if idx, decided := bestSettledReady(cands, results, settled); decided {
		winnerIdx = idx
	}
	// Keep draining after the decision: the remaining results still
	// reach the caller's per-probe telemetry and its fallback-reason
	// scan, and the loop is what waits for the probe goroutines.
	for sig := range sigs {
		results[sig.idx] = sig.r
		settled[sig.idx] = true
		if winnerIdx >= 0 {
			continue
		}
		if idx, decided := bestSettledReady(cands, results, settled); decided {
			winnerIdx = idx
			// Cancel the probes that can no longer change the answer;
			// their results may still arrive (we'll record them) but
			// they won't wait for the full budget.
			cancel()
		}
	}
	return winnerIdx, results
}

// bestSettledReady reports the winning candidate index once the ranking
// can no longer change: the first ready candidate whose every
// better-ranked peer has already settled as not-ready. decided is false
// while an unsettled candidate still outranks every ready one — the
// answer is not knowable yet. It is true with idx == -1 when every
// candidate settled and none was ready, which is the brief-queue
// trigger.
//
// Residency breaks a tie inside that answer, and only a tie
// (waired-agent#880). When the leading ready candidate is one the Selector
// ranked no higher than its neighbours — same Candidate.RankTier — and it
// says it does NOT hold its weights, a peer in that same tier that says it
// does is taken instead. A peer that outranks on quality, priority, error
// rate, distance or load keeps winning while it is cold; nil (a peer that
// cannot answer) ranks exactly where it did before, because "has not looked"
// is not "cold" (docs/decisions/20260820/0130-model-residency-is-a-setting.md).
//
// The measured gap is 17-56 s of first-token latency on the fleet
// (waired-agent#861), against a tie-break that was previously deviceID
// ascending — i.e. arbitrary.
func bestSettledReady(cands []router.Candidate, results []router.ProbeResult, settled []bool) (idx int, decided bool) {
	for i := range results {
		if !settled[i] {
			return -1, false
		}
		if !results[i].IsReady() {
			continue
		}
		// A tie-break can only be applied to a tie that has fully arrived.
		// Committing to a cold leader while a peer of the same rank is
		// still being probed would decide an answer that could still
		// change, which is the one thing this function exists not to do.
		warm, ok := warmerInSameTier(cands, results, settled, i)
		if !ok {
			return -1, false
		}
		return warm, true
	}
	return -1, true
}

// warmerInSameTier picks between the leading ready candidate at lead and its
// equally-ranked neighbours. ok is false while any of them is still unsettled
// and could still turn out to be the better answer.
//
// Returns lead unchanged whenever the tie-break has nothing to say: lead is
// warm or silent about it, no tier information exists (every hand-built
// Candidate is tier 0, so a test fake gets the permissive answer), or no
// equally-ranked peer claims to be warm.
func warmerInSameTier(cands []router.Candidate, results []router.ProbeResult, settled []bool, lead int) (idx int, ok bool) {
	if lead >= len(cands) || !isColdPeer(results[lead]) {
		return lead, true
	}
	tier := cands[lead].RankTier
	for j := lead + 1; j < len(results) && j < len(cands); j++ {
		if cands[j].RankTier != tier {
			break // the tier is contiguous: past it, rank decides again
		}
		if !settled[j] {
			return -1, false
		}
		if results[j].IsReady() && isWarmPeer(results[j]) {
			return j, true
		}
	}
	return lead, true
}

// isWarmPeer / isColdPeer read the peer's own residency answer. Both are
// false for nil, which is the whole point: a peer that has not looked is
// neither promoted nor demoted for it.
func isWarmPeer(r router.ProbeResult) bool {
	return r.Status.ModelResident != nil && *r.Status.ModelResident
}

func isColdPeer(r router.ProbeResult) bool {
	return r.Status.ModelResident != nil && !*r.Status.ModelResident
}

// peerDisplayID is the peer identifier every display surface must use
// for a remote Selection: the Selector's PeerDisplayID when set (the
// grant pseudonym for a Public Share peer, the DeviceID otherwise),
// falling back to the runtime suffix for Selections built before the
// field existed or by test fakes. Empty for local / external
// selections.
//
// Real foreign device identifiers must never reach a header, an event,
// a log line or a CLI surface (public share spec §8.5).
func peerDisplayID(sel router.Selection) string {
	if sel.PeerDisplayID != "" {
		return sel.PeerDisplayID
	}
	if id := strings.TrimPrefix(sel.Runtime, remoteRuntimePrefix); id != sel.Runtime {
		return id
	}
	return ""
}

// candidateDisplayID is peerDisplayID for a pre-commit Candidate.
func candidateDisplayID(c router.Candidate) string {
	if c.PeerDisplayID != "" {
		return c.PeerDisplayID
	}
	return c.PeerID
}

// displayRuntime renders Selection.Runtime for a human: the functional
// value keys a remote selection on the real DeviceID, which must not
// appear in an error body or a log line for a Public Share peer.
func displayRuntime(sel router.Selection) string {
	if !strings.HasPrefix(sel.Runtime, remoteRuntimePrefix) {
		return sel.Runtime
	}
	if id := peerDisplayID(sel); id != "" {
		return remoteRuntimePrefix + id
	}
	return sel.Runtime
}

// adapterErrorForClient renders a dispatch-path failure for a
// client-visible error body, or for a log line describing the same
// failure — agent.log gets the rendering the client does.
//
// displayRuntime alone is not enough: the production PeerAdapterFactory
// formats the peer's real DeviceID and overlay IP into its error
// strings ("peer %q not in current mesh snapshot"), and for a Public
// Share peer neither may be shown (spec §8.5). A peer dropping out of
// the snapshot between selection and dispatch is a routine race — grant
// expiry and map propagation make it the expected teardown window — so
// this is not an exotic path.
//
// A transport error is the same problem arriving by a different route:
// client.Do returns a *url.Error holding the URL it was dialling, and
// the *net.OpError beneath it holds the address again, so every
// dispatch-path error string has to come through here (waired-agent#538).
//
// Own-network selections keep the detailed error: the identifiers in it
// are the operator's own, and support relies on them.
//
// err may be nil — bufio.Scanner reports a clean end that way, and a
// renderer that panics on the ordinary case would be a poor place to put
// a privacy rule.
func adapterErrorForClient(sel router.Selection, err error) string {
	if sel.PeerDisplayID != "" && sel.PeerDisplayID != strings.TrimPrefix(sel.Runtime, remoteRuntimePrefix) {
		// Public peer: the pseudonym is the whole story the client gets.
		return fmt.Sprintf("runtime %q: peer unavailable", displayRuntime(sel))
	}
	if err == nil {
		return fmt.Sprintf("runtime %q", displayRuntime(sel))
	}
	return fmt.Sprintf("runtime %q: %s", displayRuntime(sel), err.Error())
}

// effectiveContextWindow is the input-token window the #623 guard must
// hold a request to: the window the endpoint that will ANSWER says it
// serves, falling back to this device's own computation when the
// endpoint declares nothing.
//
// The order is the whole point. Deps.ContextWindowFor is built from this
// device's manifests and this device's applied tuning, so on a mesh leg
// it describes the wrong engine — which is how a prompt sized for a
// 200k requester reached a peer tuned to 98k and was truncated at the
// head rather than compacted (waired-agent#436).
//
// 0 out of both means unknown, and the guard fails open exactly as it
// did before the field existed. That is what lets a fleet upgrade in any
// order: a peer running an older agent sends no window, and the caller
// gets today's behaviour rather than a black hole.
func effectiveContextWindow(deps Deps, sel router.Selection) int {
	if sel.ContextWindow > 0 {
		return sel.ContextWindow
	}
	if deps.ContextWindowFor == nil {
		return 0
	}
	return deps.ContextWindowFor(sel.ModelID)
}
