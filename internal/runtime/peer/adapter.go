// Package peer implements runtime.Adapter for inference engines that
// live on a *peer* in the WireGuard mesh — i.e., another waired-agent
// reachable via overlay IP. The adapter does not start, stop, or
// otherwise own the remote engine; it just exposes a stable BaseURL
// and a custom http.RoundTripper so the gateway can proxy a request
// to the peer's overlay-side inference listener (port 9474) with the
// agent-to-agent Ed25519 signing scheme attached.
//
// Phase 4 wires this lazily: when the Selector decides a request
// should run remotely (Selection.Runtime begins with "remote:"), the
// gateway handler asks a PeerAdapterFactory for a one-shot adapter
// keyed by the chosen peer's DeviceID. There is no central
// PeerAdapter pool because a peer's reachability + engine list both
// flap with the mesh aggregator — re-deriving the adapter per
// Selection avoids stale-cache bugs.
package peer

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/proto/signedreq"
)

// OverlayPort is the canonical port the inference.Server listens on
// inside each agent. Hard-coded to mirror cmd/waired-agent's
// inferenceServicePort constant; lifted here so the peer adapter
// stays self-contained.
const OverlayPort uint16 = 9474

// OverlayDialer is the subset of internal/network/wgnet.Engine the
// peer adapter needs to reach a peer's overlay listener. The agent
// supplies a real Engine; tests supply an httptest-backed dialer so
// the adapter can be exercised without spinning up netstack.
type OverlayDialer interface {
	DialOverlayTCP(ctx context.Context, ip netip.Addr, port uint16) (net.Conn, error)
}

// Adapter implements runtime.Adapter (and the optional
// runtime.Transporter) for a single remote peer.
//
// One Adapter is constructed per Selection — the agent re-derives it
// from the latest mesh snapshot and the gateway HandlerSet uses it
// for the duration of one HTTP exchange. Lifetime methods
// (EnsureRunning / Stop) are no-ops; Health is derived from the
// snapshot the factory observed at construction time.
type Adapter struct {
	selfDeviceID string
	selfPriv     ed25519.PrivateKey

	peerDeviceID string
	peerOverlay  netip.Addr
	peerPort     uint16

	now      func() time.Time
	nonce    func() (string, error)
	dialer   OverlayDialer
	timeout  time.Duration
	healthFn func() runtime.Health

	// transport is built once at construction so the gateway handler's
	// http.Client can hand off via the Transporter interface.
	transport *signingTransport
}

// Config configures a per-peer Adapter.
type Config struct {
	// SelfDeviceID is THIS agent's device id — it gets put into the
	// X-Waired-Device header so the receiving peer knows whose
	// MachinePublicKey to verify with.
	SelfDeviceID string

	// SelfPrivKey is THIS agent's machine private key (ed25519.PrivateKey
	// from the local key file). Used to sign the canonical bytes for
	// every outbound request.
	SelfPrivKey ed25519.PrivateKey

	// PeerDeviceID / PeerOverlayIP describe the destination.
	PeerDeviceID  string
	PeerOverlayIP netip.Addr
	// PeerPort defaults to OverlayPort when zero.
	PeerPort uint16

	// Dialer is the overlay-aware TCP dialer. Required.
	Dialer OverlayDialer

	// RequestTimeout is the per-request timeout. 0 → 60 s. Streaming
	// responses ignore this once headers arrive (set as
	// ResponseHeaderTimeout on the underlying transport).
	RequestTimeout time.Duration

	// Now / Nonce override clock + nonce generation in tests.
	// Production callers leave these zero for real time.Now and
	// crypto/rand.
	Now   func() time.Time
	Nonce func() (string, error)

	// HealthFn returns the latest known health for this peer, derived
	// from the inferencemesh snapshot the factory observed. Optional —
	// nil is treated as StateReady.
	HealthFn func() runtime.Health
}

// NewAdapter builds an Adapter from cfg. Returns an error when
// required fields are missing.
func NewAdapter(cfg Config) (*Adapter, error) {
	if cfg.SelfDeviceID == "" {
		return nil, errors.New("peer.NewAdapter: SelfDeviceID required")
	}
	if len(cfg.SelfPrivKey) == 0 {
		return nil, errors.New("peer.NewAdapter: SelfPrivKey required")
	}
	if cfg.PeerDeviceID == "" {
		return nil, errors.New("peer.NewAdapter: PeerDeviceID required")
	}
	if !cfg.PeerOverlayIP.IsValid() {
		return nil, errors.New("peer.NewAdapter: PeerOverlayIP required")
	}
	if cfg.Dialer == nil {
		return nil, errors.New("peer.NewAdapter: Dialer required")
	}
	port := cfg.PeerPort
	if port == 0 {
		port = OverlayPort
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	nonce := cfg.Nonce
	if nonce == nil {
		nonce = defaultNonce
	}
	timeout := cfg.RequestTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	a := &Adapter{
		selfDeviceID: cfg.SelfDeviceID,
		selfPriv:     cfg.SelfPrivKey,
		peerDeviceID: cfg.PeerDeviceID,
		peerOverlay:  cfg.PeerOverlayIP,
		peerPort:     port,
		now:          now,
		nonce:        nonce,
		dialer:       cfg.Dialer,
		timeout:      timeout,
		healthFn:     cfg.HealthFn,
	}
	a.transport = newSigningTransport(a)
	return a, nil
}

// Name implements runtime.Adapter. The "remote:<deviceID>" prefix is
// the contract the Selector emits in Selection.Runtime — the gateway
// handler uses the prefix to decide whether to look up a local
// runtime or build a peer adapter.
func (a *Adapter) Name() string { return "remote:" + a.peerDeviceID }

// EnsureRunning is a no-op: the peer manages its own engine
// lifecycle. The Selector's mesh-fallback step has already verified
// the peer reports a reachable, non-stale engine.
func (a *Adapter) EnsureRunning(ctx context.Context) error { return nil }

// Health derives from the snapshot the factory observed at
// construction time. Callers that want fresher data must construct a
// new adapter from a fresh snapshot.
func (a *Adapter) Health(ctx context.Context) runtime.Health {
	if a.healthFn != nil {
		return a.healthFn()
	}
	return runtime.Health{State: runtime.StateReady}
}

// Stop is a no-op for the same reason as EnsureRunning — the peer
// owns the lifecycle.
func (a *Adapter) Stop(ctx context.Context) error { return nil }

// BaseURL implements runtime.Adapter. The gateway proxy stitches
// this with the route path (/anthropic/v1/messages, /v1/chat/...)
// and POSTs the signed body to the peer's overlay listener.
func (a *Adapter) BaseURL() string {
	return fmt.Sprintf("http://%s", netip.AddrPortFrom(a.peerOverlay, a.peerPort).String())
}

// Transport implements runtime.Transporter. Returns a RoundTripper
// that dials over the WG overlay and signs every request with the
// peer-to-peer Ed25519 envelope (X-Waired-Device / X-Waired-Issued-At
// / X-Waired-Nonce / X-Waired-Body-Signature).
func (a *Adapter) Transport() http.RoundTripper { return a.transport }

// HTTPClient is a small convenience for tests that want to construct
// a stdlib client with this adapter's transport pre-installed.
func (a *Adapter) HTTPClient() *http.Client {
	return &http.Client{Transport: a.transport, Timeout: a.timeout}
}

// signingTransport wraps overlay-dialing http.Transport and signs each
// outbound request body before forwarding.
type signingTransport struct {
	a    *Adapter
	base *http.Transport
}

func newSigningTransport(a *Adapter) *signingTransport {
	t := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			ap, err := netip.ParseAddrPort(addr)
			if err != nil {
				return nil, fmt.Errorf("peer transport: parse %q: %w", addr, err)
			}
			return a.dialer.DialOverlayTCP(ctx, ap.Addr(), ap.Port())
		},
		// LLM responses can be large + streamed; do not let the
		// transport pool keep stale connections (overlay handshake
		// state may have rotated).
		DisableKeepAlives: true,
	}
	return &signingTransport{a: a, base: t}
}

// RoundTrip implements http.RoundTripper. Buffers the body so the
// canonical-bytes signature can be computed, attaches the four
// envelope headers, then forwards through the overlay-aware base
// transport.
//
// Streaming responses are passed through verbatim — only the request
// body is buffered for signing. (The peer-overlay listener also caps
// the request body via signedreq.ReadBody, so requests larger than
// the cap will surface a 413 from the receiver, not a local OOM.)
func (t *signingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("peer transport: read body: %w", err)
		}
	}
	nonce, err := t.a.nonce()
	if err != nil {
		return nil, fmt.Errorf("peer transport: nonce: %w", err)
	}
	issuedAt := t.a.now().UTC().Format(time.RFC3339)
	signedreq.SetHeaderEnvelope(req.Header, t.a.selfPriv, t.a.selfDeviceID, issuedAt, nonce, body)
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	req.ContentLength = int64(len(body))
	return t.base.RoundTrip(req)
}

// defaultNonce produces a 16-byte random nonce, base64-encoded
// (StdEncoding). The decoded length comfortably clears
// signedreq.NonceMinBytes (12).
func defaultNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}
