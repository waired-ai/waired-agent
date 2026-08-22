package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestPeerGuard_RejectsUnparseableRemoteAddr is a regression guard for
// waired-ai/waired#1199: this package used to carry its own loopbackOnly,
// which fell back to the raw RemoteAddr when net.SplitHostPort failed and
// then passed the request through because net.ParseIP returned nil. A guard
// that waves surprises through is not a guard, so the chain now runs
// loopbackguard.Peer, which rejects anything that does not parse as
// host:port with a loopback address.
//
// The listener is plain TCP and binds loopback, so no real client produces
// these — which is exactly why the fail-open branch went unnoticed. Each
// case is a shape that reached the routes before.
func TestPeerGuard_RejectsUnparseableRemoteAddr(t *testing.T) {
	for _, tc := range []struct {
		name       string
		remoteAddr string
	}{
		{"no port", "127.0.0.1"},
		{"empty", ""},
		{"hostname with port", "localhost:1234"},
		{"garbage", "not-an-address"},
		{"bracketless ipv6", "::1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw := newTokenedGateway(t, "")
			r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			r.RemoteAddr = tc.remoteAddr
			w := httptest.NewRecorder()
			gw.Handler().ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Fatalf("RemoteAddr %q: got %d, want 403 — the peer guard must fail closed", tc.remoteAddr, w.Code)
			}
		})
	}
}

// TestPeerGuard_AllowsLoopbackPeers is the other half: the shapes a real
// local client produces must still reach the routes, so the guard above is
// not passing by rejecting everything.
func TestPeerGuard_AllowsLoopbackPeers(t *testing.T) {
	for _, remoteAddr := range []string{"127.0.0.1:1", "127.0.0.1:54321", "[::1]:1", "127.4.5.6:80"} {
		t.Run(remoteAddr, func(t *testing.T) {
			gw := newTokenedGateway(t, "")
			r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			r.RemoteAddr = remoteAddr
			w := httptest.NewRecorder()
			gw.Handler().ServeHTTP(w, r)
			if w.Code == http.StatusForbidden {
				t.Fatalf("RemoteAddr %q: got 403, want the request to reach the routes; body=%s", remoteAddr, w.Body.String())
			}
		})
	}
}

// TestPeerGuard_RejectsNonLoopbackPeer keeps the original loopbackOnly
// property: a peer that parses but is not loopback is still refused.
func TestPeerGuard_RejectsNonLoopbackPeer(t *testing.T) {
	gw := newTokenedGateway(t, "")
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.RemoteAddr = "203.0.113.7:443"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403 for a non-loopback peer", w.Code)
	}
}
