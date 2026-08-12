package wgnet

import (
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"strings"
	"testing"
)

// recordingEngine is an Engine wired to a recording write seam instead of
// a live wireguard-go device. It records the real UAPI text of every
// write, so a test can assert both how many writes happened and what they
// said (CLAUDE.md §Test discipline — a fake that drops its argument makes
// the failing case unwritable).
func recordingEngine() (*Engine, *[]string) {
	var writes []string
	e := &Engine{
		cfg: Config{SelfName: "self", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
	}
	e.applyPeerUAPI = func(uapi string) error {
		writes = append(writes, uapi)
		return nil
	}
	return e, &writes
}

func testPeer(t *testing.T, overlay, endpoint string) Peer {
	t.Helper()
	ip, err := netip.ParseAddr(overlay)
	if err != nil {
		t.Fatalf("parse %q: %v", overlay, err)
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(ip.As4()[3])
	}
	return Peer{
		DeviceName:          "peer-" + overlay,
		OverlayIP:           ip,
		WireGuardPublicKey:  key,
		Endpoint:            endpoint,
		PersistentKeepalive: 25,
	}
}

// TestUpdatePeers_UnchangedSetIsNotReapplied is the waired-agent#624 /
// waired#1137 fix. replace_peers=true destroys and rebuilds every peer,
// which resets each one's handshake state and last-handshake time — and
// the relay-fallback safety net reads exactly that time to decide a peer
// has gone quiet. The reconciler pushes on every map frame and disco
// event, so on real hardware an unchanged map was rebuilding the peer set
// every 38-67 seconds and re-arming the downgrade it exists to prevent.
func TestUpdatePeers_UnchangedSetIsNotReapplied(t *testing.T) {
	e, writes := recordingEngine()
	peers := []Peer{
		testPeer(t, "100.87.131.5", "udp4:203.0.113.10:51820"),
		testPeer(t, "100.87.131.6", "udp4:203.0.113.11:51820"),
	}

	if err := e.UpdatePeers(peers); err != nil {
		t.Fatalf("first UpdatePeers: %v", err)
	}
	if len(*writes) != 1 {
		t.Fatalf("writes after first push = %d, want 1", len(*writes))
	}
	for i := 0; i < 5; i++ {
		if err := e.UpdatePeers(peers); err != nil {
			t.Fatalf("repeat UpdatePeers: %v", err)
		}
	}
	if len(*writes) != 1 {
		t.Errorf("writes after 5 identical pushes = %d, want 1 — every extra one "+
			"destroys the peers' handshake state", len(*writes))
	}
}

// TestUpdatePeers_EndpointChangeIsApplied is the guard on the fix: the
// relay-to-direct switch reaches the device as nothing but a changed
// endpoint on an otherwise identical peer, so a comparison coarser than
// the full UAPI text would silently pin every peer to the path it
// started on.
func TestUpdatePeers_EndpointChangeIsApplied(t *testing.T) {
	e, writes := recordingEngine()
	viaRelay := []Peer{testPeer(t, "100.87.131.5", "relay:wss://relay.example/relay/v1/connect")}
	direct := []Peer{testPeer(t, "100.87.131.5", "udp4:203.0.113.10:51820")}

	if err := e.UpdatePeers(viaRelay); err != nil {
		t.Fatalf("relay push: %v", err)
	}
	if err := e.UpdatePeers(direct); err != nil {
		t.Fatalf("direct push: %v", err)
	}
	if len(*writes) != 2 {
		t.Fatalf("writes = %d, want 2 — the path switch never reached the device", len(*writes))
	}
	if !strings.Contains((*writes)[1], "203.0.113.10:51820") {
		t.Errorf("second write does not carry the direct endpoint: %q", (*writes)[1])
	}
}

func TestUpdatePeers_MembershipChangesAreApplied(t *testing.T) {
	e, writes := recordingEngine()
	one := []Peer{testPeer(t, "100.87.131.5", "udp4:203.0.113.10:51820")}
	two := append(append([]Peer{}, one...), testPeer(t, "100.87.131.6", "udp4:203.0.113.11:51820"))

	for _, step := range []struct {
		name  string
		peers []Peer
		want  int
	}{
		{"first peer", one, 1},
		{"peer added", two, 2},
		{"same set again", two, 2},
		{"peer removed", one, 3},
		{"empty set", nil, 4},
		{"still empty", nil, 4},
	} {
		if err := e.UpdatePeers(step.peers); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if len(*writes) != step.want {
			t.Errorf("%s: writes = %d, want %d", step.name, len(*writes), step.want)
		}
	}
}

// TestUpdatePeers_FailedWriteIsNotRemembered keeps a rejected peer set
// from being treated as applied: the next push would otherwise be
// suppressed as a no-op and the device would keep the older set with
// nothing reporting it.
func TestUpdatePeers_FailedWriteIsNotRemembered(t *testing.T) {
	var writes int
	fail := true
	e := &Engine{cfg: Config{SelfName: "self", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	e.applyPeerUAPI = func(string) error {
		writes++
		if fail {
			return errors.New("ipc rejected")
		}
		return nil
	}
	peers := []Peer{testPeer(t, "100.87.131.5", "udp4:203.0.113.10:51820")}

	if err := e.UpdatePeers(peers); err == nil {
		t.Fatal("UpdatePeers returned no error when the device rejected the write")
	}
	fail = false
	if err := e.UpdatePeers(peers); err != nil {
		t.Fatalf("retry after a failed write: %v", err)
	}
	if writes != 2 {
		t.Errorf("writes = %d, want 2 — the failed set was remembered as applied", writes)
	}
}

func TestUpdatePeers_UninitializedEngineErrors(t *testing.T) {
	if err := (&Engine{}).UpdatePeers(nil); err == nil {
		t.Error("UpdatePeers on an engine with no write seam returned no error")
	}
}
