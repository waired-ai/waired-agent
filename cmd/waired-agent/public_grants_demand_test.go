package main

import (
	"context"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/controlclient"
)

// Extends the existing grant-loop harness (fakeGrantAPI, fakeMesh,
// writePublicUse, grantLoopDeps, grantWaitFor) with the router's
// acquire-demand signal (waired#827).

// TestPublicGrantDemand_WakesAcquirerEarly is the reason the signal
// exists: without it the first request after consent waits out a full
// periodic tick before any grant is acquired (spec §4.3 cold start).
// The tick here is set far beyond the test's patience, so an acquire can
// only happen if the demand wake drove it.
func TestPublicGrantDemand_WakesAcquirerEarly(t *testing.T) {
	dir := t.TempDir()
	path := writePublicUse(t, dir, "auto", 1)
	api := &fakeGrantAPI{acquireRes: controlclient.AcquirePublicGrantsResponse{
		Grants: []controlclient.PublicGrant{{GrantID: "grant_1", ProviderDeviceID: "dev_p0000001"}},
	}}
	demand := make(chan struct{}, 1)

	deps := grantLoopDeps(api, &fakeMesh{}, path)
	deps.Tick = time.Hour // periodic path can never fire during this test
	deps.Demand = demand

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go runPublicGrantLoop(ctx, deps)

	demand <- struct{}{}
	grantWaitFor(t, 2*time.Second, func() bool { return demandAcquires(api) > 0 })
}

// A demand arriving inside the throttle window is dropped rather than
// queued: the router re-signals on the next request that wants a public
// candidate, so nothing needs remembering, and the CP is not hammered.
func TestPublicGrantDemand_ThrottledAfterRecentAcquire(t *testing.T) {
	dir := t.TempDir()
	path := writePublicUse(t, dir, "auto", 1)
	// An empty grant list keeps held below K, so the acquire gate itself
	// never blocks a retry — only the demand throttle can.
	api := &fakeGrantAPI{}
	demand := make(chan struct{}, 1)

	deps := grantLoopDeps(api, &fakeMesh{}, path)
	deps.Tick = time.Hour
	deps.Demand = demand

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go runPublicGrantLoop(ctx, deps)

	demand <- struct{}{}
	grantWaitFor(t, 2*time.Second, func() bool { return demandAcquires(api) == 1 })

	// A burst immediately after must not produce a second acquire: the
	// floor is measured from the last actual attempt.
	for i := 0; i < 5; i++ {
		demand <- struct{}{}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if got := demandAcquires(api); got != 1 {
		t.Fatalf("acquire calls = %d, want 1 — the demand throttle did not hold", got)
	}
}

// A demand stream must never starve the periodic cycle: the demand arm
// deliberately leaves the timer untouched.
func TestPublicGrantDemand_DoesNotStarvePeriodicTick(t *testing.T) {
	dir := t.TempDir()
	path := writePublicUse(t, dir, "auto", 1)
	// A non-empty response avoids the "no candidates" backoff, and one
	// grant keeps held below K, so every periodic cycle genuinely
	// attempts an acquire.
	api := &fakeGrantAPI{acquireRes: controlclient.AcquirePublicGrantsResponse{
		Grants: []controlclient.PublicGrant{{GrantID: "grant_1", ProviderDeviceID: "dev_p0000001"}},
	}}
	demand := make(chan struct{}, 1)

	deps := grantLoopDeps(api, &fakeMesh{}, path) // Tick = 5ms
	deps.Demand = demand

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go runPublicGrantLoop(ctx, deps)

	// Hammer the demand channel while the periodic timer runs. Every
	// demand past the first is throttled; the periodic cycles must keep
	// firing regardless.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			select {
			case demand <- struct{}{}:
			default:
			}
			time.Sleep(time.Millisecond)
		}
	}()
	grantWaitFor(t, 3*time.Second, func() bool { return demandAcquires(api) >= 3 })
	<-done
}

// nil Demand leaves the loop purely periodic — the pre-#827 behaviour.
func TestPublicGrantLoop_NilDemandStillTicks(t *testing.T) {
	dir := t.TempDir()
	path := writePublicUse(t, dir, "auto", 1)
	api := &fakeGrantAPI{}

	deps := grantLoopDeps(api, &fakeMesh{}, path)
	deps.Demand = nil

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go runPublicGrantLoop(ctx, deps)

	grantWaitFor(t, 2*time.Second, func() bool { return demandAcquires(api) > 0 })
}

// demandAcquires reads the fake's acquire counter through its existing
// mutex-guarded snapshot accessor.
func demandAcquires(api *fakeGrantAPI) int {
	n, _, _ := api.snapshot()
	return n
}
