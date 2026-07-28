package trayhost

import "testing"

// gnomeNoHost is the only Status that can plan a repair; every case below
// starts from it and varies one host fact, so the table reads as "what changes
// the verdict".
func gnomeNoHost() RepairFacts {
	return RepairFacts{
		Status:           NoHost,
		Desktop:          DesktopGNOME,
		GnomeShellOnPath: true,
		AptOnPath:        true,
	}
}

func TestPlanRepair(t *testing.T) {
	tests := []struct {
		name string
		goos string
		in   RepairFacts
		want RepairAction
	}{
		// The server guard. This is a product contract, not a record of
		// today's behaviour: apt resolves gnome-shell-extension-appindicator
		// through gnome-shell-ubuntu-extensions on Ubuntu 26.04, which
		// `Depends: gnome-shell`, so planning an install on a host without
		// gnome-shell would install a desktop onto a server (#295).
		{
			name: "no gnome-shell never plans an install",
			goos: "linux",
			in: RepairFacts{
				Status: NoHost, Desktop: DesktopOther,
				GnomeShellOnPath: false, AptOnPath: true,
			},
			want: RepairNone,
		},
		{
			name: "no gnome-shell never plans an install even when the desktop says GNOME",
			goos: "linux",
			in: RepairFacts{
				Status: NoHost, Desktop: DesktopGNOME,
				GnomeShellOnPath: false, AptOnPath: true,
			},
			want: RepairNone,
		},

		// The free repair: the package is there, it just is not on.
		{
			name: "extension present but no host registered is enable-only",
			goos: "linux",
			in: func() RepairFacts {
				f := gnomeNoHost()
				f.ExtensionPresent = true
				return f
			}(),
			want: RepairEnableOnly,
		},

		// The privileged repair, and its degradation.
		{
			name: "GNOME with apt and no extension installs then enables",
			goos: "linux",
			in:   gnomeNoHost(),
			want: RepairInstallThenEnable,
		},
		{
			name: "GNOME without apt degrades to manual",
			goos: "linux",
			in: func() RepairFacts {
				f := gnomeNoHost()
				f.AptOnPath = false
				return f
			}(),
			want: RepairManual,
		},

		// Statuses that are not a missing host.
		{
			name: "host present needs no repair",
			goos: "linux",
			in: func() RepairFacts {
				f := gnomeNoHost()
				f.Status = HostPresent
				return f
			}(),
			want: RepairNone,
		},
		{
			name: "headless is not applicable",
			goos: "linux",
			in: func() RepairFacts {
				f := gnomeNoHost()
				f.Status = NotApplicable
				f.Desktop = DesktopNone
				return f
			}(),
			want: RepairNone,
		},
		{
			name: "MATE cannot render SNI at all so nothing is installable",
			goos: "linux",
			in: func() RepairFacts {
				f := gnomeNoHost()
				f.Status = Unsupported
				f.Desktop = DesktopMATE
				return f
			}(),
			want: RepairNone,
		},

		// Every other OS, with facts that would repair on Linux.
		{name: "darwin has a native tray host", goos: "darwin", in: gnomeNoHost(), want: RepairNone},
		{name: "windows has a native tray host", goos: "windows", in: gnomeNoHost(), want: RepairNone},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := PlanRepair(tc.goos, tc.in); got != tc.want {
				t.Errorf("PlanRepair(%q, %+v) = %v, want %v", tc.goos, tc.in, got, tc.want)
			}
		})
	}
}

// TestPlanRepairNeverInstallsWithoutGnomeShell sweeps the whole fact space
// rather than trusting the two hand-written cases above: no combination of
// inputs may plan an apt install unless gnome-shell is already on the host.
func TestPlanRepairNeverInstallsWithoutGnomeShell(t *testing.T) {
	statuses := []Status{NotApplicable, HostPresent, NoHost, Unsupported}
	desktops := []Desktop{DesktopUnknown, DesktopNone, DesktopGNOME, DesktopKDE, DesktopMATE, DesktopOther}
	bools := []bool{false, true}

	for _, goos := range []string{"linux", "darwin", "windows"} {
		for _, st := range statuses {
			for _, d := range desktops {
				for _, ext := range bools {
					for _, apt := range bools {
						f := RepairFacts{
							Status: st, Desktop: d,
							ExtensionPresent: ext, GnomeShellOnPath: false, AptOnPath: apt,
						}
						if got := PlanRepair(goos, f); got.NeedsPrivilege() {
							t.Fatalf("PlanRepair(%q, %+v) = %v — planned a privileged install on a host with no gnome-shell", goos, f, got)
						}
					}
				}
			}
		}
	}
}

func TestRepairActionFixable(t *testing.T) {
	tests := []struct {
		in            RepairAction
		fixable       bool
		needsPrivsome bool
	}{
		{RepairNone, false, false},
		{RepairEnableOnly, true, false},
		{RepairInstallThenEnable, true, true},
		// Manual is deliberately not "fixable": there is advice to print, but
		// no command of ours to run, so callers must not offer to fix it.
		{RepairManual, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.in.String(), func(t *testing.T) {
			if got := tc.in.Fixable(); got != tc.fixable {
				t.Errorf("%v.Fixable() = %v, want %v", tc.in, got, tc.fixable)
			}
			if got := tc.in.NeedsPrivilege(); got != tc.needsPrivsome {
				t.Errorf("%v.NeedsPrivilege() = %v, want %v", tc.in, got, tc.needsPrivsome)
			}
		})
	}
}
