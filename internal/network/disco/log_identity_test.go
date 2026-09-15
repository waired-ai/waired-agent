package disco

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"strings"
	"testing"
	"time"

	wireframe "github.com/waired-ai/waired-agent/proto/disco"
)

// A peer present under a Public Share grant: its LogName is the grant
// pseudonym, and its device id belongs to another account. PRODUCT
// CONTRACT: public share spec §8.5 — logs name a public peer by its
// pseudonym and never keep its real device id (waired-agent#1368).
const (
	grantPeerDeviceID = "dev_guest_real_id"
	grantPeerLogName  = "guest-0001"
)

// failingRelayBind refuses every relay send, so a probe cycle reaches the
// send-failure log line.
type failingRelayBind struct{ *fakeBind }

func (failingRelayBind) SendDiscoViaRelay([]byte, string, string, string) error {
	return errors.New("relay session down")
}

func newLoggedService(t *testing.T, bind Bind) (*Service, *lockedBuffer) {
	t.Helper()
	logs := &lockedBuffer{}
	selfPriv, selfPub := newNodeKey(t)
	s, err := New(Config{
		SelfDeviceID:       "dev_self",
		SelfNodeKeyPriv:    selfPriv,
		SelfNodeKeyPub:     selfPub,
		Bind:               bind,
		Logger:             slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		STUNObserveActive:  time.Hour,
		STUNTimeout:        200 * time.Millisecond,
		ProbeReprobeActive: time.Hour,
		ProbeWindow:        5 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, logs
}

func TestProbeSendFailureNamesAGrantPeerByLogName(t *testing.T) {
	s, logs := newLoggedService(t, failingRelayBind{newFakeBind()})
	_, peerPub := newNodeKey(t)
	s.UpdatePeers(map[string]PeerSnapshot{
		encodeNodePubB64(peerPub): {
			DeviceID: grantPeerDeviceID,
			LogName:  grantPeerLogName,
			NodePub:  peerPub,
			RelayURL: "wss://r1.example/relay/v1/connect",
		},
	})

	s.probeAllPeers(context.Background())

	out := logs.String()
	if !strings.Contains(out, "disco probe send (relay)") {
		t.Fatalf("no relay send-failure line; got:\n%s", out)
	}
	if !strings.Contains(out, "device_id="+grantPeerLogName) {
		t.Errorf("send-failure line does not name the peer by its log name:\n%s", out)
	}
	if strings.Contains(out, grantPeerDeviceID) {
		t.Errorf("log carries the grant peer's device id:\n%s", out)
	}
}

func TestCallMeMaybeRejectionsNameAKnownPeerByLogName(t *testing.T) {
	s, logs := newLoggedService(t, newFakeBind())
	_, peerPub := newNodeKey(t)
	s.UpdatePeers(map[string]PeerSnapshot{
		encodeNodePubB64(peerPub): {DeviceID: grantPeerDeviceID, LogName: grantPeerLogName, NodePub: peerPub},
	})

	pkt := wireframe.Inbound{Path: wireframe.PathRelay, RelaySrcDeviceID: grantPeerDeviceID}
	s.handleCallMeMaybe(&wireframe.Frame{SrcDeviceID: grantPeerDeviceID, HasNonce: true}, pkt, peerPub)
	over := make([]netip.AddrPort, wireframe.MaxCandidateListLen+1)
	s.handleCallMeMaybe(&wireframe.Frame{SrcDeviceID: grantPeerDeviceID, HasNonce: true, CandidateList: over}, pkt, peerPub)

	out := logs.String()
	for _, msg := range []string{"call_me_maybe with empty candidate list", "call_me_maybe candidate list over cap"} {
		if !strings.Contains(out, msg) {
			t.Fatalf("no %q line; got:\n%s", msg, out)
		}
	}
	if strings.Contains(out, grantPeerDeviceID) {
		t.Errorf("log carries the grant peer's device id:\n%s", out)
	}
	if got := strings.Count(out, "device_id="+grantPeerLogName); got != 2 {
		t.Errorf("lines naming the peer by log name = %d, want 2:\n%s", got, out)
	}
}

// An id outside the current peer set is printed as it is: there is no
// name to use, and naming the sender is the point of the line. Record of
// today's behaviour, matching inboundSource.
func TestLogIDForDevice(t *testing.T) {
	s, _ := newLoggedService(t, newFakeBind())
	_, peerPub := newNodeKey(t)
	s.UpdatePeers(map[string]PeerSnapshot{
		encodeNodePubB64(peerPub): {DeviceID: grantPeerDeviceID, LogName: grantPeerLogName, NodePub: peerPub},
	})
	if got := s.logIDForDevice(grantPeerDeviceID); got != grantPeerLogName {
		t.Errorf("known peer = %q, want %q", got, grantPeerLogName)
	}
	if got := s.logIDForDevice("dev_unknown"); got != "dev_unknown" {
		t.Errorf("unknown device = %q, want the id", got)
	}
}
