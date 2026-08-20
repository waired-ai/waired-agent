package main

import (
	"context"
	"testing"
	"time"
)

// joinEngineReconcile registers the cleanup that every fixture able to
// reach endPull needs: cancel the agent context, then wait for the
// reconcile goroutine endPull fired to finish.
//
// spawnPull's defers are LIFO and its own comment states the ordering —
// endPull runs BEFORE pullsWG.Done — so `waitForPulls()` returning implies
// endPull has already run. endPull consumes the deferred retune/swap
// intent through requestEngineReconcile, which does
// `go reconcileEngineServe(p.agentCtx)` and joins nothing. That goroutine
// writes the Active selection into the provider's state dir, so a fixture
// whose store lives under t.TempDir() has a write racing the directory's
// removal:
//
//	TempDir RemoveAll cleanup: unlinkat /var/folders/.../TestX: directory not empty
//
// Under load only, which is why it reads as a flake on the slower legs
// (waired-agent#925).
//
// NOT a product defect: in the daemon that context outlives every pull,
// and no directory is being removed under it.
//
// One helper rather than a copy per fixture, because the rule is one rule
// and the first fixture to learn it (hostCutoffProvider) had already
// written the whole explanation down for itself while the bootstrap
// fixture went on racing. The bound matches what that one used; once the
// context is cancelled the reconcile has nothing slow left to do.
func joinEngineReconcile(t *testing.T, p *agentInferenceProvider, cancel context.CancelFunc) {
	t.Helper()
	t.Cleanup(func() {
		cancel()
		for range 400 {
			if !p.engineReconcileInFlight.Load() {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		// Deliberately not a t.Fatal: this runs in a cleanup, the run is
		// already over, and a reconcile still going after two seconds is
		// worth saying out loud rather than turning into a second failure
		// on top of whatever the test itself reported.
		t.Log("engine reconcile still in flight after 2s; the state dir removal below may race it")
	})
}

// The helper's own contract, since the fixtures that use it cannot assert
// it from inside: cancel, and then WAIT. A cleanup that only cancelled
// would look identical from every caller and would leave the race exactly
// where it was.
//
// The subject runs inside a subtest so its cleanup has a boundary this
// test can observe from the outside.
func TestJoinEngineReconcile_WaitsForTheReconcileToFinish(t *testing.T) {
	p := &agentInferenceProvider{}
	p.engineReconcileInFlight.Store(true)
	ctx, cancel := context.WithCancel(context.Background())

	// Stands in for reconcileEngineServe: it finishes some time after the
	// context is cancelled, not at the moment of cancellation — which is
	// the whole reason a cancel alone is not enough.
	finished := make(chan struct{})
	go func() {
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond)
		p.engineReconcileInFlight.Store(false)
		close(finished)
	}()

	cancelled := make(chan struct{})
	t.Run("holds the cleanup open", func(t *testing.T) {
		joinEngineReconcile(t, p, func() { close(cancelled); cancel() })
	})

	select {
	case <-cancelled:
	default:
		t.Fatal("the cleanup never cancelled the agent context")
	}
	select {
	case <-finished:
	default:
		t.Fatal("the cleanup returned while the reconcile was still in flight — a cancel " +
			"alone leaves the state-dir write racing the TempDir removal (waired-agent#925)")
	}
}
