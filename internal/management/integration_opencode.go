package management

import (
	"context"
	"net/http"

	"github.com/waired-ai/waired-agent/internal/integration/detect"
)

// OpenCodeIntegrationConfig is the management server's view of the
// OpenCode integration: where to look on disk, what gateway URL the
// provider block *should* point at, and (for the tray's Reconfigure
// click) a callback that re-applies `waired link opencode` server-side.
//
// OpenCode is wired through a waired-authored plugin at
// `~/.config/opencode/plugin/waired.js` that registers the "waired"
// provider via OpenCode's config hook (see internal/integration/opencode).
// Drift between the plugin's provider baseURL and the running data-plane
// gateway is the failure mode the tray surfaces — opencode itself reports
// connection refused loudly when waired is down, so there is no
// silent-breakage class to defend against. Hence this endpoint is purely
// informational + the targeted reconfigure trigger.
type OpenCodeIntegrationConfig struct {
	HomeDir string
	// ExpectedBaseURL is what the plugin's provider baseURL should match:
	// the agent's no-token OpenCode data-plane URL with `/v1` suffix
	// (e.g. "http://127.0.0.1:9479/v1"). Empty disables staleness
	// detection — every found plugin is reported as fresh.
	ExpectedBaseURL string
	// Reconfigure is invoked by POST /reconfigure to re-apply the
	// integration. The function is responsible for everything `waired
	// use opencode` would do (Force=true, NonInteractive=true). When
	// nil, the reconfigure endpoint returns 503 — the agent runs but
	// cannot self-reconfigure. Tests inject fakes here.
	Reconfigure func(ctx context.Context) error
}

// OpenCodeIntegrationStatusConfig is a public alias for detect.Result
// so consumers (the tray) need not import the internal detect package.
type OpenCodeIntegrationStatusConfig = detect.Result

// OpenCodeIntegrationStatus is the JSON returned by GET
// /waired/v1/integration/opencode.
type OpenCodeIntegrationStatus struct {
	Config OpenCodeIntegrationStatusConfig `json:"config"`
}

// WithOpenCodeIntegration enables the OpenCode integration routes.
func (s *Server) WithOpenCodeIntegration(cfg OpenCodeIntegrationConfig) *Server {
	s.openCodeIntegration = &cfg
	return s
}

func (s *Server) handleOpenCodeIntegration(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cfg := s.openCodeIntegration
	if cfg == nil {
		http.Error(w, "opencode integration not configured", http.StatusNotFound)
		return
	}
	body := buildOpenCodeIntegration(*cfg)
	writeJSON(w, http.StatusOK, body)
}

// handleOpenCodeReconfigure runs the configured Reconfigure callback.
// The HTTP shape mirrors the pause/resume controls: empty body, 200 +
// `{"applied": true}` on success, 5xx + JSON error on failure. The
// caller (tray) does not need to parse anything beyond the status code
// for the happy path; the body just gives debug visibility for ops.
func (s *Server) handleOpenCodeReconfigure(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cfg := s.openCodeIntegration
	if cfg == nil {
		http.Error(w, "opencode integration not configured", http.StatusNotFound)
		return
	}
	if cfg.Reconfigure == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "reconfigure callback not registered",
		})
		return
	}
	if err := cfg.Reconfigure(r.Context()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"applied": true})
}

// buildOpenCodeIntegration is split out so tests can drive it without a
// full HTTP roundtrip.
func buildOpenCodeIntegration(cfg OpenCodeIntegrationConfig) OpenCodeIntegrationStatus {
	return OpenCodeIntegrationStatus{
		Config: detect.OpenCode(cfg.HomeDir, cfg.ExpectedBaseURL),
	}
}
