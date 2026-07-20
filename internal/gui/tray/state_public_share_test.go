package tray

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// enrolledConnected is the minimal signed-in + Connected snapshot the
// public-share projection needs; the tests below add the public fields.
func enrolledConnected() Snapshot {
	return Snapshot{
		Health:   HealthOnline,
		Identity: &management.IdentityView{Enrolled: true, AccountEmail: "a@b"},
		Status:   &management.Status{Phase: "active"},
	}
}

func boolPtr(b bool) *bool { return &b }

// Older daemon: neither public endpoint answered, so both snapshot fields
// stay nil and the whole "Public share" parent must stay hidden — the
// pre-feature menu renders unchanged.
func TestPublicShare_HiddenWhenDaemonLacksBothEndpoints(t *testing.T) {
	got := Update(enrolledConnected())
	if got.ShowPublicShareMenu {
		t.Errorf("ShowPublicShareMenu=true, want false when both endpoints absent")
	}
	if got.PublicShareToggleAction != "" || got.ShowPublicUse || got.PublicMoreURL != "" {
		t.Errorf("expected all public fields zero, got %+v", got)
	}
}

func TestPublicShare_On(t *testing.T) {
	snap := enrolledConnected()
	snap.PublicShare = &management.PublicShareStateResponse{
		State:        string(state.PublicShareOn),
		DesiredState: string(state.PublicShareOn),
		CPSynced:     boolPtr(true),
	}
	got := Update(snap)
	if !got.ShowPublicShareMenu {
		t.Fatal("ShowPublicShareMenu=false, want true")
	}
	if got.PublicShareToggleAction != "Stop sharing this computer publicly" {
		t.Errorf("toggle=%q", got.PublicShareToggleAction)
	}
	if got.PublicShareStateLabel != "Public sharing: on" {
		t.Errorf("state label=%q", got.PublicShareStateLabel)
	}
	if got.PublicShareNote != "" {
		t.Errorf("note=%q, want empty when synced", got.PublicShareNote)
	}
}

func TestPublicShare_Off(t *testing.T) {
	snap := enrolledConnected()
	snap.PublicShare = &management.PublicShareStateResponse{
		State:        string(state.PublicShareOff),
		DesiredState: string(state.PublicShareOff),
		CPSynced:     boolPtr(true),
	}
	got := Update(snap)
	if got.PublicShareToggleAction != "Share this computer publicly" {
		t.Errorf("toggle=%q", got.PublicShareToggleAction)
	}
	if got.PublicShareStateLabel != "Public sharing: off" {
		t.Errorf("state label=%q", got.PublicShareStateLabel)
	}
}

// A pending control-plane sync annotates the label with " (saving…)" and
// surfaces the SERVED note verbatim.
func TestPublicShare_PendingSyncShowsSavingAndNote(t *testing.T) {
	snap := enrolledConnected()
	snap.PublicShare = &management.PublicShareStateResponse{
		State:        string(state.PublicShareOff),
		DesiredState: string(state.PublicShareOn),
		CPSynced:     boolPtr(false),
		Note:         management.PublicSharePendingNote,
	}
	got := Update(snap)
	want := "Public sharing: on (saving…)"
	if got.PublicShareStateLabel != want {
		t.Errorf("state label=%q, want %q", got.PublicShareStateLabel, want)
	}
	if got.PublicShareNote != management.PublicSharePendingNote {
		t.Errorf("note=%q, want the served pending note verbatim", got.PublicShareNote)
	}
}

// Not yet enrolled in Public Share (empty current AND desired): the parent
// still shows (the endpoint exists) but the toggle stays blank so we don't
// bait clicks. Must not panic.
func TestPublicShare_NotEnrolledEmptyStateHidesToggle(t *testing.T) {
	snap := enrolledConnected()
	snap.PublicShare = &management.PublicShareStateResponse{CPSynced: boolPtr(true)}
	got := Update(snap)
	if !got.ShowPublicShareMenu {
		t.Error("ShowPublicShareMenu=false, want true when endpoint present")
	}
	if got.PublicShareToggleAction != "" || got.PublicShareStateLabel != "" {
		t.Errorf("expected blank toggle/label when not enrolled, got toggle=%q label=%q",
			got.PublicShareToggleAction, got.PublicShareStateLabel)
	}
}

// mode "auto" selects exactly one row — the auto row.
func TestPublicShare_PublicUse_ModeRowsSelection(t *testing.T) {
	snap := enrolledConnected()
	snap.PublicUse = &management.PublicUseResponse{Mode: "auto", Consented: true}
	got := Update(snap)
	if !got.ShowPublicUse {
		t.Fatal("ShowPublicUse=false, want true")
	}
	if len(got.PublicUseModes) != 3 {
		t.Fatalf("want 3 mode rows, got %d", len(got.PublicUseModes))
	}
	var selected []string
	for _, r := range got.PublicUseModes {
		if r.Selected {
			selected = append(selected, r.Mode)
		}
	}
	if len(selected) != 1 || selected[0] != "auto" {
		t.Errorf("selected modes=%v, want exactly [auto]", selected)
	}
	if !got.PublicUseConsented {
		t.Error("PublicUseConsented=false, want true")
	}
}

func TestPublicShare_PublicUse_NotConsented(t *testing.T) {
	snap := enrolledConnected()
	snap.PublicUse = &management.PublicUseResponse{Mode: "off", Consented: false}
	got := Update(snap)
	if !got.ShowPublicUse {
		t.Fatal("ShowPublicUse=false, want true")
	}
	if got.PublicUseConsented {
		t.Error("PublicUseConsented=true, want false")
	}
	// "off" is the default selection when unconsented.
	if !got.PublicUseModes[0].Selected || got.PublicUseModes[0].Mode != "off" {
		t.Errorf("want off row selected, got %+v", got.PublicUseModes)
	}
}

// The two endpoints gate independently: share present + use absent shows
// the provider toggle but no use rows.
func TestPublicShare_HiddenWhenUseEndpointMissingButSharePresent(t *testing.T) {
	snap := enrolledConnected()
	snap.PublicShare = &management.PublicShareStateResponse{
		State:        string(state.PublicShareOff),
		DesiredState: string(state.PublicShareOff),
		CPSynced:     boolPtr(true),
	}
	// snap.PublicUse stays nil (that endpoint 404'd).
	got := Update(snap)
	if !got.ShowPublicShareMenu {
		t.Error("ShowPublicShareMenu=false, want true")
	}
	if got.ShowPublicUse {
		t.Error("ShowPublicUse=true, want false when the use endpoint is absent")
	}
	if got.PublicShareToggleAction == "" {
		t.Error("provider toggle should still render")
	}
}

// The "Privacy & safety…" link is extracted from the SERVED warning text,
// never hardcoded: feeding the real management warning yields the docs URL.
// End-to-end through Update() (with a use endpoint present so the parent
// renders) the link lands on PublicMoreURL.
func TestPublicShare_PublicMoreURL_ExtractsServedLink(t *testing.T) {
	if got := publicMoreURL(management.PublicShareWarningText); got != "https://docs.waired.ai/public-share" {
		t.Fatalf("publicMoreURL=%q, want the served docs link", got)
	}
	snap := enrolledConnected()
	snap.PublicUse = &management.PublicUseResponse{Mode: "off"}
	snap.PublicWarning = &management.PublicWarningResponse{Text: management.PublicShareWarningText}
	if got := Update(snap); got.PublicMoreURL != "https://docs.waired.ai/public-share" {
		t.Errorf("Update PublicMoreURL=%q, want the served docs link", got.PublicMoreURL)
	}
}

func TestPublicShare_PublicMoreURL_AbsentReturnsEmpty(t *testing.T) {
	if got := publicMoreURL("No link line here.\nJust prose."); got != "" {
		t.Errorf("publicMoreURL=%q, want empty when no More: line", got)
	}
}

func TestPublicUseModeRowLabel_SelectedGlyph(t *testing.T) {
	sel := publicUseModeRowLabel(PublicUseModeRow{Label: "X", Selected: true})
	if sel != "● X" {
		t.Errorf("selected label=%q, want ● X", sel)
	}
	un := publicUseModeRowLabel(PublicUseModeRow{Label: "X", Selected: false})
	if un != "○ X" {
		t.Errorf("unselected label=%q, want ○ X", un)
	}
}
