package main

import (
	"context"
	"testing"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// PRODUCT CONTRACT (waired-agent#597; waired#1109/#1110, the waired#835
// §6 pair-contract amendment): the wizard's explicit local-AI answer is
// applied once per VALUE. The CP re-sends its instruction on every map
// frame, so anything keyed to the frame would re-disable forever.
func TestDesiredInference_OffAppliesOncePerValue(t *testing.T) {
	f := &fakeSetupProvider{stateDir: t.TempDir()}
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	ctx := context.Background()

	off := &signer.InferenceState{DesiredInference: signer.DesiredInferenceOff}
	r.Apply(ctx, off)
	r.Apply(ctx, off)
	r.Apply(ctx, off)

	if got := f.localInferenceDisableCount(); got != 1 {
		t.Fatalf("disables = %d, want exactly 1 — once per value, not per frame", got)
	}
	rec, err := state.ReadSetupInference(f.setupStateDir())
	if err != nil || rec.Value != signer.DesiredInferenceOff {
		t.Fatalf("acted record = %+v err=%v, want a persisted off", rec, err)
	}
}

// PRODUCT CONTRACT (#597, the #465 rule that an opt-in silently reverted
// on the next boot is no opt-in at all): the acted marker is DURABLE. A
// restarted daemon fed the same standing instruction acts on nothing —
// without this, every restart would re-apply a weeks-old wizard answer
// over a person's later local `waired inference on|off`.
func TestDesiredInference_PersistedRecordStopsARestartReplay(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	f := &fakeSetupProvider{stateDir: dir}
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	r.Apply(ctx, &signer.InferenceState{DesiredInference: signer.DesiredInferenceOff})
	if got := f.localInferenceDisableCount(); got != 1 {
		t.Fatalf("disables before restart = %d, want 1", got)
	}

	// The daemon restarts; the CP replays the standing instruction.
	g := &fakeSetupProvider{stateDir: dir}
	r2 := newSetupReconciler(g, nil, "dev-1", nil, quietLogger())
	r2.Apply(ctx, &signer.InferenceState{DesiredInference: signer.DesiredInferenceOff})
	if got := g.localInferenceDisableCount(); got != 0 {
		t.Fatalf("disables after the restart replay = %d, want 0 — the record must survive", got)
	}
}

// A value flip acts in both directions: on re-enables through the same
// door the serve-ask uses, and a later off acts again (#597).
func TestDesiredInference_ValueFlipsActEachTime(t *testing.T) {
	f := &fakeSetupProvider{stateDir: t.TempDir()}
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	ctx := context.Background()

	r.Apply(ctx, &signer.InferenceState{DesiredInference: signer.DesiredInferenceOff})
	r.Apply(ctx, &signer.InferenceState{DesiredInference: signer.DesiredInferenceOn})
	r.Apply(ctx, &signer.InferenceState{DesiredInference: signer.DesiredInferenceOff})

	if got := f.localInferenceDisableCount(); got != 2 {
		t.Fatalf("disables = %d, want 2 (off, then off again after on)", got)
	}
	if got := f.localInferenceEnableCount(); got != 1 {
		t.Fatalf("enables = %d, want 1 (the on between them)", got)
	}
	rec, err := state.ReadSetupInference(f.setupStateDir())
	if err != nil || rec.Value != signer.DesiredInferenceOff {
		t.Fatalf("acted record = %+v err=%v, want the last value persisted", rec, err)
	}
}

// A vocabulary this build does not know is left PENDING — un-acted and
// unrecorded — so a newer CP's instruction is still there for the build
// that understands it, and a later known value still applies (#597).
func TestDesiredInference_UnknownValueIsLeftPending(t *testing.T) {
	f := &fakeSetupProvider{stateDir: t.TempDir()}
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	ctx := context.Background()

	r.Apply(ctx, &signer.InferenceState{DesiredInference: "standby"})
	if f.localInferenceDisableCount() != 0 || f.localInferenceEnableCount() != 0 {
		t.Fatalf("an unknown value must act on nothing (disables=%d enables=%d)",
			f.localInferenceDisableCount(), f.localInferenceEnableCount())
	}
	if rec, err := state.ReadSetupInference(f.setupStateDir()); err != nil || rec.Value != "" {
		t.Fatalf("an unknown value must not be recorded as acted: %+v err=%v", rec, err)
	}

	r.Apply(ctx, &signer.InferenceState{DesiredInference: signer.DesiredInferenceOff})
	if got := f.localInferenceDisableCount(); got != 1 {
		t.Fatalf("a known value after an unknown one must still apply, disables = %d", got)
	}
}

// PRODUCT CONTRACT (#597): an inference-only change beside a standing
// engine must not fire the serve-ask enable — a wizard writing "off"
// would otherwise be answered with an enable a breath before the off
// applies.
func TestDesiredInference_OffBesideAStandingEngineDoesNotAskToServe(t *testing.T) {
	f := &fakeSetupProvider{stateDir: t.TempDir()}
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	ctx := context.Background()

	standing := &signer.InferenceState{
		DesiredEngine:  signer.InferenceTypeOllama,
		DesiredModelID: "qwen3-8b-instruct",
	}
	r.Apply(ctx, standing)
	base := f.localInferenceEnableCount()

	withOff := *standing
	withOff.DesiredInference = signer.DesiredInferenceOff
	r.Apply(ctx, &withOff)

	if got := f.localInferenceEnableCount(); got != base {
		t.Fatalf("enables went %d → %d — an inference-only change fired the serve-ask", base, got)
	}
	if got := f.localInferenceDisableCount(); got != 1 {
		t.Fatalf("disables = %d, want the off applied once", got)
	}
}

// PRODUCT CONTRACT (#597; waired#1109): the acted-on off is echoed as a
// done step so the CP's completion derivation can count an off-host as
// COMPLETE with no engine or model rows at all.
func TestDesiredInference_SnapshotEchoesTheActedOff(t *testing.T) {
	f := &fakeSetupProvider{stateDir: t.TempDir()}
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	ctx := context.Background()

	r.Apply(ctx, &signer.InferenceState{DesiredInference: signer.DesiredInferenceOff})
	p := r.snapshot(ctx)
	if p == nil {
		t.Fatal("an off-host with desired state must still push a snapshot")
	}
	var found bool
	for _, s := range p.Steps {
		if s.ID == setupStepInferenceOff {
			found = true
			if s.Status != signer.SetupStatusDone {
				t.Fatalf("inference_off status = %q, want done", s.Status)
			}
		}
		if s.ID == setupStepEngineInstall || s.ID == setupStepModelPull {
			t.Fatalf("an off-host must not report an %s row", s.ID)
		}
	}
	if !found {
		t.Fatalf("steps = %+v, want the inference_off echo", p.Steps)
	}
}
