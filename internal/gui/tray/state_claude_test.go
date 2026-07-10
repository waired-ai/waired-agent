package tray

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
)

func baseSnapshot() Snapshot {
	return Snapshot{
		Health: HealthOnline,
		Identity: &management.IdentityView{
			Enrolled:     true,
			AccountEmail: "u@example.com",
			DeviceID:     "dev-1",
			OverlayIP:    "100.96.0.10",
			ControlURL:   "https://cp.example.com",
		},
		Status: &management.Status{
			DeviceName: "alice",
			OverlayIP:  "100.96.0.10",
			Phase:      "active",
		},
	}
}

func TestUpdate_Claude_HiddenWithoutEndpoint(t *testing.T) {
	got := Update(baseSnapshot())
	if got.ShowClaude {
		t.Errorf("ShowClaude must be false when Snapshot.Claude is nil")
	}
}

func TestUpdate_Claude_ActiveKeepsConnectedIcon(t *testing.T) {
	snap := baseSnapshot()
	snap.Claude = &management.ClaudeIntegrationStatus{
		Wrapper:    management.ClaudeWrapperView{Reachable: true},
		BinaryPath: "/p/waired",
	}
	got := Update(snap)
	if !got.ShowClaude {
		t.Fatal("ShowClaude should be true")
	}
	if got.Icon != IconConnected {
		t.Errorf("Icon = %d, want IconConnected", got.Icon)
	}
	if !strings.Contains(got.ClaudeHeader, "active") {
		t.Errorf("Header = %q", got.ClaudeHeader)
	}
}

func TestUpdate_Claude_DegradedSwapsIcon(t *testing.T) {
	snap := baseSnapshot()
	snap.Claude = &management.ClaudeIntegrationStatus{
		Wrapper:    management.ClaudeWrapperView{Reachable: false, Reason: "agent-stopped"},
		BinaryPath: "/p/waired",
	}
	got := Update(snap)
	if got.Icon != IconDegraded {
		t.Errorf("Icon = %d, want IconDegraded", got.Icon)
	}
	if !strings.Contains(got.ClaudeHeader, "agent-stopped") {
		t.Errorf("Header = %q", got.ClaudeHeader)
	}
}

func TestUpdate_Claude_DegradedDoesNotOverrideErrorIcon(t *testing.T) {
	snap := baseSnapshot()
	snap.Status.Phase = "error"
	snap.Claude = &management.ClaudeIntegrationStatus{
		Wrapper: management.ClaudeWrapperView{Reachable: false, Reason: "agent-stopped"},
	}
	got := Update(snap)
	if got.Icon != IconError {
		t.Errorf("Icon = %d, want IconError (network error must outrank claude warning)", got.Icon)
	}
}

func TestUpdate_Claude_DegradedDoesNotOverrideDisconnectedIcon(t *testing.T) {
	snap := baseSnapshot()
	snap.Status.Phase = "paused"
	snap.Claude = &management.ClaudeIntegrationStatus{
		Wrapper: management.ClaudeWrapperView{Reachable: false, Reason: "agent-stopped"},
	}
	got := Update(snap)
	if got.Icon != IconDisconnected {
		t.Errorf("Icon = %d, want IconDisconnected (paused must outrank claude warning)", got.Icon)
	}
}

// TestUpdate_Claude_ManagedSettingsLabels covers the managed-settings status
// row the menu renders (#488): whether Claude Code is wired to waired's local
// gateway via ANTHROPIC_BASE_URL.
func TestUpdate_Claude_ManagedSettingsLabels(t *testing.T) {
	const expected = "http://127.0.0.1:9472"
	cases := []struct {
		name    string
		ms      management.ClaudeManagedSettingsView
		wantSub string
	}{
		{"unsupported", management.ClaudeManagedSettingsView{Supported: false}, "unsupported on this OS"},
		{"not-configured", management.ClaudeManagedSettingsView{Supported: true, ExpectedBaseURL: expected}, "✗ not configured"},
		{"configured", management.ClaudeManagedSettingsView{
			Supported: true, Present: true, BaseURL: expected, ExpectedBaseURL: expected, Configured: true,
		}, "✓ routed to local gateway"},
		{"set-elsewhere", management.ClaudeManagedSettingsView{
			Supported: true, Present: true, BaseURL: "http://127.0.0.1:9999", ExpectedBaseURL: expected,
		}, "set elsewhere"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := baseSnapshot()
			snap.Claude = &management.ClaudeIntegrationStatus{
				Wrapper:         management.ClaudeWrapperView{Reachable: true},
				ManagedSettings: tc.ms,
			}
			got := Update(snap)
			if !strings.Contains(got.ClaudeProxyLabel, tc.wantSub) {
				t.Errorf("ClaudeProxyLabel = %q, want substr %q", got.ClaudeProxyLabel, tc.wantSub)
			}
		})
	}
}
