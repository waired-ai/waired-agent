package management

import (
	"encoding/json"
	"net/http"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// ClaudeRoutingRequest is the body of POST /waired/v1/integration/claude/route.
// Main and Sub are independent pointers so a caller can set one class without
// touching the other — a nil field is left unchanged. At least one must be
// present. Main accepts auto|waired|anthropic; Sub additionally accepts "same"
// (inherit main).
type ClaudeRoutingRequest struct {
	Main *state.ClaudeRouteClass `json:"main,omitempty"`
	Sub  *state.ClaudeRouteClass `json:"sub,omitempty"`
}

func (s *Server) handleClaudeRouting(w http.ResponseWriter, r *http.Request) {
	if s.claudeRouting == nil {
		http.Error(w, "claude routing control not configured", http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.claudeRouting.State())
	case http.MethodPost:
		s.applyClaudeRoutingRequest(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, errorBody("method_not_allowed", "GET or POST only"))
	}
}

func validClaudeMainRoute(c state.ClaudeRouteClass) bool {
	switch c {
	case state.ClaudeRouteAuto, state.ClaudeRouteWaired, state.ClaudeRouteAnthropic:
		return true
	}
	return false
}

func (s *Server) applyClaudeRoutingRequest(w http.ResponseWriter, r *http.Request) {
	var req ClaudeRoutingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("bad_request", "invalid JSON: "+err.Error()))
		return
	}
	if req.Main == nil && req.Sub == nil {
		writeJSON(w, http.StatusBadRequest, errorBody("bad_request", "provide main and/or sub"))
		return
	}
	if req.Main != nil && !validClaudeMainRoute(*req.Main) {
		writeJSON(w, http.StatusBadRequest, errorBody("bad_request", "unknown main route "+string(*req.Main)))
		return
	}
	if req.Sub != nil && *req.Sub != state.ClaudeRouteSame && !validClaudeMainRoute(*req.Sub) {
		writeJSON(w, http.StatusBadRequest, errorBody("bad_request", "unknown sub route "+string(*req.Sub)))
		return
	}

	ctx := r.Context()
	if req.Main != nil {
		if err := s.claudeRouting.SetClass(ctx, state.ClaudeClassMain, *req.Main); err != nil {
			writeJSON(w, http.StatusInternalServerError, errorBody("claude_routing_set_failed", err.Error()))
			return
		}
	}
	if req.Sub != nil {
		if err := s.claudeRouting.SetClass(ctx, state.ClaudeClassSub, *req.Sub); err != nil {
			writeJSON(w, http.StatusInternalServerError, errorBody("claude_routing_set_failed", err.Error()))
			return
		}
	}
	writeJSON(w, http.StatusOK, s.claudeRouting.State())
}
