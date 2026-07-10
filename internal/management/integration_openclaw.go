package management

import (
	"context"
	"net/http"

	"github.com/waired-ai/waired-agent/internal/integration/detect"
)

// OpenClawIntegrationConfig is the management server's view of the OpenClaw
// integration: where to look on disk, what data-plane URL the plugin's
// provider *should* point at, and (for the tray's Reconfigure click) a
// callback that re-applies `waired link openclaw` server-side.
//
// OpenClaw is wired through a waired-authored plugin at
// `~/.openclaw/plugins/waired/` plus a small openclaw.json merge that
// registers + enables it (see internal/integration/openclaw). Drift between
// the plugin's provider baseURL and the running data-plane gateway is the
// failure mode the tray surfaces — openclaw reports connection refused
// loudly when waired is down, so there is no silent-breakage class to defend
// against. Hence this endpoint is purely informational + the targeted
// reconfigure trigger.
type OpenClawIntegrationConfig struct {
	HomeDir string
	// ExpectedBaseURL is what the plugin's BASE_URL should match: the
	// agent's no-token data-plane URL with the `/v1` suffix (e.g.
	// "http://127.0.0.1:9479/v1"). Empty disables staleness detection —
	// every found plugin is reported as fresh.
	ExpectedBaseURL string
	// Reconfigure is invoked by POST /reconfigure to re-apply the
	// integration. The function is responsible for everything `waired link
	// openclaw` would do (Force=true, NonInteractive=true). When nil, the
	// reconfigure endpoint returns 503 — the agent runs but cannot
	// self-reconfigure. Tests inject fakes here.
	Reconfigure func(ctx context.Context) error
}

// OpenClawIntegrationStatusConfig is a public alias for detect.Result so
// consumers (the tray) need not import the internal detect package.
type OpenClawIntegrationStatusConfig = detect.Result

// OpenClawIntegrationStatus is the JSON returned by GET
// /waired/v1/integration/openclaw.
type OpenClawIntegrationStatus struct {
	Config OpenClawIntegrationStatusConfig `json:"config"`
}

// WithOpenClawIntegration enables the OpenClaw integration routes.
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
	body := buildOpenClawIntegration(*cfg)
	writeJSON(w, http.StatusOK, body)
}

// handleOpenClawReconfigure runs the configured Reconfigure callback. The
// HTTP shape mirrors the opencode/pause controls: empty body, 200 +
// `{"applied": true}` on success, 5xx + JSON error on failure.
func (s *Server) handleOpenClawReconfigure(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cfg := s.openClawIntegration
	if cfg == nil {
		http.Error(w, "openclaw integration not configured", http.StatusNotFound)
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

// buildOpenClawIntegration is split out so tests can drive it without a full
// HTTP roundtrip.
func buildOpenClawIntegration(cfg OpenClawIntegrationConfig) OpenClawIntegrationStatus {
	return OpenClawIntegrationStatus{
		Config: detect.OpenClaw(cfg.HomeDir, cfg.ExpectedBaseURL),
	}
}
