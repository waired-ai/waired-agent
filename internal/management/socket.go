package management

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/waired-ai/waired-agent/internal/platform/localipc"
)

// pingPath is the one mutating-verb route writeGuard lets through on the
// loopback TCP listener: POST /waired/v1/ping is a liveness probe, not a
// state change, and CLI/daemon-reachability checks poll it over TCP.
const pingPath = "/waired/v1/ping"

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

// isMutating reports whether an HTTP method changes state. The management
// API writes with POST (and DELETE for model removal); GET is always a
// read. POST /waired/v1/ping is a liveness probe and is exempted by
// writeGuard via its path, not its method.
func isMutating(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
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
		if socketUp.Load() && isMutating(r.Method) && r.URL.Path != pingPath {
			writeJSON(w, http.StatusForbidden,
				errorBody("forbidden", "writes must use the local management socket, not the loopback TCP port"))
			return
		}
		next.ServeHTTP(w, r)
	})
}
