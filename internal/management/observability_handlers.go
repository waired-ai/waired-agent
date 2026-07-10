package management

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/waired-ai/waired-agent/internal/observability"
)

// ObservabilityState is the gauge-style snapshot served by
// /waired/v1/observability/state. The agent populates it at request
// time from its live subsystems (inference, mesh, version, uptime).
// All fields are optional; zero values render as their JSON zero
// (omitempty applies where it makes the consumer's life easier).
type ObservabilityState struct {
	Agent         AgentState     `json:"agent"`
	Mesh          MeshState      `json:"mesh"`
	LastInference *LastInference `json:"last_inference,omitempty"`
}

// AgentState is the "this agent right now" portion of the state.
// Empty / zero values are acceptable when a subsystem hasn't yet
// reported (e.g. pre-enrollment device_id is "").
type AgentState struct {
	DeviceID      string `json:"device_id,omitempty"`
	Version       string `json:"version,omitempty"`
	UptimeSeconds int64  `json:"uptime_seconds"`
	EngineReady   bool   `json:"engine_ready"`
	ModelID       string `json:"model_id,omitempty"`
	ShareEnabled  bool   `json:"share_enabled"`
	Paused        bool   `json:"paused"`
	CapacityTotal int    `json:"capacity_total"`
	CapacityUsed  int    `json:"capacity_used"`
	Inflight      int    `json:"inflight"`

	// Engine provenance (see RuntimeStatus.Mode / LiveVersion /
	// VersionWarning) — duplicated here so `waired doctor` can flag a
	// version-mismatched or unmanaged engine from the observability
	// state alone. Empty on agents predating these fields.
	EngineMode           string `json:"engine_mode,omitempty"`
	EngineVersion        string `json:"engine_version,omitempty"`
	EngineVersionWarning string `json:"engine_version_warning,omitempty"`
	// EngineTuningWarning mirrors RuntimeStatus.TuningWarning (#621):
	// a floored context window, silent f16 KV fallback, spill to
	// system RAM, or an untunable reuse engine. Empty when the serve
	// tuning applied cleanly (or on agents predating it).
	EngineTuningWarning string `json:"engine_tuning_warning,omitempty"`
}

// MeshState reports the mesh-peer counts by lifecycle state.
type MeshState struct {
	PeersEnrolled  int `json:"peers_enrolled"`
	PeersReachable int `json:"peers_reachable"`
	PeersReady     int `json:"peers_ready"`
}

// LastInference summarizes the most recent kind=request event in
// the ring, surfaced as a convenience for tray-style consumers that
// want the latest activity headline without polling /events. nil
// when the ring holds no request events yet.
type LastInference struct {
	TS          string `json:"ts"`
	Decision    string `json:"decision"`
	PeerID      string `json:"peer_id,omitempty"`
	Model       string `json:"model"`
	HadFallback bool   `json:"had_fallback"`
	LatencyMs   uint32 `json:"latency_ms"`
}

// ObservabilityStateProvider returns the current gauge-style state.
// Implemented by the agent (cmd/waired-agent) which has the truth
// about engine readiness, mesh counts, and inflight.
type ObservabilityStateProvider interface {
	ObservabilityState() ObservabilityState
}

// ObservabilityConfig bundles the wiring the management server needs
// to expose Phase 9 endpoints. All fields are optional; missing
// pieces simply 503 their respective routes so partial setups still
// boot.
type ObservabilityConfig struct {
	// Ring is the event ring backing /waired/v1/observability/events.
	Ring *observability.Ring

	// MetricsHandler is the prometheus exposition handler (typically
	// promhttp.HandlerFor on the same registry the agent's
	// observability.Metrics was registered against).
	MetricsHandler http.Handler

	// State, when non-nil, powers /waired/v1/observability/state.
	State ObservabilityStateProvider
}

// WithObservability attaches a Phase 9 ObservabilityConfig. Pass a
// zero ObservabilityConfig (all fields nil) to disable every Phase 9
// endpoint; pass a partially-populated one to expose only the routes
// whose dependencies are wired.
func (s *Server) WithObservability(cfg ObservabilityConfig) *Server {
	s.observability = cfg
	return s
}

func (s *Server) handleObservabilityEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET only"})
		return
	}
	if s.observability.Ring == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "observability ring not configured",
		})
		return
	}

	since := parseUintQuery(r, "since", 0)
	limit := parseIntQuery(r, "limit", 0)
	kindsRaw := r.URL.Query().Get("kinds")

	var kinds []observability.Kind
	if kindsRaw != "" {
		for _, raw := range strings.Split(kindsRaw, ",") {
			if k := strings.TrimSpace(raw); k != "" {
				kinds = append(kinds, observability.Kind(k))
			}
		}
	}

	events, oldestSeq, gap := s.observability.Ring.Since(since, kinds, limit)
	resp := struct {
		Events    []observability.Event `json:"events"`
		NextSince uint64                `json:"next_since"`
		OldestSeq uint64                `json:"oldest_seq"`
		Gap       bool                  `json:"gap"`
	}{
		Events:    events,
		OldestSeq: oldestSeq,
		Gap:       gap,
	}
	if len(events) > 0 {
		resp.NextSince = events[len(events)-1].Seq
	} else {
		// Preserve the caller's cursor so a no-new-events poll round-trips
		// the same since value back to the client.
		resp.NextSince = since
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleObservabilityState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET only"})
		return
	}
	if s.observability.State == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "observability state provider not configured",
		})
		return
	}
	state := s.observability.State.ObservabilityState()
	if state.LastInference == nil && s.observability.Ring != nil {
		state.LastInference = lastInferenceFromRing(s.observability.Ring)
	}
	writeJSON(w, http.StatusOK, state)
}

// lastInferenceFromRing walks the ring newest-first looking for the
// most recent kind=request event and projects it into the wire shape.
// Returns nil when the ring has not yet seen a request.
func lastInferenceFromRing(ring *observability.Ring) *LastInference {
	ev := ring.LatestRequest()
	if ev == nil || ev.Request == nil {
		return nil
	}
	return &LastInference{
		TS:          ev.TS.UTC().Format("2006-01-02T15:04:05.000000000Z"),
		Decision:    ev.Request.Decision,
		PeerID:      ev.Request.PeerID,
		Model:       ev.Request.Model,
		HadFallback: ev.Request.FallbackFrom != "",
		LatencyMs:   ev.Request.LatencyMs,
	}
}

func parseUintQuery(r *http.Request, key string, fallback uint64) uint64 {
	v := r.URL.Query().Get(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return fallback
	}
	return n
}

func parseIntQuery(r *http.Request, key string, fallback int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}
