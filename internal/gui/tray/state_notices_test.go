package tray

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/notice"
)

// noticeConnectedSnapshot is a healthy connected host carrying ns.
//
// This file replaces state_recommendation_test.go. The rules it used to
// pin — a dismissed suggestion says nothing, an empty target says
// nothing, the step-down wins over the step-up — moved to the daemon
// with the derivation itself (cmd/waired-agent/notices_test.go), because
// the tray is no longer the only surface applying them. What is left
// here is what the tray still decides: how a published notice becomes a
// row.
func noticeConnectedSnapshot(ns []notice.Notice) Snapshot {
	return Snapshot{
		Health: HealthOnline,
		Identity: &management.IdentityView{
			Enrolled: true, AccountEmail: "a@b.c", DeviceID: "dev-1",
			DeviceName: "host", OverlayIP: "100.96.0.10", ControlURL: "https://cp",
		},
		Status:  &management.Status{Phase: "active"},
		Notices: ns,
	}
}

func TestUpdate_NoticeBecomesARow(t *testing.T) {
	got := Update(noticeConnectedSnapshot([]notice.Notice{
		notice.LighterModel("heavy", "light", 42, 60),
	}))

	if len(got.Notices) != 1 {
		t.Fatalf("got %d rows, want 1", len(got.Notices))
	}
	row := got.Notices[0]
	if !strings.Contains(row.Label, "light") {
		t.Errorf("label = %q, want it to name the target model", row.Label)
	}
	if !strings.HasPrefix(row.Label, "⚠ ") {
		t.Errorf("label = %q, want the warning marker", row.Label)
	}
	if row.Action != notice.ActionModelSuggestion || row.Target != "light" {
		t.Errorf("row = %+v, want the suggestion's action and target", row)
	}
}

func TestUpdate_UpgradeNoticeCarriesItsOwnMarker(t *testing.T) {
	got := Update(noticeConnectedSnapshot([]notice.Notice{
		notice.BetterModel("light", "heavy", 118, 64),
	}))

	if len(got.Notices) != 1 {
		t.Fatalf("got %d rows, want 1", len(got.Notices))
	}
	if !strings.HasPrefix(got.Notices[0].Label, "⬆ ") {
		t.Errorf("label = %q, want the step-up marker rather than a warning", got.Notices[0].Label)
	}
}

// TestUpdate_NoNoticesRendersTheMenuItAlwaysDid
//
// PRODUCT CONTRACT: the "404 leaves the field nil and the group hides"
// convention every other best-effort poll in this tray follows. It is
// what lets a tray from this build run against a daemon that predates
// the notice route without showing an error or a blank row.
func TestUpdate_NoNoticesRendersTheMenuItAlwaysDid(t *testing.T) {
	if got := Update(noticeConnectedSnapshot(nil)); len(got.Notices) != 0 {
		t.Fatalf("got %+v, want no rows", got.Notices)
	}
	if got := Update(noticeConnectedSnapshot([]notice.Notice{})); len(got.Notices) != 0 {
		t.Fatalf("got %+v, want no rows", got.Notices)
	}
}

// TestUpdate_NoticesTruncateToTheSlotsThatExist
//
// PRODUCT CONTRACT: systray cannot add menu items after the menu is
// built (tray.go, onReady), so the renderer pre-allocates
// notice.MaxActive rows. A daemon newer than this tray must not be able
// to produce a projection with nowhere to render.
func TestUpdate_NoticesTruncateToTheSlotsThatExist(t *testing.T) {
	var many []notice.Notice
	for range notice.MaxActive + 4 {
		many = append(many, notice.LighterModel("heavy", "light", 42, 60))
	}

	got := Update(noticeConnectedSnapshot(many))
	if len(got.Notices) != notice.MaxActive {
		t.Fatalf("got %d rows, want %d", len(got.Notices), notice.MaxActive)
	}
}

// TestUpdate_NoticeWithNoTitleIsNotARow records today's behaviour: a
// notice a newer daemon sent with nothing to say renders nothing rather
// than an empty row with a marker on it.
func TestUpdate_NoticeWithNoTitleIsNotARow(t *testing.T) {
	got := Update(noticeConnectedSnapshot([]notice.Notice{{Kind: "mystery"}}))
	if len(got.Notices) != 0 {
		t.Fatalf("got %+v, want no rows", got.Notices)
	}
}

// TestUpdate_NoticesShowBeforeSignIn records today's behaviour: what a
// notice reports is true of this computer, not of the account, so it is
// not gated on enrollment — the same reasoning as the update banner
// beside it.
func TestUpdate_NoticesShowBeforeSignIn(t *testing.T) {
	snap := noticeConnectedSnapshot([]notice.Notice{notice.LighterModel("heavy", "light", 42, 60)})
	snap.Identity = &management.IdentityView{Enrolled: false}

	if got := Update(snap); len(got.Notices) != 1 {
		t.Fatalf("got %d rows, want 1", len(got.Notices))
	}
}
