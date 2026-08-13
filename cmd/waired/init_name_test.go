package main

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/identity"
)

// PRODUCT CONTRACT (waired-agent#767 + waired-ai/waired#1204). What this
// machine reports at enrollment is its own hostname, never the name
// stored in identity.json.
//
// The control plane writes the reported value to the hostname column and
// leaves the display name alone on a re-enrollment, so re-sending the
// stored copy is how `waired init` used to revert a rename made in the
// web console. `existing` is a parameter precisely so this can be pinned:
// a stored name must not change the answer.
func TestReportedDeviceName(t *testing.T) {
	renamed := &identity.Identity{DeviceID: "dev_1", DeviceName: "renamed-in-the-console"}
	cases := []struct {
		name     string
		flag     string
		existing *identity.Identity
		hostname string
		want     string
	}{
		{
			name:     "fresh enrollment reports the hostname",
			hostname: "workshop-mac",
			want:     "workshop-mac",
		},
		{
			name:     "--device-name wins over the hostname",
			flag:     "chosen-name",
			hostname: "workshop-mac",
			want:     "chosen-name",
		},
		{
			name:     "a stored name does not become the reported one",
			existing: renamed,
			hostname: "workshop-mac",
			want:     "workshop-mac",
		},
		{
			name:     "--device-name still wins when one is stored",
			flag:     "chosen-name",
			existing: renamed,
			hostname: "workshop-mac",
			want:     "chosen-name",
		},
		{
			// A machine that cannot name itself reports nothing and lets
			// the control plane apply its own fallback.
			name:     "no hostname and nothing stored reports nothing",
			existing: renamed,
			want:     "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reportedDeviceName(tc.flag, tc.existing, tc.hostname); got != tc.want {
				t.Errorf("reportedDeviceName() = %q, want %q", got, tc.want)
			}
		})
	}
}
