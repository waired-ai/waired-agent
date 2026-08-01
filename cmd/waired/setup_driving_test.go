package main

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
)

// TestSetupDriving is the #308 rule in one place: an instruction on the
// device's map entry only means "a browser is driving this host" while
// the daemon has watched it arrive. The control plane never clears
// desired_engine / desired_model_id, so presence alone is a fact about
// the past.
func TestSetupDriving(t *testing.T) {
	cases := []struct {
		name string
		st   management.SetupStateResponse
		want bool
	}{
		{"a wizard writing now",
			management.SetupStateResponse{Active: true}, true},
		{"leftovers from an earlier run",
			management.SetupStateResponse{Active: true, DesiredStale: true}, false},
		{"no instruction at all",
			management.SetupStateResponse{}, false},
		// Unreachable from the daemon (it only marks a stale instruction
		// it actually holds), pinned so the predicate cannot be read as
		// "stale wins over absent".
		{"stale without an instruction",
			management.SetupStateResponse{DesiredStale: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := setupDriving(tc.st); got != tc.want {
				t.Errorf("setupDriving(%+v) = %v, want %v", tc.st, got, tc.want)
			}
		})
	}
}

// A daemon too old to answer the question sends no field at all, which
// decodes to false. That must keep meaning "driving", or upgrading the
// CLI alone would strand every browser-driven setup in terminal mode.
func TestSetupDrivingOlderDaemonStillDrives(t *testing.T) {
	if !setupDriving(management.SetupStateResponse{Active: true}) {
		t.Error("an older daemon's answer stopped counting as browser-driven")
	}
}
