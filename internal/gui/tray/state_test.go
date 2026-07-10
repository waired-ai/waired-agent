package tray

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
)

func TestUpdate_DaemonDown(t *testing.T) {
	got := Update(Snapshot{Health: HealthOffline})
	if got.Kind != MenuDaemonDown {
		t.Errorf("Kind=%d, want MenuDaemonDown", got.Kind)
	}
	if got.Icon != IconError {
		t.Errorf("Icon=%d, want IconError", got.Icon)
	}
	if got.HeaderTitle == "" || got.StatusMsg == "" {
		t.Errorf("daemon-down should populate HeaderTitle and StatusMsg, got %+v", got)
	}
	if got.ToggleAction != "" {
		t.Errorf("daemon-down should hide toggle, got %q", got.ToggleAction)
	}
}

func TestUpdate_NotSignedIn_NilIdentity(t *testing.T) {
	got := Update(Snapshot{Health: HealthOnline, Identity: nil})
	if got.Kind != MenuNotSignedIn {
		t.Errorf("Kind=%d, want MenuNotSignedIn", got.Kind)
	}
	if got.ToggleAction != "Log in..." {
		t.Errorf("ToggleAction=%q, want %q", got.ToggleAction, "Log in...")
	}
}

func TestUpdate_NotSignedIn_EnrolledFalse(t *testing.T) {
	got := Update(Snapshot{
		Health:   HealthOnline,
		Identity: &management.IdentityView{Enrolled: false},
	})
	if got.Kind != MenuNotSignedIn {
		t.Errorf("Kind=%d, want MenuNotSignedIn", got.Kind)
	}
	if got.ToggleAction != "Log in..." {
		t.Errorf("ToggleAction=%q, want Log in...", got.ToggleAction)
	}
}

func TestUpdate_Connected_Active(t *testing.T) {
	id := &management.IdentityView{
		Enrolled:     true,
		AccountEmail: "alice@example.com",
		NetworkName:  "alice-net",
		DeviceID:     "dev-1",
		DeviceName:   "alice-laptop",
		OverlayIP:    "100.96.0.10",
		ControlURL:   "https://control.example.com",
	}
	st := &management.Status{Phase: "active", PeerCount: 3}
	got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st})
	if got.Kind != MenuConnected {
		t.Errorf("Kind=%d, want MenuConnected", got.Kind)
	}
	if got.Icon != IconConnected {
		t.Errorf("Icon=%d, want IconConnected", got.Icon)
	}
	if got.AccountEmail != "alice@example.com" {
		t.Errorf("AccountEmail=%q", got.AccountEmail)
	}
	if got.OverlayIP != "100.96.0.10" {
		t.Errorf("OverlayIP=%q", got.OverlayIP)
	}
	if got.PeerCount != 3 {
		t.Errorf("PeerCount=%d", got.PeerCount)
	}
	if got.AdminURL != "https://control.example.com/admin" {
		t.Errorf("AdminURL=%q", got.AdminURL)
	}
	if got.ToggleAction != "Disconnect" {
		t.Errorf("ToggleAction=%q", got.ToggleAction)
	}
}

func TestUpdate_Connected_PrePauseMergeNoPhase(t *testing.T) {
	// Older daemons (before agent-pause merges) don't populate Status.Phase.
	// Empty phase should still render as Connected so the tray works against
	// older builds.
	id := &management.IdentityView{Enrolled: true, AccountEmail: "a@b"}
	st := &management.Status{} // Phase is ""
	got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st})
	if got.Kind != MenuConnected {
		t.Errorf("empty Phase should render Connected, got Kind=%d", got.Kind)
	}
	if got.ToggleAction != "Disconnect" {
		t.Errorf("ToggleAction=%q", got.ToggleAction)
	}
}

func TestUpdate_Disconnected_Paused(t *testing.T) {
	id := &management.IdentityView{Enrolled: true, AccountEmail: "a@b", ControlURL: "https://c.example.com/"}
	st := &management.Status{Phase: "paused"}
	got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st})
	if got.Kind != MenuDisconnected {
		t.Errorf("Kind=%d, want MenuDisconnected", got.Kind)
	}
	if got.Icon != IconDisconnected {
		t.Errorf("Icon=%d, want IconDisconnected", got.Icon)
	}
	if got.ToggleAction != "Connect" {
		t.Errorf("ToggleAction=%q, want Connect", got.ToggleAction)
	}
	if got.AdminURL != "https://c.example.com/admin" {
		t.Errorf("AdminURL trim-trailing-slash failed: %q", got.AdminURL)
	}
}

func TestUpdate_Connecting(t *testing.T) {
	id := &management.IdentityView{Enrolled: true}
	for _, phase := range []string{"starting", "stopping"} {
		st := &management.Status{Phase: phase}
		got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st})
		if got.Kind != MenuConnecting {
			t.Errorf("phase=%q: Kind=%d, want MenuConnecting", phase, got.Kind)
		}
		if got.ToggleAction != "" {
			t.Errorf("phase=%q: should hide toggle while transitioning, got %q", phase, got.ToggleAction)
		}
	}
}

func TestUpdate_Error(t *testing.T) {
	id := &management.IdentityView{Enrolled: true, AccountEmail: "a@b"}
	st := &management.Status{Phase: "error"}
	got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st})
	if got.Kind != MenuError {
		t.Errorf("Kind=%d, want MenuError", got.Kind)
	}
	if got.Icon != IconError {
		t.Errorf("Icon=%d, want IconError", got.Icon)
	}
	if got.StatusMsg == "" {
		t.Errorf("error state should provide a StatusMsg")
	}
}

// Transition coverage — drive the state machine through a realistic
// session: cold start (offline) → up but not enrolled → enrolled+active →
// paused → resumed → daemon dies. The transitions must produce the
// expected MenuKind sequence.
func TestUpdate_TransitionSequence(t *testing.T) {
	cases := []struct {
		name string
		in   Snapshot
		want MenuKind
	}{
		{"cold start", Snapshot{Health: HealthOffline}, MenuDaemonDown},
		{"daemon up, identity not yet fetched", Snapshot{Health: HealthOnline}, MenuNotSignedIn},
		{
			"enrolled active",
			Snapshot{
				Health:   HealthOnline,
				Identity: &management.IdentityView{Enrolled: true, AccountEmail: "x@y"},
				Status:   &management.Status{Phase: "active"},
			},
			MenuConnected,
		},
		{
			"user paused",
			Snapshot{
				Health:   HealthOnline,
				Identity: &management.IdentityView{Enrolled: true, AccountEmail: "x@y"},
				Status:   &management.Status{Phase: "paused"},
			},
			MenuDisconnected,
		},
		{
			"user resumed",
			Snapshot{
				Health:   HealthOnline,
				Identity: &management.IdentityView{Enrolled: true, AccountEmail: "x@y"},
				Status:   &management.Status{Phase: "active"},
			},
			MenuConnected,
		},
		{"daemon crash", Snapshot{Health: HealthOffline}, MenuDaemonDown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Update(c.in); got.Kind != c.want {
				t.Errorf("Update(%s).Kind=%d, want %d", c.name, got.Kind, c.want)
			}
		})
	}
}

func TestUpdate_OpenCode_Configured(t *testing.T) {
	id := &management.IdentityView{Enrolled: true}
	st := &management.Status{Phase: "active"}
	oc := &management.OpenCodeIntegrationStatus{
		Config: management.OpenCodeIntegrationStatusConfig{
			Path:       "/home/u/.config/opencode/opencode.json",
			Configured: true,
		},
	}
	got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, OpenCode: oc})
	if !got.ShowOpenCode {
		t.Fatalf("ShowOpenCode=false, want true")
	}
	if got.OpenCodeHeader != "OpenCode integration: ● configured" {
		t.Errorf("Header=%q", got.OpenCodeHeader)
	}
	if got.OpenCodeConfigLabel != "Config: ✓ /home/u/.config/opencode/opencode.json" {
		t.Errorf("Config=%q", got.OpenCodeConfigLabel)
	}
	if got.OpenCodeReconfigureLabel != "Reconfigure…" {
		t.Errorf("Reconfigure=%q", got.OpenCodeReconfigureLabel)
	}
	if got.Icon != IconConnected {
		t.Errorf("Icon=%d, want IconConnected (no degrade for fresh config)", got.Icon)
	}
}

func TestUpdate_OpenCode_StaleWhileConnectedDegrades(t *testing.T) {
	id := &management.IdentityView{Enrolled: true}
	st := &management.Status{Phase: "active"}
	oc := &management.OpenCodeIntegrationStatus{
		Config: management.OpenCodeIntegrationStatusConfig{
			Path:         "/home/u/.config/opencode/opencode.json",
			Configured:   true,
			Stale:        true,
			CurrentValue: "http://127.0.0.1:9999/v1",
		},
	}
	got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, OpenCode: oc})
	if got.OpenCodeHeader != "OpenCode integration: ⚠ stale (http://127.0.0.1:9999/v1)" {
		t.Errorf("Header=%q", got.OpenCodeHeader)
	}
	if got.OpenCodeConfigLabel != "Config: ⚠ stale (http://127.0.0.1:9999/v1)" {
		t.Errorf("Config=%q", got.OpenCodeConfigLabel)
	}
	if got.Icon != IconDegraded {
		t.Errorf("Icon=%d, want IconDegraded for stale + Connected", got.Icon)
	}
}

func TestUpdate_OpenCode_NotConfigured(t *testing.T) {
	id := &management.IdentityView{Enrolled: true}
	st := &management.Status{Phase: "active"}
	oc := &management.OpenCodeIntegrationStatus{
		Config: management.OpenCodeIntegrationStatusConfig{
			Path:       "/home/u/.config/opencode/opencode.json",
			Configured: false,
		},
	}
	got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, OpenCode: oc})
	if got.OpenCodeHeader != "OpenCode integration: ○ not configured" {
		t.Errorf("Header=%q", got.OpenCodeHeader)
	}
	if got.Icon != IconConnected {
		t.Errorf("Icon=%d, want IconConnected (missing config does not degrade)", got.Icon)
	}
}

func TestUpdate_OpenCode_UnreadableNoteSurfacedAndDegrades(t *testing.T) {
	id := &management.IdentityView{Enrolled: true}
	st := &management.Status{Phase: "active"}
	oc := &management.OpenCodeIntegrationStatus{
		Config: management.OpenCodeIntegrationStatusConfig{
			Path:       "/home/u/.config/opencode/opencode.json",
			Configured: false,
			Note:       "parse: invalid character",
		},
	}
	got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, OpenCode: oc})
	if got.OpenCodeHeader != "OpenCode integration: ⚠ unreadable (parse: invalid character)" {
		t.Errorf("Header=%q", got.OpenCodeHeader)
	}
	if got.Icon != IconDegraded {
		t.Errorf("Icon=%d, want IconDegraded for unreadable + Connected", got.Icon)
	}
}

// TestUpdate_OpenCode_NilSnapHidesGroup verifies that on a daemon
// predating the opencode integration endpoint (Snapshot.OpenCode=nil),
// the menu model does not surface the group at all — preserving the
// pre-extension menu shape.
func TestUpdate_OpenCode_NilSnapHidesGroup(t *testing.T) {
	id := &management.IdentityView{Enrolled: true}
	st := &management.Status{Phase: "active"}
	got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, OpenCode: nil})
	if got.ShowOpenCode {
		t.Errorf("ShowOpenCode=true with nil snapshot, want false")
	}
}

func TestUpdate_OpenClaw_Configured(t *testing.T) {
	id := &management.IdentityView{Enrolled: true}
	st := &management.Status{Phase: "active"}
	ow := &management.OpenClawIntegrationStatus{
		Config: management.OpenClawIntegrationStatusConfig{
			Path:       "/home/u/.openclaw/plugins/waired/index.mjs",
			Configured: true,
		},
	}
	got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, OpenClaw: ow})
	if !got.ShowOpenClaw {
		t.Fatalf("ShowOpenClaw=false, want true")
	}
	if got.OpenClawHeader != "OpenClaw integration: ● configured" {
		t.Errorf("Header=%q", got.OpenClawHeader)
	}
	if got.OpenClawConfigLabel != "Config: ✓ /home/u/.openclaw/plugins/waired/index.mjs" {
		t.Errorf("Config=%q", got.OpenClawConfigLabel)
	}
	if got.OpenClawReconfigureLabel != "Reconfigure…" {
		t.Errorf("Reconfigure=%q", got.OpenClawReconfigureLabel)
	}
	if got.Icon != IconConnected {
		t.Errorf("Icon=%d, want IconConnected (no degrade for fresh config)", got.Icon)
	}
}

func TestUpdate_OpenClaw_StaleWhileConnectedDegrades(t *testing.T) {
	id := &management.IdentityView{Enrolled: true}
	st := &management.Status{Phase: "active"}
	ow := &management.OpenClawIntegrationStatus{
		Config: management.OpenClawIntegrationStatusConfig{
			Path:         "/home/u/.openclaw/plugins/waired/index.mjs",
			Configured:   true,
			Stale:        true,
			CurrentValue: "http://127.0.0.1:9999/v1",
		},
	}
	got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, OpenClaw: ow})
	if got.OpenClawHeader != "OpenClaw integration: ⚠ stale (http://127.0.0.1:9999/v1)" {
		t.Errorf("Header=%q", got.OpenClawHeader)
	}
	if got.Icon != IconDegraded {
		t.Errorf("Icon=%d, want IconDegraded for stale + Connected", got.Icon)
	}
}

func TestUpdate_OpenClaw_NilSnapHidesGroup(t *testing.T) {
	id := &management.IdentityView{Enrolled: true}
	st := &management.Status{Phase: "active"}
	got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: st, OpenClaw: nil})
	if got.ShowOpenClaw {
		t.Errorf("ShowOpenClaw=true with nil snapshot, want false")
	}
}
