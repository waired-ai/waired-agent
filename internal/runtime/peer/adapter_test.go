package peer_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/internal/runtime/peer"
	"github.com/waired-ai/waired-agent/proto/signedreq"
)

// httptestDialer adapts an httptest.Server's local listener to the
// OverlayDialer interface so the adapter can be exercised end-to-end
// without netstack.
//
// dialAddr is captured per call so tests can assert which (overlay
// IP, port) tuple the adapter attempted to reach.
type httptestDialer struct {
	target   string // host:port of the underlying listener
	lastAddr atomic.Value
}

func newHTTPTestDialer(s *httptest.Server) *httptestDialer {
	u, err := url.Parse(s.URL)
	if err != nil {
		panic(err)
	}
	return &httptestDialer{target: u.Host}
}

func (d *httptestDialer) DialOverlayTCP(ctx context.Context, ip netip.Addr, port uint16) (net.Conn, error) {
	d.lastAddr.Store(netip.AddrPortFrom(ip, port).String())
	dialer := net.Dialer{}
	return dialer.DialContext(ctx, "tcp", d.target)
}

func (d *httptestDialer) Last() string {
	v := d.lastAddr.Load()
	if v == nil {
		return ""
	}
	return v.(string)
}

func mustKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	return pub, priv
}

// peerEcho is a tiny http handler that verifies the peer-overlay
// signature against pub, then echoes the body back. Tests assert on
// what it observed.
func peerEcho(t *testing.T, expectedDeviceID string, pub ed25519.PublicKey, now time.Time) (http.Handler, *atomic.Pointer[bytes.Buffer]) {
	t.Helper()
	seen := &atomic.Pointer[bytes.Buffer]{}
	seen.Store(&bytes.Buffer{})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_, _, err = signedreq.VerifyHeaderEnvelope(r.Header, body, pub, expectedDeviceID, now, time.Minute)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		buf := &bytes.Buffer{}
		buf.Write(body)
		seen.Store(buf)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}), seen
}

func TestAdapter_RoundTripSignsRequest(t *testing.T) {
	pub, priv := mustKey(t)
	now := time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC)
	handler, seen := peerEcho(t, "self-A", pub, now)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	dialer := newHTTPTestDialer(srv)
	adapter, err := peer.NewAdapter(peer.Config{
		SelfDeviceID:  "self-A",
		SelfPrivKey:   priv,
		PeerDeviceID:  "peer-B",
		PeerOverlayIP: netip.MustParseAddr("100.96.0.42"),
		Dialer:        dialer,
		Now:           func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}

	body := []byte(`{"hello":"world"}`)
	resp, err := adapter.HTTPClient().Post(adapter.BaseURL()+"/anthropic/v1/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, got)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("response body mismatch: got %q want %q", got, body)
	}
	if !bytes.Equal(seen.Load().Bytes(), body) {
		t.Fatalf("server saw %q, want %q", seen.Load().Bytes(), body)
	}
	// The dialer should have been asked for the peer's overlay IP.
	if got := dialer.Last(); !strings.HasPrefix(got, "100.96.0.42:") {
		t.Fatalf("dialer.Last = %q, want overlay-IP-shaped", got)
	}
}

func TestAdapter_BaseURLAndName(t *testing.T) {
	_, priv := mustKey(t)
	a, err := peer.NewAdapter(peer.Config{
		SelfDeviceID:  "self-A",
		SelfPrivKey:   priv,
		PeerDeviceID:  "peer-B",
		PeerOverlayIP: netip.MustParseAddr("100.96.0.42"),
		Dialer:        nopDialer{},
	})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	if got := a.Name(); got != "remote:peer-B" {
		t.Fatalf("Name() = %q", got)
	}
	if got := a.BaseURL(); got != "http://100.96.0.42:9474" {
		t.Fatalf("BaseURL() = %q", got)
	}
}

func TestAdapter_HealthFromHook(t *testing.T) {
	_, priv := mustKey(t)
	called := 0
	a, err := peer.NewAdapter(peer.Config{
		SelfDeviceID:  "self",
		SelfPrivKey:   priv,
		PeerDeviceID:  "peer",
		PeerOverlayIP: netip.MustParseAddr("100.96.0.99"),
		Dialer:        nopDialer{},
		HealthFn: func() runtime.Health {
			called++
			return runtime.Health{State: runtime.StateFailed, LastErr: "stale"}
		},
	})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	got := a.Health(t.Context())
	if got.State != runtime.StateFailed || got.LastErr != "stale" {
		t.Fatalf("Health = %+v", got)
	}
	if called != 1 {
		t.Fatalf("HealthFn invocation count = %d", called)
	}
}

func TestAdapter_HealthDefaultReady(t *testing.T) {
	_, priv := mustKey(t)
	a, _ := peer.NewAdapter(peer.Config{
		SelfDeviceID:  "self",
		SelfPrivKey:   priv,
		PeerDeviceID:  "peer",
		PeerOverlayIP: netip.MustParseAddr("100.96.0.99"),
		Dialer:        nopDialer{},
	})
	if got := a.Health(t.Context()).State; got != runtime.StateReady {
		t.Fatalf("default Health = %q, want ready", got)
	}
}

func TestAdapter_EnsureRunningStopAreNoOps(t *testing.T) {
	_, priv := mustKey(t)
	a, _ := peer.NewAdapter(peer.Config{
		SelfDeviceID:  "self",
		SelfPrivKey:   priv,
		PeerDeviceID:  "peer",
		PeerOverlayIP: netip.MustParseAddr("100.96.0.99"),
		Dialer:        nopDialer{},
	})
	if err := a.EnsureRunning(t.Context()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if err := a.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestAdapter_NewRequiresFields(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*peer.Config)
	}{
		{"no SelfDeviceID", func(c *peer.Config) { c.SelfDeviceID = "" }},
		{"no SelfPrivKey", func(c *peer.Config) { c.SelfPrivKey = nil }},
		{"no PeerDeviceID", func(c *peer.Config) { c.PeerDeviceID = "" }},
		{"invalid PeerOverlayIP", func(c *peer.Config) { c.PeerOverlayIP = netip.Addr{} }},
		{"no Dialer", func(c *peer.Config) { c.Dialer = nil }},
	}
	_, priv := mustKey(t)
	base := peer.Config{
		SelfDeviceID:  "self",
		SelfPrivKey:   priv,
		PeerDeviceID:  "peer",
		PeerOverlayIP: netip.MustParseAddr("100.96.0.10"),
		Dialer:        nopDialer{},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := base
			c.mod(&cfg)
			if _, err := peer.NewAdapter(cfg); err == nil {
				t.Fatalf("expected error for %s", c.name)
			}
		})
	}
}

func TestAdapter_TransportInterface(t *testing.T) {
	_, priv := mustKey(t)
	a, _ := peer.NewAdapter(peer.Config{
		SelfDeviceID:  "self",
		SelfPrivKey:   priv,
		PeerDeviceID:  "peer",
		PeerOverlayIP: netip.MustParseAddr("100.96.0.10"),
		Dialer:        nopDialer{},
	})
	// Confirm the optional Transporter interface is satisfied — the
	// gateway handler relies on this for routing remote selections.
	var _ runtime.Adapter = a
	tt, ok := any(a).(runtime.Transporter)
	if !ok {
		t.Fatalf("peer.Adapter must implement runtime.Transporter")
	}
	if tt.Transport() == nil {
		t.Fatalf("Transport() must not be nil")
	}
}

func TestAdapter_NonceErrorBubbles(t *testing.T) {
	pub, priv := mustKey(t)
	now := time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC)
	handler, _ := peerEcho(t, "self", pub, now)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	dialer := newHTTPTestDialer(srv)

	a, err := peer.NewAdapter(peer.Config{
		SelfDeviceID:  "self",
		SelfPrivKey:   priv,
		PeerDeviceID:  "peer",
		PeerOverlayIP: netip.MustParseAddr("100.96.0.10"),
		Dialer:        dialer,
		Now:           func() time.Time { return now },
		Nonce:         func() (string, error) { return "", errors.New("noise") },
	})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	_, err = a.HTTPClient().Post(a.BaseURL()+"/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	if err == nil || !strings.Contains(err.Error(), "nonce") {
		t.Fatalf("expected nonce error to surface, got %v", err)
	}
}

// nopDialer is used in tests that don't actually issue HTTP traffic.
type nopDialer struct{}

func (nopDialer) DialOverlayTCP(ctx context.Context, ip netip.Addr, port uint16) (net.Conn, error) {
	return nil, errors.New("nopDialer: not connected")
}
