package tray

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/notice"
)

// TestNoticeSlotCountMatchesTheRegistryCap
//
// PRODUCT CONTRACT: systray cannot add menu items once the menu is built
// (tray.go, onReady), so the number of rows the projection can produce
// and the number of slots the tray allocates have to be the same number.
// That is why both read notice.MaxActive instead of each carrying its
// own constant — and why there is no overflow row: a slot nobody can
// reach would be a row nobody could ever see.
func TestNoticeSlotCountMatchesTheRegistryCap(t *testing.T) {
	var many []notice.Notice
	for range notice.MaxActive + 3 {
		many = append(many, notice.LighterModel("heavy", "light", 42, 60))
	}
	m := Update(noticeConnectedSnapshot(many))
	if len(m.Notices) > notice.MaxActive {
		t.Fatalf("the projection emits %d rows but the tray allocates %d slots",
			len(m.Notices), notice.MaxActive)
	}
}

// TestStatusReport_CarriesTheNotices
//
// PRODUCT CONTRACT (waired-agent#1205): the status report is where a
// notice row goes when its own action cannot be carried out — the notice
// poll and the catalog poll are independent, so the tray can be showing
// a suggestion whose live details it does not have. A report that did
// not mention the notice would make that click a non-sequitur.
func TestStatusReport_CarriesTheNotices(t *testing.T) {
	snap := noticeConnectedSnapshot([]notice.Notice{notice.LighterModel("heavy", "light", 42, 60)})

	_, details := statusReport(Update(snap), snap, "0.0.3-rc4", "90dd4a5", testReportNow())

	for _, want := range []string{"NOTICES", "Lighter model recommended", "light"} {
		if !strings.Contains(details, want) {
			t.Errorf("report is missing %q:\n%s", want, details)
		}
	}
}

// TestStatusReport_OmitsAnEmptyNoticesSection records today's behaviour:
// a host with nothing to be told shows no heading for it, like every
// other section in the report.
func TestStatusReport_OmitsAnEmptyNoticesSection(t *testing.T) {
	snap := noticeConnectedSnapshot(nil)

	_, details := statusReport(Update(snap), snap, "0.0.3-rc4", "90dd4a5", testReportNow())

	if strings.Contains(details, "NOTICES") {
		t.Errorf("a host with nothing to say still printed a NOTICES heading:\n%s", details)
	}
}

// TestNoticeClickTarget
//
// PRODUCT CONTRACT (decision record 20260905/0000, rule 5: a click with
// no live recommendation opens the status report rather than doing
// nothing). The notices poll and the polls that fill the tray's local
// state are independent best-effort GETs with independent nil states, so
// the disagreement cases below are reachable in the field, not
// hypothetical — and a menu row that does nothing when clicked is the
// worst thing a menu can do.
func TestNoticeClickTarget(t *testing.T) {
	for _, tc := range []struct {
		name       string
		action     notice.Action
		haveRec    bool
		haveUpdate bool
		want       noticeClick
	}{
		{"suggestion with a live recommendation", notice.ActionModelSuggestion, true, false, noticeClickRecommendation},
		{"suggestion without one", notice.ActionModelSuggestion, false, false, noticeClickStatusReport},
		{"update with a live update status", notice.ActionInstallUpdate, false, true, noticeClickUpdate},
		{"update without one", notice.ActionInstallUpdate, false, false, noticeClickStatusReport},
		{"no action at all", notice.ActionNone, true, true, noticeClickStatusReport},
		{"an action from a newer daemon", notice.Action(99), true, true, noticeClickStatusReport},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := noticeClickTarget(tc.action, tc.haveRec, tc.haveUpdate); got != tc.want {
				t.Errorf("noticeClickTarget(%v, rec=%v, upd=%v) = %v, want %v",
					tc.action, tc.haveRec, tc.haveUpdate, got, tc.want)
			}
		})
	}
}

// TestUpdateNoticeIsARowLikeAnyOther
//
// PRODUCT CONTRACT (owner ruling, 2026-09-05: trayの既存行は二重にならない
// ようにする). The banner and the notice would otherwise both be drawn, and
// the whole point of publishing it was that one thing is said once.
func TestUpdateNoticeIsARowLikeAnyOther(t *testing.T) {
	m := Update(Snapshot{
		Health:   HealthOnline,
		Identity: &management.IdentityView{Enrolled: true, AccountEmail: "a@b"},
		Status:   &management.Status{Phase: "active"},
		Update:   availUpdate(),
		Notices:  []notice.Notice{notice.UpdateAvailable("1.2.3", "1.4.0")},
	})
	if len(m.Notices) != 1 {
		t.Fatalf("Notices = %+v, want the published update row", m.Notices)
	}
	if !strings.Contains(m.Notices[0].Label, "1.4.0") {
		t.Errorf("row = %q, want the version", m.Notices[0].Label)
	}
	if m.Notices[0].Action != notice.ActionInstallUpdate {
		t.Errorf("row action = %v, want the install action", m.Notices[0].Action)
	}
	// And the click data the tray still resolves for itself.
	if !m.UpdateAvailable || m.UpdateVersion != "1.4.0" {
		t.Errorf("click data lost: %+v", m)
	}
}
