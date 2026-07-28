package main

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/platform/trayhost"
)

// TestPlanDoctorFix pins the fix flow. These are product contracts, not a
// record of today's behaviour:
//
//   - a fixable tray warning earns the "Press f to fix" prompt even when
//     nothing has failed (before #295 the prompt only appeared on a failure,
//     so the tray warning was unfixable in practice);
//   - repairing only the tray does not also re-link every coding agent;
//   - a tray warning never turns into a non-zero exit (asserted separately in
//     TestPlanDoctorFix_TrayNeverAffectsExitCode).
func TestPlanDoctorFix(t *testing.T) {
	tests := []struct {
		name          string
		hasFail       bool
		tray          trayhost.RepairAction
		forced        bool
		noInteractive bool
		tty           bool
		want          doctorFixPlan
	}{
		{
			name: "clean run on a TTY does nothing",
			tty:  true,
			tray: trayhost.RepairNone,
			want: doctorFixPlan{},
		},
		{
			name:    "a failure on a TTY prompts and repairs the integration",
			hasFail: true, tty: true, tray: trayhost.RepairNone,
			want: doctorFixPlan{Prompt: true, Integration: true},
		},
		{
			name: "a fixable tray warning alone prompts for just the tray",
			tty:  true, tray: trayhost.RepairEnableOnly,
			want: doctorFixPlan{Prompt: true, Tray: true},
		},
		{
			name: "a privileged tray repair alone also prompts for just the tray",
			tty:  true, tray: trayhost.RepairInstallThenEnable,
			want: doctorFixPlan{Prompt: true, Tray: true},
		},
		{
			name: "a manual tray warning is not offered as a fix",
			tty:  true, tray: trayhost.RepairManual,
			want: doctorFixPlan{},
		},
		{
			name:    "a failure plus a fixable tray repairs both",
			hasFail: true, tty: true, tray: trayhost.RepairInstallThenEnable,
			want: doctorFixPlan{Prompt: true, Integration: true, Tray: true},
		},
		{
			name:   "--fix skips the prompt and repairs both",
			forced: true, tray: trayhost.RepairEnableOnly,
			want: doctorFixPlan{Integration: true, Tray: true},
		},
		{
			name:   "--fix with nothing tray-side still runs the integration",
			forced: true, tray: trayhost.RepairNone,
			want: doctorFixPlan{Integration: true},
		},
		{
			name:   "--fix wins over --no-interactive and a missing TTY",
			forced: true, noInteractive: true, tty: false, tray: trayhost.RepairEnableOnly,
			want: doctorFixPlan{Integration: true, Tray: true},
		},
		{
			name:          "--no-interactive never prompts",
			hasFail:       true,
			noInteractive: true, tty: true, tray: trayhost.RepairEnableOnly,
			want: doctorFixPlan{},
		},
		{
			name:    "no TTY never prompts (CI)",
			hasFail: true, tty: false, tray: trayhost.RepairEnableOnly,
			want: doctorFixPlan{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := planDoctorFix(tc.hasFail, tc.tray, tc.forced, tc.noInteractive, tc.tty)
			if got != tc.want {
				t.Errorf("planDoctorFix(hasFail=%v, tray=%v, forced=%v, noInteractive=%v, tty=%v)\n got %+v\nwant %+v",
					tc.hasFail, tc.tray, tc.forced, tc.noInteractive, tc.tty, got, tc.want)
			}
		})
	}
}

// TestPlanDoctorFix_TrayNeverAffectsExitCode guards the boundary the fix flow
// must not cross: making the tray warning actionable must not make it fatal.
// runDoctorBody's non-zero exit is driven by hasFail alone, so no tray action —
// offered, taken, or declined — may set Integration when nothing failed, which
// is what would drag an unrelated repair (and its error return) into a run whose
// only finding was a warning.
func TestPlanDoctorFix_TrayNeverAffectsExitCode(t *testing.T) {
	actions := []trayhost.RepairAction{
		trayhost.RepairNone, trayhost.RepairEnableOnly,
		trayhost.RepairInstallThenEnable, trayhost.RepairManual,
	}
	for _, a := range actions {
		for _, tty := range []bool{false, true} {
			for _, noInteractive := range []bool{false, true} {
				// forced is excluded: --fix is an explicit request to run the
				// integration repair, independent of any finding.
				got := planDoctorFix(false, a, false, noInteractive, tty)
				if got.Integration {
					t.Errorf("planDoctorFix(hasFail=false, tray=%v, noInteractive=%v, tty=%v) = %+v — a tray-only run must not trigger the integration repair",
						a, noInteractive, tty, got)
				}
			}
		}
	}
}
