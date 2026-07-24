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
	// A NON-empty response is essential: an empty grant list arms the
	// 5-minute publicGrantBackoff, which would hold this test green even
	// with publicGrantDemandMinInterval set to zero. One grant keeps held
	// below K, so the only thing that can stop a second acquire is the
	// demand throttle itself.
	api := &fakeGrantAPI{acquireRes: controlclient.AcquirePublicGrantsResponse{
		Grants: []controlclient.PublicGrant{{GrantID: "grant_1", ProviderDeviceID: "dev_p0000001"}},
	}}
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
// deliberately leaves the timer untouched, and a throttled demand
// `continue`s before renew. So under a burst of throttled demands, the
// only thing that can keep RENEWING a held grant is the periodic timer —
// which makes renew progress the observable that proves the timer fires
// (acquisition is demand-driven now, so it is no longer periodic;
// waired#898).
func TestPublicGrantDemand_DoesNotStarvePeriodicTick(t *testing.T) {
	dir := t.TempDir()
	path := writePublicUse(t, dir, "auto", 1)
	// A grant whose expiry is already near/behind now, so it is
	// perpetually renew-due and every periodic cycle renews it. Usage is
	// left nil, so renewal is not last-use gated here.
	soon := time.Now().Add(20 * time.Millisecond).UTC().Format(time.RFC3339Nano)
	api := &fakeGrantAPI{
		acquireRes: controlclient.AcquirePublicGrantsResponse{
			Grants: []controlclient.PublicGrant{{GrantID: "grant_1", ProviderDeviceID: "dev_p0000001", ExpiresAt: soon}},
		},
		renewRes: controlclient.RenewPublicGrantsResponse{Status: "ok", Renewed: []string{"grant_1"}, ExpiresAt: soon},
	}
	mesh := &fakeMesh{}
	mesh.setGrantPeers("grant_1")
	demand := make(chan struct{}, 1)

	deps := grantLoopDeps(api, mesh, path) // Tick = 5ms
	deps.Demand = demand

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go runPublicGrantLoop(ctx, deps)

	// Acquire the grant via one demand, then hammer demand. Every demand
	// past the first is throttled (so it does not even reach renew); the
	// periodic renew cycles must keep firing regardless.
	demand <- struct{}{}
	grantWaitFor(t, 2*time.Second, func() bool { return demandAcquires(api) >= 1 })

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
	grantWaitFor(t, 3*time.Second, func() bool { _, renews, _ := api.snapshot(); return len(renews) >= 3 })
	<-done
}

// nil Demand leaves the loop with no acquisition trigger at all: since
// acquisition is demand-driven (waired#898), a consented loop with a nil
// Demand acquires nothing. It must still run its periodic maintenance and
// shut down cleanly.
func TestPublicGrantLoop_NilDemandNeverAcquires(t *testing.T) {
	dir := t.TempDir()
	path := writePublicUse(t, dir, "auto", 1)
	api := &fakeGrantAPI{acquireRes: controlclient.AcquirePublicGrantsResponse{
		Grants: []controlclient.PublicGrant{{GrantID: "grant_1", ProviderDeviceID: "dev_p0000001"}},
	}}

	deps := grantLoopDeps(api, &fakeMesh{}, path) // Tick = 5ms
	deps.Demand = nil

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { runPublicGrantLoop(ctx, deps); close(done) }()

	time.Sleep(80 * time.Millisecond) // many periodic ticks
	if calls := demandAcquires(api); calls != 0 {
		cancel()
		<-done
		t.Fatalf("nil-demand loop acquired %d times, want 0 (acquisition is demand-driven)", calls)
	}
	cancel()
	<-done // clean shutdown
}

// demandAcquires reads the fake's acquire counter through its existing
// mutex-guarded snapshot accessor.
func demandAcquires(api *fakeGrantAPI) int {
	n, _, _ := api.snapshot()
	return n
}
