package hostspeed

import (
	"testing"
	"time"
)

// The whole reason this package exists. Product contract
// (waired-agent#579): the measurement stands in FRONT of the download, so
// its window has to leave the download a real share of the wait — and the
// two numbers live in two `package main`s that cannot see each other, which
// is how 16 minutes ended up inside 10.
//
// Failing message carries the run, because the next person to move either
// number needs to know what moving it broke last time.
func TestInstallWindow_FitsInsideTheWaitItStandsIn(t *testing.T) {
	if defaultInstallWindow >= ModelWait {
		t.Fatalf("InstallWindow %v is not inside ModelWait %v — the measurement would still be "+
			"running when init stops waiting, which is waired-agent#579 exactly (run 31316731884: "+
			"pre-pull released 14:28:49, model dispatched 14:45:11, download 21.9 s)",
			defaultInstallWindow, ModelWait)
	}
	// Inside is not enough: a measurement allowed 9 of the 10 minutes
	// leaves a 20-45 GB download one. Half is the ruling, and the half is
	// what the anchor below is checked against.
	if 2*defaultInstallWindow > ModelWait {
		t.Fatalf("InstallWindow %v is more than half of ModelWait %v — the download gets the rest, "+
			"and the rest has to be the larger share", defaultInstallWindow, ModelWait)
	}
}

// The anchor that makes InstallWindow a size rather than a guess: the
// reference host the 45 s threshold was derived from finishes its whole
// measurement inside it, including the one run of that repeat that shared
// the machine with another job (+21 %).
//
// Product contract (waired-agent#579): a host at or above the anchor keeps
// its full three samples on the install path. If this stops holding, the
// install path starts publishing one-sample figures for healthy hosts and
// the spread that catches a contended reading is gone.
//
// Figures: proto/hostfit/host_cutoff.go, the HostCutoffTurnBudgetSeconds
// doc (66.6 s median of 65.3 / 65.9 / 66.6 / 80.4 / 67.5) and
// hostCutoffCalibrationLines (~4 s of calibration on that host).
func TestInstallWindow_HoldsTheReferenceHostsWholeMeasurement(t *testing.T) {
	const (
		calibration = 4 * time.Second
		samples     = 3
	)
	for _, tc := range []struct {
		name string
		per  time.Duration
	}{
		{"reference host, median run", 66600 * time.Millisecond},
		{"reference host, the contended run", 80400 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			whole := calibration + samples*tc.per
			if whole > defaultInstallWindow {
				t.Fatalf("%v for calibration + %d x %v does not fit in InstallWindow %v — a host at "+
					"the anchor the budget was derived from would lose samples on the install path",
					whole, samples, tc.per, defaultInstallWindow)
			}
		})
	}
}
