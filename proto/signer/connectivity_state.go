package signer

// ConnectivityState captures whether a device is reaching its mesh
// peers directly over UDP or via a relay (#252, spec §16.2). It travels
// two places:
//
//   - agent → CP push body (POST /v1/devices/self/connectivity-status)
//   - Spanner Device.connectivity_state JSON column
//
// Unlike InferenceState it is NOT distributed to peers in the network
// map: it is admin-facing telemetry only (the Device detail page), so
// it never rides on a signed NetworkMap and has no canonical-form
// constraints beyond the RFC3339Nano timestamp kept for consistency.
//
// The agent pushes raw per-path counts rather than a single
// direct/relay/mixed verdict; the web UI derives the overall label and
// applies a staleness window against LastCheck. Keeping the wire format
// to counts means the same payload also answers "how many peers" without
// a second field, and a future per-peer breakdown can be added without
// breaking the overall derivation.
type ConnectivityState struct {
	// DirectPeers is the number of peers the agent is currently
	// reaching over a direct UDP path.
	DirectPeers int `json:"direct_peers"`

	// RelayPeers is the number of peers the agent is currently
	// reaching via a relay.
	RelayPeers int `json:"relay_peers"`

	// TotalPeers is the number of peers the agent is tracking. It may
	// exceed DirectPeers+RelayPeers if some peers have no established
	// path yet; the UI treats that remainder as "connecting".
	TotalPeers int `json:"total_peers"`

	// LastCheck is the agent's wall-clock time at the snapshot,
	// formatted as RFC3339Nano. The UI ignores states older than its
	// staleness threshold so a crashed agent ages out of the display.
	LastCheck string `json:"last_check"`
}
