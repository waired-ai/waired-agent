// Package inferencemesh implements the agent-side aggregator that
// fuses (a) this device's local engine probe and (b) every peer's
// pushed InferenceState (received via the network map) into a single
// in-memory mesh snapshot.
//
// The snapshot is consumed by:
//   - GET /waired/v1/inference/mesh on the management API (this is
//     the JSON shape it returns)
//   - `waired claude --waired-diagnose` (decoded back into Snapshot
//     so the human-readable table can render uniformly)
//
// Phase 3 scope: data plumbing only. The wrapper's gate stays on the
// runtime/state InferenceReachableLocal flag (= self only), not the
// mesh aggregate computed here. Phase 4 (peer-engine routing) is
// what flips the wrapper to consume Reachable from this snapshot.
package inferencemesh

import "github.com/waired-ai/waired-agent/proto/signer"

// Snapshot is the wire JSON shape returned by GET /waired/v1/inference/mesh.
// generated_at is the agent's wall-clock at compute time, RFC3339Nano.
//
// Reachable is the **peers-only** OR aggregate (per the Phase 3
// design): true iff at least one peer (excluding self) has a fresh
// InferenceState with reachable=true. self lives in Self for
// observability — its Reachable bool maps to runtime/state's
// InferenceReachableLocal — but does NOT contribute to Reachable.
//
// The peers-only choice falls out of the gateway architecture: a
// peer entry in this aggregate is only useful if peer-engine routing
// can actually reach it, which requires Phase 4. For self, the local
// gateway already routes to the local runtime — so self has its own
// dedicated axis (InferenceReachableLocal).
type Snapshot struct {
	GeneratedAt  string `json:"generated_at"`
	SelfDeviceID string `json:"self_device_id"`
	Reachable    bool   `json:"reachable"`
	// StalenessThresholdMS is Policy.AdvertisedLiveness: how old a
	// peer's advertised last_check may be **at the moment its network
	// map frame arrives**. It is not a continuously-applied deadline —
	// see Policy (waired-agent#323).
	StalenessThresholdMS int64 `json:"staleness_threshold_ms"`
	// FrameStalenessMS is Policy.FrameStaleness: how old the newest map
	// frame may be before every peer entry derived from it is dropped.
	FrameStalenessMS int64 `json:"frame_staleness_ms"`
	// MapReceivedAt / MapAgeMS describe the newest network map frame
	// (RFC3339Nano, and its age at compute time). Empty / zero before
	// the first frame arrives. Diagnostic only: they exist so an
	// operator reading `waired peers list --json` can tell "the peer
	// went quiet" apart from "our map stream went quiet".
	MapReceivedAt string     `json:"map_received_at,omitempty"`
	MapAgeMS      int64      `json:"map_age_ms,omitempty"`
	Self          PeerView   `json:"self"`
	Peers         []PeerView `json:"peers"`
}

// PeerView is the per-device entry the snapshot exposes. State may be
// nil for peers that have never pushed an inference status. Stale=true
// means the aggregator treats the peer as unusable for the purpose of
// Snapshot.Reachable (the entry still appears in Peers so consumers can
// render "this peer used to be reachable, now it's stale"). A peer is
// stale when ANY of: the newest map frame is older than
// Policy.FrameStaleness, the peer's advertised last_check was already
// older than Policy.AdvertisedLiveness when that frame arrived, or the
// disco prober reports it as once-seen-now-silent.
//
// Phase 7 inputs (Capacity, Hardware) are accessed via InferenceState
// directly — they are not re-hoisted to the PeerView level so the wire
// shape stays the same shape the pre-Phase-7 management/diagnose UI
// already consumes. The Selector reads `pv.InferenceState.Capacity`
// etc. when Stale==false and the state is non-nil.
type PeerView struct {
	DeviceID       string                 `json:"device_id"`
	DeviceName     string                 `json:"device_name"`
	OverlayIP      string                 `json:"overlay_ip"`
	Stale          bool                   `json:"stale"`
	InferenceState *signer.InferenceState `json:"inference_state,omitempty"`
	// Grant is set for foreign peers injected into the map under a
	// Public Share grant (nil for own-network peers). The router uses
	// it to partition own vs public candidates (D2), and consumers
	// must display Grant.Pseudonym — never the real DeviceID — for
	// such peers.
	Grant *signer.PeerGrant `json:"grant,omitempty"`
}
