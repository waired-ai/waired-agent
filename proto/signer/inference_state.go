package signer

// InferenceState captures the operator-visible state of one device's
// local inference engine. It travels three places:
//
//   - agent → CP push body (POST /v1/devices/self/inference-status)
//   - Spanner Device.inference_state JSON column
//   - NetworkMapPeer.InferenceState (long-poll distribution to peers)
//
// Defining it in signer (a leaf package) keeps the wire format the
// single source of truth: store imports it for persistence, api uses
// it to receive pushes, controlclient uses it to send them.
//
// Timestamps are RFC3339Nano strings rather than time.Time so the
// canonical-JSON form is byte-deterministic across marshalling
// libraries (NetworkMap is signed; non-deterministic time formatting
// would break verification).
type InferenceState struct {
	// Reachable is the agent's last probe verdict for its local
	// engine. The wrapper's Phase-3 gate uses self only via the
	// runtime/state file, but peers consume this field to compute
	// the mesh-wide aggregate.
	Reachable bool `json:"reachable"`

	// Type is the engine kind. One of InferenceTypeOllama,
	// InferenceTypeVLLM, or InferenceTypeNone. Other values are
	// rejected by the API validator.
	Type string `json:"type"`

	// Endpoint is the local HTTP base URL the agent's gateway
	// proxies to (e.g., "http://127.0.0.1:11434"). Loopback by
	// design for v0; broadcast to the mesh as informational only —
	// peers cannot dial another peer's loopback. Future
	// peer-engine routing (Phase 4) will replace this with an
	// overlay-IP listener.
	Endpoint string `json:"endpoint"`

	// Models is the list of engine-reported model names (from
	// e.g. /api/tags). Bounded by the API validator; the field is
	// `omitempty` so zero-state pushes don't bloat network maps.
	Models []string `json:"models,omitempty"`

	// LastError is the last probe error message when Reachable is
	// false. Empty when Reachable is true. Bounded by the API
	// validator; `omitempty` keeps healthy peers' map entries small.
	LastError string `json:"last_error,omitempty"`

	// LastCheck is the agent's wall-clock time at the most recent
	// probe, formatted as RFC3339Nano. Acts as the K8s NodeStatus-
	// style heartbeat: peers ignore states older than the staleness
	// threshold (Phase 3 default: 15 s) when computing mesh
	// aggregation, so an agent that crashes silently ages out of
	// the aggregate naturally.
	LastCheck string `json:"last_check"`

	// Hardware summarises the GPU/RAM the agent has available. Phase 7
	// uses this for display only (e.g. tray UI showing "peer X:
	// RTX 4090, 24 GB"); the router does NOT score on raw hardware
	// because the same data is already encoded in Capacity. nil for
	// agents that predate the field, in which case peers fall back to
	// hardware-blind display.
	Hardware *HardwareSummary `json:"hardware,omitempty"`

	// Capacity is the number of concurrent inference requests this
	// agent will accept on its peer-overlay listener before returning
	// 503 waired_inference_overloaded. Derived at agent boot from a
	// token/s benchmark of the local engine (see Phase 7 plan §11).
	// 0 means "unlimited" — both the explicit semantics for external
	// (openai-compat) endpoints, and the zero-value backward-compat
	// fallback for agents that predate the field.
	Capacity int `json:"capacity,omitempty"`

	// Priority is the admin routing preference the CP folds in for this
	// device: High(1) / Middle(0) / Low(-1). The requesting agent's router
	// uses it as the dominant peer-selection key, preferring higher-priority
	// peers among those that can serve a request. Unlike the agent-pushed
	// fields above, this is CP-injected at map-assembly time (the agent never
	// sets it on its own push).
	//
	// `omitempty` keeps Middle (the default, 0) off the wire so the common
	// case is byte-identical and older agents verify the signed map unchanged.
	// Setting High/Low emits a non-zero field; agents that predate this field
	// would then reject the whole map (canonical re-marshal drops the unknown
	// field) — so non-default priority must only be set after the fleet is
	// upgraded. Low is negative so it stays distinct from the omitted default.
	Priority int `json:"priority,omitempty"`

	// ExcludeMain / ExcludeSub are the CP-injected Claude Code serving-eligibility
	// flags: when true, this device is NOT eligible to serve that traffic class
	// (main / sub) for the mesh, and the requesting router's buildMeshCandidates
	// drops the peer for the matching request Class. Negative sense + `omitempty`
	// so the default (eligible for both) stays off the wire and the common-case
	// signed map is byte-identical for older agents — the same fleet-upgrade
	// ordering caveat as Priority applies (only set an exclusion after the fleet
	// knows the field). CP-injected at map-assembly time (effectiveInferenceState);
	// the agent never sets these on its own push.
	ExcludeMain bool `json:"exclude_main,omitempty"`
	ExcludeSub  bool `json:"exclude_sub,omitempty"`

	// DesiredParallel is the operator's max-concurrent-requests target that the CP
	// injects at map-assembly time ONLY when an admin inference_max_clients
	// override is set (`effectiveInferenceState`) — it equals that override. The
	// serving agent drives OLLAMA_NUM_PARALLEL from it.
	//
	// CRITICAL: this is DISTINCT from Capacity. Capacity is the admission ceiling
	// and, absent an admin override, carries the agent's benchmark-derived value
	// — which must NOT be read as a parallelism target (doing so would restart the
	// engine on every fresh host). DesiredParallel is 0/omitted unless an admin
	// explicitly set the cap, so a default host never re-tunes parallelism.
	// `omitempty` + only-set-on-override keeps the common case byte-identical.
	DesiredParallel int `json:"desired_parallel,omitempty"`

	// RecommendedMaxParallel is the agent-computed VRAM-safe engine parallelism
	// ceiling (floor(maxCtx/ctx) in the no-spill regime; 1 when spilling or when
	// the host is unsizable). It is ADVISORY telemetry for the Device detail page
	// (the operator may exceed it via an informed override) — NOT a routing input.
	//
	// It travels only agent → CP push → Spanner inference_state JSON → the
	// management API's inference_detail. It MUST be stripped from the served
	// NetworkMap (effectiveInferenceState zeros it) because, unlike the fields
	// above, it is non-zero in the common case (any host with an engine) and so
	// would break the byte-identical signed-map contract older agents rely on.
	// `omitempty` keeps 0 (unknown/unsizable) off the push.
	RecommendedMaxParallel int `json:"recommended_max_parallel,omitempty"`
}

// HardwareSummary is the subset of the agent's hardware profile that
// the inference mesh broadcasts via NetworkMap. Kept minimal — the
// full profile lives in management/inference status responses and
// doesn't need to ride on every peer update.
type HardwareSummary struct {
	// GPUs lists each detected accelerator. Empty / nil for CPU-only
	// hosts. Multi-GPU agents list one entry per device.
	GPUs []HardwareGPUSummary `json:"gpus,omitempty"`

	// RAMTotalGB is the total system RAM in GB (rounded). Used for
	// display when a peer is serving an ollama (CPU-bound) variant.
	RAMTotalGB int `json:"ram_total_gb,omitempty"`
}

// HardwareGPUSummary identifies one GPU. Fields mirror
// hardware.GPU but stripped to what other peers can act on.
type HardwareGPUSummary struct {
	// Model is the vendor-reported model name, e.g. "NVIDIA GeForce
	// RTX 4090". Free-form; do not parse for routing decisions.
	Model string `json:"model"`

	// VRAMTotalMB is the device's total VRAM in megabytes.
	VRAMTotalMB int `json:"vram_total_mb,omitempty"`

	// ComputeCap is the CUDA compute capability formatted as a
	// string (e.g. "8.9" for Ada Lovelace). Empty for non-CUDA.
	ComputeCap string `json:"compute_cap,omitempty"`
}

// Engine type constants — accepted values for InferenceState.Type.
const (
	InferenceTypeOllama = "ollama"
	InferenceTypeVLLM   = "vllm"
	InferenceTypeNone   = "none"
)

// IsValidInferenceType reports whether t is one of the accepted
// engine type values. Used by the CP API validator and by the
// agent push client's pre-flight check.
func IsValidInferenceType(t string) bool {
	switch t {
	case InferenceTypeOllama, InferenceTypeVLLM, InferenceTypeNone:
		return true
	}
	return false
}
