package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/controlclient"
)

// waired-agent#806: a fresh node's own engine becoming ready ends the
// eligibility backoff, and ends only that one.
//
// The Public Share eligibility check is reciprocity — to consume public
// capacity you must be offering it — so a node whose engine has not
// finished starting is refused, correctly. That refusal is transient and
// self-resolving: the engine comes up seconds later. But the acquirer was
// asleep on a flat five-minute wall-clock timer the readiness change did
// not touch, so first public inference on a new install could be up to
// five minutes late for a condition that resolved in ten seconds.
//
// Every test here is built around the property that makes this safe to
// ship without a hardware reproduction: a readiness edge NEVER acquires.
// It clears a wait; the next real demand does the acquiring, still under
// publicGrantDemandMinInterval. No new outbound path is created.

// grantClock is the loop's time source under test control.
//
// Injected rather than slept through, and that is not only about speed.
// publicGrantDemandMinInterval is 15 s and publicGrantBackoff is 5 min, so
// a test that waited in real time could only observe the SHORTER of the
// two walls — and every "the backoff still stands" assertion below would
// have passed on the demand floor instead, with the backoff never
// consulted. Advancing past the floor and stopping short of the backoff is
// what makes those tests about the thing they name.
type grantClock struct {
	mu sync.Mutex
	t  time.Time
}

func newGrantClock() *grantClock {
	// A fixed instant: nothing here reads the wall clock, so nothing here
	// can vary with when the suite runs.
	return &grantClock{t: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)}
}

func (c *grantClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *grantClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// pastTheDemandFloor is enough to clear publicGrantDemandMinInterval and
// nowhere near publicGrantBackoff, so after it the ONLY thing that can
// still stop an acquire is the backoff.
const pastTheDemandFloor = 2 * publicGrantDemandMinInterval

// notEligibleOnce refuses the first acquire with not_eligible and succeeds
// afterwards: the shape of "the engine was still coming up, and then it
// was not".
type notEligibleOnce struct {
	*fakeGrantAPI
}

func (f *notEligibleOnce) AcquirePublicGrants(ctx context.Context, req controlclient.AcquirePublicGrantsRequest) (controlclient.AcquirePublicGrantsResponse, error) {
	res, err := f.fakeGrantAPI.AcquirePublicGrants(ctx, req)
	if demandAcquires(f.fakeGrantAPI) == 1 {
		return controlclient.AcquirePublicGrantsResponse{}, controlclient.ErrPublicShareNotEligible
	}
	return res, err
}

// readyLoopFixture starts the acquirer with a demand channel, a readiness
// channel and a controllable clock, and drives it to its first acquire.
func readyLoopFixture(t *testing.T, api publicGrantAPI, counter *fakeGrantAPI) (demand, ready chan struct{}, clk *grantClock) {
	t.Helper()
	dir := t.TempDir()
	path := writePublicUse(t, dir, "auto", 1)
	demand = make(chan struct{}, 1)
	ready = make(chan struct{}, 1)
	clk = newGrantClock()

	deps := grantLoopDeps(counter, &fakeMesh{}, path)
	deps.API = api
	deps.Tick = time.Hour // only the two signals may drive these tests
	deps.Demand = demand
	deps.Ready = ready
	deps.Now = clk.Now

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go runPublicGrantLoop(ctx, deps)

	demand <- struct{}{}
	waitUntil(t, "the first acquire attempt", func() bool { return demandAcquires(counter) == 1 })
	return demand, ready, clk
}

// pressDemandFor re-fires demand for a while and reports the acquire count
// afterwards. Re-firing matters: a single send could be dropped by the
// throttle, and a test that then saw no acquire would credit the backoff
// for the throttle's work.
func pressDemandFor(demand chan struct{}, d time.Duration, counter *fakeGrantAPI) int {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		fireDemand(demand)
		time.Sleep(5 * time.Millisecond)
	}
	return demandAcquires(counter)
}

// The headline case. Without the readiness arm the second demand is
// swallowed by a five-minute backoff for a condition that has resolved.
func TestPublicGrantReady_EndsTheEligibilityBackoff(t *testing.T) {
	counter := &fakeGrantAPI{acquireRes: controlclient.AcquirePublicGrantsResponse{
		Grants: []controlclient.PublicGrant{{GrantID: "grant_1", ProviderDeviceID: "dev_p0000001"}},
	}}
	demand, ready, clk := readyLoopFixture(t, &notEligibleOnce{fakeGrantAPI: counter}, counter)

	// Ten seconds in, the engine comes up. Still deep inside the backoff.
	clk.Advance(10 * time.Second)
	ready <- struct{}{}

	// The edge alone must not acquire — that is waired#898's rule, and
	// this is where a fix could quietly break it.
	time.Sleep(50 * time.Millisecond)
	if got := demandAcquires(counter); got != 1 {
		t.Fatalf("acquire calls = %d after a readiness edge with no demand, want 1 — an "+
			"edge is this node noticing something about ITSELF, not a request for capacity", got)
	}

	// Now a real request wants capacity. Past the demand floor, nowhere
	// near the backoff's own expiry, so this can only succeed if the edge
	// cleared it.
	clk.Advance(pastTheDemandFloor)
	waitUntil(t, "a second acquire once the readiness edge cleared the backoff", func() bool {
		fireDemand(demand)
		return demandAcquires(counter) > 1
	})
}

// rate_limited is the control plane asking for room. Nothing about this
// node's own state ends it, so a readiness edge must not.
func TestPublicGrantReady_LeavesARateLimitBackoffAlone(t *testing.T) {
	counter := &fakeGrantAPI{acquireErr: controlclient.ErrPublicShareRateLimited}
	demand, ready, clk := readyLoopFixture(t, counter, counter)

	ready <- struct{}{}
	clk.Advance(pastTheDemandFloor)
	if got := pressDemandFor(demand, 200*time.Millisecond, counter); got != 1 {
		t.Fatalf("acquire calls = %d, want 1 — a readiness edge ended a rate-limit backoff, "+
			"which is the control plane's state and not this node's", got)
	}

	// Anti-vacuity: the fixture DOES acquire again once the backoff's own
	// wall-clock expiry passes. Without this the assertion above would
	// hold just as well if the loop had stopped acquiring for some other
	// reason entirely.
	clk.Advance(publicGrantBackoff)
	waitUntil(t, "the rate-limit backoff to expire on its own clock", func() bool {
		fireDemand(demand)
		return demandAcquires(counter) > 1
	})
}

// An empty candidate list is a fact about the FLEET — nobody is offering
// right now — so this node becoming readier changes nothing about it.
func TestPublicGrantReady_LeavesAZeroCandidateBackoffAlone(t *testing.T) {
	// Eligible, and no candidates: acquire succeeds with an empty list.
	counter := &fakeGrantAPI{}
	demand, ready, clk := readyLoopFixture(t, counter, counter)

	ready <- struct{}{}
	clk.Advance(pastTheDemandFloor)
	if got := pressDemandFor(demand, 200*time.Millisecond, counter); got != 1 {
		t.Fatalf("acquire calls = %d, want 1 — a readiness edge ended a zero-candidate "+
			"backoff, which is the fleet's state and not this node's", got)
	}

	clk.Advance(publicGrantBackoff)
	waitUntil(t, "the zero-candidate backoff to expire on its own clock", func() bool {
		fireDemand(demand)
		return demandAcquires(counter) > 1
	})
}

// THE ANTI-HAMMER PIN. A readiness edge with no demand of any kind must
// reach the control plane not at all — the issue's own warning is that
// this interval is easy to get wrong in the direction of hammering it.
func TestPublicGrantReady_AloneNeverTouchesTheControlPlane(t *testing.T) {
	dir := t.TempDir()
	path := writePublicUse(t, dir, "auto", 1)
	api := &fakeGrantAPI{acquireRes: controlclient.AcquirePublicGrantsResponse{
		Grants: []controlclient.PublicGrant{{GrantID: "grant_1", ProviderDeviceID: "dev_p0000001"}},
	}}

	ready := make(chan struct{}, 1)
	deps := grantLoopDeps(api, &fakeMesh{}, path)
	deps.Tick = time.Hour
	deps.Ready = ready

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go runPublicGrantLoop(ctx, deps)

	for range 20 {
		ready <- struct{}{}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)

	acquires, renews, releases := api.snapshot()
	if acquires != 0 {
		t.Errorf("acquire calls = %d, want 0 — a readiness edge must issue no request of "+
			"its own; the next real demand does the acquiring", acquires)
	}
	if len(renews) != 0 || len(releases) != 0 {
		t.Errorf("renew/release calls = %d/%d, want 0/0", len(renews), len(releases))
	}
}

// Clearing a wall this node put up must not also remove the one
// waired#898 put up.
func TestPublicGrantReady_DoesNotBypassTheDemandFloor(t *testing.T) {
	counter := &fakeGrantAPI{acquireRes: controlclient.AcquirePublicGrantsResponse{
		Grants: []controlclient.PublicGrant{{GrantID: "grant_1", ProviderDeviceID: "dev_p0000001"}},
	}}
	demand, ready, _ := readyLoopFixture(t, &notEligibleOnce{fakeGrantAPI: counter}, counter)

	ready <- struct{}{}
	// The clock is NOT advanced, so the demand floor is still standing
	// even though the eligibility backoff has just been cleared.
	if got := pressDemandFor(demand, 200*time.Millisecond, counter); got != 1 {
		t.Fatalf("acquire calls = %d inside the %s demand floor, want 1 — clearing the "+
			"eligibility backoff must not also lift the floor waired#898 put up",
			got, publicGrantDemandMinInterval)
	}
}

// A loop with no readiness signal wired behaves exactly as it did before:
// a nil Ready is a select arm that never fires.
func TestPublicGrantReady_NilChannelIsInert(t *testing.T) {
	dir := t.TempDir()
	path := writePublicUse(t, dir, "auto", 1)
	api := &fakeGrantAPI{acquireRes: controlclient.AcquirePublicGrantsResponse{
		Grants: []controlclient.PublicGrant{{GrantID: "grant_1", ProviderDeviceID: "dev_p0000001"}},
	}}

	demand := make(chan struct{}, 1)
	deps := grantLoopDeps(api, &fakeMesh{}, path)
	deps.Tick = time.Hour
	deps.Demand = demand
	deps.Ready = nil

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go runPublicGrantLoop(ctx, deps)

	demand <- struct{}{}
	waitUntil(t, "the demand path still works with no readiness signal wired", func() bool {
		return demandAcquires(api) > 0
	})
}
