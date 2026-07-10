package management

import (
	"encoding/json"
	"net/http"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// WorkerRequest is the body of POST /waired/v1/worker. Empty Mode is
// rejected — callers must spell out the desired mode explicitly.
//
//   - {"mode": "auto"}                            → SetMode(auto)
//   - {"mode": "local-only"}                      → SetMode(local-only)
//   - {"mode": "peer-preferred"}                  → SetMode(peer-preferred)
//   - {"mode": "pinned", "pinned_peer_device_id": "dev_abc"} → SetPin(dev_abc)
type WorkerRequest struct {
	Mode               state.RoutingMode `json:"mode"`
	PinnedPeerDeviceID string            `json:"pinned_peer_device_id,omitempty"`
}

// WorkerResponse is the body of GET /waired/v1/worker AND the 202
// body of POST /waired/v1/worker. PinnedPeerName and PinnedPeerStatus
// are derived from the inferencemesh aggregator (when wired) so the
// tray can render the row label and "(unavailable)" warning without a
// second round-trip.
type WorkerResponse struct {
	Mode               state.RoutingMode `json:"mode"`
	PinnedPeerDeviceID string            `json:"pinned_peer_device_id,omitempty"`

	// PinnedPeerName is the operator-visible device name when the
	// pinned peer is currently in the mesh snapshot. Empty in three
	// cases: (1) mode != pinned, (2) infMesh is not wired, or
	// (3) pinned peer dropped out of the snapshot.
	PinnedPeerName string `json:"pinned_peer_name,omitempty"`

	// PinnedPeerStatus reports tray-friendly health of the pin:
	//   "ok"          — peer reachable + non-stale + serving model(s)
	//   "unavailable" — peer present but stale OR serving inactive
	//   "absent"      — peer not in current mesh snapshot at all
	// Empty when mode != pinned.
	PinnedPeerStatus string `json:"pinned_peer_status,omitempty"`
}

func (s *Server) handleWorker(w http.ResponseWriter, r *http.Request) {
	if s.workerControl == nil {
		http.Error(w, "worker control not configured", http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.writeWorkerState(w, r)
	case http.MethodPost:
		s.applyWorkerRequest(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, errorBody("method_not_allowed", "GET or POST only"))
	}
}

func (s *Server) writeWorkerState(w http.ResponseWriter, r *http.Request) {
	_, desired := s.workerControl.State()
	resp := WorkerResponse{
		Mode:               desired.Mode,
		PinnedPeerDeviceID: desired.PinnedPeerDeviceID,
	}
	if desired.Mode == state.RoutingModePinned && desired.PinnedPeerDeviceID != "" {
		resp.PinnedPeerName, resp.PinnedPeerStatus = s.resolvePinStatus(r, desired.PinnedPeerDeviceID)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) applyWorkerRequest(w http.ResponseWriter, r *http.Request) {
	var req WorkerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("bad_request", "invalid JSON: "+err.Error()))
		return
	}

	ctx := r.Context()
	switch req.Mode {
	case state.RoutingModeAuto, "":
		if req.PinnedPeerDeviceID != "" {
			writeJSON(w, http.StatusBadRequest, errorBody("bad_request",
				"auto mode must not carry pinned_peer_device_id"))
			return
		}
		if err := s.workerControl.SetMode(ctx, state.RoutingModeAuto); err != nil {
			writeJSON(w, http.StatusInternalServerError, errorBody("worker_set_failed", err.Error()))
			return
		}
	case state.RoutingModeLocalOnly:
		if req.PinnedPeerDeviceID != "" {
			writeJSON(w, http.StatusBadRequest, errorBody("bad_request",
				"local-only mode must not carry pinned_peer_device_id"))
			return
		}
		if err := s.workerControl.SetMode(ctx, state.RoutingModeLocalOnly); err != nil {
			writeJSON(w, http.StatusInternalServerError, errorBody("worker_set_failed", err.Error()))
			return
		}
	case state.RoutingModePeerPreferred:
		if req.PinnedPeerDeviceID != "" {
			writeJSON(w, http.StatusBadRequest, errorBody("bad_request",
				"peer-preferred mode must not carry pinned_peer_device_id"))
			return
		}
		if err := s.workerControl.SetMode(ctx, state.RoutingModePeerPreferred); err != nil {
			writeJSON(w, http.StatusInternalServerError, errorBody("worker_set_failed", err.Error()))
			return
		}
	case state.RoutingModePinned:
		if req.PinnedPeerDeviceID == "" {
			writeJSON(w, http.StatusBadRequest, errorBody("bad_request",
				"pinned mode requires pinned_peer_device_id"))
			return
		}
		if err := s.workerControl.SetPin(ctx, req.PinnedPeerDeviceID); err != nil {
			writeJSON(w, http.StatusInternalServerError, errorBody("worker_set_failed", err.Error()))
			return
		}
	default:
		writeJSON(w, http.StatusBadRequest, errorBody("bad_request",
			"unknown mode "+string(req.Mode)))
		return
	}
	s.writeWorkerState(w, r)
}

// resolvePinStatus derives (name, status) from the inferencemesh
// aggregator. Returns ("", "absent") when infMesh is not wired or the
// peer is missing from the snapshot; ("", "absent") rather than "" so
// the tray can distinguish "not configured" (mode != pinned) from
// "peer gone".
func (s *Server) resolvePinStatus(r *http.Request, deviceID string) (name, status string) {
	_ = r
	if s.infMesh == nil {
		return "", "absent"
	}
	snap := s.infMesh.Snapshot()
	for _, p := range snap.Peers {
		if p.DeviceID != deviceID {
			continue
		}
		name = p.DeviceName
		switch {
		case p.Stale:
			status = "unavailable"
		case p.InferenceState == nil || !p.InferenceState.Reachable:
			status = "unavailable"
		case len(p.InferenceState.Models) == 0:
			status = "unavailable"
		default:
			status = "ok"
		}
		return name, status
	}
	return "", "absent"
}
