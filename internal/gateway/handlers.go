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
		rec:           h.deps.Recorder,
		start:         time.Now(),
		ev:            observability.RequestEvent{Kind: kind},
		onUsage:       h.deps.OnUsage,
		onPeerOutcome: h.deps.OnPeerOutcome,
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

func (rr *requestRec) setSelection(sel router.Selection, fallbackFrom, fallbackReason string) {
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
func (rr *requestRec) failSelection(err error) {
	if rr == nil {
		return
	}
	rr.fail(selectionStatus(err), selectionErrorReason(err))
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
	case errors.Is(err, router.ErrModelNotFound):
		return "model_not_found"
	case errors.Is(err, router.ErrCapabilityNotMet):
		return "capability_not_met"
	case errors.Is(err, router.ErrModelNotReady):
		return "model_not_ready"
	case errors.Is(err, router.ErrAllPeersOverloaded):
		return "all_peers_overloaded"
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
func selectionStatus(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, router.ErrModelNotFound):
		return http.StatusNotFound
	case errors.Is(err, router.ErrCapabilityNotMet),
		errors.Is(err, router.ErrHardwareInsufficient):
		return http.StatusBadRequest
	case errors.Is(err, router.ErrModelNotReady),
		errors.Is(err, router.ErrAllPeersOverloaded),
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
