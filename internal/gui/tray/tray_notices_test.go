package tray

import (
	"strings"
	"testing"

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
