// Package intercept implements the plain-HTTP loopback proxy that Claude Code
// talks to via the managed-settings `ANTHROPIC_BASE_URL`. It is the successor
// to the retired :443 MITM transparent proxy: same routing, no TLS termination,
// no CA, no /etc/hosts redirect.
//
// Claude Code (configured credential-less, so it keeps its claude.ai
// subscription) sends requests to `http://127.0.0.1:<ClaudeGatewayPort>` and
// this server routes each one:
//
//   - POST /v1/messages and /v1/messages/count_tokens are served by the LOCAL
//     gateway (waired's Anthropic->OpenAI->Ollama translation), unless the
//     agent is degraded (paused / inference disabled / no reachable engine),
//     in which case they FAIL OPEN and pass through to the real Anthropic API.
//   - Everything else (OAuth, quota checks, telemetry on api.anthropic.com)
//     passes through to the real Anthropic API verbatim, carrying Claude's
//     subscription OAuth bearer so the subscription/auto-mode stay intact.
//
// The fail-open behaviour is the central robustness property: as long as this
// listener is alive it must never make Claude Code worse off than talking to
// Anthropic directly. A degraded-but-alive agent transparently relays.
//
// No bearer token is enforced here: credential-less Claude presents its
// subscription OAuth token, not waired's gateway token, so the loopback bind is
// the trust boundary (same posture as the no-token OpenCode gateway). The
// LocalInference handler is therefore the BARE gateway HandlerSet, not the
// token-gated loopback gateway.Server.
package intercept

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"sync/atomic"
	"time"
)

// Per-class route values the intercept dispatches on, read per request via
// Deps.ClassRoute so a switch takes effect on the next Claude Code request
// without a restart. String values mirror state.ClaudeRoute{Auto,Waired,
// Anthropic}; the sub-only "same" sentinel is resolved to a concrete value
// by the controller before it reaches this layer. Literals are duplicated
// to keep this fail-open package stdlib-only — keep both sides in sync.
//
//	auto       serve locally when healthy (fail-open on degrade) and retry a
//	           pre-first-byte local error against the real Anthropic API.
//	waired     serve locally only; never passthrough — errors when local
//	           inference is unavailable rather than leaking upstream.
//	anthropic  pass through to the real Anthropic API, degrading to local
//	           only when the API is transport-unreachable (#665).
const (
	routeAuto      = "auto"
	routeWaired    = "waired"
	routeAnthropic = "anthropic"
)

// maxFallbackBodyBytes caps the request body buffered to enable a
// post-dispatch fallback retry. A larger request streams to local
// inference transparently (no buffering, no fallback) to bound memory.
// A var (not const) so tests can shrink it without a multi-MB fixture.
var maxFallbackBodyBytes int64 = 8 << 20 // 8 MiB

// fallbackHeader marks a response that was rerouted after dispatch:
// auto-mode's local→Anthropic fallback ("anthropic; reason=...") or an
// anthropic-target request served locally because the upstream was
// unreachable ("local; reason=...", #665). Claude Code does not surface
// it, but it makes the reroute observable to curl / logs / proxies.
const fallbackHeader = "X-Waired-Fallback"

// Traffic classes the per-class routing policy is keyed by (#645). String
// values mirror state.ClaudeClass* — literals duplicated to keep this
// fail-open package stdlib-only; keep both sides in sync.
const (
	classMain = "main"
	classSub  = "sub"
)

// localErrorHeader mirrors gateway.HeaderLocalError: a machine-readable
// reason ("no_local_model") the local gateway stages on an uncommitted
// error so dispatchAuto can emit a specific fallback reason instead of
// the generic local_status_<code>. The literal is duplicated here to keep
// this fail-open package stdlib-only — keep both sides in sync.
const localErrorHeader = "X-Waired-Local-Error"

// localModelHeader mirrors gateway.HeaderLocalModel: the catalog model id
// the local gateway stamps on a mapped success so the serving model is
// observable (#600). The intercept reads it at commit time to report which
// model answered a locally-served Claude request (#602). The literal is
// duplicated here to keep this fail-open package stdlib-only — keep both
// sides in sync.
const localModelHeader = "X-Waired-Local-Model"

// localErrContextOverflow is the localErrorHeader value the local gateway
// stages on a #623 context-window 400 (prompt exceeds the served model's
// effective window). Unlike other local errors it must NOT trigger the
// auto-mode fallback to the real Anthropic API: the 400 has to reach Claude
// Code so it auto-compacts and the session keeps serving locally. dispatchAuto
// recognises this exact value as "surface, don't fall back". Mirrors
// gateway.LocalErrorContextOverflow — keep both sides in sync.
const localErrContextOverflow = "context_overflow"

// inferencePeerHeader mirrors gateway.HeaderInferencePeer: the mesh peer
// DeviceID the gateway stamps on every remote-served response. Since the
// Claude surface became mesh-capable (#601) a committed response may have
// been served by a peer; the intercept reads this at commit time so peer
// serving is attributed instead of being misreported as local. The
// literal is duplicated here to keep this fail-open package stdlib-only —
// keep both sides in sync.
const inferencePeerHeader = "X-Waired-Inference-Peer"

// fallbackAllowedHeader mirrors gateway.HeaderFallbackAllowed. The intercept
// sets it as a REQUEST header on its auto-dispatch leg only, authorizing the
// gateway's pre-commit TTFB abort (#757). It is absent on waired/anthropic
// (pinned) legs, so a stalled peer under a pinned route is never aborted into
// a surfaced 502 — the operator's routing lock stands. The literal is
// duplicated here to keep this fail-open package stdlib-only — keep in sync.
const fallbackAllowedHeader = "X-Waired-Fallback-Allowed"

// ttfbBudgetHeader mirrors gateway.HeaderTTFBBudgetMs: the budget (ms) the
// gateway stages alongside localErrPeerTTFBTimeout so the reroute notice can
// name it (#757). Duplicated here (stdlib-only) — keep in sync.
const ttfbBudgetHeader = "X-Waired-TTFB-Budget-Ms"

// localErrPeerTTFBTimeout mirrors gateway.LocalErrorPeerTTFBTimeout: the
// localErrorHeader value the gateway stages when a peer leg produced no first
// byte within its TTFB budget (#757). It IS a normal fallback reason (the
// abort is pre-commit). Duplicated here (stdlib-only) — keep in sync.
const localErrPeerTTFBTimeout = "peer_ttfb_timeout"

// Config controls the listener and passthrough behaviour.
type Config struct {
	// Addr is the listen address. Production uses
	// "127.0.0.1:<ClaudeGatewayPort>" (the loopback port Claude Code's
	// ANTHROPIC_BASE_URL points at). Tests pass "127.0.0.1:0".
	Addr string
	// UpstreamScheme is the scheme used to reach the real upstream on
	// passthrough. Defaults to "https".
	UpstreamScheme string
	// UpstreamHost is the real API host every passthrough request is sent to,
	// e.g. "api.anthropic.com". This MUST be set to the real host rather than
	// derived from the inbound request: Claude Code connects to the loopback
	// base URL (127.0.0.1:<port>), so the inbound Host header is the loopback
	// — using it would loop passthrough back onto this listener. Defaults to
	// "api.anthropic.com".
	UpstreamHost string
	// PassthroughModelOverride, when non-empty, replaces waired/* model
	// ids on real-Anthropic legs instead of the last-observed main-loop
	// model / built-in default (#646). No agentconfig plumbing yet — a
	// knob for tests and future config.
	PassthroughModelOverride string

	// AnnotateReroute, when true, splices a short human-readable notice into
	// the fallen-back (Anthropic) response stream on an auto-mode reroute
	// (#757) so the user can tell in-conversation that waired rerouted the
	// turn — a subagent-side record alone is invisible to the user. It is
	// fail-open (see reroute_notice.go) and never fires for a structured
	// (tool_use) or non-stream response. Zero value off so tests opt in;
	// production wiring (buildClaudeListener) sets it true.
	AnnotateReroute bool

	// ModelRouteDirectives (#52), when true, makes routeInference inspect each
	// request's model id and, when it is a reserved directive id
	// (wairedLocalModel → route=waired, wairedCloudModel → route=anthropic),
	// force that route for the request — overriding the per-class ClassRoute
	// policy. This is what lets a Claude Code /model switch pick the backend
	// in-session, alongside /waired-route. Default on in agentconfig; when set
	// it makes every main message-path request buffer+parse its body to read
	// the id (for the auto route the body is buffered anyway, so the extra cost
	// falls on the pinned waired route). The zero value keeps the historical
	// fast path untouched. The gateway advertises the same ids in /v1/models
	// under the same flag.
	ModelRouteDirectives bool
}

// Deps bundles the collaborators. Caller (cmd/waired-agent) wires the real
// implementations; tests assemble fakes.
type Deps struct {
	// LocalInference handles /v1/messages(+/count_tokens) when the agent is
	// healthy. It expects the gateway's Anthropic route convention
	// (/anthropic/v1/messages...), so the server rewrites the inbound
	// /v1/... path to /anthropic/v1/... before delegating. Nil makes every
	// request pass through (no local serving) — useful for a
	// passthrough-only mode and for tests.
	LocalInference http.Handler

	// Degraded reports whether local inference is currently unavailable
	// (paused, inference disabled, or no reachable engine). When it returns
	// true the message paths FAIL OPEN to the real Anthropic API instead of
	// being served locally. Nil == never degraded.
	Degraded func() bool

	// PassthroughTransport reaches the REAL api.anthropic.com. With the
	// /etc/hosts redirect retired this is an ordinary http.Transport;
	// standard DNS already resolves the real host. Required.
	PassthroughTransport http.RoundTripper

	// ClassRoute reports the operator's live route for a traffic class
	// ("main"/"sub") — one of "auto"/"waired"/"anthropic". The controller
	// resolves the subagent-only "same" sentinel to a concrete value before
	// it reaches here. Consulted per request; the intercept classifies the
	// request (via ClassifyModel) only when the two classes' routes differ.
	// Nil == every class is auto.
	ClassRoute func(class string) string

	// OnFallback, if set, is invoked with a short reason string whenever an
	// auto-routed request is retried against the real Anthropic API by the
	// post-dispatch fallback. Used for visibility (the last-fallback record
	// surfaced by `waired claude route`). Nil == no-op.
	OnFallback func(reason string)

	// ClassifyModel derives the traffic class ("main"/"sub", #646) from
	// the ORIGINAL request model id, mirroring the gateway classifier.
	// Consulted only when the main and sub routes differ. Nil == everything
	// main.
	ClassifyModel func(modelID string) string

	// OnNodeFallback, if set, is invoked (class, reason) when an
	// anthropic-routed request was served locally because the real Anthropic
	// API could not be reached. The persisted policy is not demoted — the
	// next request tries upstream again — so this record is what keeps the
	// degrade from being silent. Nil == no-op.
	OnNodeFallback func(class, reason string)

	// OnServed, if set, is invoked with the catalog model id (the
	// gateway's X-Waired-Local-Model response header) and the serving
	// peer's device id (X-Waired-Inference-Peer; "" when this device
	// served) each time a dispatched request commits a successful
	// response. Used for visibility (the last-served record surfaced by
	// the statusline, #602; peer attribution since #601 made the Claude
	// surface mesh-capable — without it a peer-served response would be
	// misreported as local). Never invoked on fallback or on responses
	// without the model header. Nil == no-op.
	OnServed func(modelID, peerDeviceID string)

	// Logger is optional; defaults to slog.Default().
	Logger *slog.Logger
}

// Server is the plain-HTTP loopback Anthropic proxy.
type Server struct {
	cfg     Config
	deps    Deps
	log     *slog.Logger
	rp      *httputil.ReverseProxy
	httpSrv *http.Server

	// lastMainModel holds the most recent real (non-waired) model id
	// observed on the message paths — the rewrite target for
	// subagent-labelled bodies on real-Anthropic legs (#646). See
	// model_rewrite.go.
	lastMainModel atomic.Value // string
}

// NewServer validates deps and wires the routing handler + passthrough reverse
// proxy. It does not bind any socket; call Serve or ListenAndServe.
func NewServer(cfg Config, deps Deps) (*Server, error) {
	if deps.PassthroughTransport == nil {
		return nil, errors.New("intercept: PassthroughTransport is required")
	}
	if cfg.UpstreamScheme == "" {
		cfg.UpstreamScheme = "https"
	}
	if cfg.UpstreamHost == "" {
		cfg.UpstreamHost = "api.anthropic.com"
	}
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	s := &Server{cfg: cfg, deps: deps, log: log}
	s.rp = &httputil.ReverseProxy{
		// Rewrite (not Director) so the proxy does NOT inject X-Forwarded-*
		// headers: passthrough must look like the original client talking
		// straight to Anthropic. The upstream host is the FIXED real API host
		// (cfg.UpstreamHost), NOT pr.In.Host — Claude reaches us at the loopback
		// base URL, so the inbound Host is 127.0.0.1:<port>; routing there would
		// loop passthrough back onto this listener.
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = cfg.UpstreamScheme
			pr.Out.URL.Host = cfg.UpstreamHost
			pr.Out.Host = cfg.UpstreamHost
		},
		Transport:    deps.PassthroughTransport,
		ErrorHandler: s.passthroughError,
	}
	s.httpSrv = &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s, nil
}

// handler builds the routing mux. ServeMux exact-matches the two message paths
// and routes everything else (including unknown /v1/messages/* subpaths) to
// passthrough via the "/" catch-all.
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", s.routeInference)
	mux.HandleFunc("/v1/messages/count_tokens", s.routeInference)
	// #623: serve model discovery locally (route-aware) so Claude Code
	// learns the effective LOCAL context window instead of the real
	// Anthropic 1M/200k metadata. The trailing-slash pattern also catches
	// /v1/models/{id}.
	mux.HandleFunc("/v1/models", s.routeModels)
	mux.HandleFunc("/v1/models/", s.routeModels)
	mux.HandleFunc("/", s.passthrough)
	return mux
}

// Handler exposes the routing handler for tests that drive it over plain HTTP
// via httptest.
func (s *Server) Handler() http.Handler { return s.handler() }

// routeInference dispatches the message paths per the unified per-class
// routing policy. Each traffic class (main / subagents) independently routes
// auto / waired / anthropic. The two classes' routes are read first; only
// when they DIFFER is the request body classified (main vs sub) to pick this
// request's route — the default (subagents follow main) keeps the historical
// no-classification fast path.
func (s *Server) routeInference(w http.ResponseWriter, r *http.Request) {
	// #52: a reserved /model directive id forces this request's route,
	// overriding the per-class /waired-route policy. Opt-in; only when on do we
	// always buffer+parse to read the id (a directive request rides the
	// buffered body straight into dispatchRoute). A non-directive id, a missing
	// model, or an over-cap/unreadable body falls through to the per-class
	// policy below — fail-open, exactly as the classify path already handles an
	// unbufferable body.
	if s.cfg.ModelRouteDirectives {
		if body, buffered := readCappedBody(r, maxFallbackBodyBytes); buffered {
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			if model, ok := bodyModel(body); ok {
				if route, forced := directiveRoute(model); forced {
					class := classMain
					if s.deps.ClassifyModel != nil {
						class = s.deps.ClassifyModel(model)
					}
					s.log.Debug("intercept: model-route directive forcing route",
						"path", r.URL.Path, "model", model, "route", route, "class", class)
					s.dispatchRoute(w, r, route, class, body)
					return
				}
			}
		}
	}

	mainRoute := s.classRoute(classMain)
	subRoute := s.classRoute(classSub)
	if mainRoute == subRoute {
		s.dispatchRoute(w, r, mainRoute, classMain, nil)
		return
	}
	// Classes diverge — classify this request. Buffer the body (bounded); an
	// over-cap or unreadable body cannot be classified and rides the main
	// route (fail-open, matching dispatchAuto's own no-buffer handling).
	class := classMain
	var body []byte
	if b, buffered := readCappedBody(r, maxFallbackBodyBytes); buffered {
		body = b
		if s.deps.ClassifyModel != nil {
			if model, ok := bodyModel(body); ok {
				class = s.deps.ClassifyModel(model)
			}
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
	}
	route := mainRoute
	if class == classSub {
		route = subRoute
	}
	s.dispatchRoute(w, r, route, class, body)
}

// dispatchRoute serves one message-path request for a resolved route.
// class/body are the classified class and (for the anthropic leg) the
// buffered body; body may be nil, in which case the anthropic leg buffers it
// itself. waired never leaves the device (it serves locally even while
// Degraded, surfacing the local error rather than leaking upstream); auto
// fails open to Anthropic when local inference is down or degraded; anthropic
// degrades to local only on a transport-unreachable upstream.
func (s *Server) dispatchRoute(w http.ResponseWriter, r *http.Request, route, class string, body []byte) {
	switch route {
	case routeAnthropic:
		s.log.Debug("intercept: route=anthropic, passthrough", "path", r.URL.Path, "class", class)
		if body == nil {
			b, buffered := readCappedBody(r, maxFallbackBodyBytes)
			if !buffered {
				// Over-cap / unreadable: cannot buffer for the degrade leg;
				// plain passthrough (r.Body restored to the full stream).
				s.passthroughMessages(w, r)
				return
			}
			body = b
		}
		s.passthroughWithLocalDegrade(w, r, body, class)
	case routeWaired:
		// Never leaks upstream: serve locally even when Degraded (the local
		// handler surfaces its own error). Only a wholly-unwired local
		// inference yields the 503.
		if s.deps.LocalInference == nil {
			s.log.Warn("intercept: route=waired but no local inference wired", "path", r.URL.Path)
			s.localUnavailable(w)
			return
		}
		s.dispatchLocal(s.observeLocalModel(w), r)
	default: // routeAuto
		if s.deps.LocalInference == nil || (s.deps.Degraded != nil && s.deps.Degraded()) {
			s.log.Debug("intercept: fail-open passthrough for message path",
				"path", r.URL.Path, "local_inference_nil", s.deps.LocalInference == nil)
			s.passthroughMessages(w, r)
			return
		}
		s.dispatchAuto(s.observeLocalModel(w), r, class)
	}
}

// classRoute resolves one traffic class's live route, defaulting to auto.
func (s *Server) classRoute(class string) string {
	if s.deps.ClassRoute == nil {
		return routeAuto
	}
	if r := s.deps.ClassRoute(class); r != "" {
		return r
	}
	return routeAuto
}

// passthroughWithLocalDegrade relays an anthropic-target message-path
// request to the real Anthropic API and, when the upstream cannot be
// reached at all (transport-level failure, no response byte received),
// serves the request locally instead of failing the turn (#665). It is
// the mirror image of dispatchAuto's local→Anthropic fallback with the
// same pre-first-byte discipline: an HTTP error from Anthropic
// (401/429/5xx) IS a response and is relayed verbatim, and once bytes
// stream the response is committed. The persisted policy is never
// demoted; the degrade is recorded via OnNodeFallback so it is not
// silent.
func (s *Server) passthroughWithLocalDegrade(w http.ResponseWriter, r *http.Request, body []byte, class string) {
	// The upstream leg carries the rewritten body (a waired/* id is
	// meaningless upstream, #646); the local degrade leg below replays
	// the ORIGINAL bytes — the local gateway is exactly the layer that
	// understands waired ids.
	upstreamBody := s.preparePassthroughBody(body, r.URL.Path)
	out := r.Clone(r.Context())
	out.Body = io.NopCloser(bytes.NewReader(upstreamBody))
	out.ContentLength = int64(len(upstreamBody))

	// Per-request shallow copy of the shared proxy: same Rewrite +
	// Transport, but a transport failure is captured instead of being
	// rendered as a 502. The ReverseProxy guarantees nothing has been
	// written to w when ErrorHandler runs (the round trip failed before
	// any response byte), so the writer is still clean for the local
	// leg.
	var transportErr error
	rp := *s.rp
	rp.ErrorHandler = func(_ http.ResponseWriter, _ *http.Request, err error) {
		transportErr = err
	}
	rp.ServeHTTP(w, out)
	if transportErr == nil {
		return // upstream responded (success or its own error), relayed verbatim
	}
	if r.Context().Err() != nil {
		// A cancelled inbound request also lands in ErrorHandler; the
		// client is gone, nothing to degrade for.
		return
	}
	if s.deps.LocalInference == nil {
		s.passthroughError(w, r, transportErr)
		return
	}
	const reason = "anthropic_unreachable"
	s.log.Warn("intercept: anthropic-target upstream unreachable; serving locally",
		"class", class, "path", r.URL.Path, "err", transportErr)
	if s.deps.OnNodeFallback != nil {
		s.deps.OnNodeFallback(class, reason)
	}
	w.Header().Set(fallbackHeader, "local; reason="+reason)
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	s.dispatchLocal(s.observeLocalModel(w), r)
}

// passthroughMessages sends a message-path request to the real Anthropic
// API, rewriting a waired/* model id first (#646): managed settings pin
// subagents to a model id only the local gateway understands, so a
// verbatim passthrough of those bodies would be rejected upstream —
// breaking the route=anthropic escape hatch and the degraded fail-open
// for every subagent turn. Fail-open invariant preserved: an unreadable
// or over-cap body passes through unmodified (only subagent-labelled
// bodies carry the prefix, and those come from Claude Code well under
// the cap).
func (s *Server) passthroughMessages(w http.ResponseWriter, r *http.Request) {
	body, buffered := readCappedBody(r, maxFallbackBodyBytes)
	if !buffered {
		s.passthrough(w, r)
		return
	}
	body = s.preparePassthroughBody(body, r.URL.Path)
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	s.passthrough(w, r)
}

// routeModels serves GET /v1/models(/{id}) — Claude Code's gateway model
// discovery (#623). When serving locally the gateway returns the LOCAL
// catalog with each model's effective context window (max_input_tokens),
// so Claude Code compacts against the real window instead of overrunning
// it. This is best-effort (discovery runs once at startup): the per-request
// 400 in routeInference is the invariant. Model discovery is not classified
// (it carries no traffic class) so it follows the MAIN route, without the
// auto-mode fallback dance — a GET has no body to replay, and model metadata
// is not a "served model" event:
//
//   - anthropic  → passthrough (real Anthropic model list).
//   - waired     → serve locally (the list needs only manifests + tuning,
//     no running engine); passthrough only if no local handler is wired.
//   - auto       → serve locally when healthy, else passthrough.
//
// On any passthrough leg the reserved #52 directive ids are spliced into the
// upstream list (passthroughModels) when the feature is on, so they surface in
// Claude Code's /model picker even on the anthropic route — the only way to
// switch back to Waired from /model once the session started on anthropic. The
// local-serving path (dispatchLocal → gateway) already advertises them.
func (s *Server) routeModels(w http.ResponseWriter, r *http.Request) {
	switch s.classRoute(classMain) {
	case routeAnthropic:
		s.passthroughModels(w, r)
	case routeWaired:
		if s.deps.LocalInference == nil {
			s.passthroughModels(w, r)
			return
		}
		s.dispatchLocal(w, r)
	default: // routeAuto
		if s.deps.LocalInference == nil || (s.deps.Degraded != nil && s.deps.Degraded()) {
			s.passthroughModels(w, r)
			return
		}
		s.dispatchLocal(w, r)
	}
}

// localRequest clones r onto the gateway's /anthropic route convention
// (handlers.go registers /anthropic/v1/messages...).
func (s *Server) localRequest(r *http.Request) *http.Request {
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/anthropic" + r.URL.Path
	r2.RequestURI = "" // unused when dispatching to a handler directly; keep it clean
	// #757 anti-spoof: strip any client-supplied fallback-allowed header on
	// EVERY local leg. Only dispatchAuto re-adds it after this call, so a
	// pinned (waired) leg can never carry it — Claude Code cannot force the
	// gateway's TTFB abort on a route the operator locked to local.
	r2.Header.Del(fallbackAllowedHeader)
	return r2
}

// dispatchLocal serves the request from the local gateway with no fallback.
func (s *Server) dispatchLocal(w http.ResponseWriter, r *http.Request) {
	s.deps.LocalInference.ServeHTTP(w, s.localRequest(r))
}

// observeLocalModel wraps the client ResponseWriter so a committed local
// success reports the gateway's mapped model id to Deps.OnLocalServed
// (#602). Wrapping the OUTER writer covers every local-serving shape with
// one mechanism: dispatchLocal writes to it directly, dispatchAuto's
// fallbackRecorder copies its staged headers onto it on commit, and a
// fallback passthrough writes an upstream response that never carries the
// header (so nothing fires).
func (s *Server) observeLocalModel(w http.ResponseWriter) http.ResponseWriter {
	if s.deps.OnServed == nil {
		return w
	}
	return &localModelObserver{ResponseWriter: w, onServed: s.deps.OnServed}
}

// localModelObserver reports localModelHeader (plus the serving peer
// from inferencePeerHeader) once, at commit time (first
// WriteHeader/Write), and only for non-error statuses.
type localModelObserver struct {
	http.ResponseWriter
	onServed func(modelID, peerDeviceID string)
	observed bool
}

func (o *localModelObserver) WriteHeader(code int) {
	o.observe(code)
	o.ResponseWriter.WriteHeader(code)
}

func (o *localModelObserver) Write(p []byte) (int, error) {
	o.observe(http.StatusOK) // implicit 200, mirror net/http
	return o.ResponseWriter.Write(p)
}

// Flush keeps the gateway's SSE streaming path working through the wrapper
// (fallbackRecorder and ReverseProxy both type-assert http.Flusher).
func (o *localModelObserver) Flush() {
	o.observe(http.StatusOK)
	if f, ok := o.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (o *localModelObserver) observe(status int) {
	if o.observed {
		return
	}
	o.observed = true
	if status >= 400 {
		return
	}
	if m := o.ResponseWriter.Header().Get(localModelHeader); m != "" {
		o.onServed(m, o.ResponseWriter.Header().Get(inferencePeerHeader))
	}
}

// dispatchAuto serves locally (the "auto" route) and, when the request body
// is small enough to replay, retries a pre-first-byte local error against the
// real Anthropic API so the turn keeps working. The privacy opt-out that used
// to gate this is now the "waired" route (dispatchLocal), which never reaches
// here.
func (s *Server) dispatchAuto(w http.ResponseWriter, r *http.Request, class string) {
	body, buffered := readCappedBody(r, maxFallbackBodyBytes)
	if !buffered {
		// Unreadable or over the cap: r.Body has been restored to the full
		// stream; serve locally with no fallback.
		s.dispatchLocal(w, r)
		return
	}
	// Remember the main-loop model (#646): it is the rewrite target for
	// subagent-labelled bodies should this — or any later — request take
	// a real-Anthropic leg.
	if model, ok := bodyModel(body); ok {
		s.observeMainModel(model)
	}

	rec := newFallbackRecorder(w)
	local := s.localRequest(r)
	// #757: this is the ONLY leg that authorizes the gateway's pre-commit
	// TTFB abort — auto mode has a real fallback (below). localRequest just
	// stripped any spoofed copy, so setting it here is the sole source.
	local.Header.Set(fallbackAllowedHeader, "1")
	local.Body = io.NopCloser(bytes.NewReader(body))
	local.ContentLength = int64(len(body))
	s.deps.LocalInference.ServeHTTP(rec, local)

	if !rec.eligibleForFallback() {
		rec.flushBuffered() // commit whatever the handler produced (already streamed, or a bodyless 2xx)
		return
	}

	// #623: a context-window overflow 400 must reach Claude Code so it
	// auto-compacts and keeps serving locally — NOT fall back to the real
	// Anthropic API (which would abandon local serving for the rest of the
	// session). The gateway stages this exact marker (rec is uncommitted
	// here, so Header() is the staged map) on such a 400.
	if rec.Header().Get(localErrorHeader) == localErrContextOverflow {
		rec.flushBuffered() // surface the 400 to the client
		return
	}

	reason := fmt.Sprintf("local_status_%d", rec.status)
	// #757: rec is uncommitted here, so Header() is the staged map — capture
	// the serving peer + TTFB budget for the reroute notice before they are
	// discarded with the rest of the staged error on fallback.
	localErr := rec.Header().Get(localErrorHeader)
	peer := rec.Header().Get(inferencePeerHeader)
	budgetMs := rec.Header().Get(ttfbBudgetHeader)
	if localErr != "" {
		// the marker is read for the reason and then discarded with the rest
		// of the staged error on fallback; it never reaches the client.
		reason = "local_" + localErr
	}
	s.log.Warn("intercept: local inference errored before first byte; falling back to upstream",
		"path", r.URL.Path, "status", rec.status, "reason", reason)
	if s.deps.OnFallback != nil {
		s.deps.OnFallback(reason)
	}
	// The replay goes to the real Anthropic API: a waired/* model id
	// (subagent label, #646) must be rewritten or upstream rejects it
	// and the fallback saves nothing.
	if rewritten, ok := rewritePassthroughModel(body, s.passthroughReplacement()); ok {
		s.log.Info("intercept: rewrote waired model id for fallback replay",
			"path", r.URL.Path, "to", s.passthroughReplacement())
		body = rewritten
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	w.Header().Set(fallbackHeader, "anthropic; reason="+reason)
	// #757: surface the reroute in-conversation (not just status/tray/log) so
	// the user can tell a turn/subagent left the mesh. Fail-open + tool_use-safe
	// (reroute_notice.go); the OnFallback record above still fires regardless.
	if s.cfg.AnnotateReroute {
		s.passthroughWithNotice(w, r, buildRerouteNotice(class, localErr, peer, budgetMs))
		return
	}
	s.passthrough(w, r)
}

// localUnavailable renders an Anthropic-shaped error for the "waired" route
// when local inference is unavailable (the route never leaks upstream).
func (s *Server) localUnavailable(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "waired_local_unavailable",
			"message": "waired claude route is 'waired' but local inference is not available",
		},
	})
}

// passthrough relays the request to the real upstream via the reverse proxy.
func (s *Server) passthrough(w http.ResponseWriter, r *http.Request) {
	s.rp.ServeHTTP(w, r)
}

// passthroughError renders an Anthropic-shaped JSON error when the upstream is
// unreachable, instead of the reverse proxy's default bare 502.
func (s *Server) passthroughError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Warn("intercept: passthrough to upstream failed", "host", r.Host, "path", r.URL.Path, "err", err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "waired_upstream_unreachable",
			"message": fmt.Sprintf("waired proxy could not reach the upstream API: %v", err),
		},
	})
}

// Serve serves on ln (a plain loopback listener). Shutdown is triggered by ctx
// cancellation.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.httpSrv.Shutdown(shutdownCtx)
	}()
	if err := s.httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// ListenAndServe binds cfg.Addr as a plain loopback TCP listener and serves
// until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("intercept: listen %s: %w", s.cfg.Addr, err)
	}
	return s.Serve(ctx, ln)
}
