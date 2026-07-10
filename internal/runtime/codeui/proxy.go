package codeui

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// This file is the security front door for the bundled coding agent.
//
// `opencode serve` is started on an ephemeral loopback port guarded by HTTP
// Basic auth (OPENCODE_SERVER_PASSWORD). That password blocks any sibling
// local user who finds the port — but it is a clumsy gate for a browser (a
// Basic-auth prompt, and opencode's own web-UI Basic-auth flow has been buggy:
// sst/opencode#9066). So the user never talks to opencode directly. Instead a
// waired-owned reverse proxy, running AS the user, sits in front:
//
//   - it authenticates the BROWSER with one of the Authenticator strategies
//     below (a capability token that the tray/CLI inject into the opened URL,
//     or HTTP Basic), and
//   - it injects the opencode Basic credentials onto every upstream request,
//     so the backend stays locked even though the user typed nothing.
//
// Net effect: the legitimate launcher is friction-free, while other local
// users and external clients are denied at both the proxy and the backend.

// CookieName is the proxy session cookie set after a valid capability token.
const CookieName = "waired_codeui"

// CapabilityTokenParam is the URL query parameter carrying the one-shot
// capability token in the link the tray/CLI open (e.g. ".../?wt=<token>").
const CapabilityTokenParam = "wt"

// Authenticator gates inbound browser requests to the proxy. Wrap returns a
// handler that either serves an auth response (401 / challenge / cookie+302)
// or delegates to next once the caller is authorized.
type Authenticator interface {
	Wrap(next http.Handler) http.Handler
	// Mode is a short label for logs / runtime metadata ("token" | "basic").
	Mode() string
}

// GenerateToken returns a 256-bit random token, hex-encoded — used both for
// the capability token and the backend OPENCODE_SERVER_PASSWORD.
func GenerateToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("codeui: generate token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// ProxyHandler builds the full front-door handler: authenticate the browser,
// then reverse-proxy to the password-protected opencode backend (injecting its
// Basic credentials). Handles plain HTTP, SSE streaming, and websocket upgrades
// (net/http/httputil.ReverseProxy tunnels Upgrade requests transparently).
func ProxyHandler(backendAddr, backendPassword string, auth Authenticator) http.Handler {
	return auth.Wrap(newReverseProxy(backendAddr, backendPassword))
}

func newReverseProxy(backendAddr, backendPassword string) *httputil.ReverseProxy {
	target := &url.URL{Scheme: "http", Host: backendAddr}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target) // scheme+host → backend, inbound path/query preserved
			// Align Host with the backend bind and never leak the proxy
			// session cookie to the opencode process.
			pr.Out.Host = target.Host
			pr.Out.Header.Del("Cookie")
			// The browser authenticated to the proxy, not to opencode; the
			// proxy supplies opencode's Basic credentials so the backend stays
			// locked even though the user typed nothing.
			pr.Out.Header.Del("Authorization")
			if backendPassword != "" {
				pr.Out.SetBasicAuth(OpenCodeServerUser, backendPassword)
			}
		},
	}
}

// --- capability-token authenticator (default) ---------------------------------

type tokenAuth struct{ secret string }

// NewTokenAuth gates on a capability token. The tray/CLI open
// "http://<addr>/?wt=<secret>"; the first hit sets an HttpOnly, SameSite=Strict
// cookie and 302-redirects to strip the token from the address bar. Subsequent
// requests (including the websocket handshake — browsers send same-origin
// cookies on ws) must carry the cookie. No WWW-Authenticate is sent, so the
// legitimate user never sees a password prompt; everyone else gets a plain 401.
func NewTokenAuth(secret string) Authenticator { return &tokenAuth{secret: secret} }

func (a *tokenAuth) Mode() string { return "token" }

func (a *tokenAuth) Wrap(next http.Handler) http.Handler {
	want := []byte(a.secret)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 1. Capability link: redeem ?wt=<token> for a cookie, then redirect.
		if tok := r.URL.Query().Get(CapabilityTokenParam); tok != "" &&
			subtle.ConstantTimeCompare([]byte(tok), want) == 1 {
			http.SetCookie(w, &http.Cookie{
				Name:     CookieName,
				Value:    a.secret,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
			})
			q := r.URL.Query()
			q.Del(CapabilityTokenParam)
			dest := r.URL.Path
			if enc := q.Encode(); enc != "" {
				dest += "?" + enc
			}
			http.Redirect(w, r, dest, http.StatusFound)
			return
		}
		// 2. Established session cookie.
		if c, err := r.Cookie(CookieName); err == nil &&
			subtle.ConstantTimeCompare([]byte(c.Value), want) == 1 {
			next.ServeHTTP(w, r)
			return
		}
		denyToken(w)
	})
}

func denyToken(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte("Unauthorized — open the coding agent via `waired codeui open` " +
		"(or `waired codeui url`) to get an access link.\n"))
}

// --- HTTP Basic authenticator (opt-in, e.g. for overlay exposure) -------------

type basicAuth struct{ user, pass string }

// NewBasicAuth gates on HTTP Basic credentials. Used when the user opts into a
// real credential (e.g. exposing the agent on the waired overlay to peers),
// where a shareable username/password fits better than a capability link.
func NewBasicAuth(user, pass string) Authenticator { return &basicAuth{user: user, pass: pass} }

func (a *basicAuth) Mode() string { return "basic" }

func (a *basicAuth) Wrap(next http.Handler) http.Handler {
	wantUser := []byte(a.user)
	wantPass := []byte(a.pass)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if ok &&
			subtle.ConstantTimeCompare([]byte(u), wantUser) == 1 &&
			subtle.ConstantTimeCompare([]byte(p), wantPass) == 1 {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="waired coding agent"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}
