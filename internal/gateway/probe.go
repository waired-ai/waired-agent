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

// Phase 8 tuning constants. The Phase 8 work record documents the
// reasoning behind each (probe is GPU-free so a 50ms budget is cheap;
// 250ms brief queue covers in-flight churn from completing requests).
const (
	probeFanoutK    = 3
	probeBudget     = 50 * time.Millisecond
	briefQueueDelay = 250 * time.Millisecond
)

// Wire header names the Phase 8 gateway sets on responses so the
// caller (claude-code, codex, custom waired plugin) can surface
// fallback events without parsing logs. Promoted as constants so
// downstream consumers (tray UI / plugin code) can import them.
const (
	HeaderInferencePeer  = "X-Waired-Inference-Peer"
	HeaderFallbackFrom   = "X-Waired-Fallback-From"
	HeaderFallbackReason = "X-Waired-Fallback-Reason"
	// HeaderLocalModel names the catalog model id an unknown Anthropic
	// model id was mapped to via Deps.ResolveUnknownModel (#600). Set
	// only when mapping occurred and selection succeeded.
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

	// HeaderTTFBBudgetMs is a response header staged alongside
	// LocalErrorPeerTTFBTimeout carrying the budget (milliseconds) that
	// elapsed, so the intercept can name it in the user-facing reroute
	// notice (#757). Duplicated in internal/proxy/intercept — keep in sync.
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

// selectAndProbe is the Phase 8 probe-then-commit pipeline shared
// between the OpenAI and Anthropic handlers. It:
//
//  1. SelectK(k=3) ranked candidates.
//  2. ParallelProbe (50 ms budget) — first ready wins.
//  3. Commit the winner. If Commit fails (capacity hit between probe
//     and commit), walk forward through the remaining ready candidates.
//  4. If no candidate ever committed, brief-queue 250 ms and retry
//     the full SelectK + ParallelProbe + Commit chain once.
//  5. If both rounds fail, return ErrAllPeersOverloaded.
//
// The handler caller defers Selection.Release; selectAndProbe never
// holds an admission slot itself after returning.
func (h *HandlerSet) selectAndProbe(ctx context.Context, req router.Request) (probedSelection, error) {
	got, ok, err := h.tryProbeAndCommit(ctx, req)
	if ok || err != nil {
		return got, err
	}
	// Special case: every probe failed with the same typed error
	// (ErrPeerRoutingDisabled is the load-bearing case — overlay-side
	// listeners pin PeerAdapterFactory=nil for loop prevention).
	// Surface that directly so operators see "runtime_unavailable"
	// rather than "all peers overloaded".
	if e := uniformProbeErr(got.probeResults, ErrPeerRoutingDisabled); e != nil {
		return probedSelection{}, e
	}
	// Brief queue: short sleep that often coincides with another
	// request completing on a peer (in-flight count drops below
	// capacity). Cheaper than asking the client to retry.
	slog.Debug("gateway brief-queue retry", "delay_ms", briefQueueDelay.Milliseconds())
	select {
	case <-time.After(briefQueueDelay):
	case <-ctx.Done():
		return probedSelection{}, ctx.Err()
	}
	got, ok, err = h.tryProbeAndCommit(ctx, req)
	if err != nil {
		if h.deps.Recorder != nil {
			h.deps.Recorder.RecordBriefQueueRetry("failed")
		}
		return probedSelection{}, err
	}
	if !ok {
		if h.deps.Recorder != nil {
			h.deps.Recorder.RecordBriefQueueRetry("failed")
		}
		if e := uniformProbeErr(got.probeResults, ErrPeerRoutingDisabled); e != nil {
			return probedSelection{}, e
		}
		return probedSelection{}, router.ErrAllPeersOverloaded
	}
	if h.deps.Recorder != nil {
		h.deps.Recorder.RecordBriefQueueRetry("succeeded")
	}
	return got, nil
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
	winnerIdx, results := ParallelProbe(ctx, cands, h.peerProbeLookup, probeBudget)
	slog.Debug("probe complete", "candidates", len(cands), "winner_idx", winnerIdx)
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
		return probedSelection{probeResults: results}, false, nil
	}
	// Try the winner first, then walk forward through the remaining
	// ready candidates if commit fails (capacity hit between probe
	// and commit).
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
	return probedSelection{probeResults: results}, false, nil
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

// ParallelProbe fans probes out to up to N candidates and returns the
// index of the first one whose probe completed with IsReady() ==
// true, plus the full slice of per-candidate results.
//
// Fast path: when candidates[0] has ExecutionMode != "remote" (local
// or external), no probing happens — the coordinator returns
// winnerIdx=0 with a synthetic ready result. The probe-then-commit
// pattern is meaningful only for the mesh path.
//
// Cancellation: derives a child context with the given budget and
// cancels it as soon as the first ready probe arrives so the losers
// drop their RoundTrip immediately. If no candidate becomes ready
// within the budget, returns winnerIdx=-1 with all the
// not-ready / failed results populated; the coordinator caller
// (gateway/openai or anthropic) then runs the brief-queue retry.
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
			continue
		}
		if lookup == nil {
			results[i] = router.ProbeResult{
				Outcome: router.ProbeTransportError,
				Err:     errors.New("gateway: probe lookup nil"),
			}
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
	for sig := range sigs {
		results[sig.idx] = sig.r
		if winnerIdx < 0 && sig.r.IsReady() {
			winnerIdx = sig.idx
			// Cancel the other in-flight probes; their results may
			// still arrive (we'll record them) but they won't wait
			// for the full budget.
			cancel()
		}
	}
	return winnerIdx, results
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

// adapterErrorForClient renders a peer-adapter lookup failure for a
// client-visible error body.
//
// displayRuntime alone is not enough: the production PeerAdapterFactory
// formats the peer's real DeviceID and overlay IP into its error
// strings ("peer %q not in current mesh snapshot"), and for a Public
// Share peer neither may be shown (spec §8.5). A peer dropping out of
// the snapshot between selection and dispatch is a routine race — grant
// expiry and map propagation make it the expected teardown window — so
// this is not an exotic path.
//
// Own-network selections keep the detailed error: the identifiers in it
// are the operator's own, and support relies on them.
func adapterErrorForClient(sel router.Selection, err error) string {
	if sel.PeerDisplayID != "" && sel.PeerDisplayID != strings.TrimPrefix(sel.Runtime, remoteRuntimePrefix) {
		// Public peer: the pseudonym is the whole story the client gets.
		return fmt.Sprintf("runtime %q: peer unavailable", displayRuntime(sel))
	}
	return fmt.Sprintf("runtime %q: %s", displayRuntime(sel), err.Error())
}
