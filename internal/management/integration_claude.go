package management

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"runtime"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/integration/claudemanaged"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// ClaudeIntegrationConfig is what the management server needs to answer
// GET /waired/v1/integration/claude. The agent constructs this once at
// startup and hands it to WithClaudeIntegration; the handler re-reads disk
// on every request (it's cheap and the tray polls at 5s, well below any
// contention threshold).
//
// Since #488 replaced the transparent MITM proxy with Claude Code managed
// settings, this endpoint reports the MANAGED-SETTINGS status — whether the
// system-wide managed-settings.json is present and what ANTHROPIC_BASE_URL it
// carries — alongside the live serving state (Wrapper).
type ClaudeIntegrationConfig struct {
	StateDir string
	HomeDir  string
	// BinaryPath is the absolute path of the running waired binary. Kept
	// for the JSON response (the tray displays it).
	BinaryPath string

	// Now is overridable for tests; defaults to time.Now.
	Now func() time.Time
	// StaleAfter overrides state.DefaultStaleAfter for tests.
	StaleAfter time.Duration
	// ManagedSettingsPath overrides the OS managed-settings.json location for
	// tests (#604 — the real per-OS file exists on dogfooding hosts); empty
	// means the real path (claudemanaged.Path()).
	ManagedSettingsPath string
}

func (c ClaudeIntegrationConfig) now() time.Time {
	if c.Now == nil {
		return time.Now()
	}
	return c.Now()
}

func (c ClaudeIntegrationConfig) staleAfter() time.Duration {
	if c.StaleAfter > 0 {
		return c.StaleAfter
	}
	return state.DefaultStaleAfter
}

func (c ClaudeIntegrationConfig) managedSettingsPath() string {
	if c.ManagedSettingsPath != "" {
		return c.ManagedSettingsPath
	}
	return claudemanaged.Path()
}

// expectedBaseURL is the loopback Anthropic base URL waired serves and writes
// into managed settings, derived from the configured ClaudeGatewayPort.
func (c ClaudeIntegrationConfig) expectedBaseURL() string {
	cfg := agentconfig.Defaults()
	_ = cfg.MergeJSON(agentconfig.JSONPathFor(c.StateDir))
	return fmt.Sprintf("http://127.0.0.1:%d", cfg.Inference.ClaudeGatewayPort)
}

// ClaudeIntegrationStateView is a slim projection of runtime/state for
// JSON. Mirrors the fields the tray needs without leaking the
// internal Writer struct.
type ClaudeIntegrationStateView struct {
	Phase                   string `json:"phase"`
	PID                     int    `json:"pid"`
	Updated                 string `json:"updated"`
	GatewayURL              string `json:"gateway_url"`
	InferenceReachableLocal bool   `json:"inference_reachable_local"`
}

// ClaudeWrapperView reports whether claude requests are currently being
// served by local inference (Reachable=true) or falling through to the real
// Anthropic API (Reachable=false + Reason). The loopback gateway always fails
// open, so "not reachable" means "falling back", not "claude is broken".
type ClaudeWrapperView struct {
	Reachable bool                        `json:"reachable"`
	Reason    string                      `json:"reason,omitempty"`
	State     *ClaudeIntegrationStateView `json:"state,omitempty"`
}

// ClaudeManagedSettingsView reports the system-wide Claude Code managed-settings
// status: whether the OS supports the path, the path itself, whether the file is
// present, the ANTHROPIC_BASE_URL it carries, and the value waired expects.
type ClaudeManagedSettingsView struct {
	Supported       bool   `json:"supported"`
	Path            string `json:"path"`
	Present         bool   `json:"present"`
	BaseURL         string `json:"base_url"`
	ExpectedBaseURL string `json:"expected_base_url"`
	// Configured is true when the file is present and its ANTHROPIC_BASE_URL
	// matches the expected loopback gateway URL — i.e. Claude Code is wired to
	// waired. The tray uses this for its one-line status.
	Configured bool `json:"configured"`
	// SubagentModel is the CLAUDE_CODE_SUBAGENT_MODEL the file carries —
	// the subagent traffic label (#646). Empty when unset (pre-#646
	// setups until the next `waired claude enable` / init).
	SubagentModel string `json:"subagent_model,omitempty"`
}

// ClaudeIntegrationStatus is the JSON returned by GET
// /waired/v1/integration/claude.
type ClaudeIntegrationStatus struct {
	Wrapper         ClaudeWrapperView         `json:"wrapper"`
	ManagedSettings ClaudeManagedSettingsView `json:"managed_settings"`
	BinaryPath      string                    `json:"binary_path"`
}

// WithClaudeIntegration enables the GET /waired/v1/integration/claude
// route. Pass a zero-valued config to disable.
func (s *Server) WithClaudeIntegration(cfg ClaudeIntegrationConfig) *Server {
	s.claudeIntegration = &cfg
	return s
}

func (s *Server) handleClaudeIntegration(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cfg := s.claudeIntegration
	if cfg == nil {
		http.Error(w, "claude integration not configured", http.StatusNotFound)
		return
	}
	body := buildClaudeIntegration(*cfg)
	writeJSON(w, http.StatusOK, body)
}

// buildClaudeIntegration is split out so tests can drive it directly.
func buildClaudeIntegration(cfg ClaudeIntegrationConfig) ClaudeIntegrationStatus {
	out := ClaudeIntegrationStatus{
		BinaryPath:      cfg.BinaryPath,
		ManagedSettings: buildManagedSettingsView(cfg),
	}
	st, err := state.Read(cfg.StateDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		out.Wrapper = ClaudeWrapperView{Reachable: false, Reason: state.ReasonAgentStopped}
	case err != nil:
		out.Wrapper = ClaudeWrapperView{Reachable: false, Reason: "state-read-error"}
	default:
		ok, reason := st.Reason(cfg.now(), cfg.staleAfter())
		view := &ClaudeIntegrationStateView{
			Phase:                   string(st.Phase),
			PID:                     st.PID,
			Updated:                 st.Updated.UTC().Format(time.RFC3339),
			GatewayURL:              st.GatewayURL,
			InferenceReachableLocal: st.InferenceReachableLocal,
		}
		// Wrapper Stage 3 also rejects when InferenceReachableLocal
		// is false, even with a fresh agent — surface that condition
		// as a reason so the tray reflects the same gating logic.
		if ok && !st.InferenceReachableLocal {
			ok = false
			reason = state.ReasonInferenceUnavailable
		}
		out.Wrapper = ClaudeWrapperView{Reachable: ok, Reason: reason, State: view}
	}
	return out
}

// buildManagedSettingsView reads the OS managed-settings.json (best-effort) and
// reports whether Claude Code is wired to waired's loopback gateway.
func buildManagedSettingsView(cfg ClaudeIntegrationConfig) ClaudeManagedSettingsView {
	path := cfg.managedSettingsPath()
	present, baseURL := claudemanaged.ViewAt(path)
	expected := cfg.expectedBaseURL()
	return ClaudeManagedSettingsView{
		Supported:       runtime.GOOS == "linux" || runtime.GOOS == "darwin" || runtime.GOOS == "windows",
		Path:            path,
		Present:         present,
		BaseURL:         baseURL,
		ExpectedBaseURL: expected,
		Configured:      present && baseURL == expected,
		SubagentModel:   claudemanaged.SubagentModelAt(path),
	}
}
