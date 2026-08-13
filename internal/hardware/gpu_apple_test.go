package hardware

import (
	"errors"
	"strings"
	"testing"
)

// TestAppleGPUModel covers the three outcomes of one system_profiler
// run. It is untagged on purpose: detectApple is darwin-only and had no
// test on any platform, so the decision it makes now lives in a pure
// function every runner can reach (CLAUDE.md §Test discipline — put the
// seam below the behaviour under test).
//
// PRODUCT CONTRACT — waired-agent#35 and
// docs/decisions/20260728/0250-gpu-presence-from-driver-not-path.md:
// a device that is present but could not be described is UNKNOWN, and
// unknown is an error, never a silently complete answer.
func TestAppleGPUModel(t *testing.T) {
	const named = `{"SPDisplaysDataType":[{"sppci_model":"Apple M3 Max"}]}`
	const olderKey = `{"SPDisplaysDataType":[{"_name":"Apple M1"}]}`
	const noGPU = `{"SPDisplaysDataType":[]}`

	for _, tc := range []struct {
		name      string
		out       string
		probeErr  error
		wantModel string
		wantWarn  bool
	}{
		{"a named GPU is reported as itself", named, nil, "Apple M3 Max", false},
		{"the older key still names it", olderKey, nil, "Apple M1", false},
		{
			// The case that used to return no error: the tool ran, said
			// nothing useful, and the host reported a fully-formed device.
			"an answer that names no GPU is unknown, not complete",
			noGPU, nil, appleGPUFallbackModel, true,
		},
		{
			"a failed probe is unknown, not complete",
			"", errors.New("exec: system_profiler: not found"), appleGPUFallbackModel, true,
		},
		{
			"unparseable output is unknown, not complete",
			"not json at all", nil, appleGPUFallbackModel, true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model, warn := appleGPUModel([]byte(tc.out), tc.probeErr)
			if model != tc.wantModel {
				t.Errorf("model = %q, want %q", model, tc.wantModel)
			}
			if tc.wantWarn && warn == nil {
				t.Fatal("no warning: a GPU this host has but could not describe was reported as " +
					"fully known, which is the ABSENT/UNKNOWN conflation VendorDetector forbids")
			}
			if !tc.wantWarn && warn != nil {
				t.Fatalf("unexpected warning for a readable answer: %v", warn)
			}
			if warn != nil && !strings.Contains(warn.Error(), "apple") {
				t.Errorf("warning does not name the vendor it came from: %v", warn)
			}
		})
	}
}

// TestAppleGPUModel_WarningStillCarriesADevice pins the half that makes
// the warning safe to add: the caller returns the device ALONGSIDE the
// error, and composeDetectors keeps devices while joining errors. If
// this became "no device", an Apple Silicon host whose system_profiler
// hiccuped would profile as CPU-only — the #67 failure, on a different
// vendor.
func TestAppleGPUModel_WarningStillCarriesADevice(t *testing.T) {
	model, warn := appleGPUModel(nil, errors.New("boom"))
	if warn == nil {
		t.Fatal("want a warning")
	}
	if model == "" {
		t.Error("model is empty; the device must still be reported, only unnamed")
	}
}
