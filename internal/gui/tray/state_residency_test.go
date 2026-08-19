package tray

import (
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
)

// Model-residency rows in the "Inference" submenu (waired-agent#861).

func residencySnapshot(res *management.ResidencyResponse, resident *bool) Snapshot {
	return Snapshot{
		Health: HealthOnline,
		Identity: &management.IdentityView{
			Enrolled:     true,
			DeviceName:   "dev",
			NetworkName:  "net",
			AccountEmail: "alice@example.com",
		},
		Status: &management.Status{Phase: "active"},
		Inference: &management.InferenceStatus{
			SubsystemState: "ready",
			Runtimes:       map[string]management.RuntimeStatus{"ollama": {ModelResident: resident}},
			Residency:      res,
		},
	}
}

// TestResidencyPresetSlotsMatchPreallocation is the guard against a
// preset past the pre-allocated slots, which would render nowhere and be
// silently unclickable — the failure workerModeSlots' twin test exists
// to prevent.
func TestResidencyPresetSlotsMatchPreallocation(t *testing.T) {
	if got := len(residencyRows(0)); got != residencyPresetSlots {
		t.Fatalf("residencyRows emits %d rows, but %d slots are pre-allocated", got, residencyPresetSlots)
	}
}

// TestResidencyHiddenOnOlderDaemon: a daemon that does not report the
// setting gets no rows at all, rather than rows that would POST to a
// route it does not serve.
func TestResidencyHiddenOnOlderDaemon(t *testing.T) {
	m := Update(residencySnapshot(nil, nil))
	if m.ResidencyHeader != "" || len(m.ResidencyRows) != 0 || m.UnloadModelAction != "" {
		t.Fatalf("residency group rendered without a reporting daemon: header=%q rows=%d unload=%q",
			m.ResidencyHeader, len(m.ResidencyRows), m.UnloadModelAction)
	}
}

// TestResidencyIndefiniteIsSpelledOut pins the default's rendering. The
// value is a zero (owner ruling on waired-agent#861, recorded in
// docs/decisions/20260820/0130-model-residency-is-a-setting.md); shown as
// a duration it would read "0s", i.e. the opposite of what it means.
func TestResidencyIndefiniteIsSpelledOut(t *testing.T) {
	m := Update(residencySnapshot(&management.ResidencyResponse{
		IdleTimeout: "0s", HoldsIndefinitely: true,
	}, nil))
	if m.ResidencyHeader != "Keep model in memory: always" {
		t.Fatalf("ResidencyHeader=%q", m.ResidencyHeader)
	}
	if len(m.ResidencyRows) != residencyPresetSlots {
		t.Fatalf("got %d rows, want %d", len(m.ResidencyRows), residencyPresetSlots)
	}
	if !m.ResidencyRows[0].Selected || m.ResidencyRows[0].Idle != 0 {
		t.Errorf("the indefinite row is not the selected one: %+v", m.ResidencyRows[0])
	}
	for _, r := range m.ResidencyRows[1:] {
		if r.Selected {
			t.Errorf("more than one row selected: %+v", r)
		}
	}
}

func TestResidencyFiniteSelectsMatchingPreset(t *testing.T) {
	m := Update(residencySnapshot(&management.ResidencyResponse{IdleTimeout: "1h0m0s"}, nil))
	if m.ResidencyHeader != "Keep model in memory: 1 hour" {
		t.Fatalf("ResidencyHeader=%q", m.ResidencyHeader)
	}
	selected := 0
	for _, r := range m.ResidencyRows {
		if r.Selected {
			selected++
			if r.Idle != time.Hour {
				t.Errorf("selected row is %v, want 1h", r.Idle)
			}
		}
	}
	if selected != 1 {
		t.Errorf("%d rows selected, want 1", selected)
	}
}

// TestResidencyCustomValueStaysVisible: the tray offers presets, but the
// CLI and the control plane can set any duration. The caption carries
// the value so a setting with no matching row is still readable here
// rather than silently rendering as whichever preset happened to match
// (none — every row would show unselected, which reads as "unset").
func TestResidencyCustomValueStaysVisible(t *testing.T) {
	m := Update(residencySnapshot(&management.ResidencyResponse{IdleTimeout: "37m0s"}, nil))
	if m.ResidencyHeader != "Keep model in memory: 37m0s" {
		t.Fatalf("ResidencyHeader=%q, want the custom value", m.ResidencyHeader)
	}
	for _, r := range m.ResidencyRows {
		if r.Selected {
			t.Errorf("a preset row claims a value that is not set: %+v", r)
		}
	}
}

// TestUnloadRowStatesAreThree: the row is visible whenever the daemon
// can serve it, and it says WHY when it cannot act — a row greyed with
// no explanation is the defect waired-agent#831 was ruled on (owner
// ruling 2026-08-08, ratified waired#1067: a surface must not refuse
// silently).
//
// The unobserved case stays actionable. "Model not loaded" there would
// assert something the daemon did not say, and the handler reports the
// nothing-to-do outcome (waired-agent#879).
func TestUnloadRowStatesAreThree(t *testing.T) {
	res := &management.ResidencyResponse{IdleTimeout: "0s", HoldsIndefinitely: true}
	yes, no := true, false

	for _, tc := range []struct {
		name        string
		resident    *bool
		wantLabel   string
		wantEnabled bool
	}{
		{"loaded", &yes, labelUnloadModel, true},
		{"not loaded", &no, labelModelNotLoaded, false},
		{"not observed", nil, labelUnloadModel, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := Update(residencySnapshot(res, tc.resident))
			if m.UnloadModelAction != tc.wantLabel {
				t.Errorf("UnloadModelAction=%q, want %q", m.UnloadModelAction, tc.wantLabel)
			}
			if m.UnloadModelEnabled != tc.wantEnabled {
				t.Errorf("UnloadModelEnabled=%v, want %v", m.UnloadModelEnabled, tc.wantEnabled)
			}
		})
	}
}

// TestResidencyRowLabels pins the glyph convention shared with the
// worker mode rows: one "●" for the value in force, "○" for the rest.
func TestResidencyRowLabels(t *testing.T) {
	if got := residencyRowLabel(ResidencyRow{Label: "Always", Selected: true}); got != "● Always" {
		t.Errorf("selected label = %q", got)
	}
	if got := residencyRowLabel(ResidencyRow{Label: "8 hours"}); got != "○ 8 hours" {
		t.Errorf("unselected label = %q", got)
	}
}
