package management

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// handleLogout serves POST /waired/v1/logout. The body is an optional
// LogoutRequest; an empty body means an ordinary, recoverable sign-out.
//
// 404 when no controller is attached is the version-skew signal clients key
// on: an agent that predates this route answers the same way, so the app falls
// back to the elevated CLI rather than reporting a failure.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.logout == nil {
		http.Error(w, "logout controller not configured", http.StatusNotFound)
		return
	}
	var req LogoutRequest
	// Tolerate an empty body, as handleLoginStart does.
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	resp, err := s.logout.Logout(r.Context(), req)
	if err != nil {
		if errors.Is(err, ErrLoginInFlight) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
