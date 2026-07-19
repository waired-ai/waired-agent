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
	GeneratedAt          string     `json:"generated_at"`
	SelfDeviceID         string     `json:"self_device_id"`
	Reachable            bool       `json:"reachable"`
	StalenessThresholdMS int64      `json:"staleness_threshold_ms"`
	Self                 PeerView   `json:"self"`
	Peers                []PeerView `json:"peers"`
}

// PeerView is the per-device entry the snapshot exposes. State may be
// nil for peers that have never pushed an inference status. Stale=true
// means the peer's last_check exceeds the staleness threshold and the
// aggregator treats them as unreachable for the purpose of Snapshot.Reachable
// (but the entry still appears in Peers so consumers can render
// "this peer used to be reachable, now it's stale").
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
