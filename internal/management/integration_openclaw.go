package management

import (
	"net/http"
)

// OpenClawIntegrationConfig is the management server's view of the OpenClaw
// integration: the data-plane URL the plugin's provider *should* point at,
// and nothing else.
//
// OpenClaw is wired through a waired-authored plugin at
// `~/.openclaw/plugins/waired/` plus a small openclaw.json merge that
// registers + enables it (see internal/integration/openclaw). Drift between
// the plugin's provider baseURL and the running data-plane gateway is the
// failure mode the tray surfaces — openclaw reports connection refused
// loudly when waired is down, so there is no silent-breakage class to defend
// against.
//
// Like its OpenCode sibling, this endpoint answers only what the daemon
// itself knows. It does not stat the plugin and does not write it: the
// daemon's home is a service account's, not the desktop user's
// (waired-agent#986), and writing there is the privilege bridge waired#935
// keeps the daemon out of. The tray probes its own home instead.
type OpenClawIntegrationConfig struct {
	// ExpectedBaseURL is what the plugin's BASE_URL should match: the
	// agent's no-token data-plane URL with the `/v1` suffix (e.g.
	// "http://127.0.0.1:9479/v1"). Empty disables staleness detection in
	// the client — every found plugin is reported as fresh.
	ExpectedBaseURL string
}

// WithOpenClawIntegration enables the OpenClaw integration route.
func (s *Server) WithOpenClawIntegration(cfg OpenClawIntegrationConfig) *Server {
	s.openClawIntegration = &cfg
	return s
}

func (s *Server) handleOpenClawIntegration(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cfg := s.openClawIntegration
	if cfg == nil {
		http.Error(w, "openclaw integration not configured", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, IntegrationExpectation{ExpectedBaseURL: cfg.ExpectedBaseURL})
}
