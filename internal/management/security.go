package management

import (
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
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
func browserGuard(next http.Handler, enabled bool) http.Handler {
	if !enabled {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Host: a rebinding page must send the attacker's own hostname in
		// Host (that is the name being rebound), so a loopback allow-list
		// defeats it — for reads and writes alike, including the info-leaky
		// GET /status and /identity.
		if !hostIsLoopback(r.Host) {
			writeJSON(w, http.StatusForbidden, errorBody("forbidden", "invalid Host header"))
			return
		}
		// Origin, when present, must be loopback. Absent Origin is allowed:
		// the CLI, curl, and the testnet fallback scripts send none.
		if origin := r.Header.Get("Origin"); origin != "" && !originIsLoopback(origin) {
			writeJSON(w, http.StatusForbidden, errorBody("forbidden", "cross-origin request rejected"))
			return
		}
		// Content-Type on writes: require application/json so a cross-site
		// POST cannot slip through as a CORS "simple request" (text/plain,
		// form-encoded, or no type — none of which trigger a preflight).
		// DELETE (bodyless model delete) is exempt and relies on the Host /
		// Origin checks above.
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch:
			if !isJSONContentType(r.Header.Get("Content-Type")) {
				writeJSON(w, http.StatusUnsupportedMediaType,
					errorBody("unsupported_media_type", "Content-Type: application/json required"))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// hostIsLoopback reports whether an HTTP Host header names a loopback
// address. Accepts host[:port], a bare hostname, and bracketed IPv6
// literals with or without a port. "localhost" is treated as loopback by
// name because a DNS-rebinding attacker cannot forge it — they must serve
// their page from their own hostname, which is what lands in Host.
func hostIsLoopback(host string) bool {
	if host == "" {
		return false
	}
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	// SplitHostPort only strips the [] of an IPv6 literal when a port is
	// present; strip them here for the no-port case (e.g. Host: "[::1]").
	name = strings.TrimPrefix(strings.TrimSuffix(name, "]"), "[")
	if strings.EqualFold(name, "localhost") {
		return true
	}
	if addr, err := netip.ParseAddr(name); err == nil {
		return addr.IsLoopback()
	}
	return false
}

// originIsLoopback reports whether an Origin header is an http(s) URL whose
// host is loopback. A malformed Origin, or one on a non-http scheme, is
// rejected.
func originIsLoopback(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return hostIsLoopback(u.Host)
}

// isJSONContentType reports whether the Content-Type header's media type is
// application/json (ignoring parameters such as charset).
func isJSONContentType(ct string) bool {
	if ct == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	return mediaType == "application/json"
}
