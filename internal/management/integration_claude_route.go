package management

import "net/http"

// handleClaudeRouting serves GET /waired/v1/integration/claude/route: what the
// Claude surface was last asked for, and what answered it.
//
// It used to accept a POST as well, setting the per-class route the intercept
// dispatched on. That route is gone — a turn runs where its model id says, and
// waired holds none of its own that could send it elsewhere
// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md,
// owner ruling waired-ai/waired#1313) — so the endpoint reports and no longer
// decides.
func (s *Server) handleClaudeRouting(w http.ResponseWriter, r *http.Request) {
	if s.claudeRouting == nil {
		http.Error(w, "claude routing control not configured", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody("method_not_allowed", "GET only"))
		return
	}
	writeJSON(w, http.StatusOK, s.claudeRouting.State())
}
