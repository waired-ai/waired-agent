package main

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/identity"
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
