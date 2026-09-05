package management

import (
	"errors"
	"io/fs"
	"net/http"
	"runtime"
	"time"

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
// into managed settings. The derivation is claudemanaged's, not a second copy
// of it — see ExpectedBaseURL there.
func (c ClaudeIntegrationConfig) expectedBaseURL() string {
	url, _ := claudemanaged.ExpectedBaseURL(c.StateDir)
	return url
}

// ClaudeIntegrationStateView is a slim projection of runtime/state for
// JSON. Mirrors the fields the tray needs without leaking the
// internal Writer struct.
type ClaudeIntegrationStateView struct {
	Phase   string `json:"phase"`
	PID     int    `json:"pid"`
	Updated string `json:"updated"`
	// GatewayURL is the LOCAL gateway (LocalGatewayPort, 9473 by default) the
	// daemon records in runtime/state — not the Claude intercept Claude Code
	// is pointed at, which is ClaudeGatewayPort (9472) and is reported as
	// ManagedSettings.ExpectedBaseURL.
	GatewayURL string `json:"gateway_url"`
	// InferenceReachableLocal and InferenceReachableInMesh are the two axes
	// Reachable is computed from: this device's own engine, and whether any
	// peer's is answering. Both are reported so a caller can say WHICH one is
	// serving rather than re-deriving it (waired-agent#1032).
	InferenceReachableLocal  bool `json:"inference_reachable_local"`
	InferenceReachableInMesh bool `json:"inference_reachable_in_mesh"`
}

// ClaudeWrapperView reports whether claude requests are currently being served
// through Waired (Reachable=true) or falling through to the real Anthropic API
// (Reachable=false + Reason). The loopback gateway always fails open, so "not
// reachable" means "falling back", not "claude is broken".
//
// "Through Waired" is this device's own engine OR a mesh peer's. Reading only
// the local engine made every engine-less host report its healthy steady state
// as a fault — the tray narrated it as "Claude Code routing inactive" while
// `waired claude status` on the same host, at the same moment, reported the
// managed settings present, the listener up and a peer serving the request
// (waired-agent#1032). It is the same mistake waired-agent#829 took out of
// the proxy's own degrade check, which has read both axes since
// (cmd/waired-agent/main.go, proxyH.SetDegraded).
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
	// SubagentModel is whatever CLAUDE_CODE_SUBAGENT_MODEL the machine-wide
	// file carries. waired stopped writing it (waired-agent#1186) and reads
	// it only to report an operator's own value.
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
			Phase:                    string(st.Phase),
			PID:                      st.PID,
			Updated:                  st.Updated.UTC().Format(time.RFC3339),
			GatewayURL:               st.GatewayURL,
			InferenceReachableLocal:  st.InferenceReachableLocal,
			InferenceReachableInMesh: st.InferenceReachableInMesh,
		}
		// A fresh agent still is not serving when nothing can answer, so
		// surface that as a reason. Nothing means NEITHER axis: an
		// engine-less host whose peer is answering is serving through
		// Waired, and saying otherwise is what waired-agent#1032 reported.
		// This is the same predicate proxyH.SetDegraded uses to decide
		// whether to pass a request through to Anthropic at all, which is
		// what makes the two answers agree by construction.
		if ok && !st.InferenceReachableLocal && !st.InferenceReachableInMesh {
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
