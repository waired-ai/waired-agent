package main

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration"
)

// PRODUCT CONTRACT — waired-agent#800 (the fail row) and waired-agent#1005
// (the two skip rows).
//
// #800: doctor has to hold both answers to see it — signInFinding reads the
// disk and stays silent when there is no identity ("not enrolled, covered
// elsewhere"), connectionFinding reads the daemon and is told "enrolled and
// active". Each is right about what it asked. The fault is the gap between
// them, and on the reported host it left every check green while model
// pulls failed 3/3 and `waired status` said "Not enrolled".
//
// #1005: "no identity on disk" was one bool that a permission error also
// produced, so on sv-mag and pc-mbp14-m5 (apt / launchd service installs)
// every non-root run announced the identity gone and pointed at `waired
// init` — the one command that would overwrite a healthy enrollment.
func TestStateDirFinding(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		disk                      stateDiskAnswer
		daemonAnswered, daemonEnr bool
		wantStatus                integration.Status
		wantSubject               bool
	}{
		{"the #800 split-brain", diskAbsent, true, true, integration.StatusFail, true},
		{"both agree the device is enrolled", diskHasIdentity, true, true, integration.StatusUnknown, false},
		{"both agree it is not", diskAbsent, true, false, integration.StatusUnknown, false},
		// A disk identity with a daemon that says no is a different
		// situation — an unenrolled or restarting daemon — and the
		// existing checks already speak to it.
		{"disk enrolled, daemon not", diskHasIdentity, true, false, integration.StatusUnknown, false},
		// No answer from the daemon is not evidence of a gap. The
		// management probe reports an unreachable daemon with better
		// advice than this check could.
		{"daemon did not answer", diskAbsent, false, false, integration.StatusUnknown, false},
		{"daemon did not answer, disk enrolled", diskHasIdentity, false, false, integration.StatusUnknown, false},
		// #1005. Neither of these observed an empty state dir, so neither
		// may claim one — and a skip never reaches the exit code.
		{"the dir denied the read", diskUnreadable, true, true, integration.StatusSkip, true},
		{"the identity is system-wide", diskSystemWide, true, true, integration.StatusSkip, true},
		// Still nothing to say when the daemon is not enrolled, whatever
		// the disk could or could not show.
		{"system-wide, daemon not enrolled", diskSystemWide, true, false, integration.StatusUnknown, false},
		{"unreadable, daemon did not answer", diskUnreadable, false, false, integration.StatusUnknown, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := stateDirFinding(tc.disk, tc.daemonAnswered, tc.daemonEnr, "/var/lib/waired", "linux")
			if (f.Subject != "") != tc.wantSubject {
				t.Fatalf("finding = %+v, want subject present: %v", f, tc.wantSubject)
			}
			if tc.wantSubject && f.Status != tc.wantStatus {
				t.Errorf("status = %v, want %v", f.Status, tc.wantStatus)
			}
		})
	}
}

// The advice has to name the action that fixes it. `waired init` is what
// restores the identity now (the daemon rewrites it from the session it
// still holds), so the line points there rather than at a reinstall.
//
// Narrowed by #1005: this is the advice for diskAbsent ONLY. The two rows
// that did not observe an empty dir must not repeat it — asserted below.
func TestStateDirFinding_PointsAtInit(t *testing.T) {
	f := stateDirFinding(diskAbsent, true, true, "/var/lib/waired", "linux")
	if f.Detail == "" {
		t.Fatal("no detail")
	}
	for _, want := range []string{"waired init", "state directory"} {
		if !strings.Contains(f.Detail, want) {
			t.Errorf("detail %q does not mention %q", f.Detail, want)
		}
	}
}

// PRODUCT CONTRACT — waired-agent#1005. `waired init` re-enrolls the
// device: suggesting it to a user whose identity is intact but unreadable
// is what made the false failure dangerous rather than merely noisy. The
// two could-not-look rows say what would let the check run instead.
func TestStateDirFinding_UnobservedRowsNeverSuggestInit(t *testing.T) {
	for _, disk := range []stateDiskAnswer{diskUnreadable, diskSystemWide} {
		f := stateDirFinding(disk, true, true, "/var/lib/waired", "linux")
		if strings.Contains(f.Detail, "waired init") {
			t.Errorf("disk=%v detail %q suggests re-enrolling a device whose identity was never observed to be missing", disk, f.Detail)
		}
		if !strings.Contains(f.Detail, "needs elevation to check") {
			t.Errorf("disk=%v detail %q does not say what would let the check run", disk, f.Detail)
		}
	}
}

// The system-wide row is the `waired status` answer for the same host
// (waired#751) rendered as a doctor row, so it names the directory and
// uses that command's wording. Table-tested across GOOS because the
// elevation hint is the one part that differs and Windows has no sudo
// (waired#752).
func TestStateDirFinding_SystemWideWording(t *testing.T) {
	for _, tc := range []struct {
		goos, wantHint string
	}{
		{"linux", "run `sudo waired doctor`"},
		{"darwin", "run `sudo waired doctor`"},
		{"windows", "re-run `waired doctor` from an elevated (Administrator) prompt"},
	} {
		t.Run(tc.goos, func(t *testing.T) {
			f := stateDirFinding(diskSystemWide, true, true, "/var/lib/waired", tc.goos)
			for _, want := range []string{"enrolled system-wide", "/var/lib/waired", tc.wantHint} {
				if !strings.Contains(f.Detail, want) {
					t.Errorf("detail %q does not contain %q", f.Detail, want)
				}
			}
		})
	}
}

// The unreadable row is worded exactly like every other check that could
// not read the state dir (unreadableFinding, #651): one sentence, one
// remedy, no second vocabulary for the same situation.
func TestStateDirFinding_UnreadableMatchesTheOtherSkippedChecks(t *testing.T) {
	got := stateDirFinding(diskUnreadable, true, true, "", "linux").Detail
	want, ok := unreadableFinding("state directory", fs.ErrPermission)
	if !ok {
		t.Fatal("unreadableFinding did not recognise fs.ErrPermission")
	}
	if got != want.Detail {
		t.Errorf("detail = %q, want the same sentence unreadableFinding gives: %q", got, want.Detail)
	}
}
