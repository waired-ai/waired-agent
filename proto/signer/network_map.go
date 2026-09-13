package signer

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
)

// NetworkMap is the signed document Control Plane streams to each device.
// Mirrors docs/specs/waired_control_plane_auth_spec.md §9.2.
//
// Self has the same shape as Peers - the agent identifies its own row by
// matching DeviceID against the device_id stashed in identity.json.
type NetworkMap struct {
	Version            int                 `json:"version"`
	NetworkID          string              `json:"network_id"`
	MapEpoch           int64               `json:"map_epoch"`
	IssuedAt           string              `json:"issued_at"`
	Self               NetworkMapPeer      `json:"self"`
	Peers              []NetworkMapPeer    `json:"peers"`
	Relays             []NetworkMapRelay   `json:"relays,omitempty"`
	ActiveTestScenario *ActiveTestScenario `json:"active_test_scenario,omitempty"`
	Signature          string              `json:"signature,omitempty"`
}

// NetworkMapPeer is the per-peer entry distributed to subscribers. It
// intentionally omits a per-peer signed Device Certificate in step3
// minimum: the Network Map itself is signed by the CP, so peers can
// trust the contained keys via the map's signature. Per-peer certs
// will return in step4 once relays need them for independent verification.
type NetworkMapPeer struct {
	DeviceID      string `json:"device_id"`
	DeviceName    string `json:"device_name"`
	OverlayIP     string `json:"overlay_ip"`
	NodePublicKey string `json:"node_public_key"`
	// PrevNodePublicKey is the peer's previous Node Key during a rotation
	// grace window (auth spec §13.3, #228). Advisory metadata only: the
	// data plane is node-key-agnostic — the relay routes by DeviceID and
	// ignores node keys (bind.go), and WireGuard's single-private-key /
	// one-owner-per-/32 model means the direct path reconverges with a
	// brief handshake-retry blip regardless. It is published so future
	// node-key revocation logic and observability can recognise a key as
	// recently-rotated (not stale/forged) for the grace duration. omitempty
	// keeps the steady-state canonical JSON (and its signature) unchanged
	// for the common no-rotation case. See
	// docs/knowledges/20260526/1710-nodekey-rotation-dataplane-agnostic.md.
	PrevNodePublicKey string              `json:"prev_node_public_key,omitempty"`
	MachinePublicKey  string              `json:"machine_public_key"`
	AllowedServices   []string            `json:"allowed_services"`
	Endpoints         []EndpointCandidate `json:"endpoints,omitempty"`
	OwnerEmail        string              `json:"owner_email,omitempty"`
	LastSeen          string              `json:"last_seen,omitempty"`
	HomeRelay         string              `json:"home_relay,omitempty"`
	// InferenceState is the peer's most recent inference engine push
	// (Phase 3). Nil when the peer has never pushed; agents apply a
	// staleness threshold against InferenceState.LastCheck before
	// counting it toward the mesh aggregate. omitempty keeps engine-
	// less peers from inflating the network map.
	InferenceState *InferenceState `json:"inference_state,omitempty"`
	// Grant marks this peer as a cross-network Public Share peer
	// injected by the CP (public share spec §7): nil for ordinary
	// same-network peers. The CP only emits it to pollers that
	// declared CapabilityPublicShareV1 (§8.4 gate), so with omitempty
	// the signed map stays byte-identical for non-capable agents.
	Grant *PeerGrant `json:"grant,omitempty"`
	// NetworkID is the peer's home network, set by the CP only on
	// cross-network Public Share peers injected alongside Grant (public
	// share spec §7.1): it is the sender-side source for
	// frame.EncryptedPacket.DstNetworkID when addressing relay frames at
	// a foreign-network peer (§10). Empty for ordinary same-network
	// peers — their network is the map's own NetworkID — so with
	// omitempty the signed map bytes stay unchanged wherever it is
	// unset.
	NetworkID string `json:"network_id,omitempty"`
}

// Grant kinds (PeerGrant.Kind). Both sides of the wire compare the
// literal, so the control plane and the agent reference these rather
// than re-spelling the strings.
const (
	// GrantKindPublic is a temporary Public Share guest pass (public
	// share spec §7): pseudonymous, acquired and renewed by the consumer.
	GrantKindPublic = "public"
	// GrantKindTeam is a persistent Team Share grant (team share spec
	// §4.1): derived by the control plane from team membership and the
	// node's share switches, shown under the owner's real name, never
	// acquired or renewed by an agent.
	GrantKindTeam = "team"
)

// Grant roles (PeerGrant.Role): the foreign peer's role as seen from the
// map's Self device.
const (
	// GrantRoleProvider — the peer serves inference to Self.
	GrantRoleProvider = "provider"
	// GrantRoleConsumer — the peer uses Self's engine.
	GrantRoleConsumer = "consumer"
	// GrantRoleBoth — the peer does both: grants run in each direction
	// between the two devices. The wire carries one entry per peer, so
	// the two directions have to share one role. Sent on team grants only
	// (GrantKindTeam), where two teammates sharing their machines with
	// each other is the ordinary case; a Public Share entry keeps one of
	// the two single roles, since agents that declare only
	// CapabilityPublicShareV1 do not know this value.
	GrantRoleBoth = "both"
)

// PeerGrant is the CP-injected annotation on a cross-network peer entry
// — a Public Share peer (public share spec §7) or a Team Share peer
// (team share spec §9). It tells the agent why a foreign-network peer
// appears in its map and how to display it — the authoritative grant
// state lives CP-side; this is a projection.
//
// All fields omitempty: the zero value must vanish from canonical
// JSON so maps without cross-network peers keep their pre-v0.2.0
// signed bytes.
type PeerGrant struct {
	// ID is the CP-issued grant identifier. Agents echo it in usage
	// reports (PublicUsageEntry.GrantID).
	ID string `json:"id,omitempty"`
	// Kind is the sharing flavour: GrantKindPublic or GrantKindTeam.
	// The control plane only sends a kind to a poller that declared
	// its capability (CapabilityPublicShareV1 / CapabilityTeamShareV1).
	Kind string `json:"kind,omitempty"`
	// Role is the peer's role as seen from the map's Self device:
	// GrantRoleProvider when the peer serves inference to Self,
	// GrantRoleConsumer when the peer is a guest using Self's engine, and
	// on team grants GrantRoleBoth when it does both.
	Role string `json:"role,omitempty"`
	// Pseudonym is the stable nickname for a Public Share peer's owner
	// account (e.g. "guest-a7f3"), the only owner identity agents may
	// show for a public grant — real account identifiers never cross
	// that trust boundary. Empty on team grants.
	Pseudonym string `json:"pseudonym,omitempty"`
	// DisplayName is the real display name of a Team Share peer's owner
	// (team share spec §9; owner ruling 2026-09-13, waired#1370): team
	// members see each other by name. Empty on public grants, so
	// Pseudonym keeps a single meaning. A reader shows DisplayName when
	// it is set and Pseudonym otherwise, without having to look at Kind.
	DisplayName string `json:"display_name,omitempty"`
}

// EndpointCandidate is one possible address to reach a peer on the data
// plane. Agents iterate the list in priority order (low Priority value
// = preferred) and, with NAT punching enabled, probe every candidate
// in parallel until one succeeds.
//
// Addr formats:
//
//	"udp4:host:port"               direct IPv4 UDP (local LAN, observed
//	                               public, previous successful)
//	"udp6:[host]:port"             direct IPv6 UDP
//	"relay:<url>#dst=<dev>&nk=<b64>" relay-tunnelled fallback path
//
// Kind narrows the addr's source. agents use it to break ties when
// multiple candidates share Priority — "observed" beats "local" for
// cross-NAT scenarios, "ipv6" beats both when feasible.
type EndpointCandidate struct {
	Addr     string `json:"addr"`
	Kind     string `json:"kind"`
	Priority int    `json:"priority,omitempty"`
}

// Kind values for EndpointCandidate.Kind. The set is open-ended so
// future candidate sources (UPnP, NAT-PMP, etc.) can extend it without
// breaking the wire format. v0 uses the five below.
const (
	// KindLocal is the agent's bound UDP listen address on a local
	// interface (e.g., LAN IPv4). Reachable from peers on the same
	// network segment without NAT traversal.
	KindLocal = "local"
	// KindObserved is the agent's NAT-mapped public IP:port as seen
	// from a relay's UDP STUN-echo. Reachable from peers across NATs
	// once both sides have punched a hole.
	KindObserved = "observed"
	// KindIPv6 is the agent's IPv6 globally unique address. Routed
	// directly without NAT in dual-stack environments.
	KindIPv6 = "ipv6"
	// KindCached is a previously successful endpoint persisted by the
	// agent (state-dir cache). Used to short-circuit the punch cycle
	// across restarts when NAT mapping is still alive.
	KindCached = "cached"
	// KindRelay is a fallback path that tunnels packets through a
	// relay's WebSocket frame channel. Always available, higher
	// latency than direct.
	KindRelay = "relay"
)

// NetworkMapRelay describes a relay the agent may dial when direct UDP
// to a peer is impractical. URL is the WebSocket endpoint
// (wss://host:port/relay/v1/connect). The relay terminates TLS itself
// with a self-signed cert; agents pin TLSFingerprint (hex SHA-256 of
// the leaf cert DER — see auth spec §10.2).
type NetworkMapRelay struct {
	RelayID        string `json:"relay_id"`
	URL            string `json:"url"`
	Region         string `json:"region,omitempty"`
	Priority       int    `json:"priority,omitempty"`
	TLSFingerprint string `json:"tls_fingerprint,omitempty"`
	// DiscoHosts is the optional list of additional STUN-echo target
	// hosts the agent should probe alongside the host extracted from
	// URL. Each entry is a bare literal IPv4 or IPv6 (no brackets, no
	// port). Empty for legacy v4-only relays; dual-stack relays
	// populate this with both family literals so the agent's STUN
	// observer can sample both and end-to-end verify v6 reachability.
	DiscoHosts []string `json:"disco_hosts,omitempty"`
}

func (k *Key) SignNetworkMap(m NetworkMap) (NetworkMap, error) {
	m.Signature = ""
	msg, err := CanonicalJSON(m)
	if err != nil {
		return NetworkMap{}, err
	}
	m.Signature = base64.StdEncoding.EncodeToString(k.Sign(msg))
	return m, nil
}

func VerifyNetworkMap(pub ed25519.PublicKey, m NetworkMap) error {
	if m.Signature == "" {
		return errors.New("network map: missing signature")
	}
	sig, err := base64.StdEncoding.DecodeString(m.Signature)
	if err != nil {
		return fmt.Errorf("network map: signature base64: %w", err)
	}
	m.Signature = ""
	msg, err := CanonicalJSON(m)
	if err != nil {
		return err
	}
	if !ed25519.Verify(pub, msg, sig) {
		return errors.New("network map: signature does not verify")
	}
	return nil
}
