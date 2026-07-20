package management

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// User-facing notes for the provider Public Share toggle (spec §4.1).
// Like the consent warning in public_use.go this is DATA, not copy to
// edit per-surface: the management API is the single source for the
// CLI and the Tray, and the wording assumes no knowledge of Waired
// internals. Plain English only.
const (
	// PublicShareMeshNote is attached to enable responses when turning
	// public sharing on also had to turn on in-account sharing (public
	// use requires the node to report its engine state, which rides the
	// in-account sharing path — spec §4.1).
	PublicShareMeshNote = "Turning on public sharing also makes this computer available to your own other machines."

	// PublicShareDisableNote is attached to disable responses; UIs show
	// RevokedGrants alongside it.
	PublicShareDisableNote = "Public sharing is off. Any running public requests were stopped, and all guest passes for this computer were cancelled."

	// PublicSharePendingNote is attached when the change is applied on
	// this machine but the Waired service has not acknowledged it yet.
	PublicSharePendingNote = "Could not reach Waired right now. Your change is saved on this computer and will sync automatically."
)

// PublicShareDisableConfirmTitle/Text are the pre-confirmation shown before
// the kill switch (spec §4.1: warn that running public inference is cut off).
// Served as data so the CLI and tray render identical, non-hardcoded copy.
const PublicShareDisableConfirmTitle = "Stop public sharing?"
const PublicShareDisableConfirmText = "Any requests other people are running on this computer right now will be stopped, and their access is cancelled. You can turn public sharing back on at any time."

// PublicShareResult reports what a toggle transition did beyond the
// state flip, so UIs can explain side effects to the operator.
type PublicShareResult struct {
	// CPSynced is false when the control plane has not yet acknowledged
	// the change (it is saved locally and retried in the background).
	CPSynced bool
	// MaxClients is the effective public client cap echoed by the
	// control plane (0 when unknown / not synced).
	MaxClients int
	// RevokedGrants is how many guest passes an OFF transition
	// cancelled control-plane-side.
	RevokedGrants int
	// MeshShareEnabled is true when enabling public share also turned
	// on in-account mesh sharing as a prerequisite.
	MeshShareEnabled bool
}

// PublicShareController is implemented by the agent (waired#825). It
// mirrors ShareController's mutate-live-flag + persist-desired-state
// contract, and additionally syncs the toggle to the control plane:
// Enable/Disable return a PublicShareResult describing the CP sync
// outcome and side effects. Synced reports whether the control plane
// currently agrees with the local desired state.
type PublicShareController interface {
	Enable(ctx context.Context, maxClients int) (PublicShareResult, error)
	Disable(ctx context.Context) (PublicShareResult, error)
	State() (current, desired state.PublicShareState)
	Synced() bool
}

// WithPublicShareControl attaches a PublicShareController so the server
// exposes the provider Public Share toggle under /waired/v1/public/share.
// Returns the receiver for chaining.
func (s *Server) WithPublicShareControl(c PublicShareController) *Server {
	s.publicShare = c
	return s
}

// PublicShareStateResponse is the body of GET /waired/v1/public/share
// and of POST /waired/v1/public/share/{enable,disable}.
type PublicShareStateResponse struct {
	State        string `json:"state"`
	DesiredState string `json:"desired_state"`
	// CPSynced is present on toggle/status responses: false means the
	// change is local-only so far (see PublicSharePendingNote).
	CPSynced *bool `json:"cp_synced,omitempty"`
	// MaxClients is the CP-echoed effective public client cap; omitted
	// when unknown.
	MaxClients int `json:"max_clients,omitempty"`
	// RevokedGrants is how many guest passes a disable cancelled.
	RevokedGrants int `json:"revoked_grants,omitempty"`
	// Note carries the plain-English operator-facing explanation of any
	// side effect (mesh auto-enable, pending sync, disconnections).
	Note string `json:"note,omitempty"`
}

// publicShareEnableRequest is the optional POST body of
// /waired/v1/public/share/enable.
type publicShareEnableRequest struct {
	// MaxClients caps how many guests may use this node at once;
	// 0 keeps the control plane's default.
	MaxClients int `json:"max_clients"`
}

func (s *Server) handlePublicShareStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cur, desired := s.publicShare.State()
	synced := s.publicShare.Synced()
	resp := PublicShareStateResponse{
		State:        string(cur),
		DesiredState: string(desired),
		CPSynced:     &synced,
	}
	if !synced {
		resp.Note = PublicSharePendingNote
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handlePublicShareEnable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req publicShareEnableRequest
	if body, err := io.ReadAll(io.LimitReader(r.Body, 4096)); err == nil && len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
			return
		}
	}
	if req.MaxClients < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "max_clients must be >= 0"})
		return
	}
	res, err := s.publicShare.Enable(r.Context(), req.MaxClients)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s.publicShareResponse(res, true))
}

func (s *Server) handlePublicShareDisable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	res, err := s.publicShare.Disable(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s.publicShareResponse(res, false))
}

func (s *Server) publicShareResponse(res PublicShareResult, enabled bool) PublicShareStateResponse {
	cur, desired := s.publicShare.State()
	resp := PublicShareStateResponse{
		State:         string(cur),
		DesiredState:  string(desired),
		CPSynced:      &res.CPSynced,
		MaxClients:    res.MaxClients,
		RevokedGrants: res.RevokedGrants,
	}
	var notes []string
	if !enabled {
		notes = append(notes, PublicShareDisableNote)
	}
	if res.MeshShareEnabled {
		notes = append(notes, PublicShareMeshNote)
	}
	if !res.CPSynced {
		notes = append(notes, PublicSharePendingNote)
	}
	for i, n := range notes {
		if i == 0 {
			resp.Note = n
		} else {
			resp.Note += " " + n
		}
	}
	return resp
}
