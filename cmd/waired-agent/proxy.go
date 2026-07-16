package main

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/waired-ai/waired-agent/internal/proxy/intercept"
)

// proxyHandle is the indirection that lets the boot-level Claude loopback
// listener (started before enrollment) pick up the inference handler + degraded
// signal once the session activates. Both are nil at boot, which makes the
// listener fail OPEN (passthrough to real Anthropic) until the session wires
// the real local-inference path in.
type proxyHandle struct {
	handler  atomic.Pointer[http.Handler] // gateway HandlerSet, set at activation
	degraded atomic.Pointer[func() bool]  // paused||disabled||!reachable, set at activation
}

// SetLocalInference is called once the inference subsystem is up with the
// bare gateway HandlerSet (NOT the loopback gateway.Server — that requires a
// bearer token Claude's Anthropic OAuth token would fail). Pass nil to leave
// the listener in passthrough-only mode (e.g. when AllowAnthropicAPI is off).
func (p *proxyHandle) SetLocalInference(h http.Handler) {
	if h == nil {
		return
	}
	p.handler.Store(&h)
}

// SetDegraded installs the runtime degraded check (paused / inference
// disabled / no reachable engine). Until set, Degraded reports true.
func (p *proxyHandle) SetDegraded(fn func() bool) {
	if fn == nil {
		return
	}
	p.degraded.Store(&fn)
}

func (p *proxyHandle) currentHandler() http.Handler {
	if hp := p.handler.Load(); hp != nil {
		return *hp
	}
	return nil
}

// Degraded drives the listener's fail-open decision. It is true (force
// passthrough to real Anthropic) whenever local inference cannot serve: before
// the handler is wired, or when the session reports paused/disabled/unreachable.
func (p *proxyHandle) Degraded() bool {
	if p.currentHandler() == nil {
		return true
	}
	if dp := p.degraded.Load(); dp != nil {
		return (*dp)()
	}
	return false
}

// localAdapter is the http.Handler handed to intercept.Deps.LocalInference.
// It dispatches to the current handler. It is only ever invoked when
// Degraded() is false (intercept.routeInference checks Degraded first),
// which implies currentHandler() is non-nil; the nil branch is an
// unreachable guard.
func (p *proxyHandle) localAdapter() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := p.currentHandler()
		if h == nil {
			http.Error(w, "waired proxy: local handler not ready", http.StatusBadGateway)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// buildClaudeListener assembles the plain-HTTP Claude loopback gateway — the
// successor to the retired :443 MITM proxy. It binds 127.0.0.1:port, serves
// /v1/messages* locally (fail-open to real Anthropic when degraded) and reverse-
// proxies everything else to the real api.anthropic.com. port<=0 disables it
// (returns nil,nil,nil). The caller serves the returned listener via
// srv.Serve(ctx, ln).
func buildClaudeListener(port int, ph *proxyHandle, cr *claudeRoutingController, modelRouteDirectives bool, logger *slog.Logger) (*intercept.Server, net.Listener, error) {
	if port <= 0 {
		return nil, nil, nil // disabled
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	// Standard egress transport to the real api.anthropic.com. With the
	// /etc/hosts redirect retired, ordinary DNS resolves the real host; we
	// honour HTTPS_PROXY via ProxyFromEnvironment so corp egress proxies keep
	// working for the passthrough leg.
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	deps := intercept.Deps{
		PassthroughTransport: tr,
		LocalInference:       ph.localAdapter(),
		Degraded:             ph.Degraded,
		Logger:               logger,
	}
	// Wire the boot-level unified per-class routing policy when present. The
	// intercept resolves each request's route (auto|waired|anthropic) from
	// ClassRoute and dispatches accordingly — it honours an anthropic route
	// itself (only this layer can relay Claude Code's subscription
	// credentials); waired/auto legs are served by the gateway selection
	// below it. Route strings match the intercept's stdlib-only literals by
	// construction (state.ClaudeRouteClass values). Nil-safe: without the
	// controller every request defaults to auto.
	if cr != nil {
		deps.ClassRoute = cr.RouteFor
		deps.ClassifyModel = classifyClaudeModel
		deps.OnFallback = cr.RecordFallback
		deps.OnServed = cr.RecordServed
		deps.OnNodeFallback = func(class, reason string) {
			cr.RecordNodeFallback(class, "", reason)
		}
	}
	// #757: annotate an auto-mode reroute in-conversation so the user can tell
	// a turn/subagent left the mesh (a subagent-side record alone is invisible).
	// #52: honour reserved /model directive ids as per-request route overrides
	// (opt-in), the intercept half of the gateway's discovery advertisement.
	srv, err := intercept.NewServer(intercept.Config{
		Addr:                 addr,
		AnnotateReroute:      true,
		ModelRouteDirectives: modelRouteDirectives,
	}, deps)
	if err != nil {
		return nil, nil, fmt.Errorf("claude proxy: new server: %w", err)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("claude proxy: listen %s: %w", addr, err)
	}
	return srv, ln, nil
}
