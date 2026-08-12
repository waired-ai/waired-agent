package management

import (
	"net/http"

	"github.com/waired-ai/waired-agent/internal/loopbackguard"
)

// WithBrowserHardening enables the browser-facing defenses on the
// management API and returns the receiver for chaining. It layers three
// checks on top of the existing loopbackOnly guard (which only inspects
// the transport peer IP and so cannot see a DNS-rebinding / cross-origin
// browser request):
//
//   - Host allow-listing — blocks DNS-rebinding.
//   - Origin allow-listing — blocks cross-origin browser requests.
//   - Content-Type: application/json on mutating verbs — blocks the CORS
//     "simple request" CSRF bypass.
//
// It is OFF by default so the package's unit tests can drive Handler()
// with httptest.NewRequest (which sets Host to example.com and no Origin)
// without every request 403ing. Production wiring (cmd/waired-agent) calls
// this to turn it on. This mirrors the gateway's requireToken pattern,
// where an unset token disables the check (internal/gateway/server.go).
func (s *Server) WithBrowserHardening() *Server {
	s.browserHardening = true
	return s
}

// browserGuard rejects requests a browser — or a DNS-rebinding page — could
// smuggle to the loopback API even though loopbackOnly passed (the browser's
// TCP connection genuinely originates from 127.0.0.1). enabled=false returns
// next unchanged (unit tests / dev). See WithBrowserHardening for the rationale.
//
// The checks themselves live in internal/loopbackguard, shared with the Claude
// gateway (:9472) and the coding-agent data plane (:9479) since
// waired-ai/waired#1195. Only the rendering is ours: those two answer in
// API-compatible error shapes, this one in the management API's.
func browserGuard(next http.Handler, enabled bool) http.Handler {
	return loopbackguard.Browser(next, enabled, loopbackguard.Options{
		RequireJSONContentType: true,
		Reject: func(w http.ResponseWriter, _ *http.Request, status int, code, message string) {
			writeJSON(w, status, errorBody(code, message))
		},
	})
}
