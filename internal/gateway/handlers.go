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
	// ctx is the request context, captured at handler entry so finish()
	// can hand it to onUsage. Used for value lookup only — the sink
	// reads the peer identity the auth middleware stamped there — so its
	// cancellation state at emit time is irrelevant.
	ctx context.Context
	// engineModel is the engine-native identifier the request ran
	// against, kept separate from ev.Model (the catalog id) because the
	// control plane resolves a quality tier from the engine form.
	engineModel string
}

func (h *HandlerSet) startRequest(r *http.Request, kind string) *requestRec {
	rr := &requestRec{
		rec:     h.deps.Recorder,
		start:   time.Now(),
		ev:      observability.RequestEvent{Kind: kind},
		onUsage: h.deps.OnUsage,
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

func (rr *requestRec) finish() {
	if rr == nil || rr.ev.Model == "" {
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
// sees. A mid-stream truncation is NOT a failure for this purpose — the
// engine did the work and the client received part of it — and
// rr.succeed() has already recorded 200 in that case.
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

func (rr *requestRec) setSelection(sel router.Selection, fallbackFrom, fallbackReason string) {
	rr.ev.Decision = sel.ExecutionMode
	rr.ev.Model = sel.ModelID
	rr.engineModel = sel.EngineModel
	// Display identifier only — the event ring is served over the
	// management API and rendered by the tray, so a Public Share peer
	// appears as its grant pseudonym (spec §8.5).
	rr.ev.PeerID = peerDisplayID(sel)
	rr.ev.FallbackFrom = fallbackFrom
	rr.ev.FallbackReason = fallbackReason
}

func (rr *requestRec) fail(status int, reason string) {
	rr.ev.Status = status
	rr.ev.ErrorReason = reason
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
