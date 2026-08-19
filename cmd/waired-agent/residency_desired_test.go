package main

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// fakeResidencySetter records the real argument. A fake that dropped it
// would make the "the wrong value reached the controller" case
// unwritable.
type fakeResidencySetter struct {
	calls []time.Duration
	err   error
}

func (f *fakeResidencySetter) SetResidency(_ context.Context, idle time.Duration) (time.Duration, error) {
	f.calls = append(f.calls, idle)
	return idle, f.err
}

func newTestDesiredResidency(t *testing.T, f *fakeResidencySetter) (*desiredResidency, string) {
	t.Helper()
	dir := t.TempDir()
	d := newDesiredResidency(f, dir, nil)
	d.now = func() time.Time { return time.Date(2026, 8, 20, 3, 0, 0, 0, time.UTC) }
	return d, dir
}

// TestDesiredResidencyAppliesOncePerValue is the contract this type
// exists for (settled field table on waired-agent#861): the control
// plane re-sends its instruction on every map frame, and the same
// setting is locally changeable from `waired inference residency` and
// the app, so applying per frame would revert a local change within the
// poll interval.
func TestDesiredResidencyAppliesOncePerValue(t *testing.T) {
	f := &fakeResidencySetter{}
	d, dir := newTestDesiredResidency(t, f)

	for i := 0; i < 5; i++ {
		d.Apply(context.Background(), "30m0s")
	}
	if len(f.calls) != 1 || f.calls[0] != 30*time.Minute {
		t.Fatalf("controller calls = %v, want exactly one 30m", f.calls)
	}

	// A NEW value acts.
	d.Apply(context.Background(), "8h0m0s")
	if len(f.calls) != 2 || f.calls[1] != 8*time.Hour {
		t.Fatalf("controller calls = %v, want a second call of 8h", f.calls)
	}

	rec, err := state.ReadAppliedResidency(dir)
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	if rec.Value != "8h0m0s" {
		t.Errorf("recorded %q, want 8h0m0s", rec.Value)
	}
}

// TestDesiredResidencySurvivesRestart: the durable record is what stops
// a daemon restart from re-applying a weeks-old instruction over a local
// change made since.
func TestDesiredResidencySurvivesRestart(t *testing.T) {
	f := &fakeResidencySetter{}
	d, dir := newTestDesiredResidency(t, f)
	d.Apply(context.Background(), "1h0m0s")
	if len(f.calls) != 1 {
		t.Fatalf("first run calls = %v, want one", f.calls)
	}

	// A fresh daemon, same state dir, same standing instruction.
	f2 := &fakeResidencySetter{}
	d2 := newDesiredResidency(f2, dir, nil)
	d2.Apply(context.Background(), "1h0m0s")
	if len(f2.calls) != 0 {
		t.Fatalf("a restart re-applied a standing instruction: %v", f2.calls)
	}

	// But a changed instruction still gets through.
	d2.Apply(context.Background(), "15m0s")
	if len(f2.calls) != 1 || f2.calls[0] != 15*time.Minute {
		t.Fatalf("after restart, a NEW value did not act: %v", f2.calls)
	}
}

// TestDesiredResidencyEmptyIsNoInstruction pins the distinction the wire
// contract rests on (field table, waired-agent#861): the empty string is
// the absence of an instruction, so the device keeps its local setting.
// Reading it as a zero would pin every device to the default and make
// clearing the value in the control plane impossible.
func TestDesiredResidencyEmptyIsNoInstruction(t *testing.T) {
	f := &fakeResidencySetter{}
	d, dir := newTestDesiredResidency(t, f)

	d.Apply(context.Background(), "")
	if len(f.calls) != 0 {
		t.Fatalf("an empty instruction acted: %v", f.calls)
	}
	if _, err := os.Stat(state.AppliedResidencyPath(dir)); !os.IsNotExist(err) {
		t.Errorf("an empty instruction wrote a record")
	}
}

// TestDesiredResidencyZeroIsAnInstruction is the other half of the same
// distinction: "0s" means hold the model indefinitely, and it must act.
func TestDesiredResidencyZeroIsAnInstruction(t *testing.T) {
	f := &fakeResidencySetter{}
	d, dir := newTestDesiredResidency(t, f)

	d.Apply(context.Background(), "0s")
	if len(f.calls) != 1 || f.calls[0] != 0 {
		t.Fatalf("controller calls = %v, want one 0s", f.calls)
	}
	rec, err := state.ReadAppliedResidency(dir)
	if err != nil || rec.Value != "0s" {
		t.Errorf("record = %+v (err %v), want 0s", rec, err)
	}
}

// TestDesiredResidencyCanonicalisesTheSpelling: "0", "0s" and a negative
// duration are all the same instruction, so a control plane that changes
// how it writes the value must not re-apply it over a local change.
func TestDesiredResidencyCanonicalisesTheSpelling(t *testing.T) {
	f := &fakeResidencySetter{}
	d, _ := newTestDesiredResidency(t, f)

	for _, spelling := range []string{"0s", "0", "never", "-1h0m0s"} {
		d.Apply(context.Background(), spelling)
	}
	if len(f.calls) != 1 {
		t.Fatalf("controller calls = %v, want one — every spelling is the same instruction", f.calls)
	}
}

// TestDesiredResidencyUnknownStaysPending: a value this build cannot
// read is left un-acted AND unrecorded, so a newer control plane
// vocabulary is still waiting after an agent upgrade rather than
// consumed. This is the treatment applyDesiredInference gives an unknown
// value (waired-agent#597).
func TestDesiredResidencyUnknownStaysPending(t *testing.T) {
	f := &fakeResidencySetter{}
	d, dir := newTestDesiredResidency(t, f)

	d.Apply(context.Background(), "until-tuesday")
	if len(f.calls) != 0 {
		t.Fatalf("an unreadable instruction acted: %v", f.calls)
	}
	if _, err := os.Stat(state.AppliedResidencyPath(dir)); !os.IsNotExist(err) {
		t.Errorf("an unreadable instruction was recorded as acted on")
	}
}

// TestDesiredResidencyRecordsDespiteAControllerError: the controller
// reports live and persistence failures distinctly and applies what it
// can, so a failure here must not un-record the instruction — re-trying
// on the next frame would fight a local change for as long as the engine
// stayed unwell.
func TestDesiredResidencyRecordsDespiteAControllerError(t *testing.T) {
	f := &fakeResidencySetter{err: errors.New("engine did not answer")}
	d, dir := newTestDesiredResidency(t, f)

	d.Apply(context.Background(), "45m0s")
	d.Apply(context.Background(), "45m0s")
	if len(f.calls) != 1 {
		t.Fatalf("controller calls = %v, want one despite the error", f.calls)
	}
	rec, err := state.ReadAppliedResidency(dir)
	if err != nil || rec.Value != "45m0s" {
		t.Errorf("record = %+v (err %v), want 45m0s", rec, err)
	}
}

// TestDesiredResidencyDeclaresItsCapability guards the pairing the wire
// depends on: the control plane emits desired_idle_timeout only to an
// agent that declared residency-v1, so declaring it without a reader
// would have the CP send an instruction nothing acts on — and reading it
// without declaring would mean never receiving one.
func TestDesiredResidencyDeclaresItsCapability(t *testing.T) {
	if len(residencyCapabilities) != 1 || residencyCapabilities[0] != "residency-v1" {
		t.Fatalf("residencyCapabilities = %v, want [residency-v1]", residencyCapabilities)
	}
}
