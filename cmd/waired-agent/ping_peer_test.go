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
			peerByName: map[string]*signer.NetworkMapPeer{
				"peer-b": {DeviceName: "peer-b", DeviceID: "dev_b", OverlayIP: "100.87.131.5"},
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
