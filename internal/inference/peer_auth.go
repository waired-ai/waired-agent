package inference

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/proto/signedreq"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// PeerIdentity carries the bits of a NetworkMap peer that the
// overlay-side auth middleware needs to verify an inbound signed
// request. It is intentionally a tiny value type so the agent's
// network-map snapshot can be transformed into a stable lookup
// without exposing internal mutable state to this package.
type PeerIdentity struct {
	DeviceID   string
	MachineKey ed25519.PublicKey
	// Pseudonym is the name logs and events use for a foreign peer
	// present under a grant, in place of DeviceID, so real device
	// identifiers of other accounts never land in local logs (public
	// share spec §8.5): the grant pseudonym for a Public Share peer, the
	// "<device> (<owner>)" label for a Team Share peer. Empty for
	// same-network peers.
	Pseudonym string
	// Grant carries the netmap PeerGrant for foreign peers present
	// under a grant (nil for same-network peers). The serving-side gate
	// chain branches on it (public share spec §8.1, team share spec
	// §6.2): a public consumer rides the public gates, a team consumer
	// rides the team gate, neither rides the mesh shareGate, and any
	// OTHER grant peer is refused outright by grantRoleGate.
	Grant *signer.PeerGrant
}

// DisplayName returns the identifier to use for this peer in logs and
// events: the grant pseudonym when present, the real DeviceID for a
// same-network peer.
//
// A grant peer with no pseudonym gets the label the display surfaces use
// for it, never its DeviceID. That case used to fall through to the
// DeviceID, which for a Public Share peer is the identifier public share
// spec §8.5 keeps out of logs (waired-agent#1368).
func (p PeerIdentity) DisplayName() string {
	if p.Pseudonym != "" {
		return p.Pseudonym
	}
	if p.Grant != nil {
		if inferencemesh.IsTeamGrant(p.Grant) {
			return inferencemesh.TeamPeerFallbackLabel
		}
		return inferencemesh.PublicPeerLabelFor(p.Grant.ID)
	}
	return p.DeviceID
}

// IsPublicConsumer reports whether this peer is a foreign device
// consuming this agent's inference under a Public Share grant — the
// request class subject to publicShareGate / publicAdmissionGate
// rather than the intra-account shareGate. Public entries carry one of
// the two single roles only.
func (p PeerIdentity) IsPublicConsumer() bool {
	return p.Grant != nil && p.Grant.Kind == signer.GrantKindPublic && p.Grant.Role == signer.GrantRoleConsumer
}

// IsTeamConsumer reports whether this peer is a teammate's device
// consuming this agent's inference under a Team Share grant (team share
// spec §6.2) — the request class subject to teamShareGate. It skips the
// public gates and counts against total capacity only, on the same
// footing as this account's own computers.
//
// GrantRoleBoth counts: two teammates sharing their machines with each
// other have grants in both directions, and the map carries one entry,
// so the role says both.
func (p PeerIdentity) IsTeamConsumer() bool {
	if p.Grant == nil || p.Grant.Kind != signer.GrantKindTeam {
		return false
	}
	return p.Grant.Role == signer.GrantRoleConsumer || p.Grant.Role == signer.GrantRoleBoth
}

// IsGrantConsumer reports whether this peer consumes this agent's
// inference under one of the two grant kinds that entitle it to: a
// public consumer or a team consumer. grantRoleGate refuses every other
// grant peer.
func (p PeerIdentity) IsGrantConsumer() bool {
	return p.IsPublicConsumer() || p.IsTeamConsumer()
}

// IsForeignGrantPeer reports whether this peer belongs to another
// account and is present in our NetworkMap only because the control
// plane injected it under a grant (spec §7.1). Same-network peers
// carry no grant annotation, so Grant != nil is the account boundary.
func (p PeerIdentity) IsForeignGrantPeer() bool { return p.Grant != nil }

// grantRoleGate refuses inbound serving requests from foreign grant
// peers that are neither public nor team consumers — in practice the
// provider-role peers whose engines WE borrow (waired#896).
//
// Such a peer is reachable on the overlay because consuming from it
// requires a bidirectional WireGuard peering, but the grant entitles it
// to nothing on our side: the consumer-side router only ever probes and
// routes to provider-role peers (internal/router/public_candidates.go),
// so no legitimate request flows in this direction. Without this gate
// the peer classified as "not a public consumer" and therefore skipped
// publicShareGate / publicAdmissionGate entirely, fell through the
// default-ON mesh shareGate, and was accounted as an OWNER request by
// capacityGate — full local capacity, no kill switch, and the ability
// to trip the owner-priority latch against our own guests.
//
// Classification is fail-closed: only the documented consumer pairs —
// public/consumer, and team/consumer or team/both — ride on. Team was
// widened explicitly (team share spec §6.2, waired#1374); a provider
// role, "both" on a public grant, a missing Role, or an empty or unknown
// Kind still refuses rather than degrading to mesh trust.
//
// Placed between wgPeerOnly (which resolves the peer) and
// verifyPeerSignature (which consumes a nonce), so a refused peer never
// spends entries in the bounded nonce cache (spec §8.5).
func grantRoleGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if peer, ok := PeerFromContext(r.Context()); ok && peer.IsForeignGrantPeer() && !peer.IsGrantConsumer() {
			writePeerAuthError(w, http.StatusForbidden, "grant_not_consumer",
				"this peer's grant does not entitle it to consume inference from this device")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// PeerLookup resolves a WG-source overlay IP to the peer that owns
// it. ok=false when no peer in the current NetworkMap matches.
//
// Implementations must be safe for concurrent use; the agent is
// allowed to swap its underlying snapshot at any time (every
// network-map redelivery from the CP).
type PeerLookup interface {
	LookupByOverlayIP(ip netip.Addr) (PeerIdentity, bool)
}

// PeerLookupFunc is the function-shape adapter. NetworkMap snapshot
// holders typically implement LookupByOverlayIP directly; tests use
// this shape to inject a fixed map.
type PeerLookupFunc func(ip netip.Addr) (PeerIdentity, bool)

// LookupByOverlayIP implements PeerLookup.
func (f PeerLookupFunc) LookupByOverlayIP(ip netip.Addr) (PeerIdentity, bool) {
	return f(ip)
}

// peerCtxKeyType is the unexported type for the context key that
// carries the resolved peer's identity through the middleware chain
// into the handler. Handlers can read it via PeerFromContext for
// audit logging or per-peer routing decisions; it never leaves the
// agent process.
type peerCtxKeyType struct{}

var peerCtxKey = peerCtxKeyType{}

// PeerFromContext returns the resolved PeerIdentity recorded by
// wgPeerOnly + verifyPeerSignature. ok=false when called outside the
// peer-auth chain (e.g., from the loopback handler set or an
// anonymous /waired/v1/ping handler).
// ContextWithPeer attaches p under the same key wgPeerOnly uses, so
// callers outside this package — notably package-main tests that must
// synthesise a public-consumer request context — can build a context
// PeerFromContext will read back. Production code has no reason to call
// it: the auth middleware is the only writer.
func ContextWithPeer(ctx context.Context, p PeerIdentity) context.Context {
	return context.WithValue(ctx, peerCtxKey, p)
}

func PeerFromContext(ctx context.Context) (PeerIdentity, bool) {
	p, ok := ctx.Value(peerCtxKey).(PeerIdentity)
	return p, ok
}

// wgPeerOnly rejects requests whose RemoteAddr does NOT resolve to a
// peer in the current NetworkMap. Phase 4's threat model relies on
// WireGuard's source-IP↔peer-pubkey binding to rule out off-mesh
// senders, but that's a packet-layer guarantee — adding this
// HTTP-layer check ensures a misconfigured listener (bound to 0.0.0.0
// instead of the overlay IP, say) doesn't inadvertently expose the
// peer-engine surface to unrelated networks.
//
// On match, the resolved PeerIdentity is attached to the request
// context so the next layer (verifyPeerSignature) doesn't have to
// repeat the lookup.
func wgPeerOnly(next http.Handler, lookup PeerLookup) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if lookup == nil {
			writePeerAuthError(w, http.StatusForbidden, "peer_lookup_unconfigured",
				"this listener was started without a NetworkMap-backed peer lookup")
			return
		}
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		ip, err := netip.ParseAddr(host)
		if err != nil {
			writePeerAuthError(w, http.StatusForbidden, "remote_addr_unparseable",
				"could not parse RemoteAddr as IP")
			return
		}
		peer, ok := lookup.LookupByOverlayIP(ip)
		if !ok {
			writePeerAuthError(w, http.StatusForbidden, "unknown_peer",
				"source overlay IP is not in the current NetworkMap")
			return
		}
		ctx := context.WithValue(r.Context(), peerCtxKey, peer)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// verifyPeerSignature reads the request body (capping it at maxBody),
// reconstructs the signedreq canonical bytes from the HTTP envelope
// headers, and Ed25519-verifies them against the peer's MachineKey
// resolved by wgPeerOnly. Successful verification consumes the nonce
// against cache for replay-rejection and re-attaches the buffered
// body so the downstream handler reads the same bytes that were
// signed.
//
// Replay + clock-skew checks fire BEFORE the downstream handler runs
// (a request that fails auth must never reach the gateway HandlerSet),
// but the nonce is consumed AFTER signature verification so an
// attacker who guesses random body bytes can't burn legitimate
// nonces.
func verifyPeerSignature(
	next http.Handler,
	lookup PeerLookup,
	nonces nonceCache,
	skew, nonceTTL time.Duration,
	maxBody int64,
	now func() time.Time,
) http.Handler {
	if now == nil {
		now = time.Now
	}
	if maxBody <= 0 {
		maxBody = DefaultMaxBodyBytes
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, ok := PeerFromContext(r.Context())
		if !ok {
			// This indicates a wiring bug: verifyPeerSignature was
			// installed without wgPeerOnly preceding it. Defensive
			// 500 rather than silently letting an unauthenticated
			// request through.
			writePeerAuthError(w, http.StatusInternalServerError, "peer_context_missing",
				"verifyPeerSignature must follow wgPeerOnly")
			return
		}
		body, err := signedreq.ReadBody(r.Body, maxBody)
		if err != nil {
			writeSignedReqAuthError(w, err)
			return
		}
		_, nonce, err := signedreq.VerifyHeaderEnvelope(r.Header, body, peer.MachineKey, peer.DeviceID, now(), skew)
		if err != nil {
			writeSignedReqAuthError(w, err)
			return
		}
		if err := signedreq.ConsumeNonce(nonces, peer.DeviceID, nonce, now(), nonceTTL); err != nil {
			writeSignedReqAuthError(w, err)
			return
		}
		// Re-attach the buffered body so the downstream handler reads
		// the same bytes the signature covers.
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		slog.DebugContext(r.Context(), "overlay peer request authenticated",
			"device", peer.DisplayName(), "path", r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

// writePeerAuthError emits the same JSON shape the gateway uses so
// proxied responses on the loopback side don't see schema drift.
func writePeerAuthError(w http.ResponseWriter, status int, errType, msg string) {
	slog.Debug("overlay peer auth rejected", "type", errType, "status", status)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": msg,
		},
	})
}

// writeSignedReqAuthError translates a signedreq sentinel error into
// the overlay JSON envelope. Status is taken from signedreq.HTTPStatus
// so the 401/400/413/etc mapping matches the CP signed-write contract.
func writeSignedReqAuthError(w http.ResponseWriter, err error) {
	status := signedreq.HTTPStatus(err)
	code, msg := overlaySignedReqCode(err)
	writePeerAuthError(w, status, code, msg)
}

func overlaySignedReqCode(err error) (string, string) {
	switch {
	case errors.Is(err, signedreq.ErrMissingSignature):
		return "missing_body_signature", "X-Waired-Body-Signature header is missing"
	case errors.Is(err, signedreq.ErrSignatureEncoding):
		return "invalid_body_signature_encoding", "expected base64 ed25519 signature (64 bytes)"
	case errors.Is(err, signedreq.ErrSignatureMismatch):
		return "body_signature_mismatch", "signature does not match request bytes"
	case errors.Is(err, signedreq.ErrBodyTooLarge):
		return "body_too_large", err.Error()
	case errors.Is(err, signedreq.ErrBodyRead):
		return "body_read", err.Error()
	case errors.Is(err, signedreq.ErrBadIssuedAt):
		return "bad_issued_at", err.Error()
	case errors.Is(err, signedreq.ErrIssuedAtOutOfRange):
		return "issued_at_out_of_window", err.Error()
	case errors.Is(err, signedreq.ErrBadNonce):
		return "bad_nonce", "X-Waired-Nonce missing or shorter than minimum"
	case errors.Is(err, signedreq.ErrDeviceMismatch):
		return "device_id_mismatch", "X-Waired-Device disagrees with the WG source peer"
	case errors.Is(err, signedreq.ErrReplayDetected):
		return "replay_detected", "nonce already seen within TTL"
	default:
		return "internal_error", err.Error()
	}
}
