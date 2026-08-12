package main

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/inference"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// fakeOverlayPinger records the address it was asked to ping and returns
// a canned outcome. It takes the real arguments so a test can assert the
// daemon dialled the peer's overlay IP rather than something else
// (CLAUDE.md §Test discipline).
type fakeOverlayPinger struct {
	gotAddr netip.Addr
	gotPort uint16
	resp    inference.PingResponse
	rtt     time.Duration
	err     error
}

func (f *fakeOverlayPinger) Ping(_ context.Context, ip netip.Addr, port uint16) (inference.PingResponse, time.Duration, error) {
	f.gotAddr, f.gotPort = ip, port
	return f.resp, f.rtt, f.err
}

func pingerWithPeer(t *testing.T, p *fakeOverlayPinger) *agentPinger {
	t.Helper()
	return &agentPinger{
		client: p,
		provider: &agentProvider{
			peerByID: map[string]*signer.NetworkMapPeer{
				"dev_b": {DeviceName: "peer-b", DeviceID: "dev_b", OverlayIP: "100.87.131.5"},
			},
		},
	}
}

// TestAgentPinger_UnansweredPingNamesThePeer covers waired-agent#659: an
// operator pinging an offline peer got a message whose only identifier
// was their own loopback address, and read it as "my daemon is broken".
//
// Record of today's behaviour for the exact wording; the requirement that
// the failure name the peer is the issue's own "Expected" section.
func TestAgentPinger_UnansweredPingNamesThePeer(t *testing.T) {
	underlying := errors.New(`Get "http://100.87.131.5:9474/waired/v1/ping": context deadline exceeded`)
	p := &fakeOverlayPinger{err: underlying}
	_, err := pingerWithPeer(t, p).PingPeer(context.Background(), "peer-b")
	if err == nil {
		t.Fatal("PingPeer returned no error for an unanswered ping")
	}
	if !strings.Contains(err.Error(), `"peer-b"`) {
		t.Errorf("error does not name the peer: %v", err)
	}
	if !errors.Is(err, underlying) {
		t.Errorf("error does not wrap the transport cause: %v", err)
	}
	// The two sibling branches already name the peer; this one must read
	// the same way rather than inventing a second phrasing.
	if !strings.HasPrefix(err.Error(), `peer "peer-b"`) {
		t.Errorf("error = %q, want it to lead with the peer like its sibling branches", err)
	}
}

func TestAgentPinger_UnknownPeerNamesThePeer(t *testing.T) {
	p := &fakeOverlayPinger{}
	_, err := pingerWithPeer(t, p).PingPeer(context.Background(), "peer-z")
	if err == nil || !strings.Contains(err.Error(), `"peer-z"`) {
		t.Fatalf("err = %v, want it to name the unknown peer", err)
	}
	if p.gotAddr.IsValid() {
		t.Errorf("dialled %v for a peer that is not in the map", p.gotAddr)
	}
}

func TestAgentPinger_SuccessDialsThePeersOverlayAddress(t *testing.T) {
	p := &fakeOverlayPinger{
		resp: inference.PingResponse{OK: true, Device: "peer-b"},
		rtt:  62900 * time.Microsecond,
	}
	got, err := pingerWithPeer(t, p).PingPeer(context.Background(), "peer-b")
	if err != nil {
		t.Fatalf("PingPeer: %v", err)
	}
	if p.gotAddr.String() != "100.87.131.5" || p.gotPort != inferenceServicePort {
		t.Errorf("dialled %v:%d, want 100.87.131.5:%d", p.gotAddr, p.gotPort, inferenceServicePort)
	}
	if !got.OK || got.Peer != "peer-b" {
		t.Errorf("result = %+v", got)
	}
	if got.LatencyMS < 62.8 || got.LatencyMS > 63.0 {
		t.Errorf("latency_ms = %v, want ~62.9", got.LatencyMS)
	}
}

// pingerWithPeers builds a pinger over an explicit map keyed by DeviceID,
// the way replacePeers writes it.
func pingerWithPeers(p *fakeOverlayPinger, peers ...signer.NetworkMapPeer) *agentPinger {
	byID := map[string]*signer.NetworkMapPeer{}
	for i := range peers {
		byID[peers[i].DeviceID] = &peers[i]
	}
	return &agentPinger{client: p, provider: &agentProvider{peerByID: byID}}
}

// PRODUCT CONTRACT (waired-agent#723). `waired ping <name>` follows the
// rule `waired worker set --pin` already states: an ambiguous name is
// refused rather than resolved to whichever record the map happened to
// yield last.
//
// The old index was keyed by DeviceName as well as DeviceID, written in
// network-map order with no collision check, so the second of two
// same-named records simply overwrote the first. Pinging a leftover
// record from a previous enrollment reaches an overlay IP nothing is
// listening on, and the timeout reads as "that machine is unreachable"
// rather than "I addressed the wrong record" — which makes the evidence
// unreliable exactly while someone is trying to establish why a host
// cannot be reached.
func TestAgentPinger_AmbiguousNameIsRefused(t *testing.T) {
	p := &fakeOverlayPinger{}
	pinger := pingerWithPeers(p,
		signer.NetworkMapPeer{DeviceName: "workshop-mac", DeviceID: "dev_old", OverlayIP: "100.87.131.5"},
		signer.NetworkMapPeer{DeviceName: "workshop-mac", DeviceID: "dev_new", OverlayIP: "100.87.131.9"},
	)

	_, err := pinger.PingPeer(context.Background(), "workshop-mac")
	if err == nil {
		t.Fatal("PingPeer resolved an ambiguous name instead of refusing it")
	}
	if p.gotAddr.IsValid() {
		t.Errorf("dialled %v — an ambiguous name must reach no peer at all", p.gotAddr)
	}
	for _, want := range []string{"workshop-mac", "ambiguous", "dev_old", "dev_new"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q, so the operator cannot pick one: %v", want, err)
		}
	}
}

// An exact DeviceID keeps winning outright, so the escape hatch the
// ambiguity error points at actually works.
func TestAgentPinger_DeviceIDResolvesPastAnAmbiguousName(t *testing.T) {
	p := &fakeOverlayPinger{resp: inference.PingResponse{OK: true, Device: "workshop-mac"}}
	pinger := pingerWithPeers(p,
		signer.NetworkMapPeer{DeviceName: "workshop-mac", DeviceID: "dev_old", OverlayIP: "100.87.131.5"},
		signer.NetworkMapPeer{DeviceName: "workshop-mac", DeviceID: "dev_new", OverlayIP: "100.87.131.9"},
	)

	if _, err := pinger.PingPeer(context.Background(), "dev_new"); err != nil {
		t.Fatalf("PingPeer by DeviceID: %v", err)
	}
	if p.gotAddr.String() != "100.87.131.9" {
		t.Errorf("dialled %v, want the named record's overlay IP 100.87.131.9", p.gotAddr)
	}
}

// A duplicate name must not make an unrelated peer unreachable: the old
// index let a DeviceName collide with whatever else shared the map.
func TestAgentPinger_UniqueNameStillResolvesAlongsideDuplicates(t *testing.T) {
	p := &fakeOverlayPinger{resp: inference.PingResponse{OK: true, Device: "linux-gpu"}}
	pinger := pingerWithPeers(p,
		signer.NetworkMapPeer{DeviceName: "workshop-mac", DeviceID: "dev_old", OverlayIP: "100.87.131.5"},
		signer.NetworkMapPeer{DeviceName: "workshop-mac", DeviceID: "dev_new", OverlayIP: "100.87.131.9"},
		signer.NetworkMapPeer{DeviceName: "linux-gpu", DeviceID: "dev_gpu", OverlayIP: "100.87.131.7"},
	)

	if _, err := pinger.PingPeer(context.Background(), "linux-gpu"); err != nil {
		t.Fatalf("PingPeer: %v", err)
	}
	if p.gotAddr.String() != "100.87.131.7" {
		t.Errorf("dialled %v, want 100.87.131.7", p.gotAddr)
	}
}

// A Public Share peer is a stranger's machine, and only the grant
// pseudonym for its owner account may reach a CLI surface (public share
// spec §8.5). The ambiguity message is a CLI surface.
//
// This is why the message does not simply reuse resolvePeerToDeviceID's
// wording: that one prints the real DeviceIDs off an unscrubbed
// snapshot, which is the same leak in a different command.
func TestAgentPinger_AmbiguousNameNeverPrintsAGrantedPeersDeviceID(t *testing.T) {
	p := &fakeOverlayPinger{}
	pinger := pingerWithPeers(p,
		signer.NetworkMapPeer{DeviceName: "shared-box", DeviceID: "dev_mine", OverlayIP: "100.87.131.5"},
		signer.NetworkMapPeer{
			DeviceName: "shared-box", DeviceID: "dev_secret_real_id", OverlayIP: "100.87.131.9",
			Grant: &signer.PeerGrant{Kind: "public", Role: "provider", Pseudonym: "guest-a7f3"},
		},
	)

	_, err := pinger.PingPeer(context.Background(), "shared-box")
	if err == nil {
		t.Fatal("PingPeer resolved an ambiguous name instead of refusing it")
	}
	if strings.Contains(err.Error(), "dev_secret_real_id") {
		t.Errorf("the granted peer's real DeviceID crossed to the CLI: %v", err)
	}
	if !strings.Contains(err.Error(), "guest-a7f3") {
		t.Errorf("the granted peer is not identified at all, so the operator cannot act: %v", err)
	}
	// The device the operator does own is still named outright.
	if !strings.Contains(err.Error(), "dev_mine") {
		t.Errorf("own-network candidate is missing: %v", err)
	}
}
