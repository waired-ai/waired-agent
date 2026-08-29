package management

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// WorkerRequest is the body of POST /waired/v1/worker. A body that
// mentions `mode` must spell out the mode it wants; a body that does not
// mention it at all is an ordering-preference update and leaves the mode
// alone (waired-agent#1128).
//
//   - {"mode": "auto"}                            → SetMode(auto)
//   - {"mode": "local-only"}                      → SetMode(local-only)
//   - {"mode": "peer-preferred"}                  → SetMode(peer-preferred)
//   - {"mode": "pinned", "pinned_peer_device_id": "dev_abc"} → SetPin(dev_abc)
type WorkerRequest struct {
	// Mode is omitempty since waired-agent#1128: a body that does not
	// mention it leaves the mode and any pin alone, and a client sending
	// only ordering preferences must be able to say nothing about it.
	// An explicit empty string still reads as auto, as it always did.
	Mode               state.RoutingMode `json:"mode,omitempty"`
	PinnedPeerDeviceID string            `json:"pinned_peer_device_id,omitempty"`

	// Prefer and MinModelSize are the ordering preferences
	// (waired-agent#1128), and they are POINTERS: a body that does not
	// mention them leaves them alone, and one that sends an empty
	// min_model_size clears the floor. Same shape, for the same reason,
	// as PublicUseUpdateRequest.
	//
	// A body carrying only these leaves the mode and any pin untouched —
	// "where inference runs" and "which of several computers to prefer"
	// are different questions, and the tray sets them from different
	// rows.
	Prefer       *state.RoutingPrefer `json:"prefer,omitempty"`
	MinModelSize *string              `json:"min_model_size,omitempty"`
}

// WorkerResponse is the body of GET /waired/v1/worker AND the 202
// body of POST /waired/v1/worker. PinnedPeerName and PinnedPeerStatus
// are derived from the inferencemesh aggregator (when wired) so the
// tray can render the row label and "(unavailable)" warning without a
// second round-trip.
type WorkerResponse struct {
	Mode               state.RoutingMode `json:"mode"`
	PinnedPeerDeviceID string            `json:"pinned_peer_device_id,omitempty"`

	// Prefer is what the mesh ordering optimises for when several
	// computers could answer: "speed" (the default) or "size". Empty on
	// an agent predating waired-agent#1128, which behaves as speed.
	Prefer state.RoutingPrefer `json:"prefer,omitempty"`

	// MinModelSize is the smallest model class this device will route to.
	// Empty = no floor.
	MinModelSize string `json:"min_model_size,omitempty"`

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
		Prefer:             desired.Prefer,
		MinModelSize:       desired.MinModelSize,
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
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("bad_request", "read body: "+err.Error()))
		return
	}
	// Decoded twice on purpose. The typed form is what the switch below
	// reads; the raw form is the only way to tell "mode was not mentioned"
	// from "mode was sent empty", and empty has always meant auto here
	// (waired-agent#1128). Without it a body of {"prefer":"size"} would
	// reset an operator's pin.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("bad_request", "invalid JSON: "+err.Error()))
		return
	}
	var req WorkerRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("bad_request", "invalid JSON: "+err.Error()))
		return
	}

	ctx := r.Context()
	if req.Prefer != nil {
		switch *req.Prefer {
		case state.RoutingPreferSpeed, state.RoutingPreferSize:
		default:
			writeJSON(w, http.StatusBadRequest, errorBody("bad_request",
				"prefer must be speed or size (got "+string(*req.Prefer)+")"))
			return
		}
	}
	if req.MinModelSize != nil {
		if err := state.ValidateMinModelSize(*req.MinModelSize); err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody("bad_request",
				"min_model_size must be small, medium or large (got "+*req.MinModelSize+"); send an empty value to clear the floor"))
			return
		}
	}
	if req.Prefer != nil || req.MinModelSize != nil {
		if err := s.workerControl.SetRouting(ctx, req.Prefer, req.MinModelSize); err != nil {
			writeJSON(w, http.StatusInternalServerError, errorBody("worker_set_failed", err.Error()))
			return
		}
	}
	if _, hasMode := raw["mode"]; !hasMode {
		// Ordering preferences only. The mode and any pin stay as they
		// are — a body that never mentioned them has not asked about them.
		if req.Prefer == nil && req.MinModelSize == nil {
			writeJSON(w, http.StatusBadRequest, errorBody("bad_request",
				"send mode, prefer or min_model_size"))
			return
		}
		s.writeWorkerState(w, r)
		return
	}
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
