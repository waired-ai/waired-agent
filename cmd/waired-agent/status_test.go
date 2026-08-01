package main

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/identity"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// TestAgentProviderStatusListenPort fails when Status.ListenPort is
// returned as the inference service port (9474) instead of the actual
// WireGuard UDP listen port the engine bound. The tray surfaces this
// number in the device-info section, so a stale 9474 misleads users
// debugging firewalls.
func TestAgentProviderStatusListenPort(t *testing.T) {
	prov := &agentProvider{
		id: &identity.Identity{
			DeviceID:  "test-device",
			OverlayIP: "10.0.0.1",
			Endpoint:  "udp4:198.51.100.1:41010",
		},
		wgListenPort: 41010,
	}
	st := prov.Status()
	if st.ListenPort != 41010 {
		t.Errorf("Status().ListenPort = %d, want 41010 (real WG UDP port)", st.ListenPort)
	}
}

// TestAgentProviderIdentityDeviceName fails when DeviceName falls back
// to DeviceID even though Identity.DeviceName is populated. The tray's
// "This device" section relies on DeviceName for human-readable output.
func TestAgentProviderIdentityDeviceName(t *testing.T) {
	prov := &agentProvider{
		id: &identity.Identity{
			DeviceID:   "did_abc123",
			DeviceName: "alice-laptop",
		},
	}
	v := prov.Identity()
	if v.DeviceName != "alice-laptop" {
		t.Errorf("Identity().DeviceName = %q, want %q", v.DeviceName, "alice-laptop")
	}
	if v.DeviceID != "did_abc123" {
		t.Errorf("Identity().DeviceID = %q, want %q", v.DeviceID, "did_abc123")
	}
}

// TestAgentProviderIdentityDeviceNameFallback verifies the helper's
// historical behaviour is preserved: when the saved Identity has no
// DeviceName (older identity.json files written before the field was
// added), DeviceName defaults to DeviceID so the UI never renders an
// empty device label.
func TestAgentProviderIdentityDeviceNameFallback(t *testing.T) {
	prov := &agentProvider{
		id: &identity.Identity{
			DeviceID:   "did_abc123",
			DeviceName: "",
		},
	}
	v := prov.Identity()
	if v.DeviceName != "did_abc123" {
		t.Errorf("Identity().DeviceName fallback = %q, want %q", v.DeviceName, "did_abc123")
	}
}

// TestAgentProviderStatusPeersSorted pins a PRODUCT CONTRACT, not
// today's behaviour: Status().Peers is ordered by DeviceName (DeviceID
// for peers the network map has no name for), identically on every call.
//
// reconciler.Snapshot() hands back a map, and Go randomises map
// iteration, so the peer slice used to arrive in a different order on
// every 5 s tray poll. The tray fills its fixed "Peers" menu slots
// positionally, which turned that churn into node names jumping between
// rows (#326). Six peers, inserted out of order, make an accidental pass
// vanishingly unlikely.
func TestAgentProviderStatusPeersSorted(t *testing.T) {
	peers := []struct{ nodePub, deviceID, name string }{
		{"pub_04", "dev_04", "windows-desktop"},
		{"pub_02", "dev_02", "beta-node"},
		{"pub_06", "dev_06", ""}, // unnamed → sorts by DeviceID
		{"pub_01", "dev_01", "alpha-node"},
		{"pub_05", "dev_05", "linux-gpu"},
		{"pub_03", "dev_03", "mac-mini"},
	}
	rec := &reconciler{
		state: map[string]*peerPathState{},
		nm:    &signer.NetworkMap{},
	}
	prov := &agentProvider{
		id:         &identity.Identity{DeviceID: "self", DeviceName: "self-node"},
		reconciler: rec,
		peerByName: map[string]*signer.NetworkMapPeer{},
	}
	for _, p := range peers {
		rec.state[p.nodePub] = &peerPathState{currentPath: pathDirect}
		nmPeer := signer.NetworkMapPeer{NodePublicKey: p.nodePub, DeviceID: p.deviceID, DeviceName: p.name}
		rec.nm.Peers = append(rec.nm.Peers, nmPeer)
		prov.peerByName[p.deviceID] = &rec.nm.Peers[len(rec.nm.Peers)-1]
	}

	want := []string{"alpha-node", "beta-node", "dev_06", "linux-gpu", "mac-mini", "windows-desktop"}
	for i := 0; i < 20; i++ {
		st := prov.Status()
		got := make([]string, 0, len(st.Peers))
		for _, p := range st.Peers {
			name := p.DeviceName
			if name == "" {
				name = p.DeviceID
			}
			got = append(got, name)
		}
		if len(got) != len(want) {
			t.Fatalf("call %d: got %d peers, want %d", i, len(got), len(want))
		}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("call %d: peer order = %v, want %v", i, got, want)
			}
		}
	}
}
