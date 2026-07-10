package management

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// handleUpdateCheck serves POST /waired/v1/update/check. The body is an
// optional UpdateCheckRequest (an empty body is valid). It resolves the
// latest published version, compares it against the running build, and
// returns the refreshed UpdateStatus.
func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.update == nil {
		http.Error(w, "update controller not configured", http.StatusNotFound)
		return
	}
	var req UpdateCheckRequest
	// Tolerate an empty body: io.EOF from an empty reader is not an error.
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	st, err := s.update.Check(r.Context(), req)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// handleUpdateStatus serves GET /waired/v1/update/status — the last cached
// check result, without forcing a network hit. The tray polls this.
func (s *Server) handleUpdateStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.update == nil {
		http.Error(w, "update controller not configured", http.StatusNotFound)
		return
	}
	st, err := s.update.Status(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// handleUpdateSettings serves POST /waired/v1/update/settings. It persists
// the operator's "prompt me about updates" preference (#294) and returns the
// refreshed status (with the new NotifyEnabled). The body is required and
// must carry a "notify" boolean.
func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.update == nil {
		http.Error(w, "update controller not configured", http.StatusNotFound)
		return
	}
	var req UpdateSettingsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	st, err := s.update.SetNotify(r.Context(), req.Notify)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, st)
}
