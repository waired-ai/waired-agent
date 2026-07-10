package disco

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/netip"
	"testing"
	"time"

	"golang.org/x/crypto/curve25519"

	wireframe "github.com/waired-ai/waired-agent/proto/disco"
)

// encodeNodePubB64 mirrors the production base64 encoding used as the
// s.peers map key (std-base64 of the raw 32-byte NodeKey pub).
func encodeNodePubB64(pub [wireframe.NodeKeySize]byte) string {
	return base64.StdEncoding.EncodeToString(pub[:])
}

// fakeBind simulates both transports (direct UDP + relay) for the
// disco service. SendDisco records (payload, dst) on `sent`; relay
// sends record (payload, dstDeviceID, dstNodeKey, relayURL) on
// `relaySent`. Tests inject inbound packets by pushing onto Inbound.
type fakeBind struct {
	sent      chan sentDiscoPacket
	relaySent chan sentRelayDiscoPacket
	inbound   chan wireframe.Inbound
}

type sentDiscoPacket struct {
	Payload []byte
	Dst     string
}

type sentRelayDiscoPacket struct {
	Payload     []byte
	DstDeviceID string
	DstNodeKey  string
	RelayURL    string
}

func newFakeBind() *fakeBind {
	return &fakeBind{
		sent:      make(chan sentDiscoPacket, 32),
		relaySent: make(chan sentRelayDiscoPacket, 32),
		inbound:   make(chan wireframe.Inbound, 32),
	}
}

func (b *fakeBind) SendDisco(payload []byte, dst string) error {
	cp := append([]byte(nil), payload...)
	select {
	case b.sent <- sentDiscoPacket{Payload: cp, Dst: dst}:
	default:
	}
	return nil
}

func (b *fakeBind) SendDiscoViaRelay(payload []byte, dstDeviceID, dstNodeKey, relayURL string) error {
	cp := append([]byte(nil), payload...)
	select {
	case b.relaySent <- sentRelayDiscoPacket{
		Payload:     cp,
		DstDeviceID: dstDeviceID,
		DstNodeKey:  dstNodeKey,
		RelayURL:    relayURL,
	}:
	default:
	}
	return nil
}

func (b *fakeBind) DiscoInbound() <-chan wireframe.Inbound { return b.inbound }

// newNodeKey returns a fresh curve25519 keypair as [32]byte arrays —
// matches the disco.Config.SelfNodeKey{Priv,Pub} field shapes and the
// PeerSnapshot.NodePub shape.
func newNodeKey(t *testing.T) (priv, pub [wireframe.NodeKeySize]byte) {
	t.Helper()
	if _, err := rand.Read(priv[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	priv[0] &= 248
	priv[31] &= 127
	priv[31] |= 64
	out, err := curve25519.X25519(priv[:], curve25519.Basepoint)
	if err != nil {
		t.Fatalf("X25519: %v", err)
	}
	copy(pub[:], out)
	return priv, pub
}

func newService(t *testing.T, secret []byte, bind Bind) (*Service, [wireframe.NodeKeySize]byte, [wireframe.NodeKeySize]byte) {
	t.Helper()
	priv, pub := newNodeKey(t)
	cfg := Config{
		SelfDeviceID:       "dev_self",
		SelfNodeKeyPriv:    priv,
		SelfNodeKeyPub:     pub,
		RelaySharedSecret:  secret,
		Bind:               bind,
		STUNObserveActive:  100 * time.Millisecond,
		STUNTimeout:        200 * time.Millisecond,
		ProbeReprobeActive: 100 * time.Millisecond,
		ProbeWindow:        5 * time.Second,
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, priv, pub
}

func mustHMACSign(t *testing.T, f *wireframe.Frame, secret []byte) []byte {
	t.Helper()
	out, err := signFrameHMAC(f, secret)
	if err != nil {
		t.Fatalf("signFrameHMAC: %v", err)
	}
	return out
}

// mustEncodeSealed builds a sealed peer↔peer disco frame from senderPriv
// to receiverPub. Replacement for the pre-AEAD mustEd25519Sign helper —
// tests inject sealed bytes to simulate frames arriving from a peer.
func mustEncodeSealed(t *testing.T, f *wireframe.Frame, senderPriv, senderPub, receiverPub [wireframe.NodeKeySize]byte) []byte {
	t.Helper()
	out, err := wireframe.EncodeSealed(f, senderPriv, senderPub, receiverPub)
	if err != nil {
		t.Fatalf("EncodeSealed: %v", err)
	}
	return out
}

// mustDecodeSealed opens a sealed disco frame from the SUT's POV (i.e.,
// the service is the receiver). Returns the inner frame plus the
// authenticated sender NodeKey.
func mustDecodeSealed(t *testing.T, raw []byte, selfPriv, selfPub [wireframe.NodeKeySize]byte) (*wireframe.Frame, [wireframe.NodeKeySize]byte) {
	t.Helper()
	f, src, err := wireframe.DecodeSealed(raw, selfPriv, selfPub)
	if err != nil {
		t.Fatalf("DecodeSealed: %v", err)
	}
	return f, src
}

// mustDecodeSealedOrNil is the lenient variant used by tests that drain
// a channel of outbound frames and want to skip the ones that aren't
// sealed or that don't open with the given key (e.g., the SUT may emit
// STUN frames concurrently with peer↔peer sealed ones). Returns
// (nil, zero) on any decode/AEAD failure rather than t.Fatal-ing.
func mustDecodeSealedOrNil(raw []byte, selfPriv, selfPub [wireframe.NodeKeySize]byte) (*wireframe.Frame, [wireframe.NodeKeySize]byte) {
	var zero [wireframe.NodeKeySize]byte
	if !wireframe.IsSealed(raw) {
		return nil, zero
	}
	f, src, err := wireframe.DecodeSealed(raw, selfPriv, selfPub)
	if err != nil {
		return nil, zero
	}
	return f, src
}

// TestObserveRoundtrip drives a fake STUN echo: when the service emits
// a stun_request, the test fakes a matching stun_response. The service
// must emit EventObservedAddr with the responded addr.
func TestObserveRoundtrip(t *testing.T) {
	secret := []byte("relay-shared-secret")
	bind := newFakeBind()
	s, _, _ := newService(t, secret, bind)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	s.UpdateRelays([]string{"wss://relay.example.com:443/relay/v1/connect"})

	// Wait for the first stun_request.
	var sent sentDiscoPacket
	select {
	case sent = <-bind.sent:
	case <-time.After(2 * time.Second):
		t.Fatal("no stun_request observed within 2s")
	}

	// Decode the request to grab the nonce, then build a matching response.
	reqFrame, err := wireframe.Decode(sent.Payload)
	if err != nil {
		t.Fatalf("decode req: %v", err)
	}
	if reqFrame.Type != wireframe.TypeSTUNRequest {
		t.Fatalf("expected stun_request, got %s", reqFrame.Type)
	}

	observedAddr := netip.MustParseAddrPort("203.0.113.42:51820")
	resp := &wireframe.Frame{
		Type:         wireframe.TypeSTUNResponse,
		HasNonce:     true,
		Nonce:        reqFrame.Nonce,
		HasTimestamp: true,
		Timestamp:    uint64(time.Now().UnixMilli()),
		HasObserved:  true,
		ObservedAddr: observedAddr,
	}
	respBytes := mustHMACSign(t, resp, secret)
	bind.inbound <- wireframe.Inbound{Payload: respBytes, Src: netip.AddrPort{}}

	// Wait for EventObservedAddr.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-s.Events():
			if obs, ok := ev.(EventObservedAddr); ok {
				if obs.Addr != observedAddr {
					t.Fatalf("observed = %v, want %v", obs.Addr, observedAddr)
				}
				return
			}
		case <-deadline:
			t.Fatal("EventObservedAddr not received within 2s")
		}
	}
}

// TestProbeAndPongRoundtripBetweenServices spins two services A and B,
// wires their fake binds back-to-back, and verifies that A→B probe
// triggers B→A pong, surfaced as EventPongFromPeer on A.
func TestProbeAndPongRoundtripBetweenServices(t *testing.T) {
	secret := []byte("rs")
	bindA := newFakeBind()
	bindB := newFakeBind()

	privA, pubA := newNodeKey(t)
	privB, pubB := newNodeKey(t)

	cfgA := Config{
		SelfDeviceID:       "dev_a",
		SelfNodeKeyPriv:    privA,
		SelfNodeKeyPub:     pubA,
		RelaySharedSecret:  secret,
		Bind:               bindA,
		STUNObserveActive:  time.Hour, // suppress STUN noise
		ProbeReprobeActive: 100 * time.Millisecond,
		ProbeWindow:        5 * time.Second,
	}
	cfgB := Config{
		SelfDeviceID:       "dev_b",
		SelfNodeKeyPriv:    privB,
		SelfNodeKeyPub:     pubB,
		RelaySharedSecret:  secret,
		Bind:               bindB,
		STUNObserveActive:  time.Hour,
		ProbeReprobeActive: time.Hour, // disable B's probe loop
		ProbeWindow:        5 * time.Second,
	}
	sA, err := New(cfgA)
	if err != nil {
		t.Fatalf("New A: %v", err)
	}
	sB, err := New(cfgB)
	if err != nil {
		t.Fatalf("New B: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sA.Run(ctx)
	go sB.Run(ctx)

	// Both know each other.
	sA.UpdatePeers(map[string]PeerSnapshot{
		"node_pub_b": {DeviceID: "dev_b", NodePub: pubB, Candidates: []string{"udp4:198.51.100.10:51820"}},
	})
	sB.UpdatePeers(map[string]PeerSnapshot{
		"node_pub_a": {DeviceID: "dev_a", NodePub: pubA, Candidates: []string{"udp4:198.51.100.20:51820"}},
	})

	// A sends probe → captured on bindA.sent. Forward to bindB.inbound
	// as if it had arrived over UDP.
	var probe sentDiscoPacket
	select {
	case probe = <-bindA.sent:
	case <-time.After(2 * time.Second):
		t.Fatal("A didn't send a probe")
	}
	srcOfProbe := netip.MustParseAddrPort("198.51.100.20:54321")
	bindB.inbound <- wireframe.Inbound{Payload: probe.Payload, Src: srcOfProbe}

	// B should reply with a pong via bindB.sent. B may *also* be
	// firing its own outbound probes (the two-side initiator pattern),
	// so drain the channel until we see a pong type.
	var pong sentDiscoPacket
	deadlineB := time.After(2 * time.Second)
DRAIN:
	for {
		select {
		case got := <-bindB.sent:
			// B's outbound is sealed (privB → pubA). Open from A's POV.
			f, _ := mustDecodeSealedOrNil(got.Payload, privA, pubA)
			if f == nil {
				continue
			}
			if f.Type == wireframe.TypePong {
				pong = got
				break DRAIN
			}
		case <-deadlineB:
			t.Fatal("B didn't send a pong")
		}
	}
	pongFrame, _ := mustDecodeSealed(t, pong.Payload, privA, pubA)
	if pongFrame.Type != wireframe.TypePong {
		t.Fatalf("B's reply is not a pong: %s", pongFrame.Type)
	}
	if !pongFrame.HasObserved || pongFrame.ObservedAddr != srcOfProbe {
		t.Fatalf("pong missing observed_outer; got %v want %v", pongFrame.ObservedAddr, srcOfProbe)
	}

	// Forward pong back to A as if arriving over UDP.
	pongSrc := netip.MustParseAddrPort("198.51.100.10:51820")
	bindA.inbound <- wireframe.Inbound{Payload: pong.Payload, Src: pongSrc}

	// A should emit EventPongFromPeer for "node_pub_b".
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-sA.Events():
			if got, ok := ev.(EventPongFromPeer); ok {
				if got.PeerNodePub != "node_pub_b" {
					t.Fatalf("PeerNodePub = %q, want node_pub_b", got.PeerNodePub)
				}
				if got.DirectSrc != pongSrc {
					t.Fatalf("DirectSrc = %v, want %v", got.DirectSrc, pongSrc)
				}
				return
			}
		case <-deadline:
			t.Fatal("EventPongFromPeer not emitted within 2s")
		}
	}
}

func TestProbeWithBadSignatureRejected(t *testing.T) {
	secret := []byte("rs")
	bind := newFakeBind()
	s, _, selfPub := newService(t, secret, bind)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	privB, pubB := newNodeKey(t)
	privAttacker, pubAttacker := newNodeKey(t)
	s.UpdatePeers(map[string]PeerSnapshot{
		"node_pub_b": {DeviceID: "dev_b", NodePub: pubB, Candidates: []string{"udp4:198.51.100.10:51820"}},
	})

	// Attacker forges a probe claiming to be from dev_b but seals it
	// with privAttacker. AEAD opens (ECDH still works for any keypair),
	// but the srcNodeKey on the wire is pubAttacker ≠ peer.nodePub=pubB,
	// so handleProbe rejects on the impersonation cross-check.
	probe := &wireframe.Frame{
		Type:         wireframe.TypeProbe,
		SrcDeviceID:  "dev_b",
		DstDeviceID:  "dev_self",
		HasNonce:     true,
		Nonce:        [wireframe.NonceSize]byte{0x33},
		HasTimestamp: true,
		Timestamp:    uint64(time.Now().UnixMilli()),
	}
	payload := mustEncodeSealed(t, probe, privAttacker, pubAttacker, selfPub)

	bind.inbound <- wireframe.Inbound{Payload: payload, Src: netip.MustParseAddrPort("203.0.113.99:9999")}

	// No pong should be emitted within 500ms — the probe's mismatched
	// SrcNodeKey causes a silent drop. Outbound probes from the service's
	// own re-probe loop (ProbeReprobeActive=100ms) are expected and must
	// be drained without failing — they're sealed (selfPriv → pubB) so
	// we open them from B's POV.
	deadline := time.After(500 * time.Millisecond)
	for {
		select {
		case got := <-bind.sent:
			f, _ := mustDecodeSealedOrNil(got.Payload, privB, pubB)
			if f == nil {
				continue
			}
			if f.Type == wireframe.TypePong {
				t.Fatalf("unexpected pong to forged probe: dst=%s len=%d", got.Dst, len(got.Payload))
			}
		case <-deadline:
			return
		}
	}
}

func TestSTUNResponseRejectsBadHMAC(t *testing.T) {
	secret := []byte("good")
	wrong := []byte("evil")
	bind := newFakeBind()
	s, _, _ := newService(t, secret, bind)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	s.UpdateRelays([]string{"wss://relay.example.com:443/relay/v1/connect"})

	// Wait for stun_request.
	var sent sentDiscoPacket
	select {
	case sent = <-bind.sent:
	case <-time.After(2 * time.Second):
		t.Fatal("no stun_request")
	}
	reqFrame, _ := wireframe.Decode(sent.Payload)

	// Build response signed with wrong secret.
	resp := &wireframe.Frame{
		Type:         wireframe.TypeSTUNResponse,
		HasNonce:     true,
		Nonce:        reqFrame.Nonce,
		HasTimestamp: true,
		Timestamp:    uint64(time.Now().UnixMilli()),
		HasObserved:  true,
		ObservedAddr: netip.MustParseAddrPort("203.0.113.1:51820"),
	}
	out := mustHMACSign(t, resp, wrong)
	bind.inbound <- wireframe.Inbound{Payload: out}

	// No EventObservedAddr should arrive.
	select {
	case ev := <-s.Events():
		if _, ok := ev.(EventObservedAddr); ok {
			t.Fatalf("EventObservedAddr should NOT have fired with bad HMAC")
		}
	case <-time.After(500 * time.Millisecond):
		// good
	}
}

// TestProbeForUnknownPeerIgnored ensures that probes citing a SrcDeviceID
// not in UpdatePeers are dropped (= no pong emitted).
func TestProbeForUnknownPeerIgnored(t *testing.T) {
	bind := newFakeBind()
	s, _, selfPub := newService(t, []byte("s"), bind)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	priv, pub := newNodeKey(t)
	probe := &wireframe.Frame{
		Type:         wireframe.TypeProbe,
		SrcDeviceID:  "dev_unknown",
		DstDeviceID:  "dev_self",
		HasNonce:     true,
		Nonce:        [wireframe.NonceSize]byte{1, 2, 3},
		HasTimestamp: true,
		Timestamp:    uint64(time.Now().UnixMilli()),
	}
	payload := mustEncodeSealed(t, probe, priv, pub, selfPub)
	bind.inbound <- wireframe.Inbound{Payload: payload, Src: netip.MustParseAddrPort("203.0.113.55:7777")}

	select {
	case got := <-bind.sent:
		t.Fatalf("unexpected outbound: dst=%s", got.Dst)
	case <-time.After(500 * time.Millisecond):
	}
}
