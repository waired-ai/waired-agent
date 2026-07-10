package inference

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/proto/signedreq"
)

// fakeGateway is the minimal interface{ Handler() http.Handler }
// implementation used to drive the overlay route table without
// pulling in the real gateway.HandlerSet (and its Selector / runtime
// machinery).
type fakeGateway struct {
	mu             atomic.Pointer[bytes.Buffer]
	hits           int32
	echoStatusCode int
}

func newFakeGateway() *fakeGateway {
	g := &fakeGateway{echoStatusCode: http.StatusOK}
	g.mu.Store(&bytes.Buffer{})
	return g
}

func (g *fakeGateway) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&g.hits, 1)
		body, _ := io.ReadAll(r.Body)
		// Record what the handler actually saw so tests can assert
		// the body was passed through unmodified.
		buf := &bytes.Buffer{}
		buf.Write(body)
		g.mu.Store(buf)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(g.echoStatusCode)
		_, _ = w.Write(body)
	})
}

func (g *fakeGateway) Hits() int32      { return atomic.LoadInt32(&g.hits) }
func (g *fakeGateway) BodySeen() []byte { return g.mu.Load().Bytes() }

type fixedPeers map[netip.Addr]PeerIdentity

func (m fixedPeers) LookupByOverlayIP(ip netip.Addr) (PeerIdentity, bool) {
	p, ok := m[ip]
	return p, ok
}

func mustKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	return pub, priv
}

func newSignedPeerRequest(t *testing.T, target string, body []byte, deviceID string, priv ed25519.PrivateKey, now time.Time) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("nonce: %v", err)
	}
	signedreq.SetHeaderEnvelope(r.Header, priv, deviceID, now.UTC().Format(time.RFC3339), base64.StdEncoding.EncodeToString(nonce), body)
	return r
}

// newSignedGetRequest is the GET counterpart of newSignedPeerRequest,
// used by /waired/v1/inference/healthz tests. signedreq.ReadBody
// panics on a nil body, so we always provide an empty bytes.Reader.
// The signature is over an empty body, which matches what the handler
// sees when it reads the request.
func newSignedGetRequest(t *testing.T, target string, deviceID string, priv ed25519.PrivateKey, now time.Time) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, target, bytes.NewReader(nil))
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("nonce: %v", err)
	}
	signedreq.SetHeaderEnvelope(r.Header, priv, deviceID, now.UTC().Format(time.RFC3339), base64.StdEncoding.EncodeToString(nonce), nil)
	return r
}

const peerOverlayIP = "100.96.0.10"

func newOverlayServer(t *testing.T, gw gatewayHandlerSet, peer PeerIdentity, opts ...func(*Config)) (*Server, fixedPeers, *signedreq.MemoryNonceCache) {
	t.Helper()
	addr := netip.MustParseAddr(peerOverlayIP)
	peers := fixedPeers{addr: peer}
	cache := signedreq.NewMemoryNonceCache()
	cfg := Config{
		DeviceName:     "self",
		GatewayHandler: gw,
		PeerLookup:     peers,
		NonceCache:     cache,
		SkewWindow:     60 * time.Second,
		NonceTTL:       5 * time.Minute,
		Now:            func() time.Time { return time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC) },
	}
	for _, o := range opts {
		o(&cfg)
	}
	return NewServerWithConfig(cfg), peers, cache
}

func TestOverlayServer_HappyPath(t *testing.T) {
	pub, priv := mustKey(t)
	peer := PeerIdentity{DeviceID: "peer-A", MachineKey: pub}
	gw := newFakeGateway()
	s, _, _ := newOverlayServer(t, gw, peer)

	body := []byte(`{"model":"claude-3","messages":[]}`)
	req := newSignedPeerRequest(t, "/anthropic/v1/messages", body, "peer-A", priv, time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC))
	req.RemoteAddr = peerOverlayIP + ":54321"
	rec := httptest.NewRecorder()

	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gw.Hits() != 1 {
		t.Fatalf("expected handler invoked once, got %d", gw.Hits())
	}
	if !bytes.Equal(gw.BodySeen(), body) {
		t.Fatalf("downstream handler must see the un-modified body; got %q", gw.BodySeen())
	}
}

func TestOverlayServer_PingStaysAnonymous(t *testing.T) {
	pub, _ := mustKey(t)
	gw := newFakeGateway()
	s, _, _ := newOverlayServer(t, gw, PeerIdentity{DeviceID: "peer-A", MachineKey: pub})

	// No headers, no signature — ping should still return 200.
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/ping", nil)
	req.RemoteAddr = "100.99.0.99:1234" // not in NetworkMap
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ping must remain anonymous: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestOverlayServer_RejectsUnknownPeer(t *testing.T) {
	pub, priv := mustKey(t)
	peer := PeerIdentity{DeviceID: "peer-A", MachineKey: pub}
	gw := newFakeGateway()
	s, _, _ := newOverlayServer(t, gw, peer)

	body := []byte(`{}`)
	req := newSignedPeerRequest(t, "/v1/chat/completions", body, "peer-A", priv, time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC))
	req.RemoteAddr = "100.99.0.99:1234"
	rec := httptest.NewRecorder()

	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "unknown_peer") {
		t.Fatalf("body=%s", rec.Body.String())
	}
	if gw.Hits() != 0 {
		t.Fatalf("downstream must not run for unknown peer; got %d hits", gw.Hits())
	}
}

func TestOverlayServer_RejectsBadSignature(t *testing.T) {
	pub, _ := mustKey(t)
	_, otherPriv := mustKey(t)
	peer := PeerIdentity{DeviceID: "peer-A", MachineKey: pub}
	gw := newFakeGateway()
	s, _, _ := newOverlayServer(t, gw, peer)

	body := []byte(`{}`)
	req := newSignedPeerRequest(t, "/anthropic/v1/messages", body, "peer-A", otherPriv, time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC))
	req.RemoteAddr = peerOverlayIP + ":54321"
	rec := httptest.NewRecorder()

	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "body_signature_mismatch") {
		t.Fatalf("body=%s", rec.Body.String())
	}
	if gw.Hits() != 0 {
		t.Fatalf("downstream must not run on bad signature; hits=%d", gw.Hits())
	}
}

func TestOverlayServer_RejectsTamperedBody(t *testing.T) {
	pub, priv := mustKey(t)
	peer := PeerIdentity{DeviceID: "peer-A", MachineKey: pub}
	gw := newFakeGateway()
	s, _, _ := newOverlayServer(t, gw, peer)

	original := []byte(`{"a":1}`)
	req := newSignedPeerRequest(t, "/v1/chat/completions", original, "peer-A", priv, time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC))
	// Replace the body with different bytes after the signature is set.
	tampered := []byte(`{"a":2}`)
	req.Body = io.NopCloser(bytes.NewReader(tampered))
	req.ContentLength = int64(len(tampered))
	req.RemoteAddr = peerOverlayIP + ":1234"
	rec := httptest.NewRecorder()

	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestOverlayServer_RejectsReplay(t *testing.T) {
	pub, priv := mustKey(t)
	peer := PeerIdentity{DeviceID: "peer-A", MachineKey: pub}
	gw := newFakeGateway()
	s, _, _ := newOverlayServer(t, gw, peer)

	body := []byte(`{}`)
	now := time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC)

	// First request: succeeds.
	req1 := newSignedPeerRequest(t, "/v1/chat/completions", body, "peer-A", priv, now)
	req1.RemoteAddr = peerOverlayIP + ":1234"
	rec1 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request: status=%d body=%s", rec1.Code, rec1.Body.String())
	}

	// Reuse identical headers (= same nonce) → replay.
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req2.Header = req1.Header.Clone()
	req2.RemoteAddr = peerOverlayIP + ":1234"
	rec2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("replay: status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), "replay_detected") {
		t.Fatalf("expected replay_detected; body=%s", rec2.Body.String())
	}
}

func TestOverlayServer_RejectsStaleIssuedAt(t *testing.T) {
	pub, priv := mustKey(t)
	peer := PeerIdentity{DeviceID: "peer-A", MachineKey: pub}
	gw := newFakeGateway()
	s, _, _ := newOverlayServer(t, gw, peer)

	body := []byte(`{}`)
	stale := time.Date(2026, 5, 9, 17, 55, 0, 0, time.UTC) // 5 min in the past
	req := newSignedPeerRequest(t, "/v1/chat/completions", body, "peer-A", priv, stale)
	req.RemoteAddr = peerOverlayIP + ":1234"
	rec := httptest.NewRecorder()

	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "issued_at_out_of_window") {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestOverlayServer_RejectsDeviceMismatch(t *testing.T) {
	pub, priv := mustKey(t)
	peer := PeerIdentity{DeviceID: "peer-A", MachineKey: pub}
	gw := newFakeGateway()
	s, _, _ := newOverlayServer(t, gw, peer)

	body := []byte(`{}`)
	// Headers claim peer-B, but the WG source IP resolves to peer-A.
	req := newSignedPeerRequest(t, "/v1/chat/completions", body, "peer-B", priv, time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC))
	req.RemoteAddr = peerOverlayIP + ":1234"
	rec := httptest.NewRecorder()

	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "device_id_mismatch") {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestOverlayServer_PausedShortCircuits(t *testing.T) {
	pub, priv := mustKey(t)
	peer := PeerIdentity{DeviceID: "peer-A", MachineKey: pub}
	gw := newFakeGateway()
	paused := true
	s, _, _ := newOverlayServer(t, gw, peer, func(c *Config) {
		c.IsPaused = func() bool { return paused }
	})
	body := []byte(`{}`)
	req := newSignedPeerRequest(t, "/v1/chat/completions", body, "peer-A", priv, time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC))
	req.RemoteAddr = peerOverlayIP + ":1234"
	rec := httptest.NewRecorder()

	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "waired_paused") {
		t.Fatalf("body=%s", rec.Body.String())
	}
	if gw.Hits() != 0 {
		t.Fatalf("downstream must not run while paused; hits=%d", gw.Hits())
	}
}

func TestOverlayServer_InferenceDisabledShortCircuits(t *testing.T) {
	pub, priv := mustKey(t)
	peer := PeerIdentity{DeviceID: "peer-A", MachineKey: pub}
	gw := newFakeGateway()
	s, _, _ := newOverlayServer(t, gw, peer, func(c *Config) {
		c.IsInferenceDisabled = func() bool { return true }
	})
	body := []byte(`{}`)
	req := newSignedPeerRequest(t, "/v1/chat/completions", body, "peer-A", priv, time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC))
	req.RemoteAddr = peerOverlayIP + ":1234"
	rec := httptest.NewRecorder()

	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "waired_inference_disabled") {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestOverlayServer_ShareDeniedShortCircuits(t *testing.T) {
	pub, priv := mustKey(t)
	peer := PeerIdentity{DeviceID: "peer-A", MachineKey: pub}
	gw := newFakeGateway()
	s, _, _ := newOverlayServer(t, gw, peer, func(c *Config) {
		c.IsShareDenied = func() bool { return true }
	})
	body := []byte(`{}`)
	req := newSignedPeerRequest(t, "/v1/chat/completions", body, "peer-A", priv, time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC))
	req.RemoteAddr = peerOverlayIP + ":1234"
	rec := httptest.NewRecorder()

	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "waired_inference_not_shared") {
		t.Fatalf("body=%s", rec.Body.String())
	}
	if gw.Hits() != 0 {
		t.Fatalf("downstream must not run while share denied; hits=%d", gw.Hits())
	}
}

// Signature must still be verified before the share gate fires, so an
// unauthenticated peer probing a share-off agent sees the auth error
// rather than the privacy-revealing share envelope. (Privacy: don't
// leak that *this specific* host has share off to peers who can't
// prove they're peers at all.)
func TestOverlayServer_ShareDeniedAfterSignature(t *testing.T) {
	pub, _ := mustKey(t)
	_, otherPriv := mustKey(t)
	peer := PeerIdentity{DeviceID: "peer-A", MachineKey: pub}
	gw := newFakeGateway()
	s, _, _ := newOverlayServer(t, gw, peer, func(c *Config) {
		c.IsShareDenied = func() bool { return true }
	})
	body := []byte(`{}`)
	// Sign with the wrong private key (= not the peer we claim to be).
	req := newSignedPeerRequest(t, "/v1/chat/completions", body, "peer-A", otherPriv, time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC))
	req.RemoteAddr = peerOverlayIP + ":1234"
	rec := httptest.NewRecorder()

	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected signature failure first, got status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "not_shared") {
		t.Errorf("share gate must not leak share state to unauthenticated peer; body=%s", rec.Body.String())
	}
}

// Confirm the gate is a pure pass-through when nil (the default for
// Phase 4/5 agents that never wired a ShareController).
func TestOverlayServer_ShareGateNilIsPassThrough(t *testing.T) {
	pub, priv := mustKey(t)
	peer := PeerIdentity{DeviceID: "peer-A", MachineKey: pub}
	gw := newFakeGateway()
	s, _, _ := newOverlayServer(t, gw, peer) // no IsShareDenied set
	body := []byte(`{"model":"claude-3","messages":[]}`)
	req := newSignedPeerRequest(t, "/anthropic/v1/messages", body, "peer-A", priv, time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC))
	req.RemoteAddr = peerOverlayIP + ":54321"
	rec := httptest.NewRecorder()

	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("nil shareGate should pass through; status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestOverlayServer_BodyTooLarge(t *testing.T) {
	pub, priv := mustKey(t)
	peer := PeerIdentity{DeviceID: "peer-A", MachineKey: pub}
	gw := newFakeGateway()
	s, _, _ := newOverlayServer(t, gw, peer, func(c *Config) {
		c.MaxBodySize = 16
	})
	big := bytes.Repeat([]byte("A"), 32)
	req := newSignedPeerRequest(t, "/v1/chat/completions", big, "peer-A", priv, time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC))
	req.RemoteAddr = peerOverlayIP + ":1234"
	rec := httptest.NewRecorder()

	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestPeerFromContext_MissingOutsideChain(t *testing.T) {
	if _, ok := PeerFromContext(t.Context()); ok {
		t.Fatalf("expected ok=false outside the peer-auth chain")
	}
}

func TestOverlayServer_NoPeerLookupFails(t *testing.T) {
	pub, priv := mustKey(t)
	gw := newFakeGateway()
	cfg := Config{
		DeviceName:     "self",
		GatewayHandler: gw,
		PeerLookup:     nil,
		NonceCache:     signedreq.NewMemoryNonceCache(),
		Now:            func() time.Time { return time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC) },
	}
	s := NewServerWithConfig(cfg)

	body := []byte(`{}`)
	req := newSignedPeerRequest(t, "/v1/chat/completions", body, "peer-A", priv, time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC))
	req.RemoteAddr = peerOverlayIP + ":1234"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gw.Hits() != 0 {
		t.Fatalf("downstream must not run when peer lookup is nil")
	}
	_ = pub
}
