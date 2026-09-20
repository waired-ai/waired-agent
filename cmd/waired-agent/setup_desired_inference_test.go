package main

import (
	"context"
	"testing"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// PRODUCT CONTRACT (waired-agent#597; waired#1109/#1110, the waired#835
// §6 pair-contract amendment): the wizard's explicit local-AI answer is
// applied once per ASK. The CP re-sends its instruction on every map
// frame, so anything keyed to the frame would re-disable forever.
//
// An UNTIMED instruction — a control plane that predates
// DesiredInferenceSetAt — is one ask per value, which is the rule that
// stood alone until #1446 and the one this test pins verbatim. The
// stamped cases are below.
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

// PRODUCT CONTRACT (#1446): a NEW stamp on the SAME word is a new ask.
//
// This is what the wire field exists for. A person who turns local
// inference off at the machine leaves the record still naming the
// wizard's earlier "on"; without the stamp the console's "turn local AI
// back on" writes a value the applier reads as already acted on, and the
// button does nothing at all. Reproduced on sv-mag before the fix.
func TestDesiredInference_ANewStampMakesTheSameAnswerANewAsk(t *testing.T) {
	const (
		first  = "2026-09-20T10:30:05.122205419Z"
		second = "2026-09-20T10:47:11.004Z"
	)
	f := &fakeSetupProvider{stateDir: t.TempDir()}
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	ctx := context.Background()

	on := &signer.InferenceState{
		DesiredInference:      signer.DesiredInferenceOn,
		DesiredInferenceSetAt: first,
	}
	r.Apply(ctx, on)
	r.Apply(ctx, on)
	r.Apply(ctx, on)
	if got := f.localInferenceEnableCount(); got != 1 {
		t.Fatalf("enables over three replays of one ask = %d, want 1", got)
	}

	// The person turns it off at the machine. Nothing writes the acted
	// record — `waired inference off` moves the toggle only — so the
	// record still says the wizard's "on", and the browser says it again.
	again := *on
	again.DesiredInferenceSetAt = second
	r.Apply(ctx, &again)
	if got := f.localInferenceEnableCount(); got != 2 {
		t.Fatalf("enables after the operator asked again = %d, want 2 — the button does nothing", got)
	}
	rec, err := state.ReadSetupInference(f.setupStateDir())
	if err != nil || rec.AskedAt != second {
		t.Fatalf("acted record = %+v err=%v, want the second ask's time", rec, err)
	}
}

// PRODUCT CONTRACT (#1446, and the #465 rule it must not break): an
// UNTIMED instruction keeps the per-value rule exactly.
//
// The mutation this forbids is reading an empty stamp as "new": a
// control plane that predates the field re-sends the same untimed answer
// on every frame, so that reading would re-apply a weeks-old answer over
// a person's local flip once per frame — the silent revert the durable
// record exists to prevent. Nothing is migrated either way: the first
// STAMPED ask from an upgraded control plane differs from the unstamped
// record and acts once.
func TestDesiredInference_AnUnstampedAnswerKeepsThePerValueRule(t *testing.T) {
	f := &fakeSetupProvider{stateDir: t.TempDir()}
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	ctx := context.Background()

	untimed := &signer.InferenceState{DesiredInference: signer.DesiredInferenceOff}
	r.Apply(ctx, untimed)
	r.Apply(ctx, untimed)
	r.Apply(ctx, untimed)
	if got := f.localInferenceDisableCount(); got != 1 {
		t.Fatalf("disables over three untimed replays = %d, want 1", got)
	}
	rec, err := state.ReadSetupInference(f.setupStateDir())
	if err != nil || rec.AskedAt != "" {
		t.Fatalf("acted record = %+v err=%v, want no time recorded", rec, err)
	}

	stamped := *untimed
	stamped.DesiredInferenceSetAt = "2026-09-20T10:30:05.122205419Z"
	r.Apply(ctx, &stamped)
	if got := f.localInferenceDisableCount(); got != 2 {
		t.Fatalf("disables after the CP started stamping = %d, want 2 — the first stamped ask acts once", got)
	}
}

// PRODUCT CONTRACT (#1446): the stamp is DURABLE, like the value beside
// it. A restarted daemon fed the same stamped instruction acts on
// nothing — a record that forgot the time would make every restart a new
// ask, which is #465's silent revert wearing the fix's clothes.
func TestDesiredInference_TheStampSurvivesARestart(t *testing.T) {
	const at = "2026-09-20T10:30:05.122205419Z"
	dir := t.TempDir()
	ctx := context.Background()
	on := &signer.InferenceState{
		DesiredInference:      signer.DesiredInferenceOn,
		DesiredInferenceSetAt: at,
	}

	f := &fakeSetupProvider{stateDir: dir}
	newSetupReconciler(f, nil, "dev-1", nil, quietLogger()).Apply(ctx, on)
	if got := f.localInferenceEnableCount(); got != 1 {
		t.Fatalf("enables before the restart = %d, want 1", got)
	}

	g := &fakeSetupProvider{stateDir: dir}
	newSetupReconciler(g, nil, "dev-1", nil, quietLogger()).Apply(ctx, on)
	if got := g.localInferenceEnableCount(); got != 0 {
		t.Fatalf("enables after the restart replay = %d, want 0 — the stamp must survive", got)
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
