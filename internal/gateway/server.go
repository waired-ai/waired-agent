// Package gateway hosts the local gateway HTTP server (port 9473 by
// default), which exposes OpenAI- and Anthropic-compatible chat APIs
// to local clients (Claude Code, OpenCode, OpenClaw, curl, …) and
// proxies them — via the router and runtime adapters — to the
// appropriate backend engine.
//
// Phase A scope: OpenAI /v1/chat/completions + /v1/models, Anthropic
// /v1/messages (+ /count_tokens) backed by an Ollama-only runtime.
// Phase B fills in /v1/responses, vision/extended thinking, and the
// vLLM proxy path.
//
// The route table + handler methods live on HandlerSet (handlers.go)
// so both this loopback Server and the Phase 4 overlay inference
// listener can mount the same routes with different middleware
// chains.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/loopbackguard"
	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/internal/runtime"
)

// SelectorIface is the subset of router.Selector the gateway needs.
// Existing as an interface keeps unit tests independent of the real
// catalog / hardware / runtime registry plumbing.
type SelectorIface interface {
	Select(ctx context.Context, req router.Request) (router.Selection, error)
	// SelectK returns up to k ranked candidates without acquiring
	// admission slots. The Phase 8 probe-then-commit path uses this
	// to fan /healthz probes out in parallel before committing one
	// peer's admission slot. Wrappers fall back to a 1-element slice
	// for fakes that only implement Select (see fakeSelector in the
	// gateway test fixtures).
	SelectK(ctx context.Context, req router.Request, k int) ([]router.Candidate, error)
}

// Deps bundles every collaborator the gateway needs. Caller wires
// these from main; tests assemble fakes.
type Deps struct {
	Selector       SelectorIface
	Runtimes       *runtime.Registry
	ListManifests  func() []catalog.Manifest // for /v1/models — call returns a fresh slice every time
	HTTPClient     *http.Client              // injected so tests can target an httptest server
	AllowOpenAI    bool
	AllowAnthropic bool
	// IsPaused, if non-nil and returning true, makes every gateway
	// request short-circuit to 503 with a `waired_paused` error body
	// rather than reaching a handler. Wired up in cmd/waired-agent so
	// `waired pause` causes existing Claude Code sessions to see a
	// clean error instead of a malformed routing attempt. Nil disables
	// the gate (default in unit tests).
	IsPaused func() bool

	// PeerAdapterFactory is the Phase 4 hook that turns a
	// Selection.Runtime of the form "remote:<deviceID>" into a
	// runtime.Adapter (typically internal/runtime/peer.Adapter)
	// capable of proxying to that peer's overlay-side inference
	// listener. Returning a nil adapter or an error fails the request
	// with a 503 — the inferencemesh aggregator should have filtered
	// out unreachable peers BEFORE the Selector picked them, so a
	// factory error usually indicates the snapshot moved between the
	// Selector pass and the dispatch.
	//
	// nil here disables peer-engine routing on this listener. The
	// agent's overlay-side gateway (the one mounted on port 9474)
	// passes nil so a peer-side request never recurses to a third
	// peer; loop prevention is layered: Selector sees a nil mesh
	// snapshot AND PeerAdapterFactory is nil here.
	PeerAdapterFactory func(deviceID string) (runtime.Adapter, error)

	// LocalAdmission, when non-nil, is called for every request this
	// listener dispatches to THIS machine's engine, and returns the
	// release to run when the request lets go of it. It exists so the
	// owner's own local traffic shares one admission counter with the
	// peer-overlay listener (spec §8.2, waired#899): without it,
	// Config.Capacity only ever described overlay arrivals, the engine
	// could be oversubscribed by local + peer work at once, and the
	// owner-priority latch could not fire on a machine whose only load
	// is its owner's.
	//
	// Wired on the LOCAL surfaces (loopback gateway, Claude intercept,
	// data plane) in cmd/waired-agent. The overlay listener leaves it nil
	// — its requests are counted by the inference server's capacityGate
	// before they ever reach these handlers.
	//
	// Never consulted for remote: selections; those run on a peer,
	// not here.
	LocalAdmission func(ctx context.Context) (release func())

	// OnPeerOutcome, when non-nil, receives one verdict per request this
	// listener dispatched to a MESH PEER: which peer, and whether it
	// served the request. It is the write half of the per-peer error
	// window whose Snapshot the Selector already reads back as a
	// same-score tie-break — a window that, until waired-agent#281, was
	// consulted on every mesh selection and fed by nothing.
	//
	// deviceID is the real mesh DeviceID, peeled off Selection.Runtime,
	// because that is the key the Selector matches its snapshot against.
	// It is NOT the display identifier, which is a grant pseudonym for a
	// Public Share peer (spec §8.5) and would key a second, permanently
	// empty entry. Being a real identifier, it is an in-process routing
	// signal only: it must never be logged, returned or serialised.
	//
	// A local or external selection produces no call — it says nothing
	// about any peer — and neither does a failure that never chose one.
	//
	// Wired on the LOCAL surfaces (loopback gateway, Claude intercept,
	// data plane) in cmd/waired-agent, the same set as LocalAdmission and
	// for the same reason: those are the listeners that can send work to
	// a peer. The overlay listener leaves it nil — it holds no mesh
	// snapshot and no PeerAdapterFactory, so it cannot dispatch to a peer
	// at all. nil leaves the tie-break reading zeros, which is the
	// behaviour before waired-agent#281.
	//
	// Called synchronously at the terminal point of the handler, so an
	// implementation must not block: the production sink takes one mutex
	// for a handful of counter increments.
	OnPeerOutcome func(deviceID string, ok bool)

	// OnUsage, when non-nil, receives one UsageSample per request that
	// reached an engine (waired#829). The gateway captures token counts
	// on every surface for local telemetry regardless; this hook is what
	// forwards them somewhere, and cmd/waired-agent wires it only on the
	// peer overlay (:9474), where the Public Share batcher reports a
	// provider's usage to the control plane.
	//
	// Called synchronously at the terminal point of the handler, so an
	// implementation must not block: the production sink appends to an
	// in-memory batch under a short-lived mutex.
	OnUsage func(ctx context.Context, s UsageSample)

	// Recorder, when non-nil, receives per-request telemetry from
	// the gateway: RecordRequest at every terminal point with a
	// resolved model, RecordFallback when probe-then-commit picks
	// a peer other than the top-1 candidate, RecordProbe for each
	// real /healthz probe completion, and RecordBriefQueueRetry on
	// the 250 ms-retry path. nil disables emission entirely and
	// preserves the Phase 8 slog.Warn fallback log line so legacy
	// tests observe the same journal output.
	Recorder Recorder

	// ResolveUnknownModel, when non-nil, maps a model id that failed
	// catalog alias resolution (router.ErrModelNotFound) on the Anthropic
	// messages surface to a servable catalog model id. Wired only on the
	// Claude-intercept HandlerSet (#600) so the Anthropic ids Claude Code
	// sends (claude-*-<anything> — never in the catalog) resolve to a
	// served model instead of 404ing; nil keeps the exact-404 semantics
	// every other listener wants. class is the ClassifyModel result for
	// the request ("" when unclassified) so the per-class node policy
	// (#647) can resolve main-class traffic to whatever model the
	// operator-selected node serves. The mapping never touches the
	// request body, so the intercept's auto-mode fallback replay still
	// carries the client's original model id.
	ResolveUnknownModel func(requested, class string) (mapped string, ok bool)

	// ClassifyModel, when non-nil, derives the coding-agent traffic
	// class ("main" / "sub", state.ClaudeClass*) from the ORIGINAL
	// client model id — before any ResolveUnknownModel remap, which
	// would erase the waired/subagent marker (#645/#646). The class is
	// stamped on router.Request.Class and folded into the sticky id so
	// main and subagent legs of one conversation don't share peer
	// affinity. nil means no classification (every other listener).
	ClassifyModel func(modelID string) string
	// ContextWindowFor, when non-nil, reports the effective input-token
	// window the given (already catalog-resolved) model id can serve on
	// this host — min(manifest native window, host-sustainable applied
	// window). It powers the #623 Claude context-window work: the
	// Anthropic /v1/models advertisement and the per-request overflow
	// guard both read it so Claude Code compacts against the real local
	// window instead of silently overrunning it (Ollama would then
	// truncate the prompt head). A return of 0 means "unknown" and both
	// callers fail open (no advertisement / no 400).
	//
	// The agent wires it on every gateway surface, because the reason to
	// guard is that a prompt is about to reach an engine and that is true
	// of all of them. nil disables the behaviour, which is what a test
	// fixture and an embedder that cannot size a window get.
	ContextWindowFor func(modelID string) int

	// ClaudeModelDirectives, when true, makes the Anthropic /v1/models
	// discovery additionally advertise the reserved route-directive ids
	// (ModelWairedLocal / ModelWairedCloud, #52) so they appear in Claude
	// Code's /model picker (which filters discovered ids to
	// ^(claude|anthropic); their display names are free-form). Selecting one
	// makes the intercept force this request's route, overriding the
	// /waired-route policy. Gated by the agentconfig toggle (default on) and
	// wired only on the Claude-intercept HandlerSet; false leaves discovery
	// unchanged.
	ClaudeModelDirectives bool

	// TTFBBudget, when non-nil, returns the pre-commit time-to-first-byte
	// deadline for a PEER inference leg of the given traffic class
	// ("main" / "sub", "" when unclassified). If the selected peer returns
	// no response headers within the budget, the leg is aborted BEFORE the
	// response commits, so the intercept's auto-mode fallback (#645/#757)
	// reroutes the turn instead of hanging on a stalled-but-reachable peer.
	// A 0 return disables the deadline for that class. The deadline is a
	// generous infinite-hang backstop, NOT a snappy reroute threshold:
	// /healthz readiness does not imply the model is loaded, so a cold
	// model load legitimately lands inside this window. Armed only for
	// peer legs (remote:*) AND only when the intercept authorizes it with
	// the X-Waired-Fallback-Allowed request header (auto mode) — a pinned
	// local/waired-only leg is never aborted. Wired only on the
	// Claude-intercept HandlerSet; nil on every other listener.
	TTFBBudget func(class string) time.Duration

	// PeerWaitCeiling, when non-nil, returns how long a PEER leg of the
	// given traffic class may wait in total while the peer keeps saying it
	// is working (waired-agent#1040). A return of 0, or a value not longer
	// than TTFBBudget's, leaves that class on the flat deadline alone.
	//
	// It turns TTFBBudget from a deadline into a GRACE PERIOD for the
	// classes it is set for: nothing is asked of the peer until the budget
	// elapses, and after that the wait continues for as long as the peer's
	// own /healthz says it is serving — because what is being waited out is
	// the peer prefilling this client's prompt, which is its speed and not
	// its health. See peerwait.go for the owner ruling that shape comes
	// from, and for what ends the wait.
	//
	// Wired only on the Claude-intercept HandlerSet, and deliberately not
	// for the subagent class: "a stalled subagent is cheap to reroute" is
	// why that budget is tighter in the first place, and Claude Code's own
	// helper requests carry a client-side deadline of their own
	// (waired-agent#1041).
	PeerWaitCeiling func(class string) time.Duration

	// PeerHealth is the test seam for the health checks PeerWaitCeiling's
	// watch makes. nil in production, where the gateway composes
	// PeerAdapterFactory with router.ProbeHealth — the same pair the
	// pre-dispatch selection probe uses.
	PeerHealth func(ctx context.Context, deviceID string) router.ProbeResult

	// LocalTTFBBudget is TTFBBudget's twin for a leg THIS device's own
	// engine serves (waired-agent#837). Same pre-commit abort, same
	// authorization gate (X-Waired-Fallback-Allowed), different leg — so a
	// route=waired or pinned leg is still never aborted, and the turn that
	// is bounded is one the intercept can reroute to the Anthropic API.
	//
	// It exists because a local cold load had no bound at all: the engine
	// withholds response headers until the weights are resident, the engine
	// client runs at Timeout: 0, and the client gave up first — then
	// retried, and the retry started the load again (waired-agent#837).
	//
	// One value for every class, deliberately not the tighter subagent one:
	// ClaudeTTFBBudgetSubMs exists because "a stalled subagent is cheap to
	// reroute", which is true of a peer that has an equivalent elsewhere and
	// false of this device. 0 disables it, restoring the unbounded wait.
	// Wired only on the Claude-intercept HandlerSet; nil elsewhere.
	LocalTTFBBudget func() time.Duration

	// StreamKeepalive is the interval at which a streaming Anthropic leg
	// with NO fallback and a LOCAL selection writes an SSE keepalive while
	// the engine has produced nothing at all (waired-agent#837).
	//
	// It is the other half of LocalTTFBBudget, on the legs where a bound is
	// not allowed: route=waired and pinned legs have nowhere else to send
	// the turn, so they wait — but waiting silently is what let the client's
	// own idle watchdog close a socket mid-load. The frames are SSE comment
	// lines, so nothing is rendered and no event ordering is perturbed; see
	// keepalive.go.
	//
	// 0 disables it, which is every listener but the Claude intercept. It
	// MUST stay 0 on the overlay listener — see the wiring comment there:
	// a serving peer that keepalives hands its caller a first byte its
	// engine never produced, disarming the caller's own TTFBBudget.
	StreamKeepalive time.Duration

	// LocalResidency, when non-nil, reports this device's most recent
	// observation of what its OWN engine holds in (V)RAM
	// (waired-agent#879). Read from the adapter's cached observation — the
	// local probe loop refreshes it once per state.HeartbeatInterval from
	// /api/ps — so consulting it never costs an engine round trip.
	//
	// The zero value (Observed false) means "we have not looked": no
	// residency probe on this host (vLLM), or none taken yet. It must never
	// be rendered as "nothing is loaded". Observation only — nothing in the
	// gateway decides on it, because a reading up to one heartbeat old is
	// evidence for a log line and not grounds to route differently.
	LocalResidency func() runtime.ModelResidency

	// LocalInflight, when non-nil, reports how many requests this machine's
	// engine is serving right now — the same counter LocalAdmission feeds,
	// so it covers peer arrivals and the owner's own work alike. Read once,
	// BEFORE this request takes its own slot, so the number is what this
	// request arrived behind (waired-agent#856 measured a session-title call
	// serialising ahead of the user's first turn on a single slot).
	// Observation only, and wired on the same LOCAL surfaces as
	// LocalAdmission.
	LocalInflight func() int

	// OnLocalEngineAbandoned, when non-nil, is called once when
	// LocalTTFBBudget fired — i.e. this device's engine was left part-way
	// through a load nobody is now waiting on. Wired to the background
	// warm-up, which is already single-flighted and checks /api/ps first, so
	// the turn leaves for the Anthropic API once and the next one is local
	// again rather than paying the same load a second time.
	OnLocalEngineAbandoned func()
}

// ServerConfig controls listener behaviour.
type ServerConfig struct {
	Addr string // "127.0.0.1:9473" — must be loopback

	// BrowserHardening adds Host and Origin allow-listing to the chain, so a
	// web page the user visits cannot reach this listener by DNS-rebinding
	// (waired-ai/waired#1195). Off by default, which keeps the package's own
	// tests — httptest.NewRequest sets Host to example.com — working
	// unchanged; the same config-gate shape as requireToken's empty token.
	//
	// The agent turns it on for every loopback gateway it binds. A bearer
	// token is not a substitute: a page cannot read the token file, but it
	// also cannot be relied on to stay in the chain, and the listeners that
	// never had one (:9476, :9472) have been carrying the allow-list alone
	// since waired-ai/waired#1195. Pointing a browser chat UI hosted off
	// this machine at the gateway is no longer supported — run it locally,
	// or put waired on its host and reach this one over the mesh
	// (waired-ai/waired#1277).
	BrowserHardening bool
}

// Server is the loopback Local Gateway HTTP server. It wraps a
// HandlerSet with the loopback-specific middleware chain.
type Server struct {
	cfg     ServerConfig
	deps    Deps
	set     *HandlerSet
	httpSrv *http.Server
}

// NewServer wires Deps and the route table and returns a ready-to-
// Serve loopback gateway. Callers typically call Serve in a goroutine
// and Shutdown during agent teardown.
func NewServer(cfg ServerConfig, deps Deps) *Server {
	set := NewHandlerSet(deps)
	s := &Server{cfg: cfg, deps: set.deps, set: set}
	s.httpSrv = &http.Server{
		Handler:           s.chain(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

// Handler exposes the loopback-wrapped handler stack for tests that
// want to bypass the listener and use httptest.NewServer directly.
func (s *Server) Handler() http.Handler { return s.chain() }

// chain is the listener's middleware stack, outermost first: the transport
// peer must be loopback, then the request itself must not look like one a web
// page smuggled in, then the runtime gates.
//
// Both the http.Server and Handler() build from here. They used to spell the
// chain out separately, which is one divergence away from tests that no longer
// exercise what production serves.
//
// There is no credential step. A local bearer token cannot separate two
// processes running as the same user — both can read the file — and on a
// system-service install the desktop user cannot read it at all, which is
// why the coding-agent listener never had one. See waired-ai/waired#1277.
func (s *Server) chain() http.Handler {
	// permission_error is what both API dialects call a 403, so the peer and
	// browser guards answer in the same shape.
	reject := func(w http.ResponseWriter, _ *http.Request, status int, _, message string) {
		writeGatewayError(w, status, "permission_error", message)
	}
	h := pausedGate(s.set.Handler(), s.deps.IsPaused)
	h = loopbackguard.Browser(h, s.cfg.BrowserHardening, loopbackguard.Options{
		// No JSON Content-Type requirement: the Origin check above already
		// rejects the cross-site simple-request POST it would defend against,
		// and this listener's clients are not browsers.
		Reject: reject,
	})
	return loopbackguard.Peer(h, reject)
}

// Serve blocks while the server is accepting requests. Shutdown
// closes ln gracefully.
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

// Shutdown calls http.Server.Shutdown.
func (s *Server) Shutdown(ctx context.Context) error { return s.httpSrv.Shutdown(ctx) }

// writeGatewayError emits a small JSON body that both OpenAI and Anthropic
// clients can deserialise, for any error the middleware chain answers with
// before a handler runs.
func writeGatewayError(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"type":    errType,
			"message": msg,
		},
	})
}

// PausedGate exposes the package-internal pause middleware so callers
// outside the gateway package (e.g., the overlay inference listener)
// can attach the same 503 + waired_paused JSON body when isPaused is
// true. Returns next unchanged when isPaused is nil.
func PausedGate(next http.Handler, isPaused func() bool) http.Handler {
	return pausedGate(next, isPaused)
}

// InferenceDisabledMessage is the 503 body's message when this host's
// local inference is off and no mesh peer could take the request
// (waired-agent#829).
//
// Verbatim what the outermost inference gate used to write before any
// routing ran. The gate is gone — it made a node with no engine unable
// to reach the mesh at all — but the wire shape a client sees for the
// case the gate was actually right about is unchanged.
const InferenceDisabledMessage = "waired-agent inference engine is disabled. " +
	"Re-enable it from the tray or POST /waired/v1/inference/enable."

// writeInferenceDisabled writes that body. Shared by both dialects'
// selection-error responders, because the gate it replaces wrapped both
// and wrote one shape for either client.
func writeInferenceDisabled(w http.ResponseWriter) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "waired_inference_disabled",
			"message": InferenceDisabledMessage,
		},
	})
}

// pausedGate short-circuits every request with HTTP 503 and an
// Anthropic-shaped JSON error body when the agent is paused. This serves
// token-bearing local clients on the loopback gateway; Claude Code's own
// managed-settings ANTHROPIC_BASE_URL listener fails open to the real Anthropic
// API while paused (the intercept Degraded check), so its sessions keep working.
func pausedGate(next http.Handler, isPaused func() bool) http.Handler {
	if isPaused == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPaused() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"type": "error",
				"error": map[string]any{
					"type":    "waired_paused",
					"message": "waired-agent is paused. Run `waired resume` to restore local serving.",
				},
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// writeJSON is a small helper to write a status + JSON body. Errors
// from the encoder are intentionally swallowed — at this point the
// response is already in flight.
// writeJSON encodes first and writes second.
//
// json.NewEncoder(w).Encode writes as it walks, so a value that fails to
// marshal part-way through leaves the client with the status already
// sent and a body that stops mid-object — a 200 that cannot be parsed
// and blames nothing. Marshalling into memory first means a failure is
// still a well-formed error the caller can act on. Responses here are
// one turn, so the buffer costs nothing worth counting.
func writeJSON(w http.ResponseWriter, status int, body any) {
	encoded, err := json.Marshal(body)
	if err != nil {
		slog.Error("gateway: response could not be encoded", "err", err, "status", status)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"api_error",` +
			`"message":"waired: the engine's response could not be encoded"}}` + "\n"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(encoded, '\n'))
}
