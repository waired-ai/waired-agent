// Package gateway hosts the Local Gateway HTTP server (port 9473 by
// default), which exposes OpenAI- and Anthropic-compatible chat APIs
// to local clients (Claude Code, Open Code, curl, …) and proxies
// them — via the router and runtime adapters — to the appropriate
// backend engine.
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
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
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
	// AuthToken, if non-empty, makes the gateway require an
	// `Authorization: Bearer <token>` header (constant-time
	// comparison) on every request. Empty disables the check —
	// kept that way for unit tests and dev `--bypass-idp` flows
	// that prefer the loopback bind alone as the trust boundary.
	// In production wiring (cmd/waired-agent), this is the same
	// value as <state>/secrets/gateway-token, which the integration
	// package writes into env.sh / opencode.json so Claude Code
	// and OpenCode automatically present it.
	AuthToken string

	// IsPaused, if non-nil and returning true, makes every gateway
	// request short-circuit to 503 with a `waired_paused` error body
	// rather than reaching a handler. Wired up in cmd/waired-agent so
	// `waired pause` causes existing Claude Code sessions to see a
	// clean error instead of a malformed routing attempt. Nil disables
	// the gate (default in unit tests).
	IsPaused func() bool

	// IsInferenceDisabled, if non-nil and returning true, gates the
	// gateway separately from IsPaused: the WireGuard data plane stays
	// up but inference requests are 503'd with a `waired_inference_disabled`
	// error body. Wired up in cmd/waired-agent so the tray can toggle
	// the local LLM gateway independently of network reachability.
	// Nil disables the gate (default in unit tests).
	IsInferenceDisabled func() bool

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
	// callers fail open (no advertisement / no 400). Wired only on the
	// Claude-intercept HandlerSet, alongside ResolveUnknownModel; nil on
	// every other listener disables the behaviour.
	ContextWindowFor func(modelID string) int

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
}

// ServerConfig controls listener behaviour.
type ServerConfig struct {
	Addr string // "127.0.0.1:9473" — must be loopback
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
		Handler:           loopbackOnly(requireToken(pausedGate(inferenceGate(set.Handler(), s.deps.IsInferenceDisabled), s.deps.IsPaused), s.deps.AuthToken)),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

// Handler exposes the loopback-wrapped handler stack for tests that
// want to bypass the listener and use httptest.NewServer directly.
func (s *Server) Handler() http.Handler {
	return loopbackOnly(requireToken(pausedGate(inferenceGate(s.set.Handler(), s.deps.IsInferenceDisabled), s.deps.IsPaused), s.deps.AuthToken))
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

// requireToken enforces `Authorization: Bearer <token>` when expected
// is non-empty. An empty expected disables the check (test / dev mode).
//
// Both OpenAI- and Anthropic-style clients send the token in the
// standard Authorization header, so we accept Bearer schemes only.
// (Anthropic's `x-api-key` alternative is not honoured here: the
// integration package writes ANTHROPIC_AUTH_TOKEN, which Claude Code
// places into Authorization: Bearer.)
//
// Constant-time comparison via crypto/subtle to avoid leaking the
// token through response timing.
func requireToken(next http.Handler, expected string) http.Handler {
	if expected == "" {
		return next
	}
	expectedBytes := []byte(expected)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(auth, prefix) {
			writeAuthError(w, http.StatusUnauthorized, "missing or malformed Authorization header (Bearer expected)")
			return
		}
		got := []byte(strings.TrimPrefix(auth, prefix))
		if subtle.ConstantTimeCompare(got, expectedBytes) != 1 {
			writeAuthError(w, http.StatusUnauthorized, "invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// writeAuthError emits a small JSON body that both OpenAI and
// Anthropic clients can deserialise as an error.
func writeAuthError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"type":    "authentication_error",
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

// InferenceGate is the public counterpart to PausedGate for the
// IsInferenceDisabled toggle.
func InferenceGate(next http.Handler, isDisabled func() bool) http.Handler {
	return inferenceGate(next, isDisabled)
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

// inferenceGate short-circuits every request with HTTP 503 and a
// distinct `waired_inference_disabled` error body when the operator
// has turned the local inference subsystem off via the tray (or the
// management API). Independent of pausedGate so the WireGuard data
// plane can stay reachable while the LLM gateway is dormant.
func inferenceGate(next http.Handler, isDisabled func() bool) http.Handler {
	if isDisabled == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isDisabled() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"type": "error",
				"error": map[string]any{
					"type":    "waired_inference_disabled",
					"message": "waired-agent inference engine is disabled. Re-enable it from the tray or POST /waired/v1/inference/enable.",
				},
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// loopbackOnly rejects requests whose remote address is not 127.0.0.1
// or ::1. This is defence-in-depth: the Server already binds only to
// 127.0.0.1, but a misconfigured listener (or a reverse proxy) could
// otherwise leak the gateway externally.
func loopbackOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		ip := net.ParseIP(host)
		if ip != nil && !ip.IsLoopback() {
			http.Error(w, "loopback only", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// writeJSON is a small helper to write a status + JSON body. Errors
// from the encoder are intentionally swallowed — at this point the
// response is already in flight.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
