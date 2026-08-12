package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/update"
)

func TestFormatUpdateSummary(t *testing.T) {
	t.Run("available", func(t *testing.T) {
		s := formatUpdateSummary(&management.UpdateStatus{
			Phase:          management.UpdatePhaseAvailable,
			CurrentVersion: "1.2.3",
			LatestVersion:  "1.4.0",
			Available:      true,
		}, true)
		if !strings.Contains(s, "1.2.3") || !strings.Contains(s, "1.4.0") {
			t.Errorf("summary missing versions: %q", s)
		}
	})
	t.Run("error phase shows reason and no latest line", func(t *testing.T) {
		s := formatUpdateSummary(&management.UpdateStatus{
			Phase:          management.UpdatePhaseError,
			CurrentVersion: "1.2.3",
			Error:          "github unreachable",
		}, true)
		if !strings.Contains(s, "github unreachable") {
			t.Errorf("summary missing error: %q", s)
		}
		if strings.Contains(s, "Latest version") {
			t.Errorf("error summary should not print latest line: %q", s)
		}
	})
	t.Run("unknown versions render a placeholder", func(t *testing.T) {
		s := formatUpdateSummary(&management.UpdateStatus{Phase: management.UpdatePhaseIdle}, true)
		if !strings.Contains(s, "(unknown)") {
			t.Errorf("expected placeholder for empty versions: %q", s)
		}
	})
	// full=false is what keeps the installer's authoritative verdict from
	// being contradicted by the line above it (#726).
	t.Run("not full prints only the current version", func(t *testing.T) {
		s := formatUpdateSummary(&management.UpdateStatus{
			Phase:            management.UpdatePhaseIdle,
			CurrentVersion:   "1.2.3",
			LatestVersion:    "1.2.3",
			LatestSource:     update.SourceAPT,
			IndexRefreshedAt: time.Now().Add(-72 * time.Hour).Format(time.RFC3339),
		}, false)
		if !strings.Contains(s, "1.2.3") {
			t.Errorf("summary missing current version: %q", s)
		}
		if strings.Contains(s, "Latest version") || strings.Contains(s, "Package index") {
			t.Errorf("non-full summary must not answer 'latest': %q", s)
		}
	})
}

func TestPackageIndexLine(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }

	t.Run("fresh index states its age without a caution", func(t *testing.T) {
		got := packageIndexLine(&management.UpdateStatus{
			LatestSource: update.SourceAPT, IndexRefreshedAt: at(3 * time.Hour),
		}, now)
		if !strings.Contains(got, "3 hours ago") {
			t.Errorf("line = %q, want the age", got)
		}
		if strings.Contains(got, "may already be published") {
			t.Errorf("fresh index must not carry the caution: %q", got)
		}
	})

	// The case that cost a verification round: the index was days old and
	// the answer looked like a verdict.
	t.Run("stale index carries the caution and the remedy", func(t *testing.T) {
		got := packageIndexLine(&management.UpdateStatus{
			LatestSource: update.SourceAPT, IndexRefreshedAt: at(72 * time.Hour),
		}, now)
		if !strings.Contains(got, "3 days ago") {
			t.Errorf("line = %q, want the age in days", got)
		}
		if !strings.Contains(got, "may already be published") {
			t.Errorf("stale index must say so: %q", got)
		}
		if !strings.Contains(got, "--check --force") {
			t.Errorf("caution must name the way out: %q", got)
		}
	})

	t.Run("a live answer has no index to age", func(t *testing.T) {
		if got := packageIndexLine(&management.UpdateStatus{
			LatestSource: update.SourceGitHub, IndexRefreshedAt: at(72 * time.Hour),
		}, now); got != "" {
			t.Errorf("line = %q, want empty for a live source", got)
		}
	})

	t.Run("no line when the instant is missing or unparseable", func(t *testing.T) {
		if got := packageIndexLine(&management.UpdateStatus{LatestSource: update.SourceAPT}, now); got != "" {
			t.Errorf("line = %q, want empty", got)
		}
		if got := packageIndexLine(&management.UpdateStatus{
			LatestSource: update.SourceAPT, IndexRefreshedAt: "not a timestamp",
		}, now); got != "" {
			t.Errorf("line = %q, want empty", got)
		}
	})

	// A legacy daemon sends neither field; the CLI must degrade to the old
	// two-line summary rather than print a half-formed third line.
	t.Run("legacy daemon", func(t *testing.T) {
		if got := packageIndexLine(&management.UpdateStatus{LatestVersion: "1.2.3"}, now); got != "" {
			t.Errorf("line = %q, want empty", got)
		}
	})
}

// The daemon and the CLI are separate processes joined only by this JSON,
// so the two new fields have to survive the wire before the summary can
// render them. `--mgmt` cannot be pointed at a stub (writes are routed to
// the daemon's unix socket regardless), which makes this round trip the
// only place the tags are actually exercised.
func TestUpdateStatusWireCarriesIndexFreshness(t *testing.T) {
	wire := `{"phase":"idle","available":false,` +
		`"current_version":"0.0.2~edge.20260811161329+2f423b6",` +
		`"latest_version":"0.0.2~edge.20260811161329+2f423b6",` +
		`"checked_at":"2026-08-12T11:43:09Z","notify_enabled":true,` +
		`"latest_source":"apt","index_refreshed_at":"2026-08-09T08:43:09Z"}`

	var st management.UpdateStatus
	if err := json.Unmarshal([]byte(wire), &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if st.LatestSource != update.SourceAPT {
		t.Errorf("LatestSource = %q, want %q — check the json tag", st.LatestSource, update.SourceAPT)
	}
	now := time.Date(2026, 8, 12, 11, 43, 9, 0, time.UTC) // 3 days after the index
	got := packageIndexLine(&st, now)
	if !strings.Contains(got, "3 days ago") || !strings.Contains(got, "--check --force") {
		t.Errorf("rendered line = %q, want the age and the remedy", got)
	}

	// A daemon from before this change sends neither field. The CLI must
	// fall back to the old two-line summary, not print a broken third line.
	var legacy management.UpdateStatus
	if err := json.Unmarshal([]byte(`{"phase":"idle","current_version":"1.2.3","latest_version":"1.2.3"}`), &legacy); err != nil {
		t.Fatalf("decode legacy: %v", err)
	}
	s := formatUpdateSummary(&legacy, true)
	if strings.Contains(s, "Package index") {
		t.Errorf("legacy daemon must not produce an index line: %q", s)
	}
	if !strings.Contains(s, "Latest version") {
		t.Errorf("legacy daemon must still get the latest line: %q", s)
	}
}

func TestHumanIndexAge(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{-5 * time.Minute, "just now"}, // clock skew
		{30 * time.Minute, "less than an hour ago"},
		{90 * time.Minute, "1 hour ago"},
		{5 * time.Hour, "5 hours ago"},
		{47 * time.Hour, "47 hours ago"},
		{50 * time.Hour, "2 days ago"},
		{72 * time.Hour, "3 days ago"},
		{10 * 24 * time.Hour, "10 days ago"},
	} {
		if got := humanIndexAge(tc.d); got != tc.want {
			t.Errorf("humanIndexAge(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestCheckRoute(t *testing.T) {
	apt := func() *management.UpdateStatus {
		return &management.UpdateStatus{Phase: management.UpdatePhaseIdle, LatestSource: update.SourceAPT}
	}
	live := func() *management.UpdateStatus {
		return &management.UpdateStatus{Phase: management.UpdatePhaseIdle, LatestSource: update.SourceGitHub}
	}
	errored := &management.UpdateStatus{Phase: management.UpdatePhaseError}

	for _, tc := range []struct {
		name                    string
		st                      *management.UpdateStatus
		requested, host, goos   string
		force, canElevate       bool
		wantInstaller, wantNote bool
	}{
		// The daemon's answer stands.
		{"stable linux host, no force", apt(), "", "stable", "linux", false, true, false, false},
		{"stable macos host, no force", live(), "", "stable", "darwin", false, true, false, false},

		// Reasons to leave the daemon that predate #726.
		{"explicit channel", apt(), "edge", "stable", "linux", false, true, true, false},
		{"edge host", apt(), "", "edge", "linux", false, true, true, false},
		{"daemon down", nil, "", "stable", "linux", false, true, true, false},
		{"daemon errored", errored, "", "stable", "linux", false, true, true, false},

		// #726: --force means "re-resolve authoritatively". On Linux only
		// the installer can refresh the package index.
		{"force on linux with an apt answer", apt(), "", "stable", "linux", true, true, true, false},
		{"force on macos stays with the live daemon answer", live(), "", "stable", "darwin", true, true, false, false},
		{"force on windows stays with the live daemon answer", live(), "", "stable", "windows", true, true, false, false},
		// A non-apt Linux host (the GitHub fallback) has no index either.
		{"force on a non-apt linux host", live(), "", "stable", "linux", true, true, false, false},
		// A daemon that predates this change names no source, but on Linux
		// it is still reading the package index. Requiring an explicit
		// "apt" would keep the old --force behaviour through exactly the
		// mixed-version window this has to work in.
		{"force against a legacy daemon", &management.UpdateStatus{Phase: management.UpdatePhaseIdle}, "", "stable", "linux", true, true, true, false},

		// Root out of reach: sudo has nothing to prompt on, so the
		// installer would fail rather than answer. Degrade and say why.
		// Reproduced on hardware: the old CLI exits 1 here with
		// "sudo: A terminal is required to authenticate".
		{"force with no way to elevate", apt(), "", "stable", "linux", true, false, false, true},
		{"edge host with no way to elevate", apt(), "", "edge", "linux", false, false, false, true},
		{"explicit channel with no way to elevate", apt(), "edge", "stable", "linux", false, false, false, true},
		// …unless there is no daemon answer to degrade to: then the
		// installer's own failure is more use than silence.
		{"daemon down still tries with no way to elevate", nil, "", "stable", "linux", false, false, true, false},
		// Only Linux needs root for the check.
		{"macos is unaffected by elevation", live(), "edge", "stable", "darwin", false, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotInstaller, gotNote := checkRoute(tc.st, tc.requested, tc.host, tc.goos, tc.force, tc.canElevate)
			if gotInstaller != tc.wantInstaller {
				t.Errorf("useInstaller = %v, want %v", gotInstaller, tc.wantInstaller)
			}
			if (gotNote != "") != tc.wantNote {
				t.Errorf("note = %q, want a note: %v", gotNote, tc.wantNote)
			}
		})
	}
}

func TestRequestedChannel(t *testing.T) {
	for _, tc := range []struct {
		edge, stable bool
		want         string
	}{
		{false, false, ""},
		{true, false, "edge"},
		{false, true, "stable"},
	} {
		if got := requestedChannel(tc.edge, tc.stable); got != tc.want {
			t.Errorf("requestedChannel(%v,%v) = %q, want %q", tc.edge, tc.stable, got, tc.want)
		}
	}
}

func TestShouldStopUpToDate(t *testing.T) {
	upToDate := &management.UpdateStatus{Phase: management.UpdatePhaseIdle, Available: false}
	available := &management.UpdateStatus{Phase: management.UpdatePhaseAvailable, Available: true}
	errored := &management.UpdateStatus{Phase: management.UpdatePhaseError, Available: false}

	for _, tc := range []struct {
		name      string
		st        *management.UpdateStatus
		requested string
		host      string
		force     bool
		want      bool
	}{
		// Stable host the daemon confirms current: the only case we stop.
		{"stable host up-to-date stops", upToDate, "", "stable", false, true},
		{"unknown host up-to-date stops", upToDate, "", "", false, true},
		// Edge host always proceeds — the daemon can't rank edge builds.
		{"edge host proceeds even if daemon says up-to-date", upToDate, "", "edge", false, false},
		// Explicit channel request always proceeds (switch/refresh).
		{"explicit stable proceeds", upToDate, "stable", "stable", false, false},
		{"explicit edge proceeds", upToDate, "edge", "stable", false, false},
		// --force always proceeds.
		{"force proceeds", upToDate, "", "stable", true, false},
		// Available or unusable daemon answers proceed.
		{"available proceeds", available, "", "stable", false, false},
		{"daemon error proceeds", errored, "", "stable", false, false},
		{"nil status proceeds", nil, "", "stable", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldStopUpToDate(tc.st, tc.requested, tc.host, tc.force); got != tc.want {
				t.Errorf("shouldStopUpToDate = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseNotifyArg(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    bool
		wantErr bool
	}{
		{"on", true, false},
		{"off", false, false},
		{"ON", true, false},
		{" Off ", false, false},
		{"true", true, false},
		{"disable", false, false},
		{"enabled", true, false},
		{"", false, true},
		{"maybe", false, true},
	} {
		got, err := parseNotifyArg(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseNotifyArg(%q): expected error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseNotifyArg(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseNotifyArg(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
