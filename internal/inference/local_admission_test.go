package inference

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestAdmitLocal_CountsAgainstTheSharedCounter: the owner's own
// local-engine work occupies the same admission counter peer requests
// are judged against, so Config.Capacity means "concurrent requests on
// this machine" rather than "concurrent requests that arrived over the
// overlay".
func TestAdmitLocal_CountsAgainstTheSharedCounter(t *testing.T) {
	srv, _, _ := newOverlayServer(t, newFakeGateway(), PeerIdentity{DeviceID: "dev-owner"}, func(c *Config) {
		c.Capacity = 4
	})

	release, _ := srv.AdmitLocal(context.Background())
	if got := srv.InflightCount(); got != 1 {
		t.Fatalf("inflight after AdmitLocal: got %d, want 1", got)
	}
	release()
	if got := srv.InflightCount(); got != 0 {
		t.Fatalf("inflight after release: got %d, want 0", got)
	}
}

// PRODUCT CONTRACT (waired-agent#703): AdmittedCount is cumulative and
// covers the same peer-plus-owner population InflightCount reports.
//
// It exists because the install-time host-speed measurement runs for 45 s
// or more and has to ask "did this machine serve anything during that",
// which a gauge cannot answer: a request that starts and finishes between
// two reads leaves InflightCount at zero on both sides. Under
// infruntime.MaxResidentModels that request and the measurement's probe
// evict each other, and the published figure describes the eviction.
func TestAdmittedCount_IsCumulativeAcrossAWindow(t *testing.T) {
	srv, _, _ := newOverlayServer(t, newFakeGateway(), PeerIdentity{DeviceID: "dev-owner"}, func(c *Config) {
		c.Capacity = 4
	})

	before := srv.AdmittedCount()

	// A whole request, begun and finished — invisible to InflightCount at
	// either end, which is the case this counter is for.
	mustAdmitLocal(t, srv, context.Background())()
	if got := srv.InflightCount(); got != 0 {
		t.Fatalf("inflight = %d, want 0 — the request finished", got)
	}
	if got := srv.AdmittedCount(); got != before+1 {
		t.Fatalf("admitted = %d, want %d — a finished request must still be counted", got, before+1)
	}

	// And the peer half of the population, through the counter the
	// capacity gate uses.
	if !srv.inflight.Acquire() {
		t.Fatal("Acquire refused below capacity")
	}
	if got := srv.AdmittedCount(); got != before+2 {
		t.Fatalf("admitted = %d, want %d — a peer request must be counted too", got, before+2)
	}
	srv.inflight.Release()
	if got := srv.AdmittedCount(); got != before+2 {
		t.Fatalf("admitted = %d after Release, want %d — releasing must not lower it", got, before+2)
	}

	// A REFUSED request is not an admitted one: it never reached the
	// engine, so it cannot have contended with anything.
	srv.inflight.setCapacity(1)
	srv.inflight.Acquire()
	full := srv.AdmittedCount()
	if srv.inflight.Acquire() {
		t.Fatal("Acquire succeeded past the ceiling")
	}
	if got := srv.AdmittedCount(); got != full {
		t.Errorf("admitted = %d after a refusal, want %d", got, full)
	}
}

// Record of today's behaviour: a ping-only server has no counter, and
// answers 0 rather than panicking. Every caller reads it through a
// nil-safe relay, so this is the floor that relay rests on.
func TestAdmittedCount_NoCounterIsZero(t *testing.T) {
	var srv Server
	if got := srv.AdmittedCount(); got != 0 {
		t.Errorf("AdmittedCount = %d on a server with no admission counter, want 0", got)
	}
}

// TestAdmitLocal_LatchesAtSaturation: a local request that arrives when
// the machine is (or goes) full raises the owner-priority latch.
//
// PRODUCT CONTRACT, and the half of spec §8.2 the 2026-09-12 ruling
// CONFIRMS: the latch is this account's priority over PUBLIC consumers.
// Below saturation nothing latches — sharing the machine while there is
// headroom is the whole point. What the ruling changed is the other half,
// the unbounded local admit; see
// TestAdmitLocal_AtCapacityWaitsInsteadOfOversubscribing.
func TestAdmitLocal_LatchesAtSaturation(t *testing.T) {
	at := time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC)
	srv, _, _ := newOverlayServer(t, newFakeGateway(), PeerIdentity{DeviceID: "dev-owner"}, func(c *Config) {
		c.Capacity = 2
		c.Now = func() time.Time { return at }
	})

	first := mustAdmitLocal(t, srv, context.Background())
	if srv.public.latched(at) {
		t.Fatal("latched below saturation: 1 of 2 slots is not owner contention")
	}
	// Second local request takes the last slot → arrival at saturation.
	second := mustAdmitLocal(t, srv, context.Background())
	if !srv.public.latched(at) {
		t.Fatal("owner request took the last slot but no owner-priority latch")
	}
	if !srv.public.latched(at.Add(29 * time.Second)) {
		t.Fatal("latch expired before the 30s window")
	}
	if srv.public.latched(at.Add(31 * time.Second)) {
		t.Fatal("latch outlived the 30s window")
	}
	second()
	first()
}

// TestAdmitLocal_AtCapacityWaitsInsteadOfOversubscribing INVERTS the
// contract this test used to carry ("the owner is never turned away on
// their own machine; the counter goes past the ceiling and latches
// instead").
//
// Owner ruling 2026-09-12, waired-agent#1302, correcting the reading of
// spec §8.2 / waired#899 rather than overturning it: "自分の機械" meant the
// computers enrolled in this ACCOUNT, not this one over another of them, so
// a request from here is an equal claimant on the ceiling with an
// own-network peer's. Measured on the rc6 fleet: one local turn drove the
// shared counter past a one-slot engine while a peer was already on it, and
// the two then contended and evicted each other's prefixes.
//
// It is a wait, not a refusal: a local leg has nowhere else to go
// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md).
func TestAdmitLocal_AtCapacityWaitsInsteadOfOversubscribing(t *testing.T) {
	at := time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC)
	srv, _, _ := newOverlayServer(t, newFakeGateway(), PeerIdentity{DeviceID: "dev-owner"}, func(c *Config) {
		c.Capacity = 1
		c.Now = func() time.Time { return at }
	})

	first, ok := srv.AdmitLocal(context.Background())
	if !ok {
		t.Fatal("the first request was not admitted into an idle engine")
	}
	if got := srv.InflightCount(); got != 1 {
		t.Fatalf("inflight = %d, want 1", got)
	}
	if !srv.public.latched(at) {
		t.Error("the request that took the last slot did not raise the owner-priority latch")
	}

	// A second one waits. The ceiling is never exceeded, which is the
	// whole of the correction.
	ctx, cancel := context.WithCancel(context.Background())
	admitted := make(chan bool, 1)
	go func() {
		release, ok := srv.AdmitLocal(ctx)
		admitted <- ok
		release()
	}()
	select {
	case <-admitted:
		t.Fatal("a second local request was admitted into a one-slot engine")
	case <-time.After(50 * time.Millisecond):
	}
	if got := srv.InflightCount(); got != 1 {
		t.Fatalf("inflight = %d while one request waits, want 1 — the ceiling was exceeded", got)
	}

	// Releasing the first hands the slot to the waiter.
	first()
	select {
	case ok := <-admitted:
		if !ok {
			t.Error("the waiter was refused after a slot came free")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the waiter was not woken by the release")
	}
	cancel()
}

// TestAdmitLocal_WaitEndsWithTheRequest: the wait is bounded by the
// request context and by nothing else, so a client that hangs up stops
// waiting and the handler is told.
func TestAdmitLocal_WaitEndsWithTheRequest(t *testing.T) {
	srv, _, _ := newOverlayServer(t, newFakeGateway(), PeerIdentity{DeviceID: "dev-owner"}, func(c *Config) {
		c.Capacity = 1
	})
	held, ok := srv.AdmitLocal(context.Background())
	if !ok {
		t.Fatal("precondition: the engine should have had a free slot")
	}
	defer held()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() {
		_, ok := srv.AdmitLocal(ctx)
		done <- ok
	}()
	cancel()
	select {
	case ok := <-done:
		if ok {
			t.Error("a cancelled request reported that it had been admitted")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelling the request did not end the wait")
	}
}

// TestAdmitLocal_ACapacityRaiseWakesAWaiter: the ceiling is retuned live
// by the control plane, and a raise can admit a waiter that no Release is
// coming for.
func TestAdmitLocal_ACapacityRaiseWakesAWaiter(t *testing.T) {
	srv, _, _ := newOverlayServer(t, newFakeGateway(), PeerIdentity{DeviceID: "dev-owner"}, func(c *Config) {
		c.Capacity = 1
	})
	held, ok := srv.AdmitLocal(context.Background())
	if !ok {
		t.Fatal("precondition: the engine should have had a free slot")
	}
	defer held()

	done := make(chan bool, 1)
	go func() {
		release, ok := srv.AdmitLocal(context.Background())
		done <- ok
		release()
	}()
	select {
	case <-done:
		t.Fatal("admitted into a full engine before the raise")
	case <-time.After(50 * time.Millisecond):
	}
	srv.SetCapacity(2)
	select {
	case ok := <-done:
		if !ok {
			t.Error("the waiter was refused after the ceiling was raised")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("raising the ceiling did not wake the waiter")
	}
}

// TestAdmitLocal_ReleaseIsIdempotent guards the counter against a
// double-release: the gateway defers the release inside handlers that
// have several return paths, so a stray second call must not drive the
// shared counter negative and hand peers phantom capacity.
func TestAdmitLocal_ReleaseIsIdempotent(t *testing.T) {
	srv, _, _ := newOverlayServer(t, newFakeGateway(), PeerIdentity{DeviceID: "dev-owner"}, func(c *Config) {
		c.Capacity = 2
	})
	release, _ := srv.AdmitLocal(context.Background())
	release()
	release()
	if got := srv.InflightCount(); got != 0 {
		t.Fatalf("inflight after double release: got %d, want 0", got)
	}
}

// TestAdmitLocal_OverlayRequestIsNotCountedTwice: a request that
// arrived over the overlay already passed capacityGate, so reaching
// AdmitLocal through the shared gateway handler must be a no-op. The
// discriminator is the peer identity the peer-auth chain puts in the
// context — a fact about the request, not a wiring convention, so
// mis-wiring the hook onto the overlay surface cannot halve capacity.
func TestAdmitLocal_OverlayRequestIsNotCountedTwice(t *testing.T) {
	at := time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC)
	srv, _, _ := newOverlayServer(t, newFakeGateway(), PeerIdentity{DeviceID: "dev-owner"}, func(c *Config) {
		c.Capacity = 1
		c.Now = func() time.Time { return at }
	})

	ctx := ContextWithPeer(context.Background(), PeerIdentity{DeviceID: "peer-A"})
	release, _ := srv.AdmitLocal(ctx)
	if got := srv.InflightCount(); got != 0 {
		t.Fatalf("inflight for an overlay-originated request: got %d, want 0", got)
	}
	if srv.public.latched(at) {
		t.Fatal("an overlay request must not raise the owner-priority latch here")
	}
	release()
	if got := srv.InflightCount(); got != 0 {
		t.Fatalf("inflight after release: got %d, want 0", got)
	}
}

// TestAdmitLocal_PingOnlyServerIsSafe: NewServer has no admission state
// at all (no gateway mounted). The relay in cmd/waired-agent points at
// whatever Server the session built, so this path must be a no-op
// rather than a nil dereference.
func TestAdmitLocal_PingOnlyServerIsSafe(t *testing.T) {
	srv := NewServer("dev-owner")
	release, _ := srv.AdmitLocal(context.Background())
	if release == nil {
		t.Fatal("AdmitLocal must always return a non-nil release")
	}
	release()
	if got := srv.InflightCount(); got != 0 {
		t.Fatalf("inflight: got %d, want 0", got)
	}
}

// TestAdmitLocal_UnlimitedCapacityNeverLatches: at Capacity 0 (the
// backward-compatible "unlimited" value, and what a freshly booted
// agent runs on until the control plane delivers its benchmarked
// capacity) saturation is undefined, so no latch — the same semantics
// the overlay capacityGate has always had.
func TestAdmitLocal_UnlimitedCapacityNeverLatches(t *testing.T) {
	at := time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC)
	srv, _, _ := newOverlayServer(t, newFakeGateway(), PeerIdentity{DeviceID: "dev-owner"}, func(c *Config) {
		c.Capacity = 0
		c.Now = func() time.Time { return at }
	})
	for range 5 {
		defer mustAdmitLocal(t, srv, context.Background())()
	}
	if srv.public.latched(at) {
		t.Fatal("unlimited capacity must not latch")
	}
}

// TestAdmitLocal_RecorderSeesLocalLoad: the inflight gauge is fed from
// the shared counter, so local work has to move it too — otherwise the
// metric reads 0 on a machine that is busy serving its owner.
func TestAdmitLocal_RecorderSeesLocalLoad(t *testing.T) {
	rec := &countingRecorder{}
	srv, _, _ := newOverlayServer(t, newFakeGateway(), PeerIdentity{DeviceID: "dev-owner"}, func(c *Config) {
		c.Capacity = 2
		c.Recorder = rec
	})
	release, _ := srv.AdmitLocal(context.Background())
	if got := rec.inflight.Load(); got != 1 {
		t.Fatalf("inflight gauge after AdmitLocal: got %d, want 1", got)
	}
	release()
	if got := rec.inflight.Load(); got != 0 {
		t.Fatalf("inflight gauge after release: got %d, want 0", got)
	}
}

type countingRecorder struct {
	inflight atomic.Int64
}

func (r *countingRecorder) RecordServed(string, uint32) {}
func (r *countingRecorder) SetInflight(n int)           { r.inflight.Store(int64(n)) }
func (r *countingRecorder) SetCapacity(int)             {}

// TestOwnerPriorityLatch_LocalRequestPausesPublicAdmission is the
// acceptance criterion of spec §15-6 for the configuration the issue
// (waired#899) describes: one machine that both serves strangers and
// runs its owner's own coding agent. The owner's request never touches
// the overlay listener, so before the fix nothing paused public
// admission.
func TestOwnerPriorityLatch_LocalRequestPausesPublicAdmission(t *testing.T) {
	base := time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC)
	var offset atomic.Int64 // seconds added to base
	now := func() time.Time { return base.Add(time.Duration(offset.Load()) * time.Second) }

	gw := newBlockingGateway()
	srv, _, guestPriv, _ := newPublicOverlayServer(t, gw, func(c *Config) {
		c.Capacity = 1
		c.PublicCapacity = 1
		c.IsPublicShareDenied = func() bool { return false }
		c.Now = now
	})

	// The owner starts local work: loopback → gateway → this machine's
	// engine. It fills the machine, so public admission pauses.
	release, _ := srv.AdmitLocal(context.Background())

	rec := do(srv, signedReqFrom(t, publicOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-guest-1", guestPriv, now()))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "waired_inference_overloaded") {
		t.Fatalf("public during owner-local latch: got %d %q, want 503 waired_inference_overloaded", rec.Code, rec.Body.String())
	}

	// The owner's request finishes, freeing the slot — the latch still
	// holds for the rest of its window (§8.2: in-flight publics drain,
	// new ones wait).
	release()
	rec = do(srv, signedReqFrom(t, publicOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-guest-1", guestPriv, now()))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("public inside the latch window after the owner finished: got %d, want 503", rec.Code)
	}

	// Past the window public admission recovers.
	offset.Store(31)
	result := make(chan int, 1)
	go func() {
		result <- do(srv, signedReqFrom(t, publicOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-guest-1", guestPriv, now())).Code
	}()
	gw.waitForInFlight(t, 1)
	gw.release()
	if code := <-result; code != http.StatusOK {
		t.Fatalf("public after latch expiry: got %d, want 200", code)
	}
}

// mustAdmitLocal is AdmitLocal for a test that is not about the wait: it
// fails the test rather than letting a refusal read as an admitted request.
func mustAdmitLocal(t *testing.T, srv *Server, ctx context.Context) func() {
	t.Helper()
	release, ok := srv.AdmitLocal(ctx)
	if !ok {
		t.Fatal("AdmitLocal did not admit; this test needs a free slot")
	}
	return release
}
