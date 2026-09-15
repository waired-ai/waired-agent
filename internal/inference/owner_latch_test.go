package inference

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// These tests pin the owner-priority latch as the owner ruled on
// 2026-09-16 (waired-agent#1387): an owner request that hit the ceiling
// keeps new public admissions paused for as long as it runs, and for
// ownerPriorityLatchWindow after it ENDS. Before, the window counted from
// the request's arrival; measured on hardware (waired#843, spec §15-6), a
// 27 s owner turn on a one-slot host left a guest paused for only 3 s
// after it, so the owner's next turn queued behind a stranger's.

// offsetClock is a test clock: base plus a settable number of seconds.
type offsetClock struct {
	base   time.Time
	offset atomic.Int64
}

func newOffsetClock() *offsetClock {
	return &offsetClock{base: time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC)}
}

func (c *offsetClock) now() time.Time {
	return c.base.Add(time.Duration(c.offset.Load()) * time.Second)
}

func (c *offsetClock) set(seconds int64) { c.offset.Store(seconds) }

// doExpectingRefusal serves a request that the test expects to be
// refused, and returns its status. A request that is admitted instead
// parks in the blocking gateway and would hang the test until its
// timeout; this fails fast rather than turning a regression into a
// ten-minute stall.
func doExpectingRefusal(t *testing.T, srv *Server, req *http.Request) int {
	t.Helper()
	done := make(chan int, 1)
	go func() { done <- do(srv, req).Code }()
	select {
	case code := <-done:
		return code
	case <-time.After(2 * time.Second):
		t.Fatal("request was admitted into the gateway, want a refusal")
		return 0
	}
}

// TestOwnerPriorityLatch_OverlayOwnerHoldsUntilItEnds: an own-network
// peer request that takes the last slot keeps public admission paused
// past the 30 s window while it is still running, then for 30 s after it
// finishes. A public consumer's own completion re-arms nothing.
func TestOwnerPriorityLatch_OverlayOwnerHoldsUntilItEnds(t *testing.T) {
	clock := newOffsetClock()
	gw := newBlockingGateway()
	srv, ownerPriv, guestPriv, _ := newPublicOverlayServer(t, gw, func(c *Config) {
		c.Capacity = 2
		c.PublicCapacity = 2
		c.IsPublicShareDenied = func() bool { return false }
		c.Now = clock.now
	})
	guest := func() *http.Request {
		return signedReqFrom(t, publicOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-guest-1", guestPriv, clock.now())
	}

	guestDone := make(chan int, 1)
	go func() { guestDone <- do(srv, guest()).Code }()
	gw.waitForInFlight(t, 1)

	// The owner takes the last slot.
	ownerDone := make(chan int, 1)
	go func() {
		ownerDone <- do(srv, signedReqFrom(t, peerOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-owner", ownerPriv, clock.now())).Code
	}()
	gw.waitForInFlight(t, 2)

	// The guest finishes, so a slot is free. The owner is still running
	// 40 s in, past the window: the latch must still hold.
	if code := gw.releaseAndWait(t, guestDone); code != http.StatusOK {
		t.Fatalf("guest request: got %d, want 200", code)
	}
	clock.set(40)
	if code := doExpectingRefusal(t, srv, guest()); code != http.StatusServiceUnavailable {
		t.Fatalf("public with a free slot while a saturating owner request runs 40 s in: got %d, want 503", code)
	}

	// The owner finishes at 40 s: the window counts from here.
	if code := gw.releaseAndWait(t, ownerDone); code != http.StatusOK {
		t.Fatalf("owner request: got %d, want 200", code)
	}
	clock.set(69)
	if code := doExpectingRefusal(t, srv, guest()); code != http.StatusServiceUnavailable {
		t.Fatalf("public 29 s after the owner finished: got %d, want 503", code)
	}

	clock.set(71)
	go func() { guestDone <- do(srv, guest()).Code }()
	gw.waitForInFlight(t, 1)
	if code := gw.releaseAndWait(t, guestDone); code != http.StatusOK {
		t.Fatalf("public 31 s after the owner finished: got %d, want 200", code)
	}
	// A guest's completion is not owner activity.
	if srv.public.latched(clock.now()) {
		t.Fatal("a public request's completion raised the owner-priority latch")
	}
}

// TestAdmitLocal_WaitingOwnerHoldsTheLatch: a local owner request that
// finds the machine full waits for a slot instead of being refused, and
// the wait latches the way an overlay refusal does. Without that, a slot a
// guest frees could go straight to the next guest while the owner waits.
// Giving up re-arms the window from the moment it gave up.
func TestAdmitLocal_WaitingOwnerHoldsTheLatch(t *testing.T) {
	clock := newOffsetClock()
	gw := newBlockingGateway()
	srv, _, guestPriv, _ := newPublicOverlayServer(t, gw, func(c *Config) {
		c.Capacity = 1
		c.PublicCapacity = 1
		c.IsPublicShareDenied = func() bool { return false }
		c.Now = clock.now
	})

	guestDone := make(chan int, 1)
	go func() {
		guestDone <- do(srv, signedReqFrom(t, publicOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-guest-1", guestPriv, clock.now())).Code
	}()
	gw.waitForInFlight(t, 1)
	if srv.public.latched(clock.now()) {
		t.Fatal("latched before any owner request arrived")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	admitted := make(chan bool, 1)
	go func() {
		release, ok := srv.AdmitLocal(ctx)
		admitted <- ok
		release()
	}()
	// Far past any window, so only a hold can answer true.
	far := clock.base.Add(time.Hour)
	deadline := time.Now().Add(2 * time.Second)
	for !srv.public.latched(far) {
		if time.Now().After(deadline) {
			t.Fatal("a local owner request waiting on a full machine did not hold the owner-priority latch")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// The owner gives up 100 s in.
	clock.set(100)
	cancel()
	if ok := <-admitted; ok {
		t.Fatal("AdmitLocal admitted after its context ended on a full machine")
	}
	if !srv.public.latched(clock.base.Add(129 * time.Second)) {
		t.Fatal("latch expired less than 30 s after the waiting owner gave up")
	}
	if srv.public.latched(clock.base.Add(131 * time.Second)) {
		t.Fatal("latch outlived the window after the waiting owner gave up")
	}

	gw.release()
	<-guestDone
}

// TestOwnerPriorityLatch_HeldUntilTheLastOwnerRequestEnds: two owner
// requests hold the latch together; the first one ending leaves it held,
// and the window runs from the end of the second.
func TestOwnerPriorityLatch_HeldUntilTheLastOwnerRequestEnds(t *testing.T) {
	clock := newOffsetClock()
	srv, _, _ := newOverlayServer(t, newFakeGateway(), PeerIdentity{DeviceID: "dev-owner"}, func(c *Config) {
		c.Capacity = 1
		c.Now = clock.now
	})

	first := mustAdmitLocal(t, srv, context.Background())
	second := make(chan func(), 1)
	go func() {
		release, _ := srv.AdmitLocal(context.Background()) // waits for first; never refused
		second <- release
	}()
	// Wait until the second request is waiting (and so holding).
	deadline := time.Now().Add(2 * time.Second)
	for srv.public.ownerHolds.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("owner holds = %d, want 2 once the second request waits", srv.public.ownerHolds.Load())
		}
		time.Sleep(2 * time.Millisecond)
	}

	clock.set(10)
	first()
	releaseSecond := <-second
	if !srv.public.latched(clock.base.Add(10 * time.Minute)) {
		t.Fatal("latch let go while the second owner request was still running")
	}

	clock.set(300)
	releaseSecond()
	if !srv.public.latched(clock.base.Add(329 * time.Second)) {
		t.Fatal("latch expired less than 30 s after the last owner request ended")
	}
	if srv.public.latched(clock.base.Add(331 * time.Second)) {
		t.Fatal("latch outlived the window after the last owner request ended")
	}
}

// TestOwnerPriorityLatch_DeadlineOnlyMovesLater: owner events can report
// with slightly different clocks; a later call carrying an earlier time
// must not shorten a window another event already set.
func TestOwnerPriorityLatch_DeadlineOnlyMovesLater(t *testing.T) {
	p := newPublicAdmission(1, 2)
	at := time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC)
	p.latch(at.Add(10*time.Second), ownerPriorityLatchWindow)
	p.latch(at, ownerPriorityLatchWindow)
	if !p.latched(at.Add(39 * time.Second)) {
		t.Fatal("an owner event with an earlier clock shortened the latch window")
	}
	if p.latched(at.Add(41 * time.Second)) {
		t.Fatal("latch outlived the later event's window")
	}
}

// TestHealthz_PublicConsumerSeesFullWhileLatched: while the latch holds,
// a public consumer's probe reads the host as full, so its router moves on
// at probe time instead of committing and taking a 503. The owner's own
// load is still not what it sees; an own-network peer's view is unchanged.
func TestHealthz_PublicConsumerSeesFullWhileLatched(t *testing.T) {
	clock := newOffsetClock()
	srv, ownerPriv, guestPriv, _ := newPublicOverlayServer(t, newFakeGateway(), func(c *Config) {
		c.Capacity = 8
		c.PublicCapacity = 2
		c.IsPublicShareDenied = func() bool { return false }
		c.Now = clock.now
	})
	healthz := func(ip, deviceID string, priv ed25519.PrivateKey) HealthSnapshot {
		t.Helper()
		req := newSignedGetRequest(t, "/waired/v1/inference/healthz", deviceID, priv, clock.now())
		req.RemoteAddr = ip + ":54321"
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("healthz for %s: status=%d body=%s", deviceID, rec.Code, rec.Body.String())
		}
		var snap HealthSnapshot
		if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
			t.Fatalf("decode: %v body=%s", err, rec.Body.String())
		}
		return snap
	}

	srv.public.latch(clock.now(), ownerPriorityLatchWindow)
	if got := healthz(publicOverlayIP, "dev-guest-1", guestPriv); got.CapacityTotal != 2 || got.CapacityUsed != 2 {
		t.Errorf("public consumer while latched: capacity %d/%d, want 2/2", got.CapacityUsed, got.CapacityTotal)
	}
	if got := healthz(peerOverlayIP, "dev-owner", ownerPriv); got.CapacityTotal != 8 || got.CapacityUsed != 0 {
		t.Errorf("own-network peer while latched: capacity %d/%d, want the real 0/8", got.CapacityUsed, got.CapacityTotal)
	}

	clock.set(31)
	if got := healthz(publicOverlayIP, "dev-guest-1", guestPriv); got.CapacityUsed != 0 {
		t.Errorf("public consumer after the window: CapacityUsed = %d, want 0", got.CapacityUsed)
	}
}
