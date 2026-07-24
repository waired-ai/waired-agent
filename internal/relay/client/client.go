// Package client implements the agent-side relay session for the
// waired-relay-v1 subprotocol.
//
// One Client owns one persistent WebSocket connection to a single relay
// URL. It performs the handshake described in
// docs/specs/waired_control_plane_auth_spec.md §10.2 and then exposes
// channels the agent can use to push outbound encrypted_packets and
// pull inbound ones.
package client

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/waired-ai/waired-agent/proto/frame"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// DefaultSendQueueDepth is the application-side back-pressure bound.
const DefaultSendQueueDepth = 256

// DefaultHandshakeTimeout caps every individual handshake step.
const DefaultHandshakeTimeout = 10 * time.Second

// DefaultHeartbeatInterval is how often the client sends a heartbeat
// frame to keep the relay's read deadline rolling forward when the
// agent's WireGuard side is idle.
const DefaultHeartbeatInterval = 5 * time.Second

// Config is the input to New().
type Config struct {
	// URL is the full ws:// or wss:// endpoint, e.g.
	// "wss://127.0.0.1:9478/relay/v1/connect".
	URL string

	// Bearer is the access_token or relay_ticket the relay will
	// introspect against the Control Plane.
	Bearer string

	NetworkID string
	DeviceID  string

	MachinePublicKey  string // std-base64
	MachinePrivateKey ed25519.PrivateKey
	NodePublicKey     string // std-base64

	DeviceCertificate signer.DeviceCertificate

	// TLSFingerprintHex pins the relay's self-signed cert. Empty disables
	// pinning (uses system roots).
	TLSFingerprintHex string

	HandshakeTimeout  time.Duration
	HeartbeatInterval time.Duration
	SendQueueDepth    int
	Logger            *slog.Logger
}

// Inbound carries one decoded encrypted_packet up to the bind layer.
type Inbound struct {
	SrcDeviceID string
	NetworkID   string
	Payload     []byte
}

// Client is one relay session. Construct with New(). The session is
// inactive until Run is called; Run blocks until ctx cancellation or a
// fatal error.
type Client struct {
	cfg Config

	sendCh    chan frame.EncryptedPacket
	inboundCh chan Inbound
}

// New validates the config and returns a Client. The connection has not
// been established yet — call Run to drive it.
func New(cfg Config) (*Client, error) {
	if cfg.URL == "" {
		return nil, errors.New("relay/client: URL required")
	}
	if cfg.Bearer == "" {
		return nil, errors.New("relay/client: Bearer required")
	}
	if cfg.NetworkID == "" || cfg.DeviceID == "" {
		return nil, errors.New("relay/client: NetworkID and DeviceID required")
	}
	if cfg.MachinePublicKey == "" || len(cfg.MachinePrivateKey) == 0 {
		return nil, errors.New("relay/client: MachinePublicKey and MachinePrivateKey required")
	}
	if cfg.NodePublicKey == "" {
		return nil, errors.New("relay/client: NodePublicKey required")
	}
	if cfg.HandshakeTimeout == 0 {
		cfg.HandshakeTimeout = DefaultHandshakeTimeout
	}
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if cfg.SendQueueDepth == 0 {
		cfg.SendQueueDepth = DefaultSendQueueDepth
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Client{
		cfg:       cfg,
		sendCh:    make(chan frame.EncryptedPacket, cfg.SendQueueDepth),
		inboundCh: make(chan Inbound, cfg.SendQueueDepth),
	}, nil
}

// Inbound returns the receive-side channel. The bind layer should range
// over it (bounded buffer; dropped on overflow).
func (c *Client) Inbound() <-chan Inbound { return c.inboundCh }

// Send is non-blocking. Returns an error when the queue is full so the
// caller can drop instead of stalling WireGuard's send loop.
func (c *Client) Send(pkt frame.EncryptedPacket) error {
	select {
	case c.sendCh <- pkt:
		return nil
	default:
		return errors.New("relay/client: send queue full")
	}
}

// Run dials the relay, drives the handshake, and pumps frames. Returns
// nil on graceful ctx cancel, or an error on failure.
func (c *Client) Run(ctx context.Context) error {
	tlsCfg, err := c.tlsConfig()
	if err != nil {
		return fmt.Errorf("tls config: %w", err)
	}
	httpClient := &http.Client{
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
		Timeout:   c.cfg.HandshakeTimeout,
	}
	dialCtx, dialCancel := context.WithTimeout(ctx, c.cfg.HandshakeTimeout)
	defer dialCancel()

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+c.cfg.Bearer)

	c.cfg.Logger.Debug("relay dial: connecting",
		"url", c.cfg.URL, "handshake_timeout", c.cfg.HandshakeTimeout)
	ws, _, err := websocket.Dial(dialCtx, c.cfg.URL, &websocket.DialOptions{
		Subprotocols: []string{frame.Subprotocol},
		HTTPClient:   httpClient,
		HTTPHeader:   hdr,
	})
	if err != nil {
		c.cfg.Logger.Debug("relay dial: failed", "url", c.cfg.URL, "err", err)
		return fmt.Errorf("relay dial: %w", err)
	}
	defer ws.CloseNow()

	c.cfg.Logger.Debug("relay dial: connected; starting handshake", "url", c.cfg.URL)
	if err := c.handshake(ctx, ws); err != nil {
		c.cfg.Logger.Debug("relay handshake: failed", "url", c.cfg.URL, "err", err)
		return fmt.Errorf("handshake: %w", err)
	}
	c.cfg.Logger.Info("relay session established",
		"url", c.cfg.URL, "device_id", c.cfg.DeviceID)

	pumpCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	writerErr := make(chan error, 1)
	go func() {
		writerErr <- c.runWriter(pumpCtx, ws)
	}()
	readerErr := c.runReader(pumpCtx, ws)
	cancel()
	<-writerErr
	c.cfg.Logger.Debug("relay session closed", "url", c.cfg.URL, "err", readerErr)
	if readerErr != nil && !errors.Is(readerErr, context.Canceled) {
		return readerErr
	}
	return nil
}

func (c *Client) handshake(ctx context.Context, ws *websocket.Conn) error {
	hello := frame.ClientHello{
		Type:              frame.TypeClientHello,
		Version:           frame.Version,
		NetworkID:         c.cfg.NetworkID,
		DeviceID:          c.cfg.DeviceID,
		NodePublicKey:     c.cfg.NodePublicKey,
		MachinePublicKey:  c.cfg.MachinePublicKey,
		DeviceCertificate: c.cfg.DeviceCertificate,
		ClientNonce:       randB64(32),
		SupportedFrames:   []string{frame.TypeEncryptedPacket, frame.TypeHeartbeat},
	}
	rawHello, err := frame.Encode(hello)
	if err != nil {
		return err
	}
	if err := writeWith(ctx, ws, rawHello, c.cfg.HandshakeTimeout); err != nil {
		return fmt.Errorf("write hello: %w", err)
	}

	v, typ, err := readFrame(ctx, ws, c.cfg.HandshakeTimeout)
	if err != nil {
		return fmt.Errorf("read challenge: %w", err)
	}
	if typ != frame.TypeRelayChallenge {
		return fmt.Errorf("expected relay_challenge, got %s", typ)
	}
	challenge := v.(frame.RelayChallenge)
	c.cfg.Logger.Debug("relay handshake: challenge received")

	transcript := frame.ProofTranscript(hello, challenge)
	sig := ed25519.Sign(c.cfg.MachinePrivateKey, transcript)
	proof := frame.ClientProof{
		Type:         frame.TypeClientProof,
		SignatureAlg: "ed25519",
		Signature:    base64.StdEncoding.EncodeToString(sig),
	}
	rawProof, err := frame.Encode(proof)
	if err != nil {
		return err
	}
	if err := writeWith(ctx, ws, rawProof, c.cfg.HandshakeTimeout); err != nil {
		return fmt.Errorf("write proof: %w", err)
	}

	v, typ, err = readFrame(ctx, ws, c.cfg.HandshakeTimeout)
	if err != nil {
		return fmt.Errorf("read established: %w", err)
	}
	if typ != frame.TypeRelayEstablished {
		return fmt.Errorf("expected relay_established, got %s", typ)
	}
	_ = v.(frame.RelayEstablished)
	return nil
}

func (c *Client) runWriter(ctx context.Context, ws *websocket.Conn) error {
	hb := time.NewTicker(c.cfg.HeartbeatInterval)
	defer hb.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case pkt := <-c.sendCh:
			raw, err := frame.Encode(pkt)
			if err != nil {
				c.cfg.Logger.Warn("relay client: encode", "err", err)
				continue
			}
			if err := writeWith(ctx, ws, raw, c.cfg.HandshakeTimeout); err != nil {
				return err
			}
		case <-hb.C:
			beat, _ := frame.Encode(frame.Heartbeat{
				Type:      frame.TypeHeartbeat,
				Timestamp: time.Now().UTC().Format(time.RFC3339),
			})
			if err := writeWith(ctx, ws, beat, c.cfg.HandshakeTimeout); err != nil {
				return err
			}
		}
	}
}

func (c *Client) runReader(ctx context.Context, ws *websocket.Conn) error {
	for {
		mt, raw, err := ws.Read(ctx)
		if err != nil {
			return err
		}
		if mt != websocket.MessageText {
			continue
		}
		v, typ, err := frame.Decode(raw)
		if err != nil {
			c.cfg.Logger.Warn("relay client: decode", "err", err)
			continue
		}
		switch typ {
		case frame.TypeEncryptedPacket:
			pkt := v.(frame.EncryptedPacket)
			payload, err := base64.StdEncoding.DecodeString(pkt.Payload)
			if err != nil {
				c.cfg.Logger.Warn("relay client: payload base64", "err", err)
				continue
			}
			select {
			case c.inboundCh <- Inbound{
				SrcDeviceID: pkt.SrcDeviceID,
				NetworkID:   pkt.NetworkID,
				Payload:     payload,
			}:
			default:
				c.cfg.Logger.Warn("relay client: inbound queue full; dropping")
			}
		case frame.TypeHeartbeat:
			// nothing to do
		default:
			c.cfg.Logger.Warn("relay client: unexpected frame", "type", typ)
		}
	}
}

func (c *Client) tlsConfig() (*tls.Config, error) {
	if !strings.HasPrefix(c.cfg.URL, "wss://") {
		return nil, nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.cfg.TLSFingerprintHex != "" {
		want, err := hex.DecodeString(c.cfg.TLSFingerprintHex)
		if err != nil {
			return nil, fmt.Errorf("tls fingerprint hex: %w", err)
		}
		cfg.InsecureSkipVerify = true
		cfg.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("no peer certificate")
			}
			got := sha256.Sum256(rawCerts[0])
			if len(want) != len(got) {
				return errors.New("relay TLS fingerprint length mismatch")
			}
			for i := range want {
				if want[i] != got[i] {
					return errors.New("relay TLS fingerprint mismatch")
				}
			}
			return nil
		}
	}
	return cfg, nil
}

func writeWith(ctx context.Context, ws *websocket.Conn, raw []byte, timeout time.Duration) error {
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return ws.Write(wctx, websocket.MessageText, raw)
}

func readFrame(ctx context.Context, ws *websocket.Conn, timeout time.Duration) (any, string, error) {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	mt, raw, err := ws.Read(rctx)
	if err != nil {
		return nil, "", err
	}
	if mt != websocket.MessageText {
		return nil, "", errors.New("expected text frame")
	}
	return frame.Decode(raw)
}

func randB64(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return base64.StdEncoding.EncodeToString(b)
}
