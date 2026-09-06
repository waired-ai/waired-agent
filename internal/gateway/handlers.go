package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/observability"
	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/internal/runtime"
)

// requestRec accumulates Phase 9 RecordRequest fields across a
// handler's lifecycle. The handler creates one with startRequest at
// entry and defer-calls finish at exit; intermediate code paths
// populate Model / Decision / ErrorReason / Status as those values
// become known. finish suppresses emission when Model is still empty
// (= pre-selection errors with no inference involvement).
type requestRec struct {
	rec   Recorder
	start time.Time
	ev    observability.RequestEvent

	// firstTokenSeen latches setFirstToken so the first call wins. The
	// latch lives here rather than on the event because ev.TTFTMs == 0
	// means "not observed" — a genuinely instant first token must not be
	// mistaken for an unstamped one, and vice versa.
	firstTokenSeen bool

	// onUsage is Deps.OnUsage, invoked once at finish() for requests
	// that actually reached an engine. nil on every surface that does
	// not meter (waired#829).
	onUsage usageSink
	// onPeerOutcome is Deps.OnPeerOutcome, invoked once at finish() for
	// requests this listener dispatched to a mesh peer. nil on the
	// overlay listener, which cannot dispatch to one (waired-agent#281).
	onPeerOutcome func(deviceID string, ok bool)
	// peerDeviceID is the real mesh DeviceID of the peer this request
	// was dispatched to, or "" for a local / external selection. Kept
	// apart from ev.PeerID, which is the display identifier and is a
	// grant pseudonym for a Public Share peer (spec §8.5): this one is
	// the routing key and never reaches a log line or the wire.
	peerDeviceID string
	// peerVariantID is the catalog variant that peer is serving, and
	// promptTokens what this request sent it. Together with TTFTMs they
	// are a prefill rate — the term peer selection had no way to see
	// (waired-agent#1127) — and the reason they are kept here is that
	// none of the three is in scope at the moment the others are.
	peerVariantID string
	promptTokens  int
	// pinnedPeer marks a dispatch to the peer the operator NAMED, where
	// #325's ruling applies: a pin is not substituted. It decides how a
	// leg that fails before it commits is answered — every other peer leg
	// has somewhere else to go, and this one does not
	// (waired-agent#1171).
	pinnedPeer bool
	// onPeerFirstToken is Deps.OnPeerFirstToken, invoked once when the
	// engine's first token arrives on a request dispatched to a peer.
	onPeerFirstToken func(deviceID, variantID string, promptTokens int, ttft time.Duration)
	// ctx is the request context, captured at handler entry. onUsage
	// reads the peer identity the auth middleware stamped on it, and
	// emitPeerOutcome reads its cancellation state to tell a peer that
	// failed apart from an operator who pressed Ctrl-C.
	ctx context.Context
	// engineModel is the engine-native identifier the request ran
	// against, kept separate from ev.Model (the catalog id) because the
	// control plane resolves a quality tier from the engine form.
	engineModel string
}

func (h *HandlerSet) startRequest(r *http.Request, kind string) *requestRec {
	rr := &requestRec{
		rec:              h.deps.Recorder,
		start:            time.Now(),
		ev:               observability.RequestEvent{Kind: kind},
		onUsage:          h.deps.OnUsage,
		onPeerOutcome:    h.deps.OnPeerOutcome,
		onPeerFirstToken: h.deps.OnPeerFirstToken,
	}
	if r != nil {
		rr.ctx = r.Context()
	}
	return rr
}

// setUsage records the upstream's own token counts. Called from the
// proxy helpers once the response has been fully forwarded.
func (rr *requestRec) setUsage(in, out int64) {
	if rr == nil {
		return
	}
	rr.ev.InputTokens, rr.ev.OutputTokens = in, out
}

// setToolRecovery records that the gateway put back a tool call the
// engine had dropped into the assistant text (#409). shape is the
// dialect it was recovered from, never the text itself.
func (rr *requestRec) setToolRecovery(shape string) {
	if rr == nil {
		return
	}
	rr.ev.ToolRecovery = shape
}

// setFirstToken records that the engine has produced its first token,
// as a wait measured from handler entry (waired-agent#874). Idempotent:
// the first call wins, including across the stream retry loop, because
// what this measures is how long the human waited — an abandoned
// attempt was still time they spent looking at a blank screen.
//
// Called only from the Anthropic streaming leg; see RequestEvent.TTFTMs
// for why the other legs deliberately leave it unobserved.
// observePeerPrefill reports what this request measured of the peer's
// prefill: the prompt it sent over the wait to the first token. Called
// from setFirstToken, which is the only place that instant is known.
//
// It reports nothing for a local or external selection (no peer), for a
// request whose token count was never taken, or where the surface wired
// no sink — and nothing for a zero wait, which firstTokenMs already
// refuses to render as an observation.
func (rr *requestRec) observePeerPrefill() {
	if rr == nil || rr.onPeerFirstToken == nil {
		return
	}
	if rr.peerDeviceID == "" || rr.promptTokens <= 0 || rr.ev.TTFTMs == 0 {
		return
	}
	rr.onPeerFirstToken(rr.peerDeviceID, rr.peerVariantID, rr.promptTokens,
		time.Duration(rr.ev.TTFTMs)*time.Millisecond)
}

func (rr *requestRec) setFirstToken() {
	if rr == nil || rr.firstTokenSeen {
		return
	}
	rr.firstTokenSeen = true
	rr.ev.TTFTMs = firstTokenMs(time.Since(rr.start))
	rr.observePeerPrefill()
}

// firstTokenMs renders an OBSERVED wait in whole milliseconds, never as
// zero. On RequestEvent zero means "not observed", so a sub-millisecond
// first token — a warm prefix on a fast host, or any test with a
// synchronous engine — must not read as an absent observation.
func firstTokenMs(d time.Duration) uint32 {
	if ms := d.Milliseconds(); ms > 0 {
		return uint32(ms)
	}
	return 1
}

// Residency verdicts recorded on RequestEvent.ModelResidency. They name what
// THIS device's engine was holding when the request reached it, never how
// long anything will take (waired-agent#837); the empty string is the fourth
// case and means the question was not asked.
const (
	residencyResident = "resident" // it already held this request's model
	residencyAbsent   = "absent"   // it held nothing
	residencyOther    = "other"    // it held a different model
)

// residencyVerdict maps one observation onto those three words.
//
// The comparison is exact against the engine-native tag, the same convention
// warmServingModelNow uses to decide it has nothing to do — so "other" means
// literally "the engine reported a different name", which is true even when
// the difference is only a `:latest` spelling. An unobserved reading yields
// "", never "absent": not having looked and having looked and found nothing
// are different answers, and merging them is the defect waired-agent#879
// exists for one level up.
func residencyVerdict(res runtime.ModelResidency, want string) string {
	switch {
	case !res.Observed:
		return ""
	case res.Model == "":
		return residencyAbsent
	case res.Model == want:
		return residencyResident
	default:
		return residencyOther
	}
}

// setEngineFacts records what this device's engine was doing when the request
// arrived: what it held, and how many requests it was already serving.
//
// Called from the handler BEFORE admitLocalEngine, because "already serving"
// is the whole meaning of the count — reading it afterwards would include
// this request and every solo turn would report 1.
func (rr *requestRec) setEngineFacts(residency string, inflight int) {
	if rr == nil {
		return
	}
	rr.ev.ModelResidency = residency
	if inflight > 0 {
		rr.ev.EngineInflight = inflight
	}
}

// localEngineFacts reads the two observations for a LOCAL selection: what the
// engine holds, and how many requests it is already serving. Both are
// zero-valued when unwired (every surface but the Claude intercept) or when
// the selection runs on a peer, where this device's engine is not the one
// answering.
func (h *HandlerSet) localEngineFacts(sel router.Selection) (runtime.ModelResidency, int) {
	if strings.HasPrefix(sel.Runtime, remoteRuntimePrefix) {
		return runtime.ModelResidency{}, 0
	}
	var res runtime.ModelResidency
	if h.deps.LocalResidency != nil {
		res = h.deps.LocalResidency()
	}
	var inflight int
	if h.deps.LocalInflight != nil {
		inflight = h.deps.LocalInflight()
	}
	return res, inflight
}

// localEngineLogFields renders what this device's engine holds for a log
// line, with the age of the observation attached so a reader can discount a
// stale one. Empty for a peer leg or an unwired surface — an absent field is
// "we did not look", and writing "none" there would be a claim.
func (h *HandlerSet) localEngineLogFields(sel router.Selection, rr *requestRec) []any {
	res, _ := h.localEngineFacts(sel)
	if !res.Observed {
		return nil
	}
	holds := res.Model
	if holds == "" {
		holds = "none"
	}
	fields := []any{"engine_holds", holds}
	if !res.At.IsZero() {
		fields = append(fields, "observed_ago_ms", time.Since(res.At).Milliseconds())
	}
	if rr != nil {
		fields = append(fields, "engine_inflight", rr.ev.EngineInflight)
	}
	return fields
}

// setCachedInput records how many prompt tokens the engine served from
// its prefix cache (waired-agent#885). Separate from setUsage, whose
// contract is "the upstream's own token counts" and which the streaming
// leg fills from a different accumulator for a different reason.
//
// Zero on every engine that does not report a breakdown. That was
// every engine but vLLM with --enable-prompt-tokens-details until
// ollama 0.33.3, which reports one on both its surfaces with no flag to
// ask for it (waired-agent#1193).
func (rr *requestRec) setCachedInput(n int64) {
	if rr == nil || n <= 0 {
		return
	}
	rr.ev.CachedInputTokens = n
}

func (rr *requestRec) finish() {
	if rr == nil {
		return
	}
	// Ahead of the Model guard, and not subject to it. That guard drops
	// telemetry for failures with no inference involvement; a request
	// that named a peer is never one of those, and a routing signal that
	// depended on the model id being set would be a silent coupling.
	rr.emitPeerOutcome()
	if rr.ev.Model == "" {
		return
	}
	rr.ev.LatencyMs = uint32(time.Since(rr.start).Milliseconds())
	if rr.rec != nil {
		rr.rec.RecordRequest(rr.ev)
	}
	rr.emitUsage()
}

// emitUsage hands the sample to Deps.OnUsage.
//
// Only requests that reached an engine are emitted: finish() also runs
// for gateway-level failures (runtime_unavailable, runtime_unhealthy,
// rewrite_failed), and counting those would inflate a ledger the user
// sees.
//
// The line is whether the engine did the work, not whether the client
// could use the answer, and both post-commit outcomes are on the working
// side of it. The openai leg's mid_stream_truncate is a stream that broke
// with the client already reading. The anthropic streaming leg's
// engine_truncated_stream is a turn no retry could make usable — the one
// that cost the MOST, since proxyAnthropicStream folds every abandoned
// attempt into setUsage precisely because the engine really drew them
// (waired-agent#458). Both record the 200 their client received, so both
// are emitted (waired-agent#554).
//
// The status alone is enough to tell those from a gateway-level failure
// only because every exit that fails BEFORE the response starts records
// the 4xx/5xx it wrote to the client (waired-agent#538). Widening this
// gate to "any error_reason" instead would take both truncations with it,
// and skipping samples with no observed tokens would take every turn an
// engine reported no usage for.
func (rr *requestRec) emitUsage() {
	if rr.onUsage == nil || rr.ev.Status <= 0 || rr.ev.Status >= 400 {
		return
	}
	ctx := rr.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	rr.onUsage(ctx, UsageSample{
		Kind:         rr.ev.Kind,
		ModelID:      rr.ev.Model,
		EngineModel:  rr.engineModel,
		Class:        rr.ev.Class,
		InputTokens:  rr.ev.InputTokens,
		OutputTokens: rr.ev.OutputTokens,
		DurationMS:   int64(rr.ev.LatencyMs),
		Status:       rr.ev.Status,
	})
}

// notPeerFault names the error_reason values a request that WAS
// dispatched to a peer can carry which say nothing about that peer.
//
// A denylist rather than an allowlist: a reason added later then
// defaults to "the peer failed", which over-counts and ages out of the
// 60 s window on its own. An allowlist would default to silence, and
// silence is the defect waired-agent#281 records.
var notPeerFault = map[string]struct{}{
	// The prompt overran the window. Either this gateway's own #623
	// guard refused it before dispatch, or the peer returned the 400 it
	// meant to return and relayPeerContextOverflow relayed it
	// (waired-agent#436). A peer that correctly refuses an oversized
	// prompt is working; charging it would rank it below one that
	// silently truncates the head.
	LocalErrorContextOverflow: {},
	// This gateway could not rewrite the request body. Ours, not theirs.
	"rewrite_failed": {},
	// The client hung up. Says nothing about whoever was serving —
	// see engineLegReason.
	LocalErrorClientDisconnected: {},
}

// engineLegReason names why a leg to an engine failed, separating the
// client's own departure from the engine's.
//
// A request whose context is already gone did not meet a failing engine:
// the client hung up, every call under that context fails with it, and
// the engine logs OUR disconnect. On ollama that is a GIN `| 500 |` row
// plus `srv stop: cancel task`, and reading those as the engine erroring
// is what waired-agent#1168 was opened about — measured on a 16 GB M4,
// aborting a curl at 45 s during a 50k-token prefill reproduces every
// line of that report.
//
// peerVerdict already declines to charge a peer for this, and its comment
// names the three reasons a disconnect lands on (engine_truncated_stream,
// engine_request_failed, mid_stream_truncate). This says it once at the
// source instead, so the metrics, the event ring and the WARN carry the
// distinction too rather than each reader re-deriving it.
//
// otherwise is what to call the failure when the client is still there.
func engineLegReason(ctx context.Context, otherwise string) string {
	if ctx != nil && ctx.Err() != nil {
		return LocalErrorClientDisconnected
	}
	return otherwise
}

// peerVerdict reads the finished record as evidence about the peer that
// served this request.
//
// charge=false means "this request says nothing about that peer", and
// records nothing at all — deliberately distinct from ok=false, because
// ErrorWindow reports failures over total observations, so a wrong
// success dilutes the rate exactly as much as a wrong failure inflates
// it.
func (rr *requestRec) peerVerdict() (ok, charge bool) {
	if rr.ev.Status <= 0 {
		return false, false
	}
	if _, skip := notPeerFault[rr.ev.ErrorReason]; skip {
		return false, false
	}
	// The operator pressed Ctrl-C. The client's disconnect cancels this
	// context and the upstream call then fails, landing on
	// engine_truncated_stream (anthropic) or, on the openai leg, on
	// engine_request_failed when the dispatch had not started yet and
	// mid_stream_truncate once it had — so without this guard every
	// interrupted turn would demote whichever peer the operator
	// interrupts most.
	//
	// It is the REQUEST context: proxyAnthropicStream's TTFB timer
	// cancels a child of it, and cancelling a child never propagates
	// upwards, so a real TTFB timeout is still charged.
	if rr.ctx != nil && errors.Is(rr.ctx.Err(), context.Canceled) {
		return false, false
	}
	// The same two fields observability.Recorder reads to label a
	// request success or error, so one request cannot be an error in the
	// metrics and a success in the routing signal.
	return rr.ev.Status < 400 && rr.ev.ErrorReason == "", true
}

// emitPeerOutcome folds this request into the caller-side per-peer error
// window (router.ErrorWindow), which the mesh Selector reads back as a
// same-score tie-break. Only a remote dispatch produces a sample: a
// local leg says nothing about any peer, and a failure before selection
// never named one — setSelection is what sets peerDeviceID.
//
// Deliberately unlogged. peerDeviceID is a real device id, and for a
// Public Share peer no log line, header or response body may carry one
// (spec §8.5); display sites use peerDisplayID(sel) as they do today.
func (rr *requestRec) emitPeerOutcome() {
	if rr.onPeerOutcome == nil || rr.peerDeviceID == "" {
		return
	}
	if ok, charge := rr.peerVerdict(); charge {
		rr.onPeerOutcome(rr.peerDeviceID, ok)
	}
}

// setSelection records what won. It takes the probed selection whole
// rather than the three fields it used to, because a fourth (Pinned)
// decides how a failed dispatch is answered and a positional argument
// that only one of two call sites remembered to pass would be a defect
// waiting to happen.
func (rr *requestRec) setSelection(probed probedSelection) {
	sel := probed.Sel
	fallbackFrom, fallbackReason := probed.FallbackFrom, probed.Reason
	rr.pinnedPeer = probed.Pinned
	rr.ev.Decision = sel.ExecutionMode
	rr.ev.Model = sel.ModelID
	rr.engineModel = sel.EngineModel
	// Display identifier only — the event ring is served over the
	// management API and rendered by the tray, so a Public Share peer
	// appears as its grant pseudonym (spec §8.5).
	rr.ev.PeerID = peerDisplayID(sel)
	// The functional identifier, alongside the display one. Selection
	// carries no DeviceID field; Runtime is what keeps the real id
	// attached, and endpoint_router.go's newRemoteCandidate says why it
	// stays keyed on it. The Selector reads its error window back by the
	// same key, so peerDisplayID here would open a second, permanently
	// empty entry for every Public Share peer.
	if strings.HasPrefix(sel.Runtime, remoteRuntimePrefix) {
		rr.peerDeviceID = strings.TrimPrefix(sel.Runtime, remoteRuntimePrefix)
		// The variant travels with the peer id for the same reason a
		// published rate carries it: a prefill rate is meaningless
		// against another model, so an observation keyed only by peer
		// would survive a model switch it should not (#1127).
		rr.peerVariantID = sel.VariantID
	}
	rr.ev.FallbackFrom = fallbackFrom
	rr.ev.FallbackReason = fallbackReason
}

// fail records the failing status and reason. Nil-safe, matching setUsage /
// finish: proxyToEngine documents that rr may be nil, and it now reports
// engine errors from inside that function.
func (rr *requestRec) fail(status int, reason string) {
	if rr == nil {
		return
	}
	rr.ev.Status = status
	rr.ev.ErrorReason = reason
}

// failSelection records a failed Selector call. A pinned-peer failure also
// names the peer: selection produced no Selection, so setSelection never
// runs and ev.PeerID would otherwise stay empty in the WARN emit and the
// event ring — leaving the operator with "a pin is down" and no way to tell
// which one (waired-agent#325).
func (rr *requestRec) failSelection(err error, status int) {
	if rr == nil {
		return
	}
	rr.fail(status, selectionErrorReason(err))
	if peer := pinnedPeerOf(err); peer != "" {
		rr.ev.PeerID = peer
	}
}

// pinnedPeerOf extracts the display identifier of the peer a pinned-routing
// failure names, or "" when err is not one. Display form only: a Public
// Share peer's real device id must never reach a header, a log line or an
// error body (spec §8.5) — the router resolves that at construction.
func pinnedPeerOf(err error) string {
	var pin *router.PinnedPeerUnreachableError
	if errors.As(err, &pin) {
		return pin.PeerDisplayID
	}
	return ""
}

// pinnedPeerNameOf is the same peer as a person would name it: the device
// name when this host knows one, and the display identifier otherwise. The
// two are separate because they answer different questions — the header and
// the event ring key on the identifier, and the message a user reads cannot
// act on one (waired-agent#1180, spec §8.5 for why a Public Share peer is
// only ever its pseudonym).
func pinnedPeerNameOf(err error) string {
	var pin *router.PinnedPeerUnreachableError
	if errors.As(err, &pin) {
		if pin.PeerName != "" {
			return pin.PeerName
		}
		return pin.PeerDisplayID
	}
	return ""
}

func (rr *requestRec) succeed() {
	if rr.ev.Status == 0 {
		rr.ev.Status = http.StatusOK
	}
}

// selectionErrorReason maps a router selection error to the
// telemetry error_reason tag the RequestEvent carries. Returns the
// empty string for nil so callers can use it inline.
func selectionErrorReason(err error) string {
	switch {
	case err == nil:
		return ""
	case router.BelowModelSizeFloor(err):
		// First, because the floor wraps whatever the branch returned and
		// every arm below would answer about the wrapped error instead.
		// The responders already treat this as its own answer — both set
		// X-Waired-Local-Error: model_too_small and write 404 — and the
		// journal said model_not_served, which sends the reader looking
		// for a model nobody has (waired-agent#1178).
		return LocalErrorModelTooSmall
	case errors.Is(err, router.ErrModelNotFound):
		return "model_not_found"
	case errors.Is(err, router.ErrCapabilityNotMet):
		return "capability_not_met"
	case errors.Is(err, router.ErrLocalInferenceOff):
		return "inference_disabled"
	case errors.Is(err, router.ErrModelNotReady):
		// waired-agent#788: two conditions the sentinel cannot separate,
		// and an operator reading the journal needs them apart — one is a
		// wait, the other is a model nobody serves.
		if router.ModelIsArriving(err) {
			return "model_not_ready"
		}
		return "model_not_served"
	case errors.Is(err, router.ErrAllPeersOverloaded):
		return "all_peers_overloaded"
	case errors.Is(err, router.ErrPeersDidNotAnswer):
		return "peers_did_not_answer"
	case errors.Is(err, router.ErrPinnedPeerUnreachable):
		return "pinned_peer_unreachable"
	case errors.Is(err, ErrPeerRoutingDisabled):
		return "runtime_unavailable"
	case errors.Is(err, router.ErrHardwareInsufficient):
		return "hardware_insufficient"
	case errors.Is(err, router.ErrRuntimeNotInstalled):
		return "runtime_not_installed"
	default:
		return "selection_failed"
	}
}

// selectionStatus maps a router selection error to the HTTP status
// the gateway will return. Used by the request-record helper before
// respondSelectionError actually writes the response.
//
// So every row here has to be the status the responders WRITE, not a
// second opinion about it: ev.Status reaches the event ring the
// management API serves and the `status` field of the WARN and DEBUG
// lines (internal/observability.Recorder.RecordRequest), where the only
// thing a reader can do with it is compare it against what the client
// reported. ErrHardwareInsufficient stayed grouped with its neighbour at
// 400 after both responders moved it to 422, so that comparison
// disagreed silently on a request class whose whole purpose is telling
// an operator "this machine cannot run this model" (waired-agent#740).
// TestSelectionRecord_MatchesWhatTheClientReceives holds the two
// together for every sentinel now.
// selectionStatus is the OpenAI surface's mapping, and the one
// management.mapRouterStatus dry-runs. The Claude surface has its own
// (anthropicSelectionStatus): its statuses are chosen against what Claude
// Code does with each one, which is not a property of the OpenAI clients.
func selectionStatus(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case router.BelowModelSizeFloor(err):
		// Both responders write 404 for the floor whatever it wrapped, so
		// this row has to as well. Without it a floor over an engine-less
		// requester recorded 503 while the client received 404 — the one
		// thing the doc comment above forbids (waired-agent#1178).
		return http.StatusNotFound
	case errors.Is(err, router.ErrModelNotFound):
		return http.StatusNotFound
	case errors.Is(err, router.ErrCapabilityNotMet):
		return http.StatusBadRequest
	case errors.Is(err, router.ErrHardwareInsufficient):
		// 422, not the 400 its neighbour gets: the request parsed, its
		// hardware requirements were the problem. Both responders read
		// it that way (openai.go / anthropic.go), and so does the
		// explain endpoint that dry-runs them (management.mapRouterStatus).
		return http.StatusUnprocessableEntity
	case errors.Is(err, router.ErrModelNotReady) && !router.ModelIsArriving(err):
		// Both responders send 404 for a model no host serves, and this
		// record has to be the status the client received
		// (waired-agent#740, #788).
		return http.StatusNotFound
	case errors.Is(err, router.ErrModelNotReady),
		errors.Is(err, router.ErrLocalInferenceOff),
		errors.Is(err, router.ErrAllPeersOverloaded),
		errors.Is(err, router.ErrPeersDidNotAnswer),
		errors.Is(err, router.ErrPinnedPeerUnreachable),
		errors.Is(err, ErrPeerRoutingDisabled),
		errors.Is(err, router.ErrRuntimeNotInstalled):
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// remoteRuntimePrefix is the marker the Selector emits in
// Selection.Runtime to mean "this Selection lives on a peer". The
// gateway handler peels the prefix off, looks up the peer adapter via
// Deps.PeerAdapterFactory, and uses its custom HTTP transport to
// proxy the request over the WG overlay.
const remoteRuntimePrefix = "remote:"

// ErrPeerRoutingDisabled is returned by lookupAdapter when a remote
// Selection arrives at a listener whose Deps.PeerAdapterFactory is
// nil. The agent's overlay-side gateway is configured this way as
// part of Phase 4 loop prevention.
var ErrPeerRoutingDisabled = errors.New("gateway: peer routing disabled on this listener")

// HandlerSet is the listener-agnostic core of the gateway: a
// http.ServeMux populated with the OpenAI- / Anthropic-compatible
// routes plus the handler methods that proxy each request through the
// router and runtime registry.
//
// It does NOT install any listener-specific middleware (loopback-only,
// bearer-token check, peer-source-IP gate, signed-body verify, …) —
// callers wrap Handler() in whatever stack is appropriate for the
// listener they're attaching it to:
//
//   - The loopback Server (cmd/waired-agent loopback :9473) wraps it
//     in loopbackOnly + requireToken + pausedGate + inferenceGate.
//   - The overlay inference listener (Phase 4, port 9474) wraps it in
//     wgPeerOnly + verifyPeerSignature + pausedGate + inferenceGate
//     and supplies a Selector with MeshSnapshotFn=nil (= local-only,
//     loop prevention).
//
// Splitting these allows both listeners to share one route table and
// one set of handler implementations without growing an
// `if listener == loopback` cascade.
type HandlerSet struct {
	deps Deps
	mux  *http.ServeMux
}

// NewHandlerSet wires the route table from deps. AllowOpenAI /
// AllowAnthropic gate which surfaces are exposed (per agentconfig);
// disabled surfaces simply have no route registered, which yields a
// vanilla 404 — indistinguishable from "the route doesn't exist", which
// matches the network-level firewalling intent that turning off OpenAI
// or Anthropic should look like an unrouted port.
func NewHandlerSet(deps Deps) *HandlerSet {
	if deps.HTTPClient == nil {
		// Streaming responses can be longer than the default 30s; the
		// caller can cap with context if needed.
		deps.HTTPClient = &http.Client{Timeout: 0}
	}
	h := &HandlerSet{deps: deps, mux: http.NewServeMux()}
	h.routes()
	return h
}

// Handler returns the bare mux. Wrap it in whatever middleware the
// listener needs.
func (h *HandlerSet) Handler() http.Handler { return h.mux }

// lookupAdapter resolves a Selection to the runtime.Adapter that
// will service it. For local selections it consults
// h.deps.Runtimes. For remote selections (Runtime starts with
// "remote:") it calls h.deps.PeerAdapterFactory.
func (h *HandlerSet) lookupAdapter(sel router.Selection) (runtime.Adapter, error) {
	if strings.HasPrefix(sel.Runtime, remoteRuntimePrefix) {
		if h.deps.PeerAdapterFactory == nil {
			return nil, ErrPeerRoutingDisabled
		}
		deviceID := strings.TrimPrefix(sel.Runtime, remoteRuntimePrefix)
		a, err := h.deps.PeerAdapterFactory(deviceID)
		if err != nil {
			return nil, err
		}
		if a == nil {
			return nil, errors.New("gateway: peer adapter factory returned nil")
		}
		return a, nil
	}
	a, ok := h.deps.Runtimes.Lookup(sel.Runtime)
	if !ok {
		return nil, errors.New("gateway: runtime not registered")
	}
	return a, nil
}

// admitLocalEngine reports the request to Deps.LocalAdmission when
// this listener is about to occupy THIS machine's engine, and returns
// the release the handler defers. Remote (peer) selections run on
// another machine and are not counted.
//
// Always returns a non-nil release, so callers can defer it
// unconditionally.
func (h *HandlerSet) admitLocalEngine(ctx context.Context, sel router.Selection) func() {
	noop := func() {}
	if h.deps.LocalAdmission == nil {
		return noop
	}
	if strings.HasPrefix(sel.Runtime, remoteRuntimePrefix) {
		return noop
	}
	if release := h.deps.LocalAdmission(ctx); release != nil {
		return release
	}
	return noop
}

// clientFor returns the http.Client to use against adapter. Adapters
// that implement runtime.Transporter (peer adapters dialing over WG
// overlay) get their own Transport-installed client; the default
// HTTPClient covers everything else.
func (h *HandlerSet) clientFor(adapter runtime.Adapter) *http.Client {
	if t, ok := adapter.(runtime.Transporter); ok {
		if rt := t.Transport(); rt != nil {
			return &http.Client{Transport: rt, Timeout: h.deps.HTTPClient.Timeout}
		}
	}
	return h.deps.HTTPClient
}

// asFailureReporter returns adapter as a runtime.FailureReporter, or nil.
//
// Same optional-interface shape as clientFor above. The nil result is the
// load-bearing case: peer adapters deliberately do NOT implement it, so a
// remote peer's 500 can never demote THIS host's engine (waired-agent#29).
func asFailureReporter(adapter runtime.Adapter) runtime.FailureReporter {
	if r, ok := adapter.(runtime.FailureReporter); ok {
		return r
	}
	return nil
}

// reportEngineFailure hands a non-2xx engine reply to the adapter when the
// adapter can act on it. Nil-safe so the anthropic proxies (which are also
// driven directly by tests) need no guard at each call site.
func reportEngineFailure(reporter runtime.FailureReporter, status int, body []byte) {
	if reporter == nil {
		return
	}
	reporter.ReportUpstreamFailure(status, body)
}

// peerProbeLookup is the PeerProbeLookup callback the Phase 8
// coordinator (ParallelProbe) drives. Resolves a mesh peer DeviceID
// to (signingTransport, baseURL) by composing PeerAdapterFactory with
// the adapter's Transporter interface. Errors propagate to the
// coordinator as ProbeTransportError, excluding the peer from the
// current request.
func (h *HandlerSet) peerProbeLookup(peerID string) (http.RoundTripper, string, error) {
	if h.deps.PeerAdapterFactory == nil {
		return nil, "", ErrPeerRoutingDisabled
	}
	a, err := h.deps.PeerAdapterFactory(peerID)
	if err != nil {
		return nil, "", err
	}
	if a == nil {
		return nil, "", errors.New("gateway: peer adapter factory returned nil")
	}
	t, ok := a.(runtime.Transporter)
	if !ok {
		return nil, "", errors.New("gateway: peer adapter does not implement Transporter")
	}
	rt := t.Transport()
	if rt == nil {
		return nil, "", errors.New("gateway: peer adapter Transport() returned nil")
	}
	return rt, a.BaseURL(), nil
}

func (h *HandlerSet) routes() {
	if h.deps.AllowOpenAI {
		h.mux.HandleFunc("/v1/models", h.handleOpenAIModels)
		h.mux.HandleFunc("/v1/chat/completions", h.handleOpenAIChatCompletions)
		h.mux.HandleFunc("/v1/responses", h.handleOpenAIResponses)
	}
	if h.deps.AllowAnthropic {
		h.mux.HandleFunc("/anthropic/v1/messages", h.handleAnthropicMessagesImpl)
		h.mux.HandleFunc("/anthropic/v1/messages/count_tokens", h.handleAnthropicCountTokensImpl)
		// #623: local Anthropic model discovery so Claude Code learns each
		// model's effective context window. The trailing-slash pattern
		// serves the /{id} single-object form.
		h.mux.HandleFunc("/anthropic/v1/models", h.handleAnthropicModels)
		h.mux.HandleFunc("/anthropic/v1/models/", h.handleAnthropicModels)
	}
}
