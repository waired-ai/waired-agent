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
	// vLLM / external openai-compat) currently accepts requests. False
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
	)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(snap)
}
