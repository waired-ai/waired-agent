package inference

import (
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// publicOverlayIP is the second fixed peer slot used by the public-gate
// tests: peerOverlayIP hosts the same-network "owner" peer,
// publicOverlayIP the foreign public-grant consumer.
const publicOverlayIP = "100.96.0.20"

func publicConsumerIdentity(pub ed25519.PublicKey) PeerIdentity {
	return PeerIdentity{
		DeviceID:   "dev-guest-1",
		MachineKey: pub,
		Pseudonym:  "guest-a7f3",
		Grant: &signer.PeerGrant{
			ID:        "grant_test1",
			Kind:      "public",
			Role:      "consumer",
			Pseudonym: "guest-a7f3",
		},
	}
}

// signedReqFrom is signedReq with a caller-chosen source overlay IP so
// tests can originate requests from the public consumer slot.
func signedReqFrom(t *testing.T, fromIP, target string, body []byte, deviceID string, priv ed25519.PrivateKey, now time.Time) *http.Request {
	t.Helper()
	r := signedReq(t, target, body, deviceID, priv, now)
	r.RemoteAddr = fromIP + ":54321"
	return r
}

// ctxGateway parks every request until its context is cancelled or the
// test releases it explicitly — the kill-switch tests need handlers
// that observe cancellation, which blockingGateway's channel-park
// cannot.
type ctxGateway struct {
	mu        sync.Mutex
	releases  []chan struct{}
	parked    int
	cancelled atomic.Int32
}

func newCtxGateway() *ctxGateway { return &ctxGateway{} }

func (g *ctxGateway) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		release := make(chan struct{})
		g.releases = append(g.releases, release)
		g.parked++
		g.mu.Unlock()
		select {
		case <-r.Context().Done():
			g.cancelled.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
		case <-release:
			w.WriteHeader(http.StatusOK)
		}
		g.mu.Lock()
		g.parked--
		// A ctx-cancelled handler abandons its release slot; drop it so
		// releaseOne only ever frees a handler that is still parked.
		for i, ch := range g.releases {
			if ch == release {
				g.releases = append(g.releases[:i], g.releases[i+1:]...)
				break
			}
		}
		g.mu.Unlock()
	})
}

func (g *ctxGateway) releaseOne() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.releases) == 0 {
		return false
	}
	close(g.releases[0])
	g.releases = g.releases[1:]
	return true
}

func (g *ctxGateway) waitParked(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		g.mu.Lock()
		p := g.parked
		g.mu.Unlock()
		if p >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("waited 2s for %d parked handlers", n)
}

func (g *ctxGateway) waitCancelled(t *testing.T, n int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if g.cancelled.Load() >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("waited 2s for %d cancelled handlers, saw %d", n, g.cancelled.Load())
}

// newPublicOverlayServer builds a two-peer overlay server: the standard
// owner peer plus a public-grant consumer, both with their own keys.
func newPublicOverlayServer(t *testing.T, gw gatewayHandlerSet, opts ...func(*Config)) (srv *Server, ownerPriv, guestPriv ed25519.PrivateKey, at time.Time) {
	t.Helper()
	ownerPub, ownerPrivK := mustKey(t)
	guestPub, guestPrivK := mustKey(t)
	srv, peers, _ := newOverlayServer(t, gw, PeerIdentity{DeviceID: "dev-owner", MachineKey: ownerPub}, opts...)
	peers[netip.MustParseAddr(publicOverlayIP)] = publicConsumerIdentity(guestPub)
	return srv, ownerPrivK, guestPrivK, time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC)
}

func do(srv *Server, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	return rec
}

// TestPublicShareGate_FailClosedByDefault: with no publicShareController
// wired (Config.IsPublicShareDenied nil), a public-grant consumer is
// rejected with waired_inference_not_public while the owner peer is
// served — serving strangers is strictly opt-in.
func TestPublicShareGate_FailClosedByDefault(t *testing.T) {
	gw := newFakeGateway()
	srv, ownerPriv, guestPriv, at := newPublicOverlayServer(t, gw)

	rec := do(srv, signedReqFrom(t, publicOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-guest-1", guestPriv, at))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "waired_inference_not_public") {
		t.Fatalf("public consumer: got %d %q, want 503 waired_inference_not_public", rec.Code, rec.Body.String())
	}
	rec = do(srv, signedReqFrom(t, peerOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-owner", ownerPriv, at))
	if rec.Code != http.StatusOK {
		t.Fatalf("owner peer: got %d %q, want 200", rec.Code, rec.Body.String())
	}
}

// TestPublicShareGate_FollowsController: the gate tracks the live
// controller state — ON serves public consumers, OFF rejects with the
// typed envelope.
func TestPublicShareGate_FollowsController(t *testing.T) {
	var denied atomic.Bool
	gw := newFakeGateway()
	srv, _, guestPriv, at := newPublicOverlayServer(t, gw, func(c *Config) {
		c.IsPublicShareDenied = denied.Load
	})

	rec := do(srv, signedReqFrom(t, publicOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-guest-1", guestPriv, at))
	if rec.Code != http.StatusOK {
		t.Fatalf("public ON: got %d %q, want 200", rec.Code, rec.Body.String())
	}
	denied.Store(true)
	rec = do(srv, signedReqFrom(t, publicOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-guest-1", guestPriv, at))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "waired_inference_not_public") {
		t.Fatalf("public OFF: got %d %q, want 503 waired_inference_not_public", rec.Code, rec.Body.String())
	}
}

// TestShareGate_PublicConsumerBypassesMeshShare: mesh-share OFF rejects
// same-network peers with waired_inference_not_shared, but a public
// consumer's admission is governed by the public gates (spec §8.1) —
// with Public Share ON it is served regardless of the mesh toggle.
func TestShareGate_PublicConsumerBypassesMeshShare(t *testing.T) {
	gw := newFakeGateway()
	srv, ownerPriv, guestPriv, at := newPublicOverlayServer(t, gw, func(c *Config) {
		c.IsShareDenied = func() bool { return true }
		c.IsPublicShareDenied = func() bool { return false }
	})

	rec := do(srv, signedReqFrom(t, peerOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-owner", ownerPriv, at))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "waired_inference_not_shared") {
		t.Fatalf("owner with mesh-share off: got %d %q, want 503 waired_inference_not_shared", rec.Code, rec.Body.String())
	}
	rec = do(srv, signedReqFrom(t, publicOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-guest-1", guestPriv, at))
	if rec.Code != http.StatusOK {
		t.Fatalf("public consumer with mesh-share off but public on: got %d %q, want 200", rec.Code, rec.Body.String())
	}
}

// TestPublicAdmission_HeadroomDefault: with PublicCapacity unset the
// public ceiling is min(2, capacity−1). Capacity 3 ⇒ 2 public slots:
// the third concurrent public request is rejected while the owner still
// fits in the remaining total slot.
func TestPublicAdmission_HeadroomDefault(t *testing.T) {
	gw := newBlockingGateway()
	srv, ownerPriv, guestPriv, at := newPublicOverlayServer(t, gw, func(c *Config) {
		c.Capacity = 3
		c.IsPublicShareDenied = func() bool { return false }
	})
	results := make(chan int, 4)
	for range 2 {
		go func() {
			results <- do(srv, signedReqFrom(t, publicOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-guest-1", guestPriv, at)).Code
		}()
	}
	gw.waitForInFlight(t, 2)

	rec := do(srv, signedReqFrom(t, publicOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-guest-1", guestPriv, at))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "waired_inference_overloaded") {
		t.Fatalf("3rd public: got %d %q, want 503 waired_inference_overloaded", rec.Code, rec.Body.String())
	}
	if got := srv.PublicInflightCount(); got != 2 {
		t.Fatalf("PublicInflightCount = %d, want 2", got)
	}

	go func() {
		results <- do(srv, signedReqFrom(t, peerOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-owner", ownerPriv, at)).Code
	}()
	gw.waitForInFlight(t, 3)

	for range 3 {
		if !gw.release() {
			t.Fatal("release failed")
		}
	}
	for range 3 {
		if code := <-results; code != http.StatusOK {
			t.Fatalf("parked request finished with %d, want 200", code)
		}
	}
}

// TestPublicAdmission_NoHeadroomClosesPublic: capacity 1 leaves no
// headroom (min(2, 0) = 0) — public consumers are rejected outright
// even when idle, and the owner is unaffected.
func TestPublicAdmission_NoHeadroomClosesPublic(t *testing.T) {
	gw := newFakeGateway()
	srv, ownerPriv, guestPriv, at := newPublicOverlayServer(t, gw, func(c *Config) {
		c.Capacity = 1
		c.IsPublicShareDenied = func() bool { return false }
	})
	rec := do(srv, signedReqFrom(t, publicOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-guest-1", guestPriv, at))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "waired_inference_overloaded") {
		t.Fatalf("public at capacity 1: got %d %q, want 503 waired_inference_overloaded", rec.Code, rec.Body.String())
	}
	rec = do(srv, signedReqFrom(t, peerOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-owner", ownerPriv, at))
	if rec.Code != http.StatusOK {
		t.Fatalf("owner at capacity 1: got %d, want 200", rec.Code)
	}
}

// TestPublicAdmission_SetPublicCapacityLive: the CP-served
// PublicCapacity overrides the headroom default and retunes live.
func TestPublicAdmission_SetPublicCapacityLive(t *testing.T) {
	gw := newBlockingGateway()
	srv, _, guestPriv, at := newPublicOverlayServer(t, gw, func(c *Config) {
		c.Capacity = 10
		c.IsPublicShareDenied = func() bool { return false }
	})
	srv.SetPublicCapacity(1)

	results := make(chan int, 2)
	go func() {
		results <- do(srv, signedReqFrom(t, publicOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-guest-1", guestPriv, at)).Code
	}()
	gw.waitForInFlight(t, 1)

	rec := do(srv, signedReqFrom(t, publicOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-guest-1", guestPriv, at))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("2nd public at cap 1: got %d, want 503", rec.Code)
	}

	srv.SetPublicCapacity(2)
	go func() {
		results <- do(srv, signedReqFrom(t, publicOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-guest-1", guestPriv, at)).Code
	}()
	gw.waitForInFlight(t, 2)
	for range 2 {
		if !gw.release() {
			t.Fatal("release failed")
		}
	}
	for range 2 {
		if code := <-results; code != http.StatusOK {
			t.Fatalf("parked request finished with %d, want 200", code)
		}
	}
}

// TestOwnerPriorityLatch: an owner request rejected at capacity pauses
// new public admissions for 30s (refreshed per attempt); public
// admission recovers once the latch expires (spec §8.2).
func TestOwnerPriorityLatch(t *testing.T) {
	base := time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC)
	var offset atomic.Int64 // seconds added to base
	now := func() time.Time { return base.Add(time.Duration(offset.Load()) * time.Second) }

	gw := newBlockingGateway()
	srv, ownerPriv, guestPriv, _ := newPublicOverlayServer(t, gw, func(c *Config) {
		c.Capacity = 2
		c.PublicCapacity = 2
		c.IsPublicShareDenied = func() bool { return false }
		c.Now = now
	})

	// Fill both total slots with public requests.
	results := make(chan int, 2)
	for range 2 {
		go func() {
			results <- do(srv, signedReqFrom(t, publicOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-guest-1", guestPriv, now())).Code
		}()
	}
	gw.waitForInFlight(t, 2)

	// Owner rejected at capacity → latch.
	rec := do(srv, signedReqFrom(t, peerOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-owner", ownerPriv, now()))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "waired_inference_overloaded") {
		t.Fatalf("owner at capacity: got %d %q, want 503 waired_inference_overloaded", rec.Code, rec.Body.String())
	}

	// Drain the public requests; slots are now free but the latch holds.
	for range 2 {
		gw.release()
	}
	for range 2 {
		<-results
	}
	rec = do(srv, signedReqFrom(t, publicOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-guest-1", guestPriv, now()))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("public during latch: got %d, want 503", rec.Code)
	}

	// Past the 30s window the latch expires and public admission
	// recovers. The request parks in the blocking gateway, so drive it
	// from a goroutine and release it once admitted.
	offset.Store(31)
	go func() {
		results <- do(srv, signedReqFrom(t, publicOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-guest-1", guestPriv, now())).Code
	}()
	gw.waitForInFlight(t, 1)
	gw.release()
	if code := <-results; code != http.StatusOK {
		t.Fatalf("public after latch expiry: got %d, want 200", code)
	}
}

// TestOwnerPriorityLatch_ArrivalAtSaturation: an owner request that
// takes the LAST total slot also latches — the owner is signalling
// demand, so public admission pauses while in-flight publics drain.
func TestOwnerPriorityLatch_ArrivalAtSaturation(t *testing.T) {
	gw := newBlockingGateway()
	srv, ownerPriv, guestPriv, at := newPublicOverlayServer(t, gw, func(c *Config) {
		c.Capacity = 2
		c.PublicCapacity = 2
		c.IsPublicShareDenied = func() bool { return false }
	})

	results := make(chan int, 2)
	go func() {
		results <- do(srv, signedReqFrom(t, publicOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-guest-1", guestPriv, at)).Code
	}()
	gw.waitForInFlight(t, 1)

	// Owner takes the last slot → arrival at saturation → latch.
	go func() {
		results <- do(srv, signedReqFrom(t, peerOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-owner", ownerPriv, at)).Code
	}()
	gw.waitForInFlight(t, 2)

	// Free one slot; the latch still blocks new public admissions.
	gw.release()
	rec := do(srv, signedReqFrom(t, publicOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-guest-1", guestPriv, at))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("public during saturation latch: got %d, want 503", rec.Code)
	}

	gw.release()
	for range 2 {
		<-results
	}
}

// TestKillSwitch_AbortsPublicInFlight: AbortPublicInFlight cancels the
// contexts of in-flight PUBLIC requests only; owner streams are
// untouched (spec §8.3 step 1).
func TestKillSwitch_AbortsPublicInFlight(t *testing.T) {
	gw := newCtxGateway()
	var denied atomic.Bool
	srv, ownerPriv, guestPriv, at := newPublicOverlayServer(t, gw, func(c *Config) {
		c.IsPublicShareDenied = denied.Load
	})

	done := make(chan int, 2)
	go func() {
		done <- do(srv, signedReqFrom(t, publicOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-guest-1", guestPriv, at)).Code
	}()
	go func() {
		done <- do(srv, signedReqFrom(t, peerOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-owner", ownerPriv, at)).Code
	}()
	gw.waitParked(t, 2)

	// Kill switch: deny new + abort in-flight public.
	denied.Store(true)
	srv.AbortPublicInFlight()
	gw.waitCancelled(t, 1)
	if code := <-done; code != http.StatusServiceUnavailable {
		t.Fatalf("aborted public request finished with %d, want 503", code)
	}
	if got := gw.cancelled.Load(); got != 1 {
		t.Fatalf("cancelled = %d, want exactly 1 (owner untouched)", got)
	}

	// The owner request is still parked; release it normally.
	if !gw.releaseOne() {
		t.Fatal("owner release failed")
	}
	if code := <-done; code != http.StatusOK {
		t.Fatalf("owner request finished with %d, want 200", code)
	}
	if got := srv.PublicInflightCount(); got != 0 {
		t.Fatalf("PublicInflightCount after abort = %d, want 0", got)
	}
}
