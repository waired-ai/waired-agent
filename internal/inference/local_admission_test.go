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

	release := srv.AdmitLocal(context.Background())
	if got := srv.InflightCount(); got != 1 {
		t.Fatalf("inflight after AdmitLocal: got %d, want 1", got)
	}
	release()
	if got := srv.InflightCount(); got != 0 {
		t.Fatalf("inflight after release: got %d, want 0", got)
	}
}

// TestAdmitLocal_LatchesAtSaturation: a local request that arrives when
// the machine is (or goes) full raises the owner-priority latch — the
// "local" half of spec §8.2. Below saturation nothing latches: sharing
// the machine while the owner has headroom is the whole point.
func TestAdmitLocal_LatchesAtSaturation(t *testing.T) {
	at := time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC)
	srv, _, _ := newOverlayServer(t, newFakeGateway(), PeerIdentity{DeviceID: "dev-owner"}, func(c *Config) {
		c.Capacity = 2
		c.Now = func() time.Time { return at }
	})

	first := srv.AdmitLocal(context.Background())
	if srv.public.latched(at) {
		t.Fatal("latched below saturation: 1 of 2 slots is not owner contention")
	}
	// Second local request takes the last slot → arrival at saturation.
	second := srv.AdmitLocal(context.Background())
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

// TestAdmitLocal_OverCapacityLatchesButNeverRejects: the owner is never
// turned away on their own machine. A local request that arrives when
// the machine is already full is admitted (the counter goes past the
// ceiling) and latches instead.
func TestAdmitLocal_OverCapacityLatchesButNeverRejects(t *testing.T) {
	at := time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC)
	srv, _, _ := newOverlayServer(t, newFakeGateway(), PeerIdentity{DeviceID: "dev-owner"}, func(c *Config) {
		c.Capacity = 1
		c.Now = func() time.Time { return at }
	})

	releases := make([]func(), 0, 3)
	for range 3 {
		releases = append(releases, srv.AdmitLocal(context.Background()))
	}
	if got := srv.InflightCount(); got != 3 {
		t.Fatalf("inflight: got %d, want 3 (the owner is never rejected)", got)
	}
	if !srv.public.latched(at) {
		t.Fatal("owner request past capacity did not latch")
	}
	for _, r := range releases {
		r()
	}
	if got := srv.InflightCount(); got != 0 {
		t.Fatalf("inflight after releases: got %d, want 0", got)
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
	release := srv.AdmitLocal(context.Background())
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
	release := srv.AdmitLocal(ctx)
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
	release := srv.AdmitLocal(context.Background())
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
		defer srv.AdmitLocal(context.Background())()
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
	release := srv.AdmitLocal(context.Background())
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
	release := srv.AdmitLocal(context.Background())

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
