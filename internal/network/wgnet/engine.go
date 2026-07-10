// Package wgnet wraps wireguard-go's userspace device and netstack TUN so the
// rest of waired can dial and listen on the overlay without touching the host
// kernel routing table. See docs/specs/waired_client_network_spec.md §23.
package wgnet

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

const DefaultMTU = 1280

// Peer is the minimal data the Engine needs about a remote device.
type Peer struct {
	DeviceName          string
	OverlayIP           netip.Addr
	WireGuardPublicKey  []byte
	Endpoint            string
	PersistentKeepalive int
}

type Config struct {
	SelfName       string
	SelfOverlayIP  netip.Addr
	SelfPrivateKey []byte
	ListenPort     int
	Peers          []Peer
	MTU            int
	Logger         *slog.Logger

	// Relay-aware fields. When SelfDeviceID is non-empty, NewEngine builds
	// a MultiplexBind that supports both "udp4:" (direct) and "relay:"
	// (WebSocket-tunnelled) peer endpoints. Empty means: keep the
	// pre-step8 behaviour of using conn.NewStdNetBind directly.
	SelfDeviceID       string
	SelfNetworkID      string
	SelfNodePub        string // std-base64
	RelayClientFactory RelayClientFactory
}

// Engine is the live WireGuard userspace + netstack instance.
type Engine struct {
	cfg    Config
	dev    *device.Device
	tnet   *netstack.Net
	bind   *MultiplexBind // non-nil only when relay multiplex was built
	closed bool
}

// MultiplexBind exposes the underlying relay-aware bind so callers
// (mainly tests) can inspect the udp/relay send counters. nil when the
// engine was built without relay support.
func (e *Engine) Bind() *MultiplexBind { return e.bind }

// LoadPrivateKey reads a base64-encoded WireGuard private key file.
func LoadPrivateKey(path string) ([]byte, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	body = []byte(strings.TrimSpace(string(body)))
	raw, err := base64.StdEncoding.DecodeString(string(body))
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("private key in %s must decode to 32 bytes (got %d)", path, len(raw))
	}
	return raw, nil
}

// NewEngine constructs and starts a userspace WireGuard engine.
func NewEngine(cfg Config) (*Engine, error) {
	if !cfg.SelfOverlayIP.IsValid() {
		return nil, errors.New("self overlay_ip is invalid")
	}
	if cfg.MTU == 0 {
		cfg.MTU = DefaultMTU
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	tunDev, tnet, err := netstack.CreateNetTUN([]netip.Addr{cfg.SelfOverlayIP}, nil, cfg.MTU)
	if err != nil {
		return nil, fmt.Errorf("create netstack tun: %w", err)
	}

	var bind conn.Bind
	var muxBind *MultiplexBind
	if cfg.SelfDeviceID != "" {
		muxBind = NewMultiplexBind(MultiplexBindConfig{
			Logger:        cfg.Logger,
			SelfDeviceID:  cfg.SelfDeviceID,
			SelfNetworkID: cfg.SelfNetworkID,
			SelfNodePub:   cfg.SelfNodePub,
			Factory:       cfg.RelayClientFactory,
		})
		bind = muxBind
	} else {
		bind = conn.NewStdNetBind()
	}
	wgLogger := wireguardLogger(cfg.Logger.With("component", "wireguard", "self", cfg.SelfName))
	dev := device.NewDevice(tunDev, bind, wgLogger)

	peers := make([]UAPIPeer, 0, len(cfg.Peers))
	for _, p := range cfg.Peers {
		peers = append(peers, UAPIPeer{
			PublicKey:                   p.WireGuardPublicKey,
			AllowedIPs:                  []string{p.OverlayIP.String() + "/32"},
			Endpoint:                    p.Endpoint,
			PersistentKeepaliveInterval: p.PersistentKeepalive,
		})
	}
	uapi, err := BuildUAPI(UAPIConfig{
		PrivateKey: cfg.SelfPrivateKey,
		ListenPort: cfg.ListenPort,
		Peers:      peers,
	})
	if err != nil {
		dev.Close()
		return nil, fmt.Errorf("build uapi: %w", err)
	}
	if err := dev.IpcSet(uapi); err != nil {
		dev.Close()
		return nil, fmt.Errorf("ipc set: %w", err)
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, fmt.Errorf("device up: %w", err)
	}
	cfg.Logger.Info("wgnet engine up",
		"self", cfg.SelfName,
		"overlay_ip", cfg.SelfOverlayIP.String(),
		"listen_port", cfg.ListenPort,
		"peer_count", len(cfg.Peers),
		"relay_enabled", muxBind != nil,
	)
	return &Engine{cfg: cfg, dev: dev, tnet: tnet, bind: muxBind}, nil
}

// UpdatePeers atomically replaces the device's peer set. Used by the
// agent to react to Network Map updates from the Control Plane. The
// device's private key and listen port are not touched.
func (e *Engine) UpdatePeers(peers []Peer) error {
	if e == nil || e.dev == nil {
		return errors.New("wgnet: engine not initialized")
	}
	uapiPeers := make([]UAPIPeer, 0, len(peers))
	for _, p := range peers {
		uapiPeers = append(uapiPeers, UAPIPeer{
			PublicKey:                   p.WireGuardPublicKey,
			AllowedIPs:                  []string{p.OverlayIP.String() + "/32"},
			Endpoint:                    p.Endpoint,
			PersistentKeepaliveInterval: p.PersistentKeepalive,
		})
	}
	uapi, err := BuildUAPIPeerReplace(uapiPeers)
	if err != nil {
		return fmt.Errorf("wgnet: build replace uapi: %w", err)
	}
	if err := e.dev.IpcSet(uapi); err != nil {
		return fmt.Errorf("wgnet: ipc set replace: %w", err)
	}
	e.cfg.Logger.Info("wgnet peers updated", "self", e.cfg.SelfName, "peer_count", len(peers))
	return nil
}

// PeerHandshakeTimes returns the last successful WireGuard handshake
// time per peer, keyed by the peer's standard-base64-encoded public key
// (matching how the agent identifies peers in Network Map terms).
//
// Peers that have never completed a handshake are included with a zero
// or near-epoch time, so callers can simply check time.Since(t) > N
// without having to special-case absence.
//
// Used by the relay-fallback loop: when a peer hasn't handshaken in
// e.g. 30 seconds and a HomeRelay is available, the agent rewrites the
// peer's endpoint to the relay form.
func (e *Engine) PeerHandshakeTimes() (map[string]time.Time, error) {
	if e == nil || e.dev == nil {
		return nil, errors.New("wgnet: engine not initialized")
	}
	raw, err := e.dev.IpcGet()
	if err != nil {
		return nil, fmt.Errorf("ipc get: %w", err)
	}
	out := map[string]time.Time{}
	var (
		currentPubB64 string
		hsSec         int64
		hsNsec        int64
	)
	finalize := func() {
		if currentPubB64 != "" {
			out[currentPubB64] = time.Unix(hsSec, hsNsec)
		}
		currentPubB64, hsSec, hsNsec = "", 0, 0
	}
	for _, line := range strings.Split(raw, "\n") {
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "public_key":
			finalize()
			rawKey, err := hex.DecodeString(v)
			if err == nil && len(rawKey) == 32 {
				currentPubB64 = base64.StdEncoding.EncodeToString(rawKey)
			}
		case "last_handshake_time_sec":
			n, _ := strconv.ParseInt(v, 10, 64)
			hsSec = n
		case "last_handshake_time_nsec":
			n, _ := strconv.ParseInt(v, 10, 64)
			hsNsec = n
		}
	}
	finalize()
	return out, nil
}

// DialOverlayTCP opens a TCP connection to a peer over the overlay.
func (e *Engine) DialOverlayTCP(ctx context.Context, peer netip.Addr, port uint16) (net.Conn, error) {
	return e.tnet.DialContextTCPAddrPort(ctx, netip.AddrPortFrom(peer, port))
}

// ListenOverlayTCP listens on the local overlay IP.
func (e *Engine) ListenOverlayTCP(port uint16) (net.Listener, error) {
	return e.tnet.ListenTCPAddrPort(netip.AddrPortFrom(e.cfg.SelfOverlayIP, port))
}

func (e *Engine) Close() error {
	if e == nil || e.closed {
		return nil
	}
	e.closed = true
	if e.dev != nil {
		e.dev.Close()
	}
	return nil
}

// wireguardLogger adapts slog.Logger to the device.Logger interface used by
// wireguard-go. wireguard-go uses Verbosef/Errorf which are formatted like
// log.Printf.
func wireguardLogger(l *slog.Logger) *device.Logger {
	return &device.Logger{
		Verbosef: func(format string, args ...any) {
			l.Debug(fmt.Sprintf(format, args...))
		},
		Errorf: func(format string, args ...any) {
			l.Error(fmt.Sprintf(format, args...))
		},
	}
}
