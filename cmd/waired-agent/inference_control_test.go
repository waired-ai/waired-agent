package main

import (
	"context"
	"testing"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

func TestInferenceController_TransitionsAndPersists(t *testing.T) {
	dir := t.TempDir()
	ic := newInferenceController(dir, state.InferenceEnabled, nil)
	if ic.IsDisabled() {
		t.Fatal("starts enabled (false)")
	}

	if err := ic.Disable(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !ic.IsDisabled() {
		t.Error("Disable did not flip flag")
	}
	got, err := state.ReadDesiredInferenceState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != state.InferenceDisabled {
		t.Errorf("desired-inference after Disable = %q, want %q", got, state.InferenceDisabled)
	}

	if err := ic.Enable(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ic.IsDisabled() {
		t.Error("Enable did not flip flag")
	}
	got, _ = state.ReadDesiredInferenceState(dir)
	if got != state.InferenceEnabled {
		t.Errorf("desired-inference after Enable = %q, want %q", got, state.InferenceEnabled)
	}
}

func TestInferenceController_StartsFromInitial(t *testing.T) {
	dir := t.TempDir()
	if err := state.WriteDesiredInferenceState(dir, state.InferenceDisabled); err != nil {
		t.Fatal(err)
	}
	ic := newInferenceController(dir, state.InferenceDisabled, nil)
	if !ic.IsDisabled() {
		t.Fatal("should start disabled when initial=Disabled")
	}
	cur, desired := ic.State()
	if cur != state.InferenceDisabled || desired != state.InferenceDisabled {
		t.Errorf("State() = (%q, %q), want (disabled, disabled)", cur, desired)
	}
}

func TestInferenceController_StateReportsDesiredFromDisk(t *testing.T) {
	dir := t.TempDir()
	ic := newInferenceController(dir, state.InferenceEnabled, nil)
	// Operator wrote a disable but daemon hasn't applied it yet (e.g.,
	// edited the file by hand). State() should surface the disk truth
	// in the desired field while leaving current unchanged.
	if err := state.WriteDesiredInferenceState(dir, state.InferenceDisabled); err != nil {
		t.Fatal(err)
	}
	cur, desired := ic.State()
	if cur != state.InferenceEnabled {
		t.Errorf("current = %q, want enabled (in-memory hasn't flipped yet)", cur)
	}
	if desired != state.InferenceDisabled {
		t.Errorf("desired = %q, want disabled", desired)
	}
}
