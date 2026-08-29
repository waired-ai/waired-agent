package inference

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// HealthSnapshot is the JSON body returned by /waired/v1/inference/healthz.
//
// The Phase 8 probe coordinator (internal/gateway/probe.go) reads this
// to decide whether to admit the inference request to this peer. The
// endpoint deliberately bypasses the operator gates (paused / inference
// disabled / share denied / capacity full) so that a single 503 cannot
// mask multiple distinct conditions — operators want to know "peer is
// admin-disabled" vs "peer is at capacity" vs "peer is no longer
// mesh-sharing", and three different probe-side fallback strategies
// follow from those three cases.
//
// Wire compatibility: every field is required; omitempty is avoided so
// the JSON shape is stable as the probe client matures. Phase 7 peers
// without /healthz answer 404; the probe coordinator treats 404 as
// "assume ready" so a mixed Phase-7/Phase-8 mesh degrades cleanly to
// the pre-Phase-8 deviceID-asc behaviour.
type HealthSnapshot struct {
	// EngineReady reports whether the local inference engine (Ollama /
	// vLLM) currently accepts requests. False
	// during boot before the engine is up, after a `waired inference
	// stop`, or while the engine is restarting after a crash.
	EngineReady bool `json:"engine_ready"`

	// ModelID is the catalog ModelID of the currently-loaded variant
	// (e.g. "qwen3:8b-q4_K_M"). Empty when EngineReady is false or no
	// model has been activated yet.
	ModelID string `json:"model_id"`

	// CapacityTotal is the Config.Capacity value (= 0 means unlimited).
	// Set by the Phase 7 boot benchmark; reported as-is for the probe
	// client to compare against CapacityUsed.
	//
	// A public-share consumer instead reads the public admission
	// ceiling: the totals belong to the owner's own network (§11), and
	// the ceiling is what actually governs that peer's admission.
	CapacityTotal int `json:"capacity_total"`

	// CapacityUsed is the live in-flight inference count. The probe
	// client treats CapacityTotal > 0 AND CapacityUsed >= CapacityTotal
	// as "full, exclude" — the same threshold capacityGate enforces on
	// the inference path.
	//
	// As with CapacityTotal, a public-share consumer reads the public
	// in-flight count, not the owner's total load.
	CapacityUsed int `json:"capacity_used"`

	// Paused mirrors the `waired pause` admin flag. True means the
	// operator has paused the agent; subsequent inference requests
	// would return 503 waired_paused.
	Paused bool `json:"paused"`

	// ShareEnabled is the inverse of IsShareDenied. False means the
	// operator has opted this agent out of mesh-share (Phase 6);
	// subsequent inference requests would return 503
	// waired_inference_not_shared. Default true preserves Phase 5
	// semantics for peers that don't wire IsShareDenied.
	ShareEnabled bool `json:"share_enabled"`

	// ModelResident reports whether the weights are in (V)RAM right
	// now (waired-agent#879). EngineReady above answers "process alive
	// + model file on disk", so a peer that will spend 17-56 s
	// reloading before its first token answers it identically to one
	// mid-stream (waired-agent#861) — and peer selection has no other
	// input that can tell them apart (waired-agent#880).
	//
	// A pointer so that a peer which has not observed residency is
	// distinguishable from one that observed nothing loaded; agents
	// predating this omit the field, which decodes as nil.
	ModelResident *bool `json:"model_resident,omitempty"`

	// Measuring is this peer saying "not yet": it is measuring what it
	// costs to use, and until that finishes it is not offering to serve
	// mesh traffic (waired-agent#1127).
	//
	// Owner ruling, 2026-08-29: init WAITS for that measurement,
	// "kubernetes の readiness probe のように" — because a node handed
	// over before anything knows its speed is a node the ranking cannot
	// place. EngineReady could not carry this: it is also the benchmark's
	// own entry gate (cmd/waired-agent/inference_bench.go), so a
	// measurement gated on it could never start.
	//
	// It clears when the measurement lands OR fails terminally: a host
	// that cannot be measured must still serve. Only the FIRST
	// measurement gates; a re-measurement after a model switch runs
	// behind the previous figure rather than dropping the host out of the
	// mesh. Omitted (false) by agents predating the field, which is the
	// pre-#1127 behaviour.
	Measuring bool `json:"measuring,omitempty"`

	// PrefillRate is what this host measured for the model it is
	// SERVING, published here rather than on the signed NetworkMap for
	// the same reason live residency is
	// (docs/decisions/20260828/0110-live-residency-rides-the-live-probe.md):
	// it is a live fact about this engine, and the map's own speed field
	// is stripped before serving. nil = nothing measured.
	PrefillRate *PrefillRate `json:"prefill_rate,omitempty"`
}

// PrefillRate is one host's prefill speed for the model it serves —
// prompt tokens per second, the term that decides a coding agent's first
// turn (waired-agent#1127). A 30k-token first turn measured 550,166 ms to
// response headers on one peer with 709 ms of decode behind it.
//
// It is a LIST because a rate is only meaningful with the depth it was
// taken at. Prefill throughput falls as the prompt grows — 833 tok/s at
// 11,526 tokens against 583 at 21,247, measured on one machine with one
// model (docs/knowledges/20260805/1830-ollama-prompt-depth-two-traps.md) —
// so two hosts measured at different depths cannot be compared at all.
// Every host climbs the same fixed rungs and publishes the ones it
// reached; a requester compares two peers at the deepest rung BOTH
// reached, and treats peers with no rung in common as not comparable.
type PrefillRate struct {
	// Rungs are shallowest first. Empty is impossible on the wire — the
	// whole field is omitted when nothing was measured.
	Rungs []PrefillRung `json:"rungs"`

	// VariantID is the catalog variant it was measured on. The figures are
	// meaningless against any other, so a requester caching them keys on
	// this and drops them when the peer switches model.
	VariantID string `json:"variant_id,omitempty"`
}

// PrefillRung is one reading at one fixed depth.
type PrefillRung struct {
	Depth int     `json:"depth_tokens"`
	Tokps float64 `json:"tokps"`

	// Bound says Tokps is an UPPER bound — this host did not get through
	// Depth tokens in the time it had, so it is no faster than this — and
	// not a measurement.
	//
	// A bound is its own field rather than a quietly weaker Tokps because
	// that is the ruling waired-agent#579 already settled for the
	// host-cutoff probe (owner, 2026-08-09): a consumer that has not been
	// taught the distinction has to be able to read "no measurement" and
	// decline to judge.
	Bound bool `json:"bound,omitempty"`

	// Samples is how many readings the figure is the median of; SpreadPct
	// is (max−min)/median across them. Samples <= 1 is a reading that was
	// never checked against another — the same meaning
	// signer.HostSpeed.Samples carries.
	Samples   int     `json:"samples,omitempty"`
	SpreadPct float64 `json:"spread_pct,omitempty"`
}

// handleHealthz serves the /waired/v1/inference/healthz endpoint. The
// caller (Handler) is responsible for wrapping this in the
// authentication chain (wgPeerOnly + grantRoleGate +
// verifyPeerSignature); the handler itself reads the operator-gate
// closures and inflight counter the Server has retained alongside the
// gate-wrappers.
//
// Capacity is reported per peer class (spec §11): a cross-account
// public consumer sees the public admission ceiling and the public
// in-flight count, never the machine's true total or the owner's own
// load. The signed Network Map already overrides a provider's advertised
// Capacity with its public cap for exactly this reason (§7.1) — healthz
// would otherwise hand back what the map hides, and the numbers a guest
// reads would not be the ones its own admission is judged against.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	snap := HealthSnapshot{ShareEnabled: true}
	if s.engineReadyFn != nil {
		snap.EngineReady, snap.ModelID = s.engineReadyFn()
	}
	if s.modelResidentFn != nil {
		if resident, observed := s.modelResidentFn(); observed {
			snap.ModelResident = &resident
		}
	}
	if s.isMeasuringFn != nil {
		snap.Measuring = s.isMeasuringFn()
	}
	if s.prefillRateFn != nil {
		snap.PrefillRate = s.prefillRateFn()
	}
	if s.isPausedFn != nil {
		snap.Paused = s.isPausedFn()
	}
	if s.isShareDeniedFn != nil {
		snap.ShareEnabled = !s.isShareDeniedFn()
	}
	peer, peerOK := PeerFromContext(r.Context())
	switch {
	case peerOK && peer.IsPublicConsumer() && s.public != nil:
		snap.CapacityTotal = s.public.effectiveCap()
		snap.CapacityUsed = int(s.public.n.Load())
	case s.inflight != nil:
		snap.CapacityTotal = int(s.inflight.capacity.Load())
		snap.CapacityUsed = int(s.inflight.InFlight())
	}
	slog.DebugContext(r.Context(), "overlay healthz served",
		"engine_ready", snap.EngineReady,
		"model_id", snap.ModelID,
		"capacity_total", snap.CapacityTotal,
		"capacity_used", snap.CapacityUsed,
		"paused", snap.Paused,
		"share_enabled", snap.ShareEnabled,
		"measuring", snap.Measuring,
	)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(snap)
}
