package inference

import (
	"crypto/ed25519"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// providerOverlayIP is the third fixed peer slot: a foreign peer present
// under a Public Share grant in which WE are the consumer — i.e. a node
// we borrow inference from. It must never be able to serve-side consume
// US (waired#896).
const providerOverlayIP = "100.96.0.30"

// grantPeerIdentity synthesises a foreign grant peer with a caller-chosen
// grant Kind/Role, so the gate can be pinned across the whole value space
// rather than just the documented "public"/"consumer" pair.
func grantPeerIdentity(pub ed25519.PublicKey, deviceID, kind, role string) PeerIdentity {
	return PeerIdentity{
		DeviceID:   deviceID,
		MachineKey: pub,
		Pseudonym:  "pub-node-1",
		Grant: &signer.PeerGrant{
			ID:        "grant_test2",
			Kind:      kind,
			Role:      role,
			Pseudonym: "pub-node-1",
		},
	}
}

// newGrantRoleServer builds a three-peer overlay server: the same-network
// owner, a public-grant consumer (guest), and a foreign grant peer whose
// Kind/Role the caller picks (the attacker slot).
func newGrantRoleServer(t *testing.T, gw gatewayHandlerSet, kind, role string, opts ...func(*Config)) (
	srv *Server, ownerPriv, guestPriv, foreignPriv ed25519.PrivateKey, at time.Time,
) {
	t.Helper()
	ownerPub, ownerPrivK := mustKey(t)
	guestPub, guestPrivK := mustKey(t)
	foreignPub, foreignPrivK := mustKey(t)
	srv, peers, _ := newOverlayServer(t, gw, PeerIdentity{DeviceID: "dev-owner", MachineKey: ownerPub}, opts...)
	peers[netip.MustParseAddr(publicOverlayIP)] = publicConsumerIdentity(guestPub)
	peers[netip.MustParseAddr(providerOverlayIP)] = grantPeerIdentity(foreignPub, "dev-foreign-1", kind, role)
	return srv, ownerPrivK, guestPrivK, foreignPrivK, time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC)
}

// TestGrantRoleGate_ProviderRolePeerRejected: a peer we hold a grant with
// as the CONSUMER (Role=="provider" from our side) has no serving-side
// business here. It is rejected with 403 grant_not_consumer regardless of
// the operator toggles — the pre-fix behaviour let it ride the mesh
// shareGate (default ON) at full capacity, bypassing every public gate
// (waired#896, spec §8.1).
func TestGrantRoleGate_ProviderRolePeerRejected(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		shareDenied, pubDenied bool
	}{
		{"mesh on, public on", false, false},
		{"mesh on, public off", false, true},
		{"mesh off, public on", true, false},
		{"mesh off, public off", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw := newFakeGateway()
			srv, _, _, foreignPriv, at := newGrantRoleServer(t, gw, "public", "provider", func(c *Config) {
				c.IsShareDenied = func() bool { return tc.shareDenied }
				c.IsPublicShareDenied = func() bool { return tc.pubDenied }
			})

			rec := do(srv, signedReqFrom(t, providerOverlayIP, "/anthropic/v1/messages", []byte(`{}`), "dev-foreign-1", foreignPriv, at))
			if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "grant_not_consumer") {
				t.Fatalf("provider-role grant peer: got %d %q, want 403 grant_not_consumer", rec.Code, rec.Body.String())
			}
			if hits := gw.Hits(); hits != 0 {
				t.Fatalf("gateway hits = %d, want 0 — the request must never reach the engine", hits)
			}
		})
	}
}

// TestGrantRoleGate_UnknownGrantClassRejected: classification is
// fail-closed. Anything carrying a grant that is not exactly the
// documented public/consumer pair — a reserved Kind, a missing Role, an
// unknown role string — is refused rather than falling through to the
// mesh trust path.
func TestGrantRoleGate_UnknownGrantClassRejected(t *testing.T) {
	for _, tc := range []struct{ name, kind, role string }{
		{"reserved kind", "team", "consumer"},
		{"empty role", "public", ""},
		{"unknown role", "public", "peer"},
		{"empty kind", "", "consumer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw := newFakeGateway()
			srv, _, _, foreignPriv, at := newGrantRoleServer(t, gw, tc.kind, tc.role, func(c *Config) {
				c.IsPublicShareDenied = func() bool { return false }
			})

			rec := do(srv, signedReqFrom(t, providerOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-foreign-1", foreignPriv, at))
			if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "grant_not_consumer") {
				t.Fatalf("grant kind=%q role=%q: got %d %q, want 403 grant_not_consumer", tc.kind, tc.role, rec.Code, rec.Body.String())
			}
			if hits := gw.Hits(); hits != 0 {
				t.Fatalf("gateway hits = %d, want 0", hits)
			}
		})
	}
}

// TestGrantRoleGate_RunsBeforeSignatureVerification: the gate sits between
// wgPeerOnly and verifyPeerSignature, so a rejected peer cannot spend
// entries in the (bounded, §8.5) nonce cache under its device ID. A
// request signed with the WRONG key still fails as grant_not_consumer
// rather than reaching the signature check.
func TestGrantRoleGate_RunsBeforeSignatureVerification(t *testing.T) {
	gw := newFakeGateway()
	srv, ownerPriv, _, _, at := newGrantRoleServer(t, gw, "public", "provider", func(c *Config) {
		c.IsPublicShareDenied = func() bool { return false }
	})

	// Signed with the owner's key but sent from the foreign peer's slot.
	rec := do(srv, signedReqFrom(t, providerOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-foreign-1", ownerPriv, at))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "grant_not_consumer") {
		t.Fatalf("got %d %q, want 403 grant_not_consumer before signature verification", rec.Code, rec.Body.String())
	}
}

// TestGrantRoleGate_HealthzRejected: /waired/v1/inference/healthz rides
// peerAuthOnly (gates deliberately bypassed so probes read gate state),
// but the grant-role decision is authorization, not an operator gate —
// a provider-role peer gets 403 there too, while the public consumer
// that legitimately probes before routing still gets its snapshot.
func TestGrantRoleGate_HealthzRejected(t *testing.T) {
	gw := newFakeGateway()
	srv, _, guestPriv, foreignPriv, at := newGrantRoleServer(t, gw, "public", "provider", func(c *Config) {
		c.IsPublicShareDenied = func() bool { return false }
	})

	r := newSignedGetRequest(t, "/waired/v1/inference/healthz", "dev-foreign-1", foreignPriv, at)
	r.RemoteAddr = providerOverlayIP + ":54321"
	if rec := do(srv, r); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "grant_not_consumer") {
		t.Fatalf("provider-role healthz: got %d %q, want 403 grant_not_consumer", rec.Code, rec.Body.String())
	}

	r = newSignedGetRequest(t, "/waired/v1/inference/healthz", "dev-guest-1", guestPriv, at)
	r.RemoteAddr = publicOverlayIP + ":54321"
	if rec := do(srv, r); rec.Code != http.StatusOK {
		t.Fatalf("public-consumer healthz: got %d %q, want 200", rec.Code, rec.Body.String())
	}
}

// TestGrantRoleGate_NoOwnerLatchFromForeignPeer: pre-fix, capacityGate
// counted a provider-role peer as an OWNER request, so a stranger could
// set the owner-priority latch and 503 the node's legitimate public
// guests (spec §8.2 turned against its owner). The rejection now happens
// before any admission accounting, so public admission is unaffected.
func TestGrantRoleGate_NoOwnerLatchFromForeignPeer(t *testing.T) {
	gw := newBlockingGateway()
	srv, _, guestPriv, foreignPriv, at := newGrantRoleServer(t, gw, "public", "provider", func(c *Config) {
		c.Capacity = 2
		c.PublicCapacity = 2
		c.IsPublicShareDenied = func() bool { return false }
	})

	// Saturate both total slots with public guests.
	results := make(chan int, 3)
	for range 2 {
		go func() {
			results <- do(srv, signedReqFrom(t, publicOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-guest-1", guestPriv, at)).Code
		}()
	}
	gw.waitForInFlight(t, 2)

	// The foreign peer arrives at saturation: 403, and no latch.
	rec := do(srv, signedReqFrom(t, providerOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-foreign-1", foreignPriv, at))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign peer at capacity: got %d %q, want 403", rec.Code, rec.Body.String())
	}
	if srv.public.latched(at) {
		t.Fatal("owner-priority latch set by a foreign grant peer")
	}

	// Drain one slot; a public guest is admitted immediately — proof the
	// latch never engaged.
	gw.release()
	go func() {
		results <- do(srv, signedReqFrom(t, publicOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-guest-1", guestPriv, at)).Code
	}()
	gw.waitForInFlight(t, 2)
	for range 3 {
		gw.release()
	}
	for range 3 {
		if code := <-results; code != http.StatusOK {
			t.Fatalf("public guest: got %d, want 200", code)
		}
	}
}

// TestGrantRoleGate_MeshAndConsumerPeersUnaffected: the gate only fires on
// grant-carrying peers that are not public consumers. Same-network peers
// (Grant == nil) and public-grant consumers keep their existing paths.
func TestGrantRoleGate_MeshAndConsumerPeersUnaffected(t *testing.T) {
	gw := newFakeGateway()
	srv, ownerPriv, guestPriv, _, at := newGrantRoleServer(t, gw, "public", "provider", func(c *Config) {
		c.IsPublicShareDenied = func() bool { return false }
	})

	if rec := do(srv, signedReqFrom(t, peerOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-owner", ownerPriv, at)); rec.Code != http.StatusOK {
		t.Fatalf("same-network peer: got %d %q, want 200", rec.Code, rec.Body.String())
	}
	if rec := do(srv, signedReqFrom(t, publicOverlayIP, "/v1/chat/completions", []byte(`{}`), "dev-guest-1", guestPriv, at)); rec.Code != http.StatusOK {
		t.Fatalf("public consumer: got %d %q, want 200", rec.Code, rec.Body.String())
	}
}
