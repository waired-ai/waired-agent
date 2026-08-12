package management

import (
	"encoding/json"
	"net/http"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
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

	// PinnedPeerModel is the model that peer is committed to serving,
	// and PinnedPeerCondition is why it is or is not serving it
	// (waired#1064). Both derived here rather than by each client for
	// the reason PinnedPeerName is: the tray already reads this body,
	// and a second derivation is how the three peer-health predicates
	// this package used to hold drifted apart.
	//
	// PinnedPeerStatus stays as it was — three values a client can
	// branch on. PinnedPeerCondition is the finer answer underneath it:
	// where the status says "unavailable", this says whether the model
	// is downloading, its pull failed, or the engine is down. Empty
	// when mode != pinned, or when the peer is absent.
	PinnedPeerModel     string `json:"pinned_peer_model,omitempty"`
	PinnedPeerCondition string `json:"pinned_peer_condition,omitempty"`

	// PinnedPeerDisplayID is the identifier a client may show for the
	// pinned peer: the grant pseudonym when the pin is a public machine,
	// the DeviceID when it is one of your own.
	//
	// PinnedPeerDeviceID above stays the real one — the tray matches it
	// against the mesh snapshot to mark the selected row and posts it
	// back to set the pin, so scrubbing it would break the round-trip.
	// Every client that rendered PinnedPeerDeviceID was therefore
	// printing a stranger's device id on a surface that may not carry one
	// (#739, public share spec §8.5). Empty when nothing is pinned, when
	// the pin is absent from the snapshot and the daemon kept no record
	// of what it was called — and when an agent predating the field
	// answered.
	PinnedPeerDisplayID string `json:"pinned_peer_display_id,omitempty"`
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
		v := s.resolvePinStatus(r, desired.PinnedPeerDeviceID)
		resp.PinnedPeerName, resp.PinnedPeerStatus = v.Name, v.Status
		resp.PinnedPeerModel, resp.PinnedPeerCondition = v.Model, v.Condition
		resp.PinnedPeerDisplayID = pinDisplayID(v, desired)
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
	case state.RoutingModePeerOnly:
		if req.PinnedPeerDeviceID != "" {
			writeJSON(w, http.StatusBadRequest, errorBody("bad_request",
				"peer-only mode must not carry pinned_peer_device_id"))
			return
		}
		if err := s.workerControl.SetMode(ctx, state.RoutingModePeerOnly); err != nil {
			writeJSON(w, http.StatusInternalServerError, errorBody("worker_set_failed", err.Error()))
			return
		}
	case state.RoutingModePinned:
		if req.PinnedPeerDeviceID == "" {
			writeJSON(w, http.StatusBadRequest, errorBody("bad_request",
				"pinned mode requires pinned_peer_device_id"))
			return
		}
		// Resolve what this peer may be called WHILE it is still in the
		// snapshot: after it drops out there is no grant to read, and the
		// display rule needs one (#739).
		display := s.resolvePinStatus(r, req.PinnedPeerDeviceID).DisplayID
		if err := s.workerControl.SetPin(ctx, req.PinnedPeerDeviceID, display); err != nil {
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

// pinDisplayID is the identifier a client may show for the current pin.
//
// The snapshot is the better source — it is current, and it carries the
// grant. But a pin that has dropped out of the snapshot is exactly the
// case where PinnedPeerName is empty too, which is where a client's
// "fall back to the device id" used to put a stranger's device id on a
// menu row (#739). So the second source is what the daemon recorded when
// the pin was set, while the peer was still in view.
//
// Empty when neither source has one: a peer absent from the snapshot,
// pinned by an agent that predates the recorded value. A client showing
// nothing is the intended outcome — the pin's own device id is not an
// alternative it may reach for.
func pinDisplayID(v pinView, desired state.RoutingPreference) string {
	if v.DisplayID != "" {
		return v.DisplayID
	}
	return desired.PinnedPeerDisplayID
}

// resolvePinStatus derives the pinned peer's view from the inferencemesh
// aggregator. Returns ("", "absent") when infMesh is not wired or the
// peer is missing from the snapshot; ("", "absent") rather than "" so
// the tray can distinguish "not configured" (mode != pinned) from
// "peer gone".
func (s *Server) resolvePinStatus(r *http.Request, deviceID string) pinView {
	_ = r
	if s.infMesh == nil {
		return pinView{Status: "absent"}
	}
	snap := s.infMesh.Snapshot()
	for _, p := range snap.Peers {
		if p.DeviceID != deviceID {
			continue
		}
		v := pinView{
			Name:      p.DeviceName,
			Model:     inferencemesh.PeerModel(p),
			Condition: inferencemesh.PeerCondition(p),
			Status:    "unavailable",
		}
		// The snapshot entry is the only place the grant is in hand, so
		// this is where the display rule gets applied for the pin (#739).
		// A public machine with no pseudonym leaves it empty rather than
		// falling back to the real id.
		v.DisplayID, _ = inferencemesh.PeerDisplayID(p)
		if inferencemesh.PeerServing(p) {
			v.Status = "ok"
		}
		return v
	}
	return pinView{Status: "absent"}
}

// pinView is what resolvePinStatus found. A struct rather than four
// same-typed string returns: the two call sites below assign every one
// of them, and a transposed pair would compile and render.
type pinView struct {
	Name      string
	Model     string
	Condition string
	Status    string
	// DisplayID is the identifier a client may show for this peer —
	// pseudonym for a public machine, DeviceID otherwise (§8.5). Empty
	// when the peer is absent from the snapshot, which is exactly when
	// the caller falls back to what the daemon recorded at pin time.
	DisplayID string
}
