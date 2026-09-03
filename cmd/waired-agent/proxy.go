package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/waired-ai/waired-agent/internal/loopbackguard"
	"github.com/waired-ai/waired-agent/internal/proxy/intercept"
)

// proxyHandle is the indirection that lets the boot-level Claude loopback
// listener (started before enrollment) pick up the inference handler once the
// session activates. It is nil at boot, and a Waired-addressed turn arriving
// then is answered with the reason nothing can serve it — the listener no
// longer relays it to the real Anthropic API on waired's own judgement
// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md).
type proxyHandle struct {
	handler atomic.Pointer[http.Handler] // gateway HandlerSet, set at activation
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

func (p *proxyHandle) currentHandler() http.Handler {
	if hp := p.handler.Load(); hp != nil {
		return *hp
	}
	return nil
}

// localAdapter is the http.Handler handed to intercept.Deps.LocalInference.
// It dispatches to the current handler. intercept only reaches it when one
// was wired (it answers with the fail-closed reason otherwise), so the nil
// branch is a guard rather than a path.
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

// claudeListenerGuard composes the loopback guards for the Claude listener
// (waired-ai/waired#1195). It lives here, not in internal/proxy/intercept:
// that package is deliberately stdlib-only so a fail-open relay cannot be
// dragged down by anything it links, and it duplicates literals rather than
// import even the gateway. Composing the guard at the wiring layer and handing
// it in as intercept.Deps.Guard keeps that property with no second copy of the
// checks themselves.
//
// browserHardening=false still yields the peer check: every other loopback
// listener the agent runs has carried one since it was written, and this one
// simply never had it.
func claudeListenerGuard(browserHardening bool) func(http.Handler) http.Handler {
	// Rejections render in the Anthropic error shape the rest of the listener
	// answers in (intercept's localUnavailable / passthroughError), so a client
	// that hits one parses it the same way. permission_error is what the API
	// itself calls a 403 — the literal is duplicated here rather than exported
	// from intercept, matching how that package carries its other literals.
	reject := func(w http.ResponseWriter, _ *http.Request, status int, _, message string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "permission_error",
				"message": message,
			},
		})
	}
	return func(next http.Handler) http.Handler {
		// Peer outermost, so an off-host caller is answered as one rather than
		// for whatever it put in Host.
		//
		// No Content-Type requirement: the "/" catch-all reverse-proxies
		// arbitrary Anthropic API calls, multipart uploads included. Under
		// DNS-rebinding the page is same-origin and sends no Origin, so Host
		// is the check doing the work here.
		return loopbackguard.Peer(
			loopbackguard.Browser(next, browserHardening, loopbackguard.Options{Reject: reject}),
			reject,
		)
	}
}

// browserHardeningEnabled resolves -browser-hardening against its deprecated
// alias -mgmt-hardening (the name it carried while the guard covered only
// :9476; waired-ai/waired#836 → waired-ai/waired#1195). Both default to true,
// so either one set to false is a request to turn the guards off — the flag
// exists as a local-debug escape hatch and an operator reaching for it means
// it, whichever name they reached for.
func browserHardeningEnabled(browserHardening, mgmtHardening bool) bool {
	return browserHardening && mgmtHardening
}

// buildClaudeListener assembles the plain-HTTP Claude loopback gateway — the
// successor to the retired :443 MITM proxy. It binds 127.0.0.1:port, serves
// /v1/messages* locally (fail-open to real Anthropic when degraded) and reverse-
// proxies everything else to the real api.anthropic.com. port<=0 disables it
// (returns nil,nil,nil). The caller serves the returned listener via
// srv.Serve(ctx, ln).
//
// browserHardening is the agent's --browser-hardening flag: it adds the
// Host/Origin allow-list that keeps a web page the user visits from reaching
// this no-token listener by DNS-rebinding (waired-ai/waired#1195).
func buildClaudeListener(port int, ph *proxyHandle, cr *claudeRoutingController, modelRouteDirectives, browserHardening bool, logger *slog.Logger) (*intercept.Server, net.Listener, error) {
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
		Guard:                claudeListenerGuard(browserHardening),
		Logger:               logger,
	}
	// The records the surfaces read: what a turn asked for and what answered
	// it. Neither steers a turn — the model id it carries does that — so the
	// listener works without them; they exist so the statusline, the tray and
	// `waired claude status` can describe traffic they never see.
	if cr != nil {
		deps.ClassifyModel = classifyClaudeModel
		deps.OnServed = cr.RecordServed
		deps.OnRequest = cr.RecordRequest
	}
	// #52: honour and advertise the reserved /model ids (opt-in), the
	// intercept half of the gateway's discovery advertisement.
	srv, err := intercept.NewServer(intercept.Config{
		Addr:                 addr,
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
