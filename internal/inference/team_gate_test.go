package inference

import (
	"crypto/ed25519"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// The pins in this file are product contract: team share spec §6.2 and
// the owner rulings recorded for implementation in waired#1370 — team
// consumers are admitted, gated by the owner's team switch alone, cut by
// that switch's kill, and served on the owner's footing (total capacity
// only, never the public ceiling, and a refusal latches public guests
// out).

// teamOverlayIP is the fourth fixed peer slot: a teammate's device
// consuming this node under a Team Share grant.
const teamOverlayIP = "100.96.0.40"

func teamConsumerIdentity(pub ed25519.PublicKey) PeerIdentity {
	return PeerIdentity{
		DeviceID:   "dev-teammate-1",
		MachineKey: pub,
		Pseudonym:  "laptop (Alice Example)",
		Grant: &signer.PeerGrant{
			ID:          "grant_team1",
			Kind:        signer.GrantKindTeam,
			Role:        "consumer",
			DisplayName: "Alice Example",
		},
	}
}

// newTeamOverlayServer builds a three-peer overlay server: the owner's
// own computer, a public guest, and a teammate.
func newTeamOverlayServer(t *testing.T, gw gatewayHandlerSet, opts ...func(*Config)) (
	srv *Server, ownerPriv, guestPriv, teamPriv ed25519.PrivateKey, at time.Time,
) {
	t.Helper()
	ownerPub, ownerPrivK := mustKey(t)
	guestPub, guestPrivK := mustKey(t)
	teamPub, teamPrivK := mustKey(t)
	srv, peers, _ := newOverlayServer(t, gw, PeerIdentity{DeviceID: "dev-owner", MachineKey: ownerPub}, opts...)
	peers[netip.MustParseAddr(publicOverlayIP)] = publicConsumerIdentity(guestPub)
	peers[netip.MustParseAddr(teamOverlayIP)] = teamConsumerIdentity(teamPub)
	return srv, ownerPrivK, guestPrivK, teamPrivK, time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC)
}

func teamReq(t *testing.T, priv ed25519.PrivateKey, at time.Time) *http.Request {
	t.Helper()
	return signedReqFrom(t, teamOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-teammate-1", priv, at)
}

// TestPeerIdentity_ConsumerClasses pins the classification the gate chain
// branches on across the grant value space.
func TestPeerIdentity_ConsumerClasses(t *testing.T) {
	for _, tc := range []struct {
		name               string
		grant              *signer.PeerGrant
		public, team, both bool
	}{
		{"own network", nil, false, false, false},
		{"public consumer", &signer.PeerGrant{Kind: "public", Role: "consumer"}, true, false, true},
		{"team consumer", &signer.PeerGrant{Kind: "team", Role: "consumer"}, false, true, true},
		{"team both ways", &signer.PeerGrant{Kind: "team", Role: "both"}, false, true, true},
		{"public provider", &signer.PeerGrant{Kind: "public", Role: "provider"}, false, false, false},
		{"public both (never sent)", &signer.PeerGrant{Kind: "public", Role: "both"}, false, false, false},
		{"team provider", &signer.PeerGrant{Kind: "team", Role: "provider"}, false, false, false},
		{"unknown kind", &signer.PeerGrant{Kind: "org", Role: "consumer"}, false, false, false},
		{"empty kind", &signer.PeerGrant{Role: "consumer"}, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := PeerIdentity{Grant: tc.grant}
			if got := p.IsPublicConsumer(); got != tc.public {
				t.Errorf("IsPublicConsumer = %v, want %v", got, tc.public)
			}
			if got := p.IsTeamConsumer(); got != tc.team {
				t.Errorf("IsTeamConsumer = %v, want %v", got, tc.team)
			}
			if got := p.IsGrantConsumer(); got != tc.both {
				t.Errorf("IsGrantConsumer = %v, want %v", got, tc.both)
			}
		})
	}
}

// TestGrantRoleGate_TeamConsumerAdmitted: a teammate consuming this node
// passes grantRoleGate and is served while the team switch is on — the
// "reserved kind" refusal this replaces was the fail-closed placeholder
// until Team Share existed (waired#896, team share spec §6.2).
func TestGrantRoleGate_TeamConsumerAdmitted(t *testing.T) {
	gw := newFakeGateway()
	srv, _, _, teamPriv, at := newTeamOverlayServer(t, gw, func(c *Config) {
		c.IsTeamShareDenied = func() bool { return false }
	})
	rec := do(srv, teamReq(t, teamPriv, at))
	if rec.Code != http.StatusOK {
		t.Fatalf("team consumer with team sharing on: got %d %q, want 200", rec.Code, rec.Body.String())
	}
	if gw.Hits() != 1 {
		t.Fatalf("gateway hits = %d, want 1", gw.Hits())
	}

	// Two teammates sharing with each other: one map entry, role "both".
	// It consumes from us as much as a consumer-role entry does.
	bothPub, bothPriv := mustKey(t)
	bothGW := newFakeGateway()
	bothSrv, peers, _ := newOverlayServer(t, bothGW, PeerIdentity{DeviceID: "dev-owner"}, func(c *Config) {
		c.IsTeamShareDenied = func() bool { return false }
	})
	both := teamConsumerIdentity(bothPub)
	both.Grant.Role = signer.GrantRoleBoth
	peers[netip.MustParseAddr(teamOverlayIP)] = both
	if rec := do(bothSrv, teamReq(t, bothPriv, at)); rec.Code != http.StatusOK {
		t.Fatalf("team peer in both roles: got %d %q, want 200", rec.Code, rec.Body.String())
	}

	r := newSignedGetRequest(t, "/waired/v1/inference/healthz", "dev-teammate-1", teamPriv, at)
	r.RemoteAddr = teamOverlayIP + ":54321"
	if rec := do(srv, r); rec.Code != http.StatusOK {
		t.Fatalf("team consumer healthz: got %d %q, want 200", rec.Code, rec.Body.String())
	}
}

// TestTeamShareGate_FailsClosed: with no controller wired, team consumers
// are refused. Serving another account is opt-in.
func TestTeamShareGate_FailsClosed(t *testing.T) {
	gw := newFakeGateway()
	srv, ownerPriv, _, teamPriv, at := newTeamOverlayServer(t, gw)
	rec := do(srv, teamReq(t, teamPriv, at))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "waired_inference_not_team_shared") {
		t.Fatalf("unwired team gate: got %d %q, want 503 waired_inference_not_team_shared", rec.Code, rec.Body.String())
	}
	if gw.Hits() != 0 {
		t.Fatalf("gateway hits = %d, want 0", gw.Hits())
	}
	// The owner's own computers are not behind the team gate.
	if rec := do(srv, signedReqFrom(t, peerOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-owner", ownerPriv, at)); rec.Code != http.StatusOK {
		t.Fatalf("owner peer: got %d %q, want 200", rec.Code, rec.Body.String())
	}
}

// TestTeamShareGate_IndependentOfOtherSwitches: the team switch governs
// team consumers only. The public switch does not reach them, the mesh
// switch does not reach them (they skip shareGate like public guests),
// and the team switch does not reach public guests.
func TestTeamShareGate_IndependentOfOtherSwitches(t *testing.T) {
	var teamDenied, publicDenied, meshDenied atomic.Bool
	gw := newFakeGateway()
	srv, _, guestPriv, teamPriv, at := newTeamOverlayServer(t, gw, func(c *Config) {
		c.IsTeamShareDenied = teamDenied.Load
		c.IsPublicShareDenied = publicDenied.Load
		c.IsShareDenied = meshDenied.Load
	})
	guest := func() int {
		return do(srv, signedReqFrom(t, publicOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-guest-1", guestPriv, at)).Code
	}
	team := func() *http.Response {
		rec := do(srv, teamReq(t, teamPriv, at))
		return rec.Result()
	}

	publicDenied.Store(true)
	meshDenied.Store(true)
	if resp := team(); resp.StatusCode != http.StatusOK {
		t.Fatalf("team consumer with public and mesh off: got %d, want 200", resp.StatusCode)
	}

	publicDenied.Store(false)
	teamDenied.Store(true)
	if resp := team(); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("team consumer with team off: got %d, want 503", resp.StatusCode)
	}
	if code := guest(); code != http.StatusOK {
		t.Fatalf("public guest with team off: got %d, want 200", code)
	}
}

// TestTeamConsumer_NotCountedAsPublic: team requests never enter public
// admission — they are admitted while the public ceiling is full and
// while the owner-priority latch holds public guests out, and they are
// absent from PublicInflightCount.
func TestTeamConsumer_NotCountedAsPublic(t *testing.T) {
	gw := newBlockingGateway()
	srv, _, guestPriv, teamPriv, at := newTeamOverlayServer(t, gw, func(c *Config) {
		c.Capacity = 4
		c.PublicCapacity = 1
		c.IsPublicShareDenied = func() bool { return false }
		c.IsTeamShareDenied = func() bool { return false }
	})

	results := make(chan int, 3)
	go func() {
		results <- do(srv, signedReqFrom(t, publicOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-guest-1", guestPriv, at)).Code
	}()
	gw.waitForInFlight(t, 1)

	// The public ceiling is full; a teammate is still admitted.
	go func() { results <- do(srv, teamReq(t, teamPriv, at)).Code }()
	gw.waitForInFlight(t, 2)
	if got := srv.PublicInflightCount(); got != 1 {
		t.Fatalf("PublicInflightCount = %d, want 1 (the teammate is not a public guest)", got)
	}
	if got := srv.TeamInflightCount(); got != 1 {
		t.Fatalf("TeamInflightCount = %d, want 1", got)
	}

	// The latch pauses public admission; a teammate is still admitted.
	srv.public.latch(at, ownerPriorityLatchWindow)
	go func() { results <- do(srv, teamReq(t, teamPriv, at)).Code }()
	gw.waitForInFlight(t, 3)

	for range 3 {
		if code := gw.releaseAndWait(t, results); code != http.StatusOK {
			t.Fatalf("parked request finished with %d, want 200", code)
		}
	}
}

// TestTeamConsumer_RefusalAtCapacityLatches: a teammate refused at the
// total ceiling pauses new public admissions, exactly as the owner's own
// request does, and is never admitted past the ceiling (#1367: the latch
// buys priority over public guests, not extra capacity).
func TestTeamConsumer_RefusalAtCapacityLatches(t *testing.T) {
	gw := newBlockingGateway()
	srv, _, guestPriv, teamPriv, at := newTeamOverlayServer(t, gw, func(c *Config) {
		c.Capacity = 1
		c.PublicCapacity = 1
		c.IsPublicShareDenied = func() bool { return false }
		c.IsTeamShareDenied = func() bool { return false }
	})

	results := make(chan int, 1)
	go func() {
		results <- do(srv, signedReqFrom(t, publicOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-guest-1", guestPriv, at)).Code
	}()
	gw.waitForInFlight(t, 1)

	rec := do(srv, teamReq(t, teamPriv, at))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "waired_inference_overloaded") {
		t.Fatalf("teammate at capacity: got %d %q, want 503 waired_inference_overloaded", rec.Code, rec.Body.String())
	}
	if !srv.public.latched(at) {
		t.Fatal("a teammate refused at capacity did not set the owner-priority latch")
	}
	if got := srv.TeamInflightCount(); got != 0 {
		t.Fatalf("TeamInflightCount = %d after the refusal, want 0", got)
	}

	if code := gw.releaseAndWait(t, results); code != http.StatusOK {
		t.Fatalf("drained guest: got %d, want 200", code)
	}
	rec = do(srv, signedReqFrom(t, publicOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-guest-1", guestPriv, at))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("public guest during the latch a teammate set: got %d, want 503", rec.Code)
	}
}

// TestKillSwitch_AbortsTeamInFlight: AbortTeamInFlight cancels running
// team requests only; the owner's and a public guest's streams run on.
func TestKillSwitch_AbortsTeamInFlight(t *testing.T) {
	gw := newCtxGateway()
	var teamDenied atomic.Bool
	srv, ownerPriv, guestPriv, teamPriv, at := newTeamOverlayServer(t, gw, func(c *Config) {
		c.IsTeamShareDenied = teamDenied.Load
		c.IsPublicShareDenied = func() bool { return false }
	})

	done := make(chan int, 3)
	go func() { done <- do(srv, teamReq(t, teamPriv, at)).Code }()
	go func() {
		done <- do(srv, signedReqFrom(t, publicOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-guest-1", guestPriv, at)).Code
	}()
	go func() {
		done <- do(srv, signedReqFrom(t, peerOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-owner", ownerPriv, at)).Code
	}()
	gw.waitParked(t, 3)

	teamDenied.Store(true)
	srv.AbortTeamInFlight()
	gw.waitCancelled(t, 1)
	if code := <-done; code != http.StatusServiceUnavailable {
		t.Fatalf("aborted team request finished with %d, want 503", code)
	}
	if got := gw.cancelled.Load(); got != 1 {
		t.Fatalf("cancelled = %d, want exactly 1 (owner and public guest untouched)", got)
	}
	for range 2 {
		if !gw.releaseOne() {
			t.Fatal("release failed")
		}
		if code := <-done; code != http.StatusOK {
			t.Fatalf("surviving request finished with %d, want 200", code)
		}
	}
	if got := srv.TeamInflightCount(); got != 0 {
		t.Fatalf("TeamInflightCount after abort = %d, want 0", got)
	}

	// New team requests are refused by the gate while the switch is off.
	// Bounded: a regression that admits it would park in ctxGateway for
	// good, and a hang is a worse failure message than this one.
	after := make(chan int, 1)
	go func() { after <- do(srv, teamReq(t, teamPriv, at)).Code }()
	select {
	case code := <-after:
		if code != http.StatusServiceUnavailable {
			t.Fatalf("team request after the kill: got %d, want 503", code)
		}
	case <-time.After(2 * time.Second):
		gw.releaseOne()
		t.Fatal("team request after the kill was admitted to the engine instead of refused")
	}
}

// TestKillSwitch_TeamAbortDuringAdmissionWindow: a team request whose
// switch read happened before the kill but whose registration happens
// after it is refused, not served. Driven through the registry directly,
// as TestPublicAdmission_RegisterCancelRejectsStaleEpoch does for public.
func TestKillSwitch_TeamAbortDuringAdmissionWindow(t *testing.T) {
	var reg cancelRegistry
	epoch := reg.killEpoch()
	reg.abortAll()
	if _, ok := reg.registerCancel(func() {}, epoch); ok {
		t.Fatal("registerCancel accepted a team request admitted before the kill")
	}
	if got := reg.registered(); got != 0 {
		t.Fatalf("registered = %d, want 0", got)
	}
	if _, ok := reg.registerCancel(func() {}, reg.killEpoch()); !ok {
		t.Fatal("registerCancel refused a request that started after the kill")
	}
}
