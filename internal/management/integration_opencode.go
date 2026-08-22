package management

import (
	"net/http"
)

// OpenCodeIntegrationConfig is the management server's view of the
// OpenCode integration: the gateway URL the plugin's provider block
// *should* point at, and nothing else.
//
// OpenCode is wired through a waired-authored plugin at
// `~/.config/opencode/plugin/waired.js` that registers the "waired"
// provider via OpenCode's config hook (see internal/integration/opencode).
// Drift between the plugin's provider baseURL and the running data-plane
// gateway is the failure mode the tray surfaces — opencode itself reports
// connection refused loudly when waired is down, so there is no
// silent-breakage class to defend against.
//
// The daemon does not look for that file and does not write it
// (waired-agent#986). It runs as a service account — LocalSystem on
// Windows, `waired` on Linux, root on macOS — whose home is not where any
// coding agent reads, so a probe from here answers about the wrong user
// and a write from here would land root-owned files in someone's home.
// That is the same boundary the wizard's coding-agent step already keeps
// (waired#935, cmd/waired/setup_integration.go): the tray and the CLI run
// as the desktop user and are the ones who observe and repair. What the
// daemon alone knows is the port its own data-plane gateway is listening
// on, so that is what it answers.
type OpenCodeIntegrationConfig struct {
	// ExpectedBaseURL is what the plugin's provider baseURL should match:
	// the agent's no-token OpenCode data-plane URL with `/v1` suffix
	// (e.g. "http://127.0.0.1:9479/v1"). Empty disables staleness
	// detection in the client — every found plugin is reported as fresh.
	ExpectedBaseURL string
}

// IntegrationExpectation is the JSON returned by GET
// /waired/v1/integration/opencode and /waired/v1/integration/openclaw.
//
// One shape for both because the answer is the same kind of fact: where
// this daemon's data-plane gateway is. The reader (tray) turns it into a
// row by probing its OWN home with internal/integration/detect.
type IntegrationExpectation struct {
	ExpectedBaseURL string `json:"expected_base_url"`
}

// WithOpenCodeIntegration enables the OpenCode integration route.
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
	writeJSON(w, http.StatusOK, IntegrationExpectation{ExpectedBaseURL: cfg.ExpectedBaseURL})
}
