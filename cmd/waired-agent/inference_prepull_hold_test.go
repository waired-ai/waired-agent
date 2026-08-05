package main

import (
	"context"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// prePullHoldProvider is the host #379 is about: the engine is already
// installed at boot, no operator preference exists yet, and the hardware
// auto-select has named a bundled model. Nothing has been told to it by a
// control plane, which is exactly the state bootstrapAfterEngineStart runs
// in about a second after the process starts.
//
// The graces are shortened so a test never waits out real setup timings;
// the branch they gate is the same one production takes.
func prePullHoldProvider(t *testing.T) (*agentInferenceProvider, *blockingRunner, *bool) {
	t.Helper()
	r := newBlockingRunner(t)
	p, installed := orderProvider(t, bounceTestManifests(), r)
	p.cfg.BundledModelID = "model-a"
	p.prePullFrameGrace = 5 * time.Millisecond
	p.prePullHoldMax = time.Minute
	return p, r, installed
}

// bootWithHold runs the boot tail and returns a cancel the caller uses to
// end a hold that is deliberately never released.
func bootWithHold(t *testing.T, p *agentInferenceProvider, installed *bool) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	*installed = true
	p.runEngineBootstrap(ctx, "boot")
	return cancel
}

// THE #379 BAR. PRODUCT CONTRACT: one model is downloaded on a boot, and
// it is the operator's — extended to the case #306's ordering could not
// reach, where the choice does not exist YET.
//
// The engine is already installed when the daemon boots (an ordinary
// restart, a re-auth reactivation, an installer that puts it in place
// before starting the service), so the fallback pre-pull dispatches within
// about a second, and the wizard's choice arrives minutes later as a
// second multi-GB download: the in-flight registry is keyed by model_id,
// so two different ids never dedupe.
func TestPrePullHold_SetupNamedAModel_TheFallbackNeverStarts(t *testing.T) {
	p, r, installed := prePullHoldProvider(t)

	cancel := bootWithHold(t, p, installed)
	defer cancel()
	// The wizard's frame lands while the hold is waiting. This is the only
	// thing the daemon needs to know: the model path now belongs to the
	// setup reconciler, which is applying that id itself.
	p.setupNoteDesired("model-b", true)
	r.releaseAll()
	p.waitForPulls()

	if got := r.pulledTags(); len(got) != 0 {
		t.Fatalf("tags pulled = %v, want none — setup named model-b, so the bundled "+
			"fallback must not add a second multi-GB download alongside it", got)
	}
}

// PRODUCT CONTRACT: "setup chose a model for this host" is permanent for
// the life of the process.
//
// Apply folds EVERY network-map frame and reports each one, and once the
// reconciler is active an empty frame is folded rather than skipped — so a
// control plane that clears its desired state (setup finished, the wizard
// page closed, the ticket expired) reports (modelID: "", driving: false)
// straight after the frame that named the model. Re-arming on that is the
// same double download by a longer route.
func TestPrePullHold_ALaterEmptyFrameDoesNotReArmTheFallback(t *testing.T) {
	p, r, installed := prePullHoldProvider(t)

	cancel := bootWithHold(t, p, installed)
	defer cancel()
	p.setupNoteDesired("model-b", true)
	p.setupNoteDesired("", false)
	r.releaseAll()
	p.waitForPulls()

	if got := r.pulledTags(); len(got) != 0 {
		t.Fatalf("tags pulled = %v, want none — setup already named model-b; a later "+
			"empty frame is the instruction being cleared, not permission to pre-pull", got)
	}
}

// PRODUCT CONTRACT: the hold is a hold, not a cancellation of the
// fallback. A frame that names no model, on a host nobody is driving, is
// the control plane answering "there is no instruction for you" — and the
// pre-pull is exactly what such a host wants.
func TestPrePullHold_AFrameWithNobodyDriving_ReleasesTheFallback(t *testing.T) {
	p, r, installed := prePullHoldProvider(t)

	cancel := bootWithHold(t, p, installed)
	defer cancel()
	p.setupNoteDesired("", false)
	r.releaseAll()
	p.waitForPulls()

	if got := r.pulledTags(); len(got) != 1 || got[0] != "a:q4" {
		t.Fatalf("tags pulled = %v, want exactly [a:q4] — an empty frame with no wizard "+
			"driving must release the fallback, not suppress it", got)
	}
}

// PRODUCT CONTRACT: a host with no control plane at all still pre-pulls.
// Unenrolled, offline, or a build with the push client disabled — no frame
// is ever folded, so the hold has to time out rather than wait forever.
func TestPrePullHold_NoFrameEverArrives_ProceedsAfterTheGrace(t *testing.T) {
	p, r, installed := prePullHoldProvider(t)

	cancel := bootWithHold(t, p, installed)
	defer cancel()
	// No setupNoteDesired call at all: prePullFrameGrace is the only thing
	// that can release this.
	r.releaseAll()
	p.waitForPulls()

	if got := r.pulledTags(); len(got) != 1 || got[0] != "a:q4" {
		t.Fatalf("tags pulled = %v, want exactly [a:q4] — a host that never hears from "+
			"a control plane must pre-pull exactly as it always did", got)
	}
}

// PRODUCT CONTRACT: the first-frame grace bounds "is anyone going to
// answer", not "has the operator chosen yet". A wizard that is driving the
// host holds the fallback back for as long as it keeps driving — the
// engine install alone routinely outlasts any short grace, and the model
// step comes after it.
//
// The negative is observed over a window rather than instantaneously,
// which is the honest shape for "nothing happens": prePullFrameGrace is
// 5 ms here, so the window is three orders of magnitude past the deadline
// the hold would have released on.
func TestPrePullHold_AWizardIsDriving_TheGraceDoesNotReleaseIt(t *testing.T) {
	p, r, installed := prePullHoldProvider(t)

	cancel := bootWithHold(t, p, installed)
	p.setupNoteDesired("", true) // driving, and no model named yet

	time.Sleep(200 * time.Millisecond)
	if n := r.calls(); n != 0 {
		t.Fatalf("pulls started = %d, want 0 — a wizard is mid-setup and about to name "+
			"a model; starting the fallback now is the double download #379 is about", n)
	}

	// Let the waiter go so the goroutine does not outlive the test.
	cancel()
	p.waitForPulls()
}

// Records today's behaviour rather than a contract: the ceiling exists so
// a setup abandoned between the engine step and the model step cannot
// leave a host with no model forever. The value it takes (the same window
// the reconciler uses to call an instruction fresh) is a judgement call,
// not something an issue ratified.
func TestPrePullHold_DrivingForeverGivesUpAtTheCeiling(t *testing.T) {
	p, r, installed := prePullHoldProvider(t)
	p.prePullHoldMax = 20 * time.Millisecond

	cancel := bootWithHold(t, p, installed)
	defer cancel()
	p.setupNoteDesired("", true)
	r.releaseAll()
	p.waitForPulls()

	if got := r.pulledTags(); len(got) != 1 || got[0] != "a:q4" {
		t.Fatalf("tags pulled = %v, want exactly [a:q4] — the hold must not outlive "+
			"a wizard that stopped without naming a model", got)
	}
}

// PRODUCT CONTRACT: the decision is re-taken at dispatch, not trusted from
// boot. An operator switch published its preference while the hold waited
// (SwapPreferredModel stores it before dispatching, and records the
// pending swap while the weights download), so the fallback is stale by
// the time the hold releases — and nothing else would stop it: the switch
// pulls a DIFFERENT model_id, which the in-flight registry never dedupes
// against.
func TestPrePullHold_AnOperatorSwitchWhileItWaited_StandsDown(t *testing.T) {
	p, r, installed := prePullHoldProvider(t)

	cancel := bootWithHold(t, p, installed)
	defer cancel()
	chosen := "model-b"
	p.preferredOverride.Store(&chosen)
	p.setupNoteDesired("", false) // the frame that would otherwise release it
	r.releaseAll()
	p.waitForPulls()

	if got := r.pulledTags(); len(got) != 0 {
		t.Fatalf("tags pulled = %v, want none — the operator switched to model-b while "+
			"the hold waited, so the bundled fallback is no longer what this host wants", got)
	}
}

// PRODUCT CONTRACT (#540, docs/decisions/20260805/1721-executor-lease-is-not-a-wizard.md):
// `waired init` must not be the reason its own model download does not start.
//
// Every other test in this file calls setupNoteDesired directly, which is the
// reconciler's ANSWER — so the question behind it, "does an executor lease
// mean a wizard is driving", was never under test at all. It does not: the
// lease is `waired init`'s, and `waired init` holds it for the whole of the
// model wait it does after installing the engine. The hold waited for the
// process that was waiting for the hold, for twenty minutes, on every
// non-interactive install. The real reconciler is wired up here so both
// halves are one test.
func TestPrePullHold_AnExecutorLeaseIsNotAWizard(t *testing.T) {
	r := newBlockingRunner(t)
	p, installed := orderProvider(t, bounceTestManifests(), r)
	p.cfg.BundledModelID = "model-a"
	// Only a folded frame may release this hold. With the ordinary short
	// grace the fallback would dispatch on the timer and the test would pass
	// having proved nothing about the lease.
	p.prePullFrameGrace = time.Hour
	p.prePullHoldMax = time.Hour
	rec := newSetupReconciler(p, nil, "dev-1", nil, quietLogger())
	ctx := context.Background()

	cancel := bootWithHold(t, p, installed)
	defer cancel()
	// attachSetupExecutor runs before the engine install and the lease is
	// released on the way out of `waired init` — so this is the state the
	// daemon is in for the whole of the model wait, not just the install.
	rec.NoteExecutor(ctx, management.SetupExecutorRequest{Attached: true, Elevated: true})
	rec.Apply(ctx, &signer.InferenceState{})

	// Bounded rather than a bare waitForPulls(): a hold that never releases
	// leaves a goroutine on pullsWG forever, which would turn this regression
	// into a package-wide timeout instead of one failing test.
	select {
	case <-r.started:
	case <-time.After(5 * time.Second):
		t.Fatal("no pull was dispatched — the boot pre-pull is still held, and the only " +
			"thing holding it is the `waired init` that is waiting for its result (#540)")
	}
	r.releaseAll()
	p.waitForPulls()

	if got := r.pulledTags(); len(got) != 1 || got[0] != "a:q4" {
		t.Fatalf("tags pulled = %v, want exactly [a:q4]", got)
	}
}

// PRODUCT CONTRACT: holding the DOWNLOAD must not hold the ACTIVATION.
//
// The already-ready arm is the only caller of activateBundledIfUnset on
// the boot path. Deferring it behind the hold would leave state.Active nil
// on a host whose weights are sitting on disk — EngineReady() false, the
// boot benchmark 400ing, /inference/benchmark 425ing, Status() reporting
// awaiting_model — for as long as the hold lasts, which on a host being
// set up from a browser is the whole wizard.
func TestPrePullHold_WeightsOnDiskActivateWithoutWaitingForSetup(t *testing.T) {
	r := newBlockingRunner(t)
	p, installed, serveTags := orderProviderServingTags(t, bounceTestManifests(), r)
	p.cfg.BundledModelID = "model-a"
	p.prePullFrameGrace = time.Hour // nothing may release the hold
	p.prePullHoldMax = time.Hour
	seedReady(t, p, "model-a", "q4", "a:q4")
	serveTags("a:q4")

	// No setupNoteDesired, and runEngineBootstrap returns as soon as the
	// tail has dispatched — so anything asserted here happened
	// synchronously, before any hold could have released.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	*installed = true
	p.runEngineBootstrap(ctx, "boot")

	st, err := p.store.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	switch {
	case st.Active == nil:
		t.Fatal("Active is nil right after boot — the bundled weights already on disk " +
			"were not committed, so the device serves nothing while the hold waits")
	case st.Active.ModelID != "model-a":
		t.Fatalf("Active.ModelID = %q, want model-a", st.Active.ModelID)
	}
	if n := r.calls(); n != 0 {
		t.Fatalf("pulls started = %d, want 0 — the weights are already on disk", n)
	}
}
