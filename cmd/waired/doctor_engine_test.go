package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/setup"
)

const testBundleMarker = "/Applications/Ollama.app/.waired-managed.json"

// TestPlanEngineRepair is a PRODUCT CONTRACT test: doctor only ever deletes a
// file waired itself wrote, and only on the one OS that has a bundle to break.
func TestPlanEngineRepair(t *testing.T) {
	for _, tc := range []struct {
		name  string
		goos  string
		facts engineDoctorFacts
		want  engineRepairAction
	}{
		{
			name: "darwin with the seal-breaking marker", goos: "darwin",
			facts: engineDoctorFacts{Installed: true, LegacyBundleMarkerPath: testBundleMarker},
			want:  engineRepairBundleMarker,
		},
		{
			name: "darwin, healthy install", goos: "darwin",
			facts: engineDoctorFacts{Installed: true},
			want:  engineRepairNone,
		},
		// #330: broken, but not by anything waired wrote. Doctor reports it
		// and stops — deleting a file we did not put there, guessed from a
		// codesign complaint, would be worse than the disease.
		{
			name: "darwin, invalid signature we did not cause", goos: "darwin",
			facts: engineDoctorFacts{Installed: true, SignatureProblem: "code object is not signed at all"},
			want:  engineRepairReinstallNeeded,
		},
		{
			// Our own marker takes precedence: the cheap fix is available and
			// is the actual cause.
			name: "darwin, marker and an invalid signature", goos: "darwin",
			facts: engineDoctorFacts{
				Installed: true, LegacyBundleMarkerPath: testBundleMarker,
				SignatureProblem: "unsealed contents present in the bundle root",
			},
			want: engineRepairBundleMarker,
		},
		{
			name: "darwin, no engine at all", goos: "darwin",
			facts: engineDoctorFacts{LegacyBundleMarkerPath: testBundleMarker},
			want:  engineRepairNone,
		},
		// The other two OSes install into plain directories: there is no
		// signature seal to break and nothing here to repair, ever.
		{
			name: "linux never repairs", goos: "linux",
			facts: engineDoctorFacts{Installed: true, LegacyBundleMarkerPath: testBundleMarker,
				SignatureProblem: "whatever"},
			want: engineRepairNone,
		},
		{
			name: "windows never repairs", goos: "windows",
			facts: engineDoctorFacts{Installed: true, LegacyBundleMarkerPath: testBundleMarker,
				SignatureProblem: "whatever"},
			want: engineRepairNone,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := planEngineRepair(tc.goos, tc.facts); got != tc.want {
				t.Errorf("planEngineRepair(%q, %+v) = %v, want %v", tc.goos, tc.facts, got, tc.want)
			}
		})
	}
}

// TestEngineFindingFrom pins the severity and the silence.
//
// Fail, not Warn: unlike the tray, a bundle in this state cannot be launched at
// all, so `waired doctor` must exit non-zero. And a healthy host — or any
// non-macOS host — gets no row at all, the same empty-Subject convention
// trayFindingFromResult uses for NotApplicable.
func TestEngineFindingFrom(t *testing.T) {
	broken := engineDoctorFacts{Installed: true, LegacyBundleMarkerPath: testBundleMarker}

	f := engineFindingFrom("darwin", broken)
	if f.Status != integration.StatusFail {
		t.Errorf("Status = %v, want StatusFail", f.Status)
	}
	if f.Subject == "" {
		t.Error("Subject is empty; the finding would be skipped")
	}
	if !strings.Contains(f.Detail, testBundleMarker) {
		t.Errorf("Detail %q does not name the offending file", f.Detail)
	}
	if !strings.Contains(f.Detail, "doctor --fix") {
		t.Errorf("Detail %q does not tell the operator how to fix it", f.Detail)
	}

	// Broken by something else: still a Fail, but it names the reinstall
	// rather than offering a --fix that cannot help.
	reinstall := engineFindingFrom("darwin", engineDoctorFacts{
		Installed: true, SignatureProblem: "code object is not signed at all"})
	if reinstall.Status != integration.StatusFail {
		t.Errorf("Status = %v, want StatusFail", reinstall.Status)
	}
	if !strings.Contains(reinstall.Detail, "runtimes install ollama") {
		t.Errorf("Detail %q does not point at the reinstall", reinstall.Detail)
	}
	if strings.Contains(reinstall.Detail, "doctor --fix") {
		t.Errorf("Detail %q offers --fix, which cannot repair this case", reinstall.Detail)
	}

	for _, tc := range []struct {
		name  string
		goos  string
		facts engineDoctorFacts
	}{
		{"healthy darwin", "darwin", engineDoctorFacts{Installed: true}},
		{"linux", "linux", broken},
		{"windows", "windows", broken},
	} {
		t.Run(tc.name+" says nothing", func(t *testing.T) {
			if got := engineFindingFrom(tc.goos, tc.facts); got.Subject != "" {
				t.Errorf("finding = %+v, want the zero value (no row)", got)
			}
		})
	}
}

// TestRepairEngineBundle_NoopWhenNothingToFix guards the hermetic contract the
// doctor tests rely on: a zero engineDoctor must not touch the host.
func TestRepairEngineBundle_NoopWhenNothingToFix(t *testing.T) {
	called := false
	prev := doctorRepairDarwinBundle
	doctorRepairDarwinBundle = func(string, string, setup.OllamaDetection) (bool, error) {
		called = true
		return false, nil
	}
	t.Cleanup(func() { doctorRepairDarwinBundle = prev })

	var out bytes.Buffer
	if err := repairEngineBundle(engineDoctor{}, "/state", &out); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("the repair ran for a zero engineDoctor")
	}
	if out.Len() != 0 {
		t.Errorf("wrote %q, want silence", out.String())
	}
}

func TestRepairEngineBundle_ReportsOutcome(t *testing.T) {
	det := setup.OllamaDetection{Installed: true, LegacyBundleMarkerPath: testBundleMarker}
	e := engineDoctor{Repair: engineRepairBundleMarker, Detection: det}

	t.Run("success is announced", func(t *testing.T) {
		var gotGOOS, gotStateDir string
		var gotDet setup.OllamaDetection
		prev := doctorRepairDarwinBundle
		doctorRepairDarwinBundle = func(goos, stateDir string, d setup.OllamaDetection) (bool, error) {
			gotGOOS, gotStateDir, gotDet = goos, stateDir, d
			return true, nil
		}
		t.Cleanup(func() { doctorRepairDarwinBundle = prev })

		var out bytes.Buffer
		if err := repairEngineBundle(e, "/state", &out); err != nil {
			t.Fatal(err)
		}
		// The repair is only correct if it is handed the detection the
		// finding was built from, and the caller's state dir.
		if gotStateDir != "/state" || gotDet.LegacyBundleMarkerPath != testBundleMarker || gotGOOS == "" {
			t.Errorf("repair called with (%q, %q, %+v)", gotGOOS, gotStateDir, gotDet)
		}
		if !strings.Contains(out.String(), "signature is valid again") {
			t.Errorf("output = %q, want the success line", out.String())
		}
	})

	t.Run("failure propagates", func(t *testing.T) {
		sentinel := errors.New("permission denied")
		prev := doctorRepairDarwinBundle
		doctorRepairDarwinBundle = func(string, string, setup.OllamaDetection) (bool, error) {
			return false, sentinel
		}
		t.Cleanup(func() { doctorRepairDarwinBundle = prev })

		var out bytes.Buffer
		if err := repairEngineBundle(e, "/state", &out); !errors.Is(err, sentinel) {
			t.Fatalf("err = %v, want %v", err, sentinel)
		}
	})
}

// TestEngineRepairActionFixable pins doctor's contract boundary: it repairs
// what is cheap and local, and never turns `--fix` into a 560 MB download.
func TestEngineRepairActionFixable(t *testing.T) {
	for action, want := range map[engineRepairAction]bool{
		engineRepairNone:            false,
		engineRepairBundleMarker:    true,
		engineRepairReinstallNeeded: false,
	} {
		if got := action.Fixable(); got != want {
			t.Errorf("engineRepairAction(%d).Fixable() = %v, want %v", action, got, want)
		}
	}
}
