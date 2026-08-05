package main

import (
	"context"
	"testing"
	"time"
)

// THE #359 REGRESSION BAR for the capacity path. PRODUCT CONTRACT: a
// control-plane capacity change does not bounce the engine while a model
// is downloading.
//
// `ollama pull` is a client of `ollama serve`, and a capacity frame
// arriving mid-download is the routine case rather than the exotic one:
// the control plane sends one within seconds of the agent joining the
// map, which on a fresh install is exactly when the first multi-GB model
// is being fetched. Stopping the engine there made the pull's child exit
// non-zero and the job recorded the model failed.
//
// Nothing is lost by waiting — desiredParallel already holds the target
// and the deferred reconcile re-reads it — and the admission cap the same
// frame carries was already applied by the caller, non-disruptively.
func TestApplyConcurrency_DefersTheRetuneWhileAPullIsInFlight(t *testing.T) {
	ctx := context.Background()
	r := newBlockingRunner(t)
	p, sp, installed, _ := bootstrapProviderServingTags(t)
	*installed = true
	p.manifests = bounceTestManifests()
	p.puller = newTestPuller(r)
	if err := p.ollama.EnsureRunning(ctx); err != nil {
		t.Fatalf("precondition: EnsureRunning: %v", err)
	}
	spawnsBefore := sp.count()

	if _, err := p.PullModel(ctx, "model-a"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	r.awaitStarted(t)

	p.ApplyConcurrency(ctx, 4)

	// The primary bar, and the only deterministic one: the intent was
	// RECORDED. A reconcile runs on its own goroutine, so "the engine was
	// not bounced" can only be observed by waiting, and a wait that is too
	// short passes for the wrong reason.
	if !p.retuneDeferred.Load() {
		t.Fatal("the capacity change was neither deferred nor recorded; " +
			"bouncing the engine here kills the download in flight")
	}
	if got := int(p.desiredParallel.Load()); got != 4 {
		t.Fatalf("desiredParallel = %d, want 4 — the target is recorded either way", got)
	}
	// Corroboration: give a bounce that should not happen time to land.
	time.Sleep(50 * time.Millisecond)
	if got := sp.count(); got != spawnsBefore {
		t.Fatalf("engine spawns = %d, want %d — the engine was restarted under the download",
			got, spawnsBefore)
	}

	// And the deferral is not a drop: the last pull to leave fires it.
	r.releaseAll()
	p.waitForPulls()
	if p.retuneDeferred.Load() {
		t.Fatal("the deferred retune was never consumed after the last pull left")
	}
}

// The other side: with nothing downloading, the retune is not held. The
// gate has to be conditional — a capacity change on an idle host must
// still take effect promptly, which is the whole reason ApplyConcurrency
// restarts the engine at all.
func TestApplyConcurrency_RetunesImmediatelyWithNoPullInFlight(t *testing.T) {
	ctx := context.Background()
	p, _, installed, _ := bootstrapProviderServingTags(t)
	*installed = true
	p.manifests = bounceTestManifests()
	if err := p.ollama.EnsureRunning(ctx); err != nil {
		t.Fatalf("precondition: EnsureRunning: %v", err)
	}

	p.ApplyConcurrency(ctx, 4)

	if p.retuneDeferred.Load() {
		t.Fatal("the retune was deferred with no pull in flight; the deferral must be " +
			"conditional, or every capacity change waits for a download that never comes")
	}
}
