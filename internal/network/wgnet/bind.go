package wgnet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.zx2c4.com/wireguard/conn"

	"github.com/waired-ai/waired-agent/internal/relay/client"
	"github.com/waired-ai/waired-agent/proto/disco"
	"github.com/waired-ai/waired-agent/proto/frame"
)

// RelayClientFactory builds a relay.Client wired to the given relay URL.
// The agent owns the bearer/keys and decides per-relay TLS pinning, so
// the bind only knows the URL it was asked to dial.
type RelayClientFactory func(url string) (*client.Client, error)

// MultiplexBind is a conn.Bind that multiplexes between a real UDP
// socket (for "udp4:...":-prefixed peer endpoints) and zero or more
// relay sessions (for "relay:..."-prefixed endpoints).
//
// Lifecycle:
//   - ParseEndpoint(s) decides the path and, for relay endpoints, lazily
//     spawns the per-relay Client goroutine on first use.
//   - Open(port) returns the StdNetBind ReceiveFuncs concatenated with a
//     single relay receive func that drains every relay client's Inbound
//     into wireguard-go.
//   - Send(bufs, ep) type-switches on ep: relayEndpoint forwards into the
//     matching relay client; everything else delegates to StdNetBind.
//   - Close() cancels every relay client, then closes the UDP bind.
type MultiplexBind struct {
	udp    conn.Bind
	logger *slog.Logger

	selfDeviceID  string
	selfNetworkID string
	selfNodePub   string

	factory RelayClientFactory

	relayMu sync.Mutex
	relays  map[string]*relayHandle // keyed by URL
	// relayRetry tracks per-URL redial backoff after a relay session
	// dies (guarded by relayMu). Entries persist across successful
	// sessions; a session that lived ≥ healthyAfter resets the failure
	// streak when it eventually dies.
	relayRetry map[string]*relayRetryState
	inbound    chan inboundRelayPkt
	closed     atomic.Bool
	rootCtx    context.Context
	rootCancel context.CancelFunc

	redialBase   time.Duration
	redialMax    time.Duration
	healthyAfter time.Duration

	udpSendCount   atomic.Int64
	relaySendCount atomic.Int64

	// peerNets maps deviceID → home NetworkID for CP-injected
	// cross-network Public Share peers (spec §10): relay frames to
	// them must carry frame.EncryptedPacket.DstNetworkID so the relay
	// can authorize + route across networks (waired#822). Same-network
	// peers never appear here, keeping their frames byte-identical
	// (omitempty). Replaced wholesale from the reconciler on every
	// netmap application; nil until the first cross-network peer.
	peerNets atomic.Pointer[map[string]string]

	// discoIn fans out incoming disco frames demultiplexed from either
	// the WG UDP socket (direct UDP path) or any active relay session
	// (relay-tunnelled disco). The disco subsystem
	// (internal/network/disco) reads from this channel. Type is defined
	// in internal/disco so both the agent disco service and wgnet share
	// one shape.
	discoIn chan disco.Inbound
	// discoSendCount counts disco frames sent via SendDisco (direct UDP)
	// so tests can assert "agent actually probed".
	discoSendCount atomic.Int64
	// discoRelaySendCount counts disco frames sent via SendDiscoViaRelay
	// so tests can assert "agent probed via relay too".
	discoRelaySendCount atomic.Int64
}

type relayHandle struct {
	cli     *client.Client
	cancel  context.CancelFunc
	url     string
	started time.Time // when the session goroutine was spawned; feeds the healthyAfter reset
	endpts  sync.Map  // dstDeviceID -> *relayEndpoint (so reads can pin a stable Endpoint)
}

// relayRetryState is the per-URL redial bookkeeping. failures counts
// consecutive short-lived sessions; nextAttempt gates ensureRelay.
type relayRetryState struct {
	failures    int
	nextAttempt time.Time
}

type inboundRelayPkt struct {
	payload []byte
	ep      *relayEndpoint
}

// MultiplexBindConfig is the New input. SelfDeviceID/SelfNetworkID/
// SelfNodePub fill the EncryptedPacket header; Factory turns a relay URL
// into a configured Client.
type MultiplexBindConfig struct {
	Logger        *slog.Logger
	SelfDeviceID  string
	SelfNetworkID string
	SelfNodePub   string // std-base64
	Factory       RelayClientFactory

	// RelayRedialBase / RelayRedialMax bound the capped-exponential
	// backoff between re-dial attempts after a relay session dies
	// (#163). RelayHealthyAfter is the minimum session lifetime that
	// counts as "was healthy" and resets the failure streak. Zero
	// values pick the production defaults (1s / 30s / 30s). The max
	// must stay below the 60s direct→relay safety-net window and the
	// 90s relay stale-TTL so a recovering relay is re-dialed before
	// either machinery tips over.
	RelayRedialBase   time.Duration
	RelayRedialMax    time.Duration
	RelayHealthyAfter time.Duration
}

// NewMultiplexBind builds a Bind composing conn.NewStdNetBind with a
// relay multiplexer. Factory may be nil if the caller never plans to
// dial a relay; in that case ParseEndpoint will reject "relay:" inputs.
func NewMultiplexBind(cfg MultiplexBindConfig) *MultiplexBind {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	redialBase := cfg.RelayRedialBase
	if redialBase <= 0 {
		redialBase = time.Second
	}
	redialMax := cfg.RelayRedialMax
	if redialMax <= 0 {
		redialMax = 30 * time.Second
	}
	healthyAfter := cfg.RelayHealthyAfter
	if healthyAfter <= 0 {
		healthyAfter = 30 * time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &MultiplexBind{
		udp:           conn.NewStdNetBind(),
		logger:        logger,
		selfDeviceID:  cfg.SelfDeviceID,
		selfNetworkID: cfg.SelfNetworkID,
		selfNodePub:   cfg.SelfNodePub,
		factory:       cfg.Factory,
		relays:        map[string]*relayHandle{},
		relayRetry:    map[string]*relayRetryState{},
		inbound:       make(chan inboundRelayPkt, 256),
		discoIn:       make(chan disco.Inbound, 64),
		rootCtx:       ctx,
		rootCancel:    cancel,
		redialBase:    redialBase,
		redialMax:     redialMax,
		healthyAfter:  healthyAfter,
	}
}

// DiscoInbound returns a receive-only channel of disco frames the WG
// UDP socket classifier filtered out of the WG stream. The disco
// subsystem reads this channel and decodes each frame.
//
// The channel is created on bind construction and stays open across
// Close()/Open() cycles (wireguard-go calls those when the listen
// port changes). Consumers must not close it.
func (b *MultiplexBind) DiscoInbound() <-chan disco.Inbound { return b.discoIn }

// DiscoSendCount returns the number of disco frames pushed through
// SendDisco. Tests use this to assert the agent actually probed.
func (b *MultiplexBind) DiscoSendCount() int64 { return b.discoSendCount.Load() }

// SendDisco transmits one disco frame from the WG UDP socket to the
// given destination. Using the same socket as WireGuard is essential —
// NAT mappings only stay open for the (src_ip, src_port, dst_ip,
// dst_port) tuple that originally created them, and we want disco
// probes to ride that mapping so the discovered observed addr matches
// what WG will use later.
//
// dstAddr accepts the wgnet endpoint syntax: "udp4:host:port",
// "udp6:[host]:port", or a bare "host:port". The "udp4:"/"udp6:"
// prefix is stripped before delegating to the underlying StdNetBind
// (which only understands the bare form). Relay endpoints are not
// allowed here — use SendDiscoViaRelay for relay-tunnelled disco.
func (b *MultiplexBind) SendDisco(payload []byte, dstAddr string) error {
	if strings.HasPrefix(dstAddr, "relay:") {
		return errors.New("multiplex bind: SendDisco refuses relay endpoint")
	}
	bare := dstAddr
	switch {
	case strings.HasPrefix(bare, "udp4:"):
		bare = strings.TrimPrefix(bare, "udp4:")
	case strings.HasPrefix(bare, "udp6:"):
		bare = strings.TrimPrefix(bare, "udp6:")
	}
	ep, err := b.udp.ParseEndpoint(bare)
	if err != nil {
		return fmt.Errorf("multiplex bind: disco parse %q: %w", dstAddr, err)
	}
	if err := b.udp.Send([][]byte{payload}, ep); err != nil {
		return fmt.Errorf("multiplex bind: disco send: %w", err)
	}
	b.discoSendCount.Add(1)
	return nil
}

// SendDiscoViaRelay tunnels a disco frame through a peer's HomeRelay
// session. The frame body is opaque to the relay (the relay forwards
// EncryptedPacket frames by DstDeviceID without decoding the payload),
// so disco probes/pongs piggyback on the same EncryptedPacket envelope
// as WG packets. The receiver's MultiplexBind classifies them by the
// disco magic prefix and routes them into b.discoIn instead of into
// the WG inbound queue (mirrors classifyDisco for the direct UDP path).
//
// dstDeviceID is the peer's device_id (= relay-level routing key).
// dstNodeKey is included in the EncryptedPacket header for symmetry
// with WG-over-relay sends; the relay does not inspect it.
// relayURL identifies the relay session (must match an endpoint the
// agent has previously dialed via ParseEndpoint or that has been
// implicitly created by an inbound peer's HomeRelay).
func (b *MultiplexBind) SendDiscoViaRelay(payload []byte, dstDeviceID, dstNodeKey, relayURL string) error {
	if relayURL == "" {
		return errors.New("multiplex bind: SendDiscoViaRelay requires relay URL")
	}
	if dstDeviceID == "" {
		return errors.New("multiplex bind: SendDiscoViaRelay requires dst device id")
	}
	h, err := b.ensureRelay(relayURL)
	if err != nil {
		return err
	}
	pkt := b.newEncryptedPacket(dstDeviceID, dstNodeKey, payload)
	if err := h.cli.Send(pkt); err != nil {
		return fmt.Errorf("multiplex bind: disco-via-relay send: %w", err)
	}
	b.discoRelaySendCount.Add(1)
	return nil
}

// DiscoRelaySendCount returns the number of disco frames pushed through
// SendDiscoViaRelay. Tests use this to assert "agent probed via relay".
func (b *MultiplexBind) DiscoRelaySendCount() int64 { return b.discoRelaySendCount.Load() }

// SetPeerNetworks replaces the cross-network peer table (public share
// spec §10): deviceID → the peer's home NetworkID for CP-injected
// foreign peers (NetworkMapPeer.NetworkID). Same-network peers must
// not appear. Called from the reconciler on every netmap application,
// BEFORE peers/endpoints are updated, so the registry is populated by
// the time any send can target a foreign peer.
func (b *MultiplexBind) SetPeerNetworks(nets map[string]string) {
	if len(nets) == 0 {
		b.peerNets.Store(nil)
		return
	}
	cp := make(map[string]string, len(nets))
	for k, v := range nets {
		cp[k] = v
	}
	b.peerNets.Store(&cp)
}

// peerNetworkFor returns the foreign home network for dstDeviceID, or
// "" for same-network peers (the common case: nil table or no entry).
func (b *MultiplexBind) peerNetworkFor(dstDeviceID string) string {
	m := b.peerNets.Load()
	if m == nil {
		return ""
	}
	return (*m)[dstDeviceID]
}

// newEncryptedPacket builds one relay frame. Both relay send paths (WG
// payloads via sendRelay, disco via SendDiscoViaRelay) MUST go through
// here: cross-network peers need DstNetworkID stamped on every frame —
// including disco, or hard-NAT×hard-NAT public pairs could never even
// rendezvous (§15-7). Same-network frames leave it empty (omitempty ⇒
// wire bytes unchanged).
func (b *MultiplexBind) newEncryptedPacket(dstDeviceID, dstNodeKey string, payload []byte) frame.EncryptedPacket {
	pkt := frame.EncryptedPacket{
		Type:         frame.TypeEncryptedPacket,
		Version:      frame.Version,
		NetworkID:    b.selfNetworkID,
		SrcDeviceID:  b.selfDeviceID,
		DstDeviceID:  dstDeviceID,
		DstNodeKeyID: dstNodeKey,
		SrcNodeKeyID: b.selfNodePub,
		Payload:      base64Std(payload),
	}
	if net := b.peerNetworkFor(dstDeviceID); net != "" && net != b.selfNetworkID {
		pkt.DstNetworkID = net
	}
	return pkt
}

// UDPSendCount returns the number of packets routed via the embedded UDP
// bind. Tests use this to assert relay-only enforcement (counter == 0).
func (b *MultiplexBind) UDPSendCount() int64 { return b.udpSendCount.Load() }

// RelaySendCount returns the number of packets routed via any relay
// client. Tests use this to assert traffic actually traversed a relay.
func (b *MultiplexBind) RelaySendCount() int64 { return b.relaySendCount.Load() }

// --- conn.Bind implementation ---

func (b *MultiplexBind) Open(port uint16) ([]conn.ReceiveFunc, uint16, error) {
	// wireguard-go calls Close() then Open() on the same bind instance
	// when the listen port changes (BindUpdate), so Open must reset any
	// "permanently closed" state we set in Close().
	b.relayMu.Lock()
	b.closed.Store(false)
	if b.relays == nil {
		b.relays = map[string]*relayHandle{}
	}
	// A fresh Open starts the relay world over — drop any redial
	// backoff accumulated before the Close()/Open() cycle.
	b.relayRetry = map[string]*relayRetryState{}
	if b.rootCtx == nil || b.rootCtx.Err() != nil {
		b.rootCtx, b.rootCancel = context.WithCancel(context.Background())
	}
	b.relayMu.Unlock()

	udpFns, actualPort, err := b.udp.Open(port)
	if err != nil {
		return nil, 0, err
	}
	classified := make([]conn.ReceiveFunc, len(udpFns))
	for i, fn := range udpFns {
		classified[i] = b.classifyDisco(fn)
	}
	relayFn := b.relayReceiveFunc()
	return append(classified, relayFn), actualPort, nil
}

// classifyDisco wraps one of the StdNetBind ReceiveFuncs so that any
// inbound packet starting with the disco magic prefix is split off into
// b.discoIn instead of flowing through to wireguard-go (which would
// promptly drop it as malformed).
//
// The classifier returns however many WG packets remain in the batch.
// If every packet in a batch was disco, the loop calls inner again so
// the wireguard-go reader stays blocked on its recvmmsg syscall instead
// of spinning on zero returns.
func (b *MultiplexBind) classifyDisco(inner conn.ReceiveFunc) conn.ReceiveFunc {
	return func(packets [][]byte, sizes []int, eps []conn.Endpoint) (int, error) {
		for {
			n, err := inner(packets, sizes, eps)
			if err != nil || n == 0 {
				return n, err
			}
			written := 0
			for i := 0; i < n; i++ {
				size := sizes[i]
				if size >= disco.HeaderSize && [6]byte(packets[i][:6]) == disco.Magic {
					payload := make([]byte, size)
					copy(payload, packets[i][:size])
					src := endpointToAddrPort(eps[i])
					select {
					case b.discoIn <- disco.Inbound{Payload: payload, Src: src}:
					default:
						b.logger.Warn("multiplex bind: disco inbound queue full; dropping")
					}
					continue
				}
				if written != i {
					copy(packets[written], packets[i][:size])
					sizes[written] = size
					eps[written] = eps[i]
				}
				written++
			}
			if written > 0 {
				return written, nil
			}
			// Whole batch was disco — go round again so we don't
			// return 0/nil (which would confuse wireguard-go's
			// receive loop).
		}
	}
}

// endpointToAddrPort extracts a netip.AddrPort from a StdNetBind
// endpoint. The standard Bind's endpoint type's DstToString() returns
// "host:port"; we parse that for src bookkeeping. Returns the zero
// AddrPort on failure (the disco subsystem treats that as "unknown
// src" and drops).
func endpointToAddrPort(ep conn.Endpoint) netip.AddrPort {
	if ep == nil {
		return netip.AddrPort{}
	}
	ap, err := netip.ParseAddrPort(ep.DstToString())
	if err != nil {
		return netip.AddrPort{}
	}
	return ap
}

func (b *MultiplexBind) Close() error {
	if b.closed.Swap(true) {
		return nil
	}
	b.rootCancel()
	b.relayMu.Lock()
	for _, h := range b.relays {
		h.cancel()
	}
	b.relays = nil
	b.relayMu.Unlock()
	return b.udp.Close()
}

func (b *MultiplexBind) SetMark(mark uint32) error { return b.udp.SetMark(mark) }

func (b *MultiplexBind) BatchSize() int { return b.udp.BatchSize() }

func (b *MultiplexBind) Send(bufs [][]byte, ep conn.Endpoint) error {
	switch e := ep.(type) {
	case *relayEndpoint:
		return b.sendRelay(bufs, e)
	default:
		b.udpSendCount.Add(int64(len(bufs)))
		return b.udp.Send(bufs, ep)
	}
}

func (b *MultiplexBind) ParseEndpoint(s string) (conn.Endpoint, error) {
	if strings.HasPrefix(s, "relay:") {
		return b.parseRelayEndpoint(s)
	}
	// fall through to StdNetBind for "udp4:host:port" and bare host:port
	return b.udp.ParseEndpoint(s)
}

// --- relay path ---

// sendRelay marshals each buf as an EncryptedPacket and pushes through
// the relay client's outbound queue. Drops the buf if the queue is full
// (relay client has its own bound queue) and counts the drop.
func (b *MultiplexBind) sendRelay(bufs [][]byte, ep *relayEndpoint) error {
	h, err := b.ensureRelay(ep.url)
	if err != nil {
		if errors.Is(err, errRelayBackoff) {
			// Drop quietly: WG retransmits on its own schedule, and a
			// Warn per packet during a relay outage would flood logs.
			b.logger.Debug("multiplex bind: relay send skipped (redial backoff)", "url", ep.url)
			return nil
		}
		return err
	}
	// Make sure the inbound side will return this exact endpoint when a
	// reply arrives. ParseEndpoint registered it earlier; this is a
	// belt-and-braces store for endpoints constructed by hand in tests.
	h.endpts.Store(ep.dstDeviceID, ep)
	for _, buf := range bufs {
		pkt := b.newEncryptedPacket(ep.dstDeviceID, ep.dstNodeKey, buf)
		if err := h.cli.Send(pkt); err != nil {
			b.logger.Warn("multiplex bind: relay send failed", "url", ep.url, "err", err)
			continue
		}
		b.relaySendCount.Add(1)
	}
	return nil
}

// relayReceiveFunc returns a single conn.ReceiveFunc that drains the
// fan-in inbound channel and writes packets into the buffers
// wireguard-go provides.
func (b *MultiplexBind) relayReceiveFunc() conn.ReceiveFunc {
	return func(packets [][]byte, sizes []int, eps []conn.Endpoint) (int, error) {
		select {
		case in, ok := <-b.inbound:
			if !ok {
				return 0, errClosed
			}
			if len(packets) == 0 {
				return 0, errors.New("relay recv: zero buffers")
			}
			n := copy(packets[0], in.payload)
			sizes[0] = n
			eps[0] = in.ep
			return 1, nil
		case <-b.rootCtx.Done():
			return 0, errClosed
		}
	}
}

// ensureRelay returns the live relay handle for url, creating one on
// first use via the configured factory. A URL whose previous session
// just died is gated by a capped-exponential backoff (errRelayBackoff)
// so a down relay costs at most ~one dial per redialMax per agent.
func (b *MultiplexBind) ensureRelay(url string) (*relayHandle, error) {
	if b.factory == nil {
		return nil, errors.New("relay endpoint requested but no RelayClientFactory configured")
	}
	b.relayMu.Lock()
	defer b.relayMu.Unlock()
	if b.closed.Load() {
		return nil, errClosed
	}
	if h, ok := b.relays[url]; ok {
		return h, nil
	}
	if st, ok := b.relayRetry[url]; ok && time.Now().Before(st.nextAttempt) {
		return nil, errRelayBackoff
	}
	cli, err := b.factory(url)
	if err != nil {
		return nil, fmt.Errorf("factory(%s): %w", url, err)
	}
	ctx, cancel := context.WithCancel(b.rootCtx)
	h := &relayHandle{cli: cli, cancel: cancel, url: url, started: time.Now()}
	b.relays[url] = h

	go func() {
		err := cli.Run(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			b.logger.Warn("multiplex bind: relay client exited", "url", url, "err", err)
		}
		// Evict so the next Send re-dials through the factory. Re-using
		// the dead client would strand the URL forever, and the factory
		// must re-resolve the TLS pin anyway — a restarted relay presents
		// a fresh self-signed cert (#163). Local shutdown (Close/DropRelay
		// cancelled ctx) is not held against the relay.
		b.evictRelay(h, ctx.Err() == nil)
	}()
	go b.fanInRelay(ctx, h)
	return h, nil
}

// evictRelay removes a dead relay handle from the session map and, when
// countFailure is set, arms the redial backoff for its URL. A session
// that stayed up ≥ healthyAfter starts a new failure streak instead of
// extending the old one.
func (b *MultiplexBind) evictRelay(h *relayHandle, countFailure bool) {
	b.relayMu.Lock()
	defer b.relayMu.Unlock()
	if cur, ok := b.relays[h.url]; ok && cur == h {
		delete(b.relays, h.url)
	}
	h.cancel() // stop fanInRelay; no-op if already cancelled
	if !countFailure {
		return
	}
	if b.relayRetry == nil {
		b.relayRetry = map[string]*relayRetryState{}
	}
	st := b.relayRetry[h.url]
	if st == nil {
		st = &relayRetryState{}
		b.relayRetry[h.url] = st
	}
	if time.Since(h.started) >= b.healthyAfter {
		st.failures = 0
	}
	st.failures++
	st.nextAttempt = time.Now().Add(nextRelayBackoff(st.failures, b.redialBase, b.redialMax))
}

// DropRelay tears down the cached client session for url (if any) and
// clears its redial backoff so the next Send re-dials immediately. The
// agent calls this when a network map delivers a NEW TLS fingerprint
// for a URL — the running session was pinned to the old cert and can
// only fail from here on.
func (b *MultiplexBind) DropRelay(url string) {
	b.relayMu.Lock()
	defer b.relayMu.Unlock()
	if h, ok := b.relays[url]; ok {
		h.cancel()
		delete(b.relays, url)
	}
	delete(b.relayRetry, url)
}

// nextRelayBackoff returns the wait before re-dial attempt number
// failures (1-based): capped exponential with ±20% jitter, so a fleet
// of agents doesn't re-dial a recovering relay in lockstep.
func nextRelayBackoff(failures int, base, max time.Duration) time.Duration {
	d := base
	for i := 1; i < failures && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	jitter := 0.8 + 0.4*rand.Float64()
	return time.Duration(float64(d) * jitter)
}

// fanInRelay copies one relay's Inbound stream into the bind's shared
// inbound channel so the single ReceiveFunc can drain all relays.
//
// Disco frames tunnelled via the relay (sent by SendDiscoViaRelay on
// the peer side) are demuxed by the disco magic prefix and forwarded
// to b.discoIn with Path="relay" so handleProbe / handlePong can route
// the response back over the same relay session. This mirrors
// classifyDisco for the direct UDP path.
func (b *MultiplexBind) fanInRelay(ctx context.Context, h *relayHandle) {
	for {
		select {
		case <-ctx.Done():
			return
		case in, ok := <-h.cli.Inbound():
			if !ok {
				return
			}
			if isDiscoPayload(in.Payload) {
				select {
				case b.discoIn <- disco.Inbound{
					Payload:          append([]byte(nil), in.Payload...),
					Path:             disco.PathRelay,
					RelayURL:         h.url,
					RelaySrcDeviceID: in.SrcDeviceID,
				}:
				default:
					b.logger.Warn("multiplex bind: relay disco inbound full; dropping")
				}
				continue
			}
			ep := b.lookupOrMakeEndpoint(h, in.SrcDeviceID)
			select {
			case b.inbound <- inboundRelayPkt{payload: in.Payload, ep: ep}:
			default:
				b.logger.Warn("multiplex bind: relay inbound full; dropping")
			}
		}
	}
}

// isDiscoPayload reports whether buf starts with the disco magic prefix.
// Used by fanInRelay to demux disco frames out of relay-tunnelled
// payloads (the same trick classifyDisco plays for direct UDP).
func isDiscoPayload(buf []byte) bool {
	return len(buf) >= disco.HeaderSize && [6]byte(buf[:6]) == disco.Magic
}

func (b *MultiplexBind) lookupOrMakeEndpoint(h *relayHandle, srcDeviceID string) *relayEndpoint {
	if v, ok := h.endpts.Load(srcDeviceID); ok {
		return v.(*relayEndpoint)
	}
	ep := &relayEndpoint{
		url:         h.url,
		dstDeviceID: srcDeviceID,
	}
	h.endpts.Store(srcDeviceID, ep)
	return ep
}

// parseRelayEndpoint accepts "relay:<url>#dst=<deviceID>&nk=<base64>".
// It registers the endpoint with the matching relay handle (creating
// the handle on first use via the factory) so a later Send dispatches
// quickly without re-parsing.
func (b *MultiplexBind) parseRelayEndpoint(s string) (*relayEndpoint, error) {
	rest := strings.TrimPrefix(s, "relay:")
	hashIdx := strings.IndexByte(rest, '#')
	if hashIdx < 0 {
		return nil, errors.New("relay endpoint: missing '#' separator")
	}
	rawURL := rest[:hashIdx]
	q, err := url.ParseQuery(rest[hashIdx+1:])
	if err != nil {
		return nil, fmt.Errorf("relay endpoint: parse fragment: %w", err)
	}
	dst := q.Get("dst")
	if dst == "" {
		return nil, errors.New("relay endpoint: missing dst")
	}
	ep := &relayEndpoint{
		url:         rawURL,
		dstDeviceID: dst,
		dstNodeKey:  q.Get("nk"),
	}
	if b.factory != nil {
		if h, err := b.ensureRelay(rawURL); err == nil {
			h.endpts.Store(dst, ep)
		}
	}
	return ep, nil
}

var errClosed = errors.New("multiplex bind: closed")

// errRelayBackoff gates re-dials to a recently-died relay session.
// sendRelay treats it as a silent drop; other callers (disco probes)
// surface it at their own cadence.
var errRelayBackoff = errors.New("multiplex bind: relay in redial backoff")
