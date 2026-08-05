package main

import (
	"context"
	"testing"
	"time"

	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// The #338 pair: allow_pull=false is a supported steady state, not a
// broken one. The engine comes up (engine_bootstrap_test.go holds that
// half), it downloads nothing, and it does not pay for the refusal an
// hour later.

// PRODUCT CONTRACT (issue #338): the pre-pull refusal MOVED onto the
// dispatcher; it was not deleted.
//
// r.pulledTags() being empty is NOT the assertion with teeth here:
// PullModel's own gate — the backstop every dispatcher shares — satisfies
// it either way, so a test resting on it alone would pass whether the
// refusal lives in bundledPrePullTarget or nowhere but PullModel.
// waitForPulls() returning is what pins the LOCATION: holdBundledPrePull
// registers on pullsWG before it parks, and with no frame sent and both
// graces an hour out, nothing else can release it.
func TestPullsDisabled_TheBundledPrePullSchedulesNoHold(t *testing.T) {
	p, r, installed := prePullHoldProvider(t)
	p.cfg.AllowPull = false
	p.prePullFrameGrace = time.Hour
	p.prePullHoldMax = time.Hour

	cancel := bootWithHold(t, p, installed)
	defer cancel()

	// The other half of #338, restated where the pre-pull is observed: a
	// host that downloads nothing still starts its engine.
	if st := p.ollama.Health(context.Background()).State; st != infruntime.StateReady {
		t.Fatalf("engine state = %s, want %s", st, infruntime.StateReady)
	}

	joined := make(chan struct{})
	go func() {
		p.waitForPulls()
		close(joined)
	}()
	select {
	case <-joined:
	case <-time.After(2 * time.Second):
		t.Fatal("a pre-pull hold was parked on a host that downloads nothing — the " +
			"refusal belongs at the dispatcher (bundledPrePullTarget), not an hour " +
			"later in PullModel")
	}
	if got := r.pulledTags(); len(got) != 0 {
		t.Fatalf("tags pulled = %v, want none", got)
	}
}

// THE #338 BAR. PRODUCT CONTRACT (issue #338): a host whose weights are
// already on disk serves them with pulls turned off.
//
// This is the report the issue was filed on. The engine-start gate meant
// `ollama serve` never came up, so bootstrapAfterEngineStart never ran, so
// activateBundledIfReady — the only caller of activateBundledIfUnset on
// the boot path — never committed the selection: Active nil,
// EngineReady() false, /inference/benchmark 425ing, and Status() reporting
// awaiting_model, on a machine holding a perfectly good model.
//
// serveTags is what makes the weights real to the engine as well as to
// state.json: activateBundledIfReady asks engineServesTag before it
// commits anything (the 9475 store cutover is why).
func TestPullsDisabled_WeightsOnDiskAreStillActivated(t *testing.T) {
	r := newBlockingRunner(t)
	p, installed, serveTags := orderProviderServingTags(t, bounceTestManifests(), r)
	p.cfg.BundledModelID = "model-a"
	p.cfg.AllowPull = false
	seedReady(t, p, "model-a", "q4", "a:q4")
	serveTags("a:q4")

	ctx := context.Background()
	*installed = true
	p.runEngineBootstrap(ctx, "boot")
	p.waitForPulls()

	st, err := p.store.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	switch {
	case st.Active == nil:
		t.Fatal("Active is nil with the weights right there on disk — pulls being " +
			"off kept the engine down, so nothing ever committed the selection")
	case st.Active.ModelID != "model-a":
		t.Fatalf("Active.ModelID = %q, want model-a", st.Active.ModelID)
	}
	if got := r.calls(); got != 0 {
		t.Fatalf("pulls executed = %d, want 0 — committing weights that are already "+
			"there must not come with a download", got)
	}
}
