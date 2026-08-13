package tray

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
)

// The model-switch grace state (waired#808): while a switch's supervised
// restart is in flight the daemon is briefly unreachable, and the tray
// should show "Switching model…" over the last online menu rather than
// the red agent-down state.

func TestOfflineModel_NotSwitching_IsDaemonDown(t *testing.T) {
	got := offlineModel(MenuModel{}, false, installedFacts())
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
	got := offlineModel(MenuModel{}, true, installedFacts())
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
	got := offlineModel(last, true, installedFacts())

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
	got := offlineModel(last, false, installedFacts())
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

// The switch-accepted wording (waired#808): the three responses the
// daemon can give mean three different things happen next, and the tray
// used to collapse two of them into "Model switched."

func TestModelSwitchAcceptedText(t *testing.T) {
	cases := []struct {
		name string
		resp *management.PreferredModelResponse
		want string
	}{
		{
			"restart fallback says so",
			&management.PreferredModelResponse{WillRestart: true},
			"Switching model — the agent will restart briefly.",
		},
		{
			"in-process swap needing a pull names the download",
			&management.PreferredModelResponse{Downloading: true},
			"Downloading Qwen3 8B Instruct. Your current model keeps answering until it is ready.",
		},
		{
			"in-process swap of on-disk weights",
			&management.PreferredModelResponse{},
			"Switching to Qwen3 8B Instruct — it will be answering in a few seconds.",
		},
		{
			// WillRestart wins: the restart is the thing that makes the
			// menu go away, and the fallback path does not pull anyway.
			"restart and downloading together reports the restart",
			&management.PreferredModelResponse{WillRestart: true, Downloading: true},
			"Switching model — the agent will restart briefly.",
		},
		{
			"a daemon that answered nothing still gets a sentence",
			nil,
			"Switching to Qwen3 8B Instruct — it will be answering in a few seconds.",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := modelSwitchAcceptedText(c.resp, "Qwen3 8B Instruct"); got != c.want {
				t.Errorf("modelSwitchAcceptedText = %q, want %q", got, c.want)
			}
		})
	}
}

func TestOnModelSwitchAccepted_ArmsGraceOnlyForTheRestart(t *testing.T) {
	cases := []struct {
		name      string
		resp      *management.PreferredModelResponse
		wantArmed bool
	}{
		{"restart fallback arms the window", &management.PreferredModelResponse{WillRestart: true}, true},
		{"in-process swap does not", &management.PreferredModelResponse{}, false},
		{"downloading in process does not", &management.PreferredModelResponse{Downloading: true}, false},
		{"no response does not", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stub := &stubNotifier{}
			notifier = stub
			t.Cleanup(func() { notifier = &stubNotifier{} })

			tr := &tray{}
			tr.onModelSwitchAccepted(c.resp, "Qwen3 8B Instruct")

			tr.mu.Lock()
			until := tr.switchingUntil
			tr.mu.Unlock()
			if armed := until.After(time.Now()); armed != c.wantArmed {
				t.Errorf("grace window armed=%v, want %v", armed, c.wantArmed)
			}
			// Every arm notifies exactly once — the silence waired#808
			// opened with is the thing that must not come back.
			if calls := stub.snapshot(); len(calls) != 1 {
				t.Fatalf("notify calls=%d, want 1", len(calls))
			} else if calls[0].body != modelSwitchAcceptedText(c.resp, "Qwen3 8B Instruct") {
				t.Errorf("notified %q, want the modelSwitchAcceptedText wording", calls[0].body)
			}
		})
	}
}

func TestModelSwitchErrorText(t *testing.T) {
	// 409: the daemon kept the preference, so the sentence has to say the
	// choice survived — otherwise the user re-picks a model that is
	// already recorded.
	got := modelSwitchErrorText(fmt.Errorf("post: %w", ErrModelSwitchUnavailable), "Qwen3 8B Instruct")
	if !strings.Contains(got, "Qwen3 8B Instruct") {
		t.Errorf("409 text = %q, want it to name the model", got)
	}
	if !strings.Contains(got, "saved") {
		t.Errorf("409 text = %q, want it to say the choice is kept", got)
	}
	if strings.Contains(got, "HTTP") || strings.Contains(got, "{") {
		t.Errorf("409 text = %q, want no transport detail or JSON body", got)
	}

	// Anything else keeps the diagnostic — an unrecognised failure with
	// its detail stripped is worse than an ugly one.
	other := modelSwitchErrorText(errors.New("dial tcp: connection refused"), "Qwen3 8B Instruct")
	if !strings.Contains(other, "connection refused") {
		t.Errorf("generic text = %q, want the underlying error preserved", other)
	}
}

func TestSwitchModelName(t *testing.T) {
	cases := []struct {
		name, display, modelID, want string
	}{
		{"display name wins", "Qwen3 8B Instruct", "qwen3-8b", "Qwen3 8B Instruct"},
		{"falls back to the model id", "", "qwen3-8b", "qwen3-8b"},
		{"and to a phrase when a slot carries neither", "", "", "the new model"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := switchModelName(c.display, c.modelID); got != c.want {
				t.Errorf("switchModelName(%q, %q) = %q, want %q", c.display, c.modelID, got, c.want)
			}
		})
	}
}

func TestFormatCatalogEntry_CarriesUndecoratedName(t *testing.T) {
	// The notification names the model in a sentence, so Name must not
	// pick up the row decoration Label carries.
	e := formatCatalogEntry(management.CatalogFamily{
		ModelID:     "qwen3-8b",
		DisplayName: "Qwen3 8B Instruct",
		Fits:        true,
		Active:      true,
	}, "ollama", management.CatalogHost{})
	if e.Name != "Qwen3 8B Instruct" {
		t.Errorf("Name=%q, want the bare display name", e.Name)
	}
	if !strings.HasPrefix(e.Label, "● ") {
		t.Errorf("precondition: Label=%q, want the active bullet (so Name is provably distinct)", e.Label)
	}

	// No DisplayName: Name falls back the same way Label does.
	e = formatCatalogEntry(management.CatalogFamily{ModelID: "qwen3-8b", Fits: true}, "ollama", management.CatalogHost{})
	if e.Name != "qwen3-8b" {
		t.Errorf("Name=%q, want the model id fallback", e.Name)
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
