package main

import (
	"context"
	"testing"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

func TestPauseManager_TransitionsAndPersists(t *testing.T) {
	dir := t.TempDir()
	w := state.NewWriter(dir, state.State{
		Phase:        state.PhaseActive,
		GatewayURL:   "http://127.0.0.1:9473",
		GatewayToken: "tok",
	})
	if err := w.Set(state.State{Phase: state.PhaseActive, GatewayURL: "http://127.0.0.1:9473", GatewayToken: "tok"}); err != nil {
		t.Fatal(err)
	}

	pm := newPauseManager(dir, w, state.PhaseActive, nil)
	if pm.IsPaused() {
		t.Fatal("starts active")
	}

	if err := pm.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !pm.IsPaused() {
		t.Error("Pause did not flip flag")
	}
	got, err := state.ReadDesiredPhase(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != state.PhasePaused {
		t.Errorf("desired-phase after Pause = %q, want %q", got, state.PhasePaused)
	}
	st, _ := state.Read(dir)
	if st.Phase != state.PhasePaused {
		t.Errorf("state file phase after Pause = %q, want %q", st.Phase, state.PhasePaused)
	}

	if err := pm.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pm.IsPaused() {
		t.Error("Resume did not flip flag")
	}
	got, _ = state.ReadDesiredPhase(dir)
	if got != state.PhaseActive {
		t.Errorf("desired-phase after Resume = %q, want %q", got, state.PhaseActive)
	}
}

func TestPauseManager_StartsFromDesiredPhase(t *testing.T) {
	dir := t.TempDir()
	if err := state.WriteDesiredPhase(dir, state.PhasePaused); err != nil {
		t.Fatal(err)
	}
	pm := newPauseManager(dir, nil, state.PhasePaused, nil)
	if !pm.IsPaused() {
		t.Fatal("should start paused when initial=Paused")
	}
	cur, desired := pm.Phase()
	if cur != state.PhasePaused || desired != state.PhasePaused {
		t.Errorf("Phase() = (%q, %q), want (paused, paused)", cur, desired)
	}
}

func TestPauseManager_PhaseReportsDesiredFromDisk(t *testing.T) {
	dir := t.TempDir()
	pm := newPauseManager(dir, nil, state.PhaseActive, nil)
	// Operator wrote a pause but daemon hasn't applied it yet.
	if err := state.WriteDesiredPhase(dir, state.PhasePaused); err != nil {
		t.Fatal(err)
	}
	cur, desired := pm.Phase()
	if cur != state.PhaseActive {
		t.Errorf("current = %q, want active (in-memory hasn't flipped yet)", cur)
	}
	if desired != state.PhasePaused {
		t.Errorf("desired = %q, want paused", desired)
	}
}

// TestPauseManager_ForcePhaseOverride covers the dev-only --dev-force-phase
// path: when forcePhase is set, Phase().current returns that string verbatim
// regardless of the in-memory paused flag. Used by docs/runbooks/tray-manual-verify.md
// to drive the tray UI through Connecting / Error states the daemon never
// holds long enough to capture.
func TestPauseManager_ForcePhaseOverride(t *testing.T) {
	dir := t.TempDir()
	for _, force := range []state.Phase{"starting", "stopping", "error"} {
		t.Run(string(force), func(t *testing.T) {
			pm := newPauseManager(dir, nil, state.PhaseActive, nil)
			pm.forcePhase = force
			cur, _ := pm.Phase()
			if cur != force {
				t.Errorf("Phase().current = %q, want %q", cur, force)
			}
			// Override must not lie about the persisted operator intent.
			if err := state.WriteDesiredPhase(dir, state.PhaseActive); err != nil {
				t.Fatal(err)
			}
			_, desired := pm.Phase()
			if desired != state.PhaseActive {
				t.Errorf("Phase().desired = %q, want active (override does not touch desired)", desired)
			}
		})
	}
}
