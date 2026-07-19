package tray

import (
	"strings"
	"testing"
	"time"
)

// The model-switch grace state (waired#808): while a switch's supervised
// restart is in flight the daemon is briefly unreachable, and the tray
// should show "Switching model…" over the last online menu rather than
// the red agent-down state.

func TestOfflineModel_NotSwitching_IsDaemonDown(t *testing.T) {
	got := offlineModel(MenuModel{}, false)
	if got.Kind != MenuDaemonDown {
		t.Errorf("Kind=%v, want MenuDaemonDown", got.Kind)
	}
	if got.Icon != IconError {
		t.Errorf("Icon=%v, want IconError", got.Icon)
	}
	if got.StatusMsg == "" {
		t.Error("daemon-down model should carry a StatusMsg hint")
	}
}

func TestOfflineModel_SwitchingButNoConnectedSnapshot_IsDaemonDown(t *testing.T) {
	// A zero MenuModel has Kind == MenuDaemonDown: before any connected
	// snapshot exists we must not fabricate a "Switching…" menu from
	// nothing even when the window is armed.
	got := offlineModel(MenuModel{}, true)
	if got.Kind != MenuDaemonDown {
		t.Errorf("Kind=%v, want MenuDaemonDown (no connected lastOnline)", got.Kind)
	}
}

func TestOfflineModel_Switching_KeepsLastOnlineAsSwitching(t *testing.T) {
	last := MenuModel{
		Kind:           MenuConnected,
		Icon:           IconConnected,
		HeaderTitle:    "● Connected",
		DegradedReason: "Claude Code routing inactive",
		AccountEmail:   "user@example.com",
		CatalogEntries: []CatalogEntryView{{ModelID: "m1", Label: "Model One"}},
	}
	got := offlineModel(last, true)

	if got.Kind != MenuConnected {
		t.Errorf("Kind=%v, want MenuConnected (last online preserved)", got.Kind)
	}
	if got.Icon != IconBusy {
		t.Errorf("Icon=%v, want IconBusy", got.Icon)
	}
	if !strings.Contains(got.HeaderTitle, "Switching") {
		t.Errorf("HeaderTitle=%q, want a Switching label", got.HeaderTitle)
	}
	if got.DegradedReason != "" {
		t.Errorf("DegradedReason=%q, want cleared during the switch", got.DegradedReason)
	}
	if got.StatusMsg == "" {
		t.Error("switching model should carry a StatusMsg")
	}
	// Rows are preserved so the menu does not blank out mid-switch.
	if got.AccountEmail != "user@example.com" {
		t.Errorf("AccountEmail=%q, want preserved", got.AccountEmail)
	}
	if len(got.CatalogEntries) != 1 {
		t.Errorf("CatalogEntries len=%d, want 1 (preserved)", len(got.CatalogEntries))
	}
}

func TestOfflineModel_WindowLapsed_FallsBackToDaemonDown(t *testing.T) {
	// A genuinely failed restart: the window lapsed (switching=false) even
	// though we still hold a connected lastOnline → honest daemon-down.
	last := MenuModel{Kind: MenuConnected, HeaderTitle: "● Connected"}
	got := offlineModel(last, false)
	if got.Kind != MenuDaemonDown {
		t.Errorf("Kind=%v, want MenuDaemonDown once the grace window lapses", got.Kind)
	}
}

func TestArmSwitching_OpensFutureWindow(t *testing.T) {
	tr := &tray{}
	tr.mu.Lock()
	start := tr.switchingUntil
	tr.mu.Unlock()
	if !start.IsZero() {
		t.Fatalf("precondition: switchingUntil=%v, want zero", start)
	}

	tr.armSwitching()

	tr.mu.Lock()
	until := tr.switchingUntil
	tr.mu.Unlock()
	if !until.After(time.Now()) {
		t.Errorf("armSwitching: switchingUntil=%v, want a future time", until)
	}
}

func TestPeersRowVisible(t *testing.T) {
	cases := []struct {
		name string
		m    MenuModel
		want bool
	}{
		{"no device, no peers", MenuModel{}, false},
		{"enrolled but zero peers", MenuModel{DeviceName: "dev", PeerCount: 0}, false},
		{"enrolled with peers", MenuModel{DeviceName: "dev", PeerCount: 2}, true},
		{"enrolled with peer hardware, zero count", MenuModel{OverlayIP: "100.64.0.1", ShowPeerHardware: true}, true},
		{"peers but not enrolled", MenuModel{PeerCount: 3}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := peersRowVisible(c.m); got != c.want {
				t.Errorf("peersRowVisible=%v, want %v", got, c.want)
			}
		})
	}
}
