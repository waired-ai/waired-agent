package inference

// Recorder is the narrow telemetry interface the overlay inference
// listener emits into. The observability composite satisfies it (and
// the router / gateway interfaces) via duck typing; nil is supported
// at every call site.
//
// Phase 9.0 wires only the served-side hooks (RecordServed and the
// inflight gauge); RecordAuthReject for peer-auth failures is
// registered in the metrics registry but not yet emitted (Tier 2,
// follow-up). Engine-state gauges (engine_ready / share_enabled /
// paused / capacity) are populated from the agent main rather than
// from inference middleware so the composite stays the single
// source of edge-triggered state events.
type Recorder interface {
	// RecordServed is called once per peer-overlay inference request
	// that passed the capacity gate, regardless of whether the
	// downstream gateway returned 2xx or 5xx. result is "success" /
	// "error"; latencyMs is wall-clock time the agent held the
	// admission slot.
	RecordServed(result string, latencyMs uint32)

	// SetInflight updates the per-process inflight gauge to n.
	// Called from the capacity-gate adapter on every Acquire / Release.
	SetInflight(n int)

	// SetCapacity records the configured admission ceiling. Called
	// once at server construction.
	SetCapacity(total int)
}
