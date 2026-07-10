package router

// Recorder is the narrow telemetry interface the router emits into.
// The agent injects an observability composite that satisfies this
// interface (and several others belonging to gateway / inference)
// via duck typing; tests can pass a mock that records calls.
//
// nil is supported anywhere: Inputs.Recorder may be left zero and
// the Selector will skip emission.
type Recorder interface {
	// RecordSelection is called once per successful SelectK return.
	// decision is the resolved candidate's ExecutionMode
	// ("local" / "remote" / "external"); peerID is the underlying
	// mesh peer DeviceID for remote selections (empty otherwise);
	// model is the resolved model id (post-alias).
	//
	// Only the first candidate of SelectK's returned slice is
	// reflected: SelectK groups by decision class, so cands[1:]
	// share the same class as cands[0].
	RecordSelection(decision, peerID, model string)

	// RecordPinnedPeerUnreachable is called from the Tailscale-exit-
	// node-style routing path on both the strict 503 branch
	// (reason="unreachable") and the soft-fallback "pin reachable
	// but lacks the requested model" branch (reason="lacks_model").
	// Tray's "Recent activity" surface picks both up so the operator
	// notices a degraded pin without grepping logs.
	//
	// Implementations may be no-ops; the composite Recorder in the
	// observability package writes a KindPinnedPeerUnreachable event
	// and bumps the matching Prometheus counter. nil Recorder is
	// handled by the Selector (= no emit).
	RecordPinnedPeerUnreachable(peerID, model, reason string)
}
