package signer

// PublicUsageReport is the batch body the PROVIDER-side agent posts to
// the CP (POST /v1/devices/self/public-usage, public share spec §12) to
// account for inference it served to Public Share guests. Defined in
// signer (the shared leaf package) because both the agent reporter and
// the CP intake must agree on the shape — the same single-source
// rationale as InferenceState.
//
// Only the provider reports: it is the side that observes engine usage.
// Reports carry aggregate counters only — never prompt or message
// content, on any path.
type PublicUsageReport struct {
	Entries []PublicUsageEntry `json:"entries"`
}

// PublicUsageEntry aggregates one (grant, model, class) triple over one
// reporting window. Timestamps are RFC3339 strings, matching the wire
// convention of the other signer types.
type PublicUsageEntry struct {
	// GrantID is the DeviceGrant this usage was served under
	// (PeerGrant.ID on the consumer's injected peer entry).
	GrantID string `json:"grant_id"`
	// ModelID is the engine-reported model that served the requests.
	ModelID string `json:"model_id"`
	// Class is the Claude Code traffic class: "main", "sub", or ""
	// for general (non-Claude) API traffic.
	Class string `json:"class,omitempty"`
	// Requests is the number of inference requests completed in the
	// window.
	Requests int64 `json:"requests"`
	// InputTokens / OutputTokens are the summed token counts the
	// engine reported for those requests.
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	// InferenceMS is the summed wall-clock serving time in
	// milliseconds.
	InferenceMS int64 `json:"inference_ms"`
	// WindowStart / WindowEnd bound the aggregation window (RFC3339).
	WindowStart string `json:"window_start"`
	WindowEnd   string `json:"window_end"`
}
