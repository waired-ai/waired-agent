package management

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// handleLoginStart serves POST /waired/v1/login/start. The body is an
// optional LoginStartRequest; an empty body is valid (the daemon uses
// its configured defaults). Returns the initial LoginStatus, which the
// client then polls via handleLoginStatus.
func (s *Server) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.login == nil {
		http.Error(w, "login controller not configured", http.StatusNotFound)
		return
	}
	var req LoginStartRequest
	// Tolerate an empty body: io.EOF from an empty reader is not an
	// error here (the daemon falls back to its configured defaults).
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	st, err := s.login.Start(r.Context(), req)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// handleLoginStatus serves GET /waired/v1/login/status?session=<id>.
// The client polls this until Phase is terminal (active / error).
func (s *Server) handleLoginStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.login == nil {
		http.Error(w, "login controller not configured", http.StatusNotFound)
		return
	}
	st, err := s.login.Status(r.Context(), r.URL.Query().Get("session"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, st)
}
