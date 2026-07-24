package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// ErrInvalidLogLevel is returned by a LogController.SetLogLevel when the
// requested level is not one of debug|info|warn|error. handleLogSettings
// maps it to HTTP 400 (any other error is a 500). Implementations should
// wrap it: fmt.Errorf("%w: %v", management.ErrInvalidLogLevel, err).
var ErrInvalidLogLevel = errors.New("invalid log level")

// LogController is the daemon hook the log-level endpoints delegate to.
// The implementation owns the process's live slog level (a slog.LevelVar)
// and persistence to agent.json, so a change applies immediately AND
// survives a restart. Pass nil to WithLogController to disable the
// endpoints.
type LogController interface {
	// LogLevel returns the current effective level name
	// (debug|info|warn|error).
	LogLevel(ctx context.Context) (string, error)
	// SetLogLevel validates level, applies it to the running process
	// immediately, persists it, and returns the new effective level. A
	// malformed level must be reported as ErrInvalidLogLevel so the
	// endpoint answers 400 rather than 500.
	SetLogLevel(ctx context.Context, level string) (string, error)
}

// LogLevelResponse is the body of GET /waired/v1/log/level and the
// success body of POST /waired/v1/log/settings.
type LogLevelResponse struct {
	Level string `json:"level"`
}

// LogSettingsRequest is the body of POST /waired/v1/log/settings.
type LogSettingsRequest struct {
	Level string `json:"level"`
}

// WithLogController attaches a LogController so the server exposes
// GET /waired/v1/log/level (read the current verbosity) and
// POST /waired/v1/log/settings (live-toggle it, e.g. to debug for
// pre-release diagnostics). The write endpoint flows over the local IPC
// socket like every other mutating verb (waired#838). Pass nil to disable.
func (s *Server) WithLogController(c LogController) *Server {
	s.logControl = c
	return s
}

// handleLogLevel serves GET /waired/v1/log/level — the process's current
// slog level. The tray polls this to mirror the daemon's verbosity.
func (s *Server) handleLogLevel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.logControl == nil {
		http.Error(w, "log controller not configured", http.StatusNotFound)
		return
	}
	lvl, err := s.logControl.LogLevel(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, LogLevelResponse{Level: lvl})
}

// handleLogSettings serves POST /waired/v1/log/settings. It sets the live
// log level (applied immediately) and persists it to agent.json, returning
// the new effective level. The body must carry a "level" string.
func (s *Server) handleLogSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.logControl == nil {
		http.Error(w, "log controller not configured", http.StatusNotFound)
		return
	}
	var req LogSettingsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	lvl, err := s.logControl.SetLogLevel(r.Context(), req.Level)
	if errors.Is(err, ErrInvalidLogLevel) {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, LogLevelResponse{Level: lvl})
}
