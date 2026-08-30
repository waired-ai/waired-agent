package tray

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
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

// Older daemon: the public-use endpoint did not answer, so the snapshot
// field stays nil and the whole "Public computers" parent must stay
// hidden — the pre-feature menu renders unchanged.
func TestPublicShare_HiddenWhenDaemonLacksBothEndpoints(t *testing.T) {
	got := Update(enrolledConnected())
	if got.ShowPublicShareMenu {
		t.Errorf("ShowPublicShareMenu=true, want false when the endpoint is absent")
	}
	if got.ShowPublicUse || got.PublicMoreURL != "" {
		t.Errorf("expected all public fields zero, got %+v", got)
	}
}

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
