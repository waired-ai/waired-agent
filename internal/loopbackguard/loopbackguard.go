// Package loopbackguard holds the checks that keep a web page the user
// visits from reaching the agent's loopback HTTP listeners.
//
// Binding 127.0.0.1 does not stop that page: a browser's connection genuinely
// originates from 127.0.0.1, so a peer-address check cannot see the attack at
// all. What separates the page from a legitimate local client is the request
// itself — a DNS-rebinding page must put the name it rebound into Host, and a
// cross-origin page announces itself in Origin.
//
// The agent runs three loopback listeners a page can reach: the Local
// Management API (:9476), the Claude gateway (:9472) and the coding-agent
// data plane (:9479). They answer in three different error formats, so this
// package renders no body of its own — the caller passes a Reject that writes
// its own shape. The checks first shipped for :9476 in waired-agent#66
// (waired-ai/waired#836); they were extended to the other two in
// waired-ai/waired#1195.
package loopbackguard

import (
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

// Reject writes a rejection response in the calling listener's own error
// format. code is a short machine-readable token and message the human half;
// a listener whose API has its own error vocabulary is free to ignore code and
// substitute its own.
type Reject func(w http.ResponseWriter, r *http.Request, status int, code, message string)

// Options configures Browser.
type Options struct {
	// RequireJSONContentType rejects POST/PUT/PATCH whose Content-Type is not
	// application/json, closing the CORS "simple request" CSRF bypass
	// (text/plain, form-encoded and absent types trigger no preflight).
	//
	// Leave it off for a listener that carries bodies a JSON requirement would
	// break: :9472's catch-all reverse-proxies arbitrary Anthropic API calls,
	// multipart uploads included. Turning it off costs little — a cross-site
	// form POST does send Origin, so the Origin check below already rejects
	// it; this is the belt to that pair of braces.
	RequireJSONContentType bool

	// Reject writes the rejection. A nil Reject falls back to plain text.
	Reject Reject
}

// Browser layers the browser-facing checks on next: Host allow-listing (all
// methods), Origin allow-listing when an Origin is present, and optionally a
// JSON Content-Type on mutating verbs. The first failure wins.
//
// enabled=false returns next unchanged. That is what lets a package's unit
// tests drive its handler with httptest.NewRequest — which sets Host to
// example.com and sends no Origin — without every request 403ing, and it
// mirrors the gateway's requireToken, where an unset token disables the check.
// Production wiring turns it on.
func Browser(next http.Handler, enabled bool, opts Options) http.Handler {
	if !enabled {
		return next
	}
	reject := opts.Reject
	if reject == nil {
		reject = rejectPlainText
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Host: a rebinding page must send the attacker's own hostname in
		// Host (that is the name being rebound), so a loopback allow-list
		// defeats it — for reads and writes alike, including info-leaky GETs.
		if !hostIsLoopback(r.Host) {
			reject(w, r, http.StatusForbidden, "forbidden", "invalid Host header")
			return
		}
		// Origin, when present, must be loopback. Absent Origin is allowed:
		// the CLI, curl, Claude Code and the testnet fallback scripts send none.
		if origin := r.Header.Get("Origin"); origin != "" && !originIsLoopback(origin) {
			reject(w, r, http.StatusForbidden, "forbidden", "cross-origin request rejected")
			return
		}
		if opts.RequireJSONContentType {
			// DELETE (bodyless model delete) is exempt and relies on the Host
			// and Origin checks above.
			switch r.Method {
			case http.MethodPost, http.MethodPut, http.MethodPatch:
				if !isJSONContentType(r.Header.Get("Content-Type")) {
					reject(w, r, http.StatusUnsupportedMediaType,
						"unsupported_media_type", "Content-Type: application/json required")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Peer rejects requests whose transport peer is not a loopback address. This
// is defence-in-depth behind the listener's own bind: it is the check that
// keeps a listener honest if its address ever becomes configurable, and it
// cannot see the browser attack Browser is for.
//
// An unparseable RemoteAddr is rejected. The listeners this guards are plain
// TCP, so a peer address that does not parse as host:port is a surprise, and
// a guard that waves surprises through is not a guard.
func Peer(next http.Handler, reject Reject) http.Handler {
	if reject == nil {
		reject = rejectPlainText
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !peerIsLoopback(r.RemoteAddr) {
			reject(w, r, http.StatusForbidden, "forbidden", "loopback only")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// peerIsLoopback reports whether a net/http RemoteAddr ("host:port") names a
// loopback address.
func peerIsLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return addr.IsLoopback()
}

// hostIsLoopback reports whether an HTTP Host header names a loopback
// address. Accepts host[:port], a bare hostname, and bracketed IPv6 literals
// with or without a port. "localhost" is treated as loopback by name because a
// DNS-rebinding attacker cannot forge it — they must serve their page from
// their own hostname, which is what lands in Host.
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

// rejectPlainText is the fallback for a caller that passed no Reject. It keeps
// a missing renderer from turning a rejection into a nil-call panic.
func rejectPlainText(w http.ResponseWriter, _ *http.Request, status int, _, message string) {
	http.Error(w, message, status)
}
