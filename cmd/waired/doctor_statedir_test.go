package main

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration"
)

// PRODUCT CONTRACT — waired-agent#800. Doctor has to hold both answers to
// see this one: signInFinding reads the disk and stays silent when there
// is no identity ("not enrolled, covered elsewhere"), connectionFinding
// reads the daemon and is told "enrolled and active". Each is right about
// what it asked. The fault is the gap between them, and on the reported
// host it left every check green while model pulls failed 3/3 and `waired
// status` said "Not enrolled".
func TestStateDirFinding(t *testing.T) {
	for _, tc := range []struct {
		name                                    string
		diskEnrolled, daemonAnswered, daemonEnr bool
		wantStatus                              integration.Status
		wantSubject                             bool
	}{
		{"the #800 split-brain", false, true, true, integration.StatusFail, true},
		{"both agree the device is enrolled", true, true, true, integration.StatusUnknown, false},
		{"both agree it is not", false, true, false, integration.StatusUnknown, false},
		// A disk identity with a daemon that says no is a different
		// situation — an unenrolled or restarting daemon — and the
		// existing checks already speak to it.
		{"disk enrolled, daemon not", true, true, false, integration.StatusUnknown, false},
		// No answer from the daemon is not evidence of a gap. The
		// management probe reports an unreachable daemon with better
		// advice than this check could.
		{"daemon did not answer", false, false, false, integration.StatusUnknown, false},
		{"daemon did not answer, disk enrolled", true, false, false, integration.StatusUnknown, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := stateDirFinding(tc.diskEnrolled, tc.daemonAnswered, tc.daemonEnr)
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
func TestStateDirFinding_PointsAtInit(t *testing.T) {
	f := stateDirFinding(false, true, true)
	if f.Detail == "" {
		t.Fatal("no detail")
	}
	for _, want := range []string{"waired init", "state directory"} {
		if !strings.Contains(f.Detail, want) {
			t.Errorf("detail %q does not mention %q", f.Detail, want)
		}
	}
}
