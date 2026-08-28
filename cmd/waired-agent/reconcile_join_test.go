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
//
// Waiting for engineReconcileInFlight alone was not enough, and
// TestBootstrap_PreferenceWithNoServableVariantStillPullsTheBundled went
// on failing the same way on the windows leg (2026-08-23) and the
// seeded-host leg (2026-08-27) after the helper existed. Two gaps, both
// visible in the flag's own lifetime:
//
//   - reconcileEngineServe's `defer p.warmServingModel()` runs BEFORE its
//     `defer p.engineReconcileInFlight.Store(false)`, and warmServingModel
//     only STARTS a goroutine (inference_warm.go:54) before returning. So
//     the flag can be false with a detached warm-up still reading the
//     provider's store.
//   - chaseEngineRespawn (inference.go:1815) sleeps and then asks for
//     another reconcile, up to respawnChaseAttempts times, on no context
//     at all. A single false reading can therefore be a gap between two
//     reconciles rather than the end of them.
//
// So this waits for BOTH flags and for the answer to hold still, rather
// than for one flag to read false once.
//
// It is not established that either gap is what writes into the directory
// on the runs that fail — neither was reproduced on linux in 20 whole-
// suite runs at -count=2 under 2x CPU oversubscription. What is
// established is that the helper's stated contract ("wait for the
// reconcile endPull fired to finish") was not what it did.
func joinEngineReconcile(t *testing.T, p *agentInferenceProvider, cancel context.CancelFunc) {
	t.Helper()
	t.Cleanup(func() {
		cancel()
		// Two consecutive quiet samples, because chaseEngineRespawn's next
		// ask can land between any two of them.
		quiet := 0
		for range 400 {
			if !p.engineReconcileInFlight.Load() && !p.warmInFlight.Load() {
				quiet++
				if quiet == 2 {
					return
				}
			} else {
				quiet = 0
			}
			time.Sleep(5 * time.Millisecond)
		}
		// Deliberately not a t.Fatal: this runs in a cleanup, the run is
		// already over, and a reconcile still going after two seconds is
		// worth saying out loud rather than turning into a second failure
		// on top of whatever the test itself reported.
		t.Logf("engine work still in flight after 2s (reconcile=%v warm=%v); the state dir "+
			"removal below may race it", p.engineReconcileInFlight.Load(), p.warmInFlight.Load())
	})
}

// The warm half of the contract, pinned separately because it is the half
// the helper did not have: a cleanup that watched only the reconcile flag
// returned while a detached warm-up was still reading the provider's
// store, and looked identical from every caller.
func TestJoinEngineReconcile_WaitsForADetachedWarmUp(t *testing.T) {
	p := &agentInferenceProvider{}
	p.warmInFlight.Store(true) // reconcileEngineServe started one on its way out
	ctx, cancel := context.WithCancel(context.Background())

	finished := make(chan struct{})
	go func() {
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond)
		p.warmInFlight.Store(false)
		close(finished)
	}()

	t.Run("holds the cleanup open", func(t *testing.T) {
		joinEngineReconcile(t, p, cancel)
	})

	select {
	case <-finished:
	default:
		t.Fatal("the cleanup returned while a warm-up was still in flight — " +
			"reconcileEngineServe clears its own flag after merely STARTING one " +
			"(waired-agent#925)")
	}
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
