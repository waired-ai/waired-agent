// Package intercept implements the plain-HTTP loopback proxy that Claude Code
// talks to via the managed-settings `ANTHROPIC_BASE_URL`. It is the successor
// to the retired :443 MITM transparent proxy: same routing, no TLS termination,
// no CA, no /etc/hosts redirect.
//
// Claude Code (configured credential-less, so it keeps its claude.ai
// subscription) sends requests to `http://127.0.0.1:<ClaudeGatewayPort>` and
// this server routes each one:
//
//   - POST /v1/messages and /v1/messages/count_tokens go where the turn's
//     model id says. A Waired id is served by the LOCAL gateway (waired's
//     Anthropic->OpenAI->Ollama translation) or fails with a reason; an id
//     the real Anthropic API serves passes through; an id neither side owns
//     is served here, because relaying it would only buy a 404 there.
//   - Everything else (OAuth, quota checks, telemetry on api.anthropic.com)
//     passes through to the real Anthropic API verbatim, carrying Claude's
//     subscription OAuth bearer so the subscription/auto-mode stay intact.
//
// A turn stays on the side it named. waired holds no route that could move it
// to the other side on waired's own judgement of a failure or an unreachable
// upstream — the turn fails instead, with a reason Claude Code shows at once
// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md,
// owner ruling waired-ai/waired#1313). The property this listener must keep is
// therefore honesty rather than availability: nothing but a turn carrying a
// real Anthropic model id leaves this machine.
//
// No bearer token is enforced here: credential-less Claude presents its
// subscription OAuth token, not waired's gateway token, so the loopback bind is
// the trust boundary (same posture as the no-token data-plane gateway). The
// LocalInference handler is therefore the BARE gateway HandlerSet, not the
// token-gated loopback gateway.Server.
//
// The bind is not the whole boundary, though. A web page the user visits can
// reach a loopback listener by DNS-rebinding, and its connection genuinely
// comes from 127.0.0.1 — so the request itself has to be checked
// (waired-ai/waired#1195). Those checks are shared with the other loopback
// listeners and live in internal/loopbackguard; this package does not import
// it. cmd/waired-agent composes the guard and hands it in as Deps.Guard, which
// keeps this package stdlib-only.
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
	"net/textproto"
	"strings"
	"sync/atomic"
	"time"
)

// Where a turn runs. The model id it carries decides, and nothing else does:
// a Waired id runs on a Waired node or fails with a reason, an id the real
// Anthropic API serves passes through, and an id neither side owns runs on a
// Waired node too — so nothing but a turn carrying a real Anthropic id leaves
// this machine. waired holds no route of its own that could move a turn
// between the two sides
// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md,
// owner ruling waired-ai/waired#1313).
//
// The values are also what the daemon publishes as last_request_route, which
// is what the routing sentinel asserts against
// (docs/decisions/20260829/1655-the-sentinel-observes-the-decision.md).
// Literals are duplicated to keep this package stdlib-only.
const (
	routeWaired    = "waired"
	routeAnthropic = "anthropic"
)

// maxInspectBodyBytes caps the request body buffered so the model id and the
// auto-mode classifier shape can be read. A larger request streams on
// unexamined, which is the posture each of those checks already took on its
// own. A var (not const) so tests can shrink it without a multi-MB fixture.
var maxInspectBodyBytes int64 = 8 << 20 // 8 MiB

// Traffic classes the per-class routing policy is keyed by (#645). String
// values mirror state.ClaudeClass* — literals duplicated to keep this
// fail-open package stdlib-only; keep both sides in sync.
const (
	classMain = "main"
	classSub  = "sub"
)

// localModelHeader mirrors gateway.HeaderLocalModel: the catalog model id
// of the selection that answered, which the local gateway stamps on every
// successful Anthropic messages selection — a local leg and a mesh leg
// alike (before #755 only on a model-mapped one, which is why a request
// naming a catalog id directly recorded nothing here). The intercept reads
// it at commit time to report which model answered a waired-served Claude
// request (#602). The literal is duplicated here to keep this fail-open
// package stdlib-only — keep both sides in sync.
const localModelHeader = "X-Waired-Local-Model"

// inferencePeerHeader mirrors gateway.HeaderInferencePeer: the mesh peer
// DeviceID the gateway stamps on every remote-served response. Since the
// Claude surface became mesh-capable (#601) a committed response may have
// been served by a peer; the intercept reads this at commit time so peer
// serving is attributed instead of being misreported as local. The
// literal is duplicated here to keep this fail-open package stdlib-only —
// keep both sides in sync.
const inferencePeerHeader = "X-Waired-Inference-Peer"

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

	// ModelRouteDirectives (#52), when true, makes waired's own reserved
	// /model ids mean what they say: an id naming this device, a peer, or any
	// Waired node runs the turn on Waired. Default on in agentconfig; the
	// gateway advertises the same ids in /v1/models under the same flag, so
	// turning it off hides the rows and stops honouring them together.
	//
	// It gates the ADVERTISEMENT only. An id that arrives is honoured either
	// way: Claude Code's picker cache has no TTL, so a session can carry an
	// id this build no longer offers, and the alternative — sending a Waired
	// id upstream — is a 404 there. An id the real Anthropic API serves
	// passes through whatever this is set to, because naming a model is
	// naming where it runs (waired-agent#1091).
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

	// PassthroughTransport reaches the REAL api.anthropic.com. With the
	// /etc/hosts redirect retired this is an ordinary http.Transport;
	// standard DNS already resolves the real host. Required.
	PassthroughTransport http.RoundTripper

	// ClassifyModel derives the traffic class ("main"/"sub", #646) from the
	// request's model id, mirroring the gateway classifier. The class no
	// longer picks a route — it sizes the peer leg's grace period, and it is
	// what the served/requested records are keyed by. Nil == everything main.
	ClassifyModel func(modelID string) string

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

	// OnRequest, if set, is invoked with the model id a turn CARRIED and the
	// route that id and the policy resolved to, before the turn is dispatched.
	// OnServed answers "what answered"; this answers "what was asked for", and
	// the two are different questions now that a /model pick can send a turn
	// somewhere the machine-wide policy did not (waired-agent#1037). A turn
	// answered by the real Anthropic API produces no OnServed at all, so
	// without this the surfaces could describe only half the traffic.
	//
	// Turn-shaped requests only: the token-counting probe is not a turn and
	// would overwrite the record with something the user never sent. Nil ==
	// no-op.
	OnRequest func(model, route, class string)

	// Guard, if set, wraps the whole route table — every route, including the
	// "/" passthrough catch-all. It is how the loopback guards reach this
	// listener without the package importing them: cmd/waired-agent composes
	// internal/loopbackguard (peer address, then Host and Origin
	// allow-listing) and passes the result in, which keeps this fail-open
	// package stdlib-only (waired-ai/waired#1195).
	//
	// Nil leaves the mux bare, which is what the package's own tests want.
	// Production always sets it; cmd/waired-agent/proxy_browser_hardening_test.go
	// is what stops that wiring rotting away.
	Guard func(http.Handler) http.Handler

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

// handler builds the routing mux and wraps it in the loopback guards.
// ServeMux exact-matches the two message paths and routes everything else
// (including unknown /v1/messages/* subpaths) to passthrough via the "/"
// catch-all.
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

	// waired-ai/waired#1195: the loopback guards, composed by cmd/waired-agent
	// (see Deps.Guard). Applied here rather than around httpSrv.Handler so
	// Handler() — what the tests drive — carries the same stack production
	// serves. nil leaves the mux bare.
	if s.deps.Guard != nil {
		return s.deps.Guard(mux)
	}
	return mux
}

// Handler exposes the routing handler for tests that drive it over plain HTTP
// via httptest.
func (s *Server) Handler() http.Handler { return s.handler() }

// routeInference dispatches the message paths on the model id the request
// carries. There is no policy left to consult: a Waired id is served here, an
// id the real Anthropic API serves is relayed there, and an id neither side
// owns is served here too, so a turn only leaves this machine when it names a
// model the real API answers
// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md).
func (s *Server) routeInference(w http.ResponseWriter, r *http.Request) {
	// One bounded read serves every decision below — the auto-mode classifier
	// check and the model id. An over-cap or unreadable body cannot be
	// inspected at all; readCappedBody has restored r.Body to the full stream
	// and the request is served here, which is the side that can read it.
	body, buffered := readCappedBody(r, maxInspectBodyBytes)
	if buffered {
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
	}

	// waired-agent#1041: Claude Code's auto-mode safety classifier is answered
	// by the real Anthropic API whatever the turn's own model id says (owner
	// ruling, waired-agent#1041; the ruling itself is untouched by the auto
	// retirement — see docs/decisions/20260828/0221). Claude Code chooses that
	// model itself and compares its verdict against thresholds pinned to it,
	// so serving the request here would decide a permission question with a
	// model neither the user nor Claude Code picked.
	//
	// Checked BEFORE the model id below, and by shape rather than by id,
	// because Claude Code re-sends the classifier under the SESSION's model
	// once a classifier request has failed — on this surface that can be a
	// Waired id, and matching the id first would route the permission
	// decision straight back onto this device.
	if buffered && bodyIsAutoModeClassifier(body) {
		s.log.Debug("intercept: auto-mode classifier request routed to the Anthropic API",
			"path", r.URL.Path)
		s.dispatchRoute(w, r, routeAnthropic, classMain, body)
		return
	}

	route, class := routeWaired, classMain
	if buffered {
		if model, ok := bodyModel(body); ok {
			route = routeForModel(model)
			if s.deps.ClassifyModel != nil {
				class = s.deps.ClassifyModel(model)
			}
		}
	}
	s.dispatchRoute(w, r, route, class, body)
}

// routeForModel reports which side a turn carrying this id runs on. An id
// neither side owns stays here: it is not a model the real Anthropic API
// answers, so relaying it would only buy a 404 there, and the sentinel's
// claim is that nothing but a real Anthropic id leaves the machine.
func routeForModel(model string) string {
	if route, forced := directiveRoute(model); forced {
		return route
	}
	return routeWaired
}

// dispatchRoute serves one message-path request on the side its model id
// named. body is the buffered request body, or nil when it could not be read.
// Neither side crosses to the other: a Waired turn is answered here or fails
// with a reason the client shows at once, and an Anthropic turn is relayed
// with its own errors intact.
func (s *Server) dispatchRoute(w http.ResponseWriter, r *http.Request, route, class string, body []byte) {
	s.observeRequestedModel(r, route, class, body)
	if route == routeAnthropic {
		s.log.Debug("intercept: the model names the real Anthropic API, passing through",
			"path", r.URL.Path, "class", class)
		s.passthroughBody(w, r, body)
		return
	}
	if s.deps.LocalInference == nil {
		s.log.Warn("intercept: a Waired model id arrived but no local inference is wired",
			"path", r.URL.Path)
		writeNothingHereCanServe(w)
		return
	}
	s.dispatchLocal(s.observeLocalModel(w), r)
}

// passthroughBody relays a message-path request to the real Anthropic API,
// rewriting a waired/* model id first (#646) when the body was buffered. An
// upstream that cannot be reached is an error the client sees: waired does
// not answer an Anthropic-addressed turn with a local model
// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md
// decision 3, retiring waired-ai/waired#665).
func (s *Server) passthroughBody(w http.ResponseWriter, r *http.Request, body []byte) {
	if body == nil {
		s.passthroughMessages(w, r)
		return
	}
	out, replaced := s.preparePassthroughBody(body, r.URL.Path)
	r.Body = io.NopCloser(bytes.NewReader(out))
	r.ContentLength = int64(len(out))
	if replaced != "" {
		// If upstream answers "no such model", the id waired chose is the
		// thing that is wrong. Retire it rather than repeating it for the
		// rest of the process lifetime (waired-agent#1036).
		w = s.observeReplacementRejection(w, replaced)
	}
	s.passthrough(w, r)
}

// passthroughMessages sends a message-path request whose body was not
// buffered to the real Anthropic API, rewriting a waired/* model id first
// (#646): managed settings pin subagents to a model id only the local
// gateway understands, so a verbatim passthrough of those bodies would be
// rejected upstream. An unreadable or over-cap body passes through
// unmodified (only subagent-labelled bodies carry the prefix, and those come
// from Claude Code well under the cap).
func (s *Server) passthroughMessages(w http.ResponseWriter, r *http.Request) {
	body, buffered := readCappedBody(r, maxInspectBodyBytes)
	if !buffered {
		s.passthrough(w, r)
		return
	}
	s.passthroughBody(w, r, body)
}

// routeModels serves GET /v1/models(/{id}) — Claude Code's gateway model
// discovery (#623). Served locally whenever a local handler is wired: the
// list needs only manifests and tuning, no running engine, and it is what
// tells Claude Code the effective LOCAL context window so it compacts
// against the real one instead of overrunning it. With no local handler at
// all the request passes through, and the reserved #52 ids are spliced into
// the upstream list (passthroughModels) so they still surface in the /model
// picker.
func (s *Server) routeModels(w http.ResponseWriter, r *http.Request) {
	if s.deps.LocalInference == nil {
		s.passthroughModels(w, r)
		return
	}
	s.dispatchLocal(w, r)
}

// localRequest builds the request handed to local inference: the gateway's
// /anthropic route convention (handlers.go registers /anthropic/v1/messages…)
// carrying only the headers on localHeaderAllowlist.
//
// It is built rather than cloned so that no client credential can reach an
// engine, a mesh peer, a log line, or the observability ring
// (waired-agent#1183). Claude Code signs in with a claude.ai subscription and
// sends its OAuth bearer on every request; the two egress paths already drop
// it (internal/gateway/openai.go proxyToEngine, anthropic.go postToEngine
// both build a fresh request), but that made the property true by the shape
// of two call sites rather than by a boundary. Here it is the boundary.
//
// Anthropic's gateway contract distinguishes headers a gateway must forward
// upstream from headers it may consume
// (https://code.claude.com/docs/en/llm-gateway-protocol#request-headers); the
// passthrough leg keeps forwarding everything byte for byte, and only this
// leg — which does not go to Anthropic — is rebuilt.
func (s *Server) localRequest(r *http.Request) *http.Request {
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/anthropic" + r.URL.Path
	r2.RequestURI = "" // unused when dispatching to a handler directly; keep it clean
	r2.Header = allowedLocalHeaders(r.Header)
	return r2
}

// localHeaderAllowlist is every header name the local-inference leg carries.
// Anything absent from it — Authorization, x-api-key, Cookie,
// Proxy-Authorization, User-Agent, and any header this build has not heard
// of — is not copied, so a header added upstream is dropped by default
// rather than forwarded by default.
//
//   - the content negotiation the engine and the peer leg need,
//   - anthropic-version / anthropic-beta, which carry the request's own
//     capabilities (the 1M tier arrives here, waired-agent#1036),
//   - x-claude-code-*, Claude Code's attribution ids, used for usage
//     attribution only.
//
// waired's own X-Waired-* request headers are admitted by prefix below.
var localHeaderAllowlist = map[string]bool{
	"Content-Type":                  true,
	"Accept":                        true,
	"Accept-Encoding":               true,
	"Anthropic-Version":             true,
	"Anthropic-Beta":                true,
	"X-Claude-Code-Session-Id":      true,
	"X-Claude-Code-Agent-Id":        true,
	"X-Claude-Code-Parent-Agent-Id": true,
}

// wairedHeaderPrefix admits waired's own request headers, which this process
// sets and reads and which carry nothing from the client.
const wairedHeaderPrefix = "X-Waired-"

// allowedLocalHeaders returns a fresh header map holding only the allowed
// names from in. Keys are compared in canonical MIME form, which is how
// net/http stores them.
func allowedLocalHeaders(in http.Header) http.Header {
	out := make(http.Header, len(localHeaderAllowlist))
	for name, values := range in {
		canonical := textproto.CanonicalMIMEHeaderKey(name)
		if !localHeaderAllowlist[canonical] && !strings.HasPrefix(canonical, wairedHeaderPrefix) {
			continue
		}
		out[canonical] = append([]string(nil), values...)
	}
	return out
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

// observeRequestedModel reports the model id this turn carried, and where that
// id sent it. It reads the already-buffered body when there is one and does not
// buffer on its own: a record for the surfaces is not worth changing how a
// request is handled, and the configuration that does not buffer (directives
// off, both classes on one route) is also the one where the id decides nothing.
func (s *Server) observeRequestedModel(r *http.Request, route, class string, body []byte) {
	if s.deps.OnRequest == nil || body == nil || r.URL.Path != "/v1/messages" {
		return
	}
	if model, ok := bodyModel(body); ok && model != "" {
		s.deps.OnRequest(model, route, class)
	}
}

// observeReplacementRejection wraps w so that a 404 from the real Anthropic API
// retires the model id waired substituted into this replay. Only the observed
// main-loop model can go stale this way; the configured override and the
// default alias are left alone by forgetObservedMainModel.
func (s *Server) observeReplacementRejection(w http.ResponseWriter, replacement string) http.ResponseWriter {
	return &replacementRejectionObserver{ResponseWriter: w, model: replacement, forget: s.forgetObservedMainModel}
}

// replacementRejectionObserver watches the upstream status for the one code
// that means "the id waired chose is not a model": 404. Every other failure is
// about the request or the account, not the substitution.
type replacementRejectionObserver struct {
	http.ResponseWriter
	model    string
	forget   func(string)
	observed bool
}

func (o *replacementRejectionObserver) WriteHeader(code int) {
	o.observe(code)
	o.ResponseWriter.WriteHeader(code)
}

func (o *replacementRejectionObserver) Write(p []byte) (int, error) {
	o.observe(http.StatusOK)
	return o.ResponseWriter.Write(p)
}

// Flush keeps SSE streaming working through the wrapper (ReverseProxy
// type-asserts http.Flusher).
func (o *replacementRejectionObserver) Flush() {
	o.observe(http.StatusOK)
	if f, ok := o.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (o *replacementRejectionObserver) observe(status int) {
	if o.observed {
		return
	}
	o.observed = true
	if status == http.StatusNotFound {
		o.forget(o.model)
	}
}

// writeNothingHereCanServe answers a Waired-addressed turn that arrived on a
// machine with no local inference wired at all.
//
// The status is 400 because that is the one Claude Code shows at once and
// verbatim: it retries 5xx, 529 and 429 up to ten times before showing
// anything, rewrites a 401/403 as an authentication failure, and replaces a
// 404's message with its own "the selected model may not exist"
// (https://code.claude.com/docs/en/errors#automatic-retries; measured against
// Claude Code 2.1.259 on 2026-09-03, one request for 400 and the message
// intact). A turn that nothing can answer is not a transient condition, so
// the ten retries the old 503 bought were a minute of an anonymous "API
// error" (waired-agent#1180). Product contract, ratified by
// docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md
// decision 4.
func writeNothingHereCanServe(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type": "waired_cannot_serve",
			"message": "Waired is not set up to answer on this computer, so this turn has nowhere to run. " +
				"Pick an Anthropic model in /model to send it to the cloud, or run `waired doctor` to see what is missing.",
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
