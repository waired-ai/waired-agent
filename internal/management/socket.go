package management

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path"
	"sync/atomic"
	"time"

	"github.com/waired-ai/waired-agent/internal/platform/localipc"
)

// pingPath is the one mutating-verb route writeGuard lets through on the
// loopback TCP listener: POST /waired/v1/ping is a liveness probe, not a
// state change, and CLI/daemon-reachability checks poll it over TCP.
const pingPath = "/waired/v1/ping"

// tcpReadRoutes are the GET routes readGuard keeps serving on the loopback
// TCP listener. They are not a judgement about which reads are harmless —
// they are the routes non-Go consumers actually call, which cannot reach a
// unix socket or a named pipe:
//
//	/status                — installtest-enroll.sh, macos-installtest-run.sh,
//	                         Invoke-EdgeVmVerify.ps1, both repos'
//	                         testnet-fallback-helpers.sh, and the private
//	                         repo's Windows test-agent startup script
//	/inference/status      — the install-test suites, routing-sentinel.yml,
//	                         installtest-inference.yml
//	/inference/runtimes    — packaging/install/install.sh (production installer)
//	/inference/catalog     — installtest-{enroll,macos,windows}
//	/setup/state           — installtest-{macos,windows,daemon-engine}
//
// Everything else is socket-only, so a route added later is socket-only
// until someone adds it here with the consumer that needs it (waired#836).
var tcpReadRoutes = map[string]struct{}{
	"/waired/v1/status":             {},
	"/waired/v1/inference/status":   {},
	"/waired/v1/inference/runtimes": {},
	"/waired/v1/inference/catalog":  {},
	"/waired/v1/setup/state":        {},
}

// WithSocketWritesOnly enables the write-guard on the loopback TCP
// listener: while the local IPC socket is up, mutating requests over TCP
// are refused (403) so writes can only arrive over the peer-local socket
// / named pipe that browsers and network peers cannot open (waired#838).
// It is a no-op until ServeLocal actually binds the socket (fail-open),
// and off by default so the package's unit tests — which drive Handler()
// directly — are unaffected. Returns the receiver for chaining.
func (s *Server) WithSocketWritesOnly(on bool) *Server {
	s.enforceSocketWrites = on
	return s
}

// WithSocketReadsOnly enables the read-guard on the loopback TCP listener:
// while the socket is up, only the routes in tcpReadRoutes are readable
// over TCP and every other read must use the socket (waired#836). Like
// WithSocketWritesOnly it is a no-op until ServeLocal binds the socket
// (fail-open), and off by default so the package's unit tests — which
// drive Handler() directly — are unaffected. Returns the receiver.
func (s *Server) WithSocketReadsOnly(on bool) *Server {
	s.enforceSocketReads = on
	return s
}

// socketHandler serves the full route mux over the local IPC socket with
// NO transport middleware: loopbackOnly would 403 a unix conn (whose
// RemoteAddr has no host:port), and browserGuard would reject the dummy
// Host an IPC client sends — neither is needed because a browser cannot
// open the socket at all.
func (s *Server) socketHandler() http.Handler {
	return s.mux()
}

// ServeLocal binds the local IPC endpoint (unix socket on Linux/macOS,
// named pipe on Windows) and serves the management mux until ctx is
// cancelled. An empty endpoint disables the socket (returns nil). A bind
// failure is returned to the caller, which logs it but does not treat it
// as fatal — writeGuard keys on socketUp, so writes fall back to the
// loopback TCP port (behind the #836 browserGuard) when the socket is
// unavailable.
func (s *Server) ServeLocal(ctx context.Context, endpoint string) error {
	if endpoint == "" {
		return nil
	}
	ln, err := localipc.Listen(endpoint)
	if err != nil {
		return err
	}
	s.socketUp.Store(true)
	defer s.socketUp.Store(false)

	srv := &http.Server{
		Handler:           s.socketHandler(),
		ReadHeaderTimeout: 5 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// isMutating reports whether an HTTP method may change state. GET and HEAD
// are the only reads; everything else — including verbs this API does not
// use — counts as a write. Stated as an allow-list on purpose: a deny-list
// would silently treat an unknown or extension verb as a read, and would
// then depend on every handler enforcing its own method to stay correct.
func isMutating(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead:
		return false
	}
	return true
}

// pathIs reports whether the request addresses exactly route. It requires
// BOTH the decoded path and the cleaned escaped path to match, because the
// two can disagree: ServeMux routes on cleanPath(r.URL.EscapedPath()) while
// r.URL.Path is percent-decoded and uncleaned, so matching only one of them
// lets a target like /waired%2Fv1/ping satisfy a guard while routing
// somewhere else. Requiring both is fail-closed in either direction. No
// such target is exploitable today — every one of them lands on the ping
// handler, a 404, or a 307 that re-runs the guard on the cleaned path — so
// this pins the property rather than fixing a live bypass.
func pathIs(r *http.Request, route string) bool {
	return r.URL.Path == route && path.Clean(r.URL.EscapedPath()) == route
}

// tcpReadAllowed reports whether a read may be served on the loopback TCP
// listener, i.e. whether it addresses one of tcpReadRoutes.
func tcpReadAllowed(r *http.Request) bool {
	if _, ok := tcpReadRoutes[r.URL.Path]; !ok {
		return false
	}
	return path.Clean(r.URL.EscapedPath()) == r.URL.Path
}

// writeGuard wraps the loopback-TCP handler. When enforced AND the local
// IPC socket is up, it refuses mutating verbs (except the /ping liveness
// probe) so writes cannot arrive over TCP — a browser or DNS-rebinding
// page can reach the loopback port but cannot open the socket. When not
// enforced, or while the socket is down (fail-open), it is a pass-through
// so control of the agent is never bricked by a socket bind failure.
func writeGuard(next http.Handler, enforce bool, socketUp *atomic.Bool) http.Handler {
	if !enforce {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The exemption is POST /ping specifically: a DELETE or PATCH to
		// the same path is not the liveness probe and has no claim on it.
		if socketUp.Load() && isMutating(r.Method) && !(r.Method == http.MethodPost && pathIs(r, pingPath)) {
			writeJSON(w, http.StatusForbidden,
				errorBody("forbidden", "writes must use the local management socket, not the loopback TCP port"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// readGuard is writeGuard's counterpart for reads (waired#836). When
// enforced AND the socket is up, the loopback TCP listener serves only the
// routes in tcpReadRoutes; every other read must come over the socket,
// which a browser cannot open. That turns the exclusion of browser reads
// from a header heuristic (browserGuard's Host/Origin allow-list) into a
// structural one for all but those routes.
//
// It leaves mutating verbs alone: writeGuard owns those, and an operator
// who turned writeGuard off with --mgmt-socket-writes-only=false must not
// have writes blocked here instead. Fail-open while the socket is down,
// for the same reason as writeGuard — an environment with no runtime dir
// to bind in (waired-agent#293/#175) keeps working over TCP.
func readGuard(next http.Handler, enforce bool, socketUp *atomic.Bool) http.Handler {
	if !enforce {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if socketUp.Load() && !isMutating(r.Method) && !tcpReadAllowed(r) {
			writeJSON(w, http.StatusForbidden,
				errorBody("forbidden", "reads of this endpoint must use the local management socket, not the loopback TCP port"))
			return
		}
		next.ServeHTTP(w, r)
	})
}
