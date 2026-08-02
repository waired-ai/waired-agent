package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
	"github.com/waired-ai/waired-agent/internal/integration/claudemanaged"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/platform/paths"
	"github.com/waired-ai/waired-agent/internal/proxy/legacycleanup"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// runClaude dispatches `waired claude <enable|disable|status>` — the
// managed-settings Claude Code integration that replaced the retired transparent
// MITM proxy (#488). enable writes the OS Claude Code managed-settings pointing
// ANTHROPIC_BASE_URL at waired's loopback gateway (no credential, so the
// subscription / auto-mode is preserved) and sweeps up any legacy MITM
// artifacts; disable reverses it; status is a read-only inspector.
// claudeLongText builds the `waired claude` help blurb with
// platform-correct elevated-command spellings — a bare `sudo …` was wrong
// on Windows (waired#752).
func claudeLongText() string {
	return fmt.Sprintf(`Claude Code integration via managed settings (#488): points Claude Code's
ANTHROPIC_BASE_URL at waired's local gateway with NO credential, so the
claude.ai subscription and auto-mode (opusplan / Max Opus->Sonnet fallback)
are preserved. Messages are served by local inference and fail open to the
real Anthropic API when local serving is down/degraded, so claude never
breaks. No MITM CA, /etc/hosts edit, or shell env needed.

  %s     (also done by 'waired init')
  %s
  waired claude status`,
		elevatedCmdline(runtime.GOOS, "waired claude enable"),
		elevatedCmdline(runtime.GOOS, "waired claude disable"))
}

func newClaudeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "claude",
		Short: "Claude Code integration via managed settings (enable / disable / status).",
		Long:  claudeLongText(),
		RunE:  namespaceRunE,
	}
	cmd.AddCommand(newClaudeEnableCmd(), newClaudeDisableCmd(), newClaudeStatusCmd(),
		newClaudeRouteCmd(), newClaudeNodeShimCmd(), newClaudeFallbackShimCmd(),
		newClaudeRouteSkillCmd(), newClaudeModelsCacheCmd(),
		newClaudeStatuslineCmd(), newClaudeFallbackHookCmd())
	return cmd
}

// claudeStateDirFlag attaches the shared --state-dir flag for the claude
// subcommands, defaulting to the system state dir.
func claudeStateDirFlag(cmd *cobra.Command, p *string) {
	cmd.Flags().StringVar(p, "state-dir", paths.StateDir(paths.System), "agent state directory")
}

func newClaudeEnableCmd() *cobra.Command {
	var stateDir string
	var noStatusline bool
	cmd := &cobra.Command{
		Use:   "enable",
		Short: "Write Claude Code managed settings (ANTHROPIC_BASE_URL → local gateway).",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return runClaudeEnable(stateDir, noStatusline) },
	}
	claudeStateDirFlag(cmd, &stateDir)
	cmd.Flags().BoolVar(&noStatusline, "no-statusline", false, "do not add the waired routing segment to the Claude Code statusline")
	return cmd
}

func newClaudeDisableCmd() *cobra.Command {
	var stateDir string
	cmd := &cobra.Command{
		Use:   "disable",
		Short: "Remove waired's ANTHROPIC_BASE_URL from Claude Code managed settings.",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return runClaudeDisable(stateDir) },
	}
	claudeStateDirFlag(cmd, &stateDir)
	return cmd
}

func newClaudeStatusCmd() *cobra.Command {
	var stateDir string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the Claude Code managed-settings state and gateway listener.",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return runClaudeStatus(stateDir) },
	}
	claudeStateDirFlag(cmd, &stateDir)
	return cmd
}

// claudeBaseURL resolves the loopback Anthropic base URL waired serves, derived
// from the configured ClaudeGatewayPort (agent.json over defaults).
func claudeBaseURL(stateDir string) (string, int) {
	c := agentconfig.Defaults()
	_ = c.MergeJSON(agentconfig.JSONPathFor(stateDir))
	port := c.Inference.ClaudeGatewayPort
	return fmt.Sprintf("http://127.0.0.1:%d", port), port
}

// claudeModelsTimeout bounds the /v1/models probe. Short on purpose: this runs
// inside `waired claude enable` and `waired init`, where the agent is normally
// up and answering in milliseconds — and where a down agent must cost a beat,
// not a stall. A miss is not fatal (see claudeLocalContextWindow).
const claudeModelsTimeout = 2 * time.Second

// claudeLocalContextWindow reports the input-token window local inference can
// actually serve on this host, for WriteOptions.LocalContextWindow (#408).
//
// The number lives in the daemon (gateway Deps.ContextWindowFor = min of the
// manifest's native window and the tuning the engine really applied), and the
// gateway already publishes it: /v1/models stamps it as max_input_tokens on
// every entry, including the local directive id. So the elevated CLI asks the
// one surface that already answers this instead of growing a management route
// for it. 0 means unknown — agent down, no active model, unknown sizing — and
// the caller must then leave the managed-settings value alone.
func claudeLocalContextWindow(stateDir string) int {
	baseURL, _ := claudeBaseURL(stateDir)
	return claudeLocalWindowAt(baseURL)
}

// claudeLocalWindowAt is claudeLocalContextWindow against an explicit base URL,
// so the fetch itself is testable against an httptest server rather than behind
// a function seam that would leave the real implementation unexercised
// (CLAUDE.md §Test discipline).
func claudeLocalWindowAt(baseURL string) int {
	cl := &http.Client{Timeout: claudeModelsTimeout}
	resp, err := cl.Get(strings.TrimRight(baseURL, "/") + "/v1/models")
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, claudeModelsMaxBody))
	if err != nil {
		return 0
	}
	return claudeLocalWindowFromModels(body)
}

// claudeModelsMaxBody caps the /v1/models read. The real response is a few KB;
// the cap only stops a wedged or hostile listener on the port from being read
// without bound into an elevated process.
const claudeModelsMaxBody = 1 << 20

// claudeLocalWindowFromModels picks the local directive id's max_input_tokens
// out of an Anthropic /v1/models body. Every malformed, missing or zero case
// collapses to 0 = unknown: this decides what an elevated process tells Claude
// Code about a window, so guessing is worse than declining.
func claudeLocalWindowFromModels(body []byte) int {
	var doc struct {
		Data []struct {
			ID             string `json:"id"`
			MaxInputTokens int    `json:"max_input_tokens"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return 0
	}
	for _, m := range doc.Data {
		if m.ID == claudecode.DirectiveModelLocal && m.MaxInputTokens > 0 {
			return m.MaxInputTokens
		}
	}
	return 0
}

// claudeManagedWriteOptions resolves the managed-settings write options from
// agent.json — the model-route-directives opt-in (#52) — plus the window local
// inference actually serves (#408), which sizes CLAUDE_CODE_MAX_CONTEXT_TOKENS
// for the local /model directive id.
//
// The window is probed even when directives are OFF: the feature-off scrub
// recognises waired's value by matching it, so it has to know what this host
// would have written.
func claudeManagedWriteOptions(stateDir string) claudemanaged.WriteOptions {
	c := agentconfig.Defaults()
	_ = c.MergeJSON(agentconfig.JSONPathFor(stateDir))
	return claudemanaged.WriteOptions{
		ModelRouteDirectives: c.Inference.ClaudeModelRouteDirectives,
		LocalContextWindow:   claudeLocalContextWindow(stateDir),
	}
}

func runClaudeEnable(stateDir string, noStatusline bool) error {
	baseURL, _ := claudeBaseURL(stateDir)

	// The flip itself — legacy-MITM sweep, managed-settings write, and the
	// two per-user extras — is applyClaudeRoute (init_route_claude.go), so
	// `waired claude enable` and `waired init`'s routing step leave a host
	// in the same state (#294).
	path, err := applyClaudeRoute(claudeRouteApplyOpts{
		StateDir:       stateDir,
		In:             bufio.NewScanner(os.Stdin),
		AllowPrompt:    true,
		SkipStatusline: noStatusline,
	})
	if err != nil {
		if errors.Is(err, claudemanaged.ErrUnsupportedOS) {
			return fmt.Errorf("waired claude enable: managed settings are not supported on this OS")
		}
		if os.IsPermission(err) {
			return fmt.Errorf("waired claude enable: %w\n  (writing %s needs elevation — %s)", err, claudemanaged.Path(), elevationHintFor(runtime.GOOS, "waired claude enable"))
		}
		return fmt.Errorf("waired claude enable: %w", err)
	}
	fmt.Printf("Claude Code managed settings written: %s\n", path)
	fmt.Printf("  ANTHROPIC_BASE_URL = %s  (no credential — subscription / auto-mode preserved)\n", baseURL)
	fmt.Println("  Restart any running `claude` session (or open a new shell) to pick it up.")
	fmt.Println("  In a Claude Code session, /waired-route switches routing (auto | waired | anthropic) live.")
	return nil
}

// managedRemoveIsFatal reports whether an error from claudemanaged.Remove should
// abort `claude disable` (true) or be tolerated so the per-user cleanup still
// runs (false). A permission error is tolerated: the managed-settings file is
// admin-owned (e.g. %ProgramFiles% on a Windows service install), so a
// non-elevated `claude disable` — the un-elevated, invoking-user phase of
// uninstall.ps1 — cannot edit it, yet it must still scrub THIS user's ~/.claude
// (route skill, statusline) and any retired-MITM artifacts; the elevated phase
// removes the managed file itself (waired#754). A nil error is not fatal.
func managedRemoveIsFatal(err error) bool {
	return err != nil && !os.IsPermission(err)
}

func runClaudeDisable(stateDir string) error {
	// The window lets the scrub recognise a host-derived
	// CLAUDE_CODE_MAX_CONTEXT_TOKENS as ours (#408). Best-effort by design:
	// disable frequently runs with the agent already stopped, and a 0 here
	// only means one inert key may survive — see RemoveOptions.
	removed, err := claudemanaged.RemoveWithOptions(claudemanaged.RemoveOptions{
		LocalContextWindow: claudeLocalContextWindow(stateDir),
	})
	if managedRemoveIsFatal(err) {
		return fmt.Errorf("waired claude disable: %w", err)
	}
	if err != nil {
		// Tolerated permission error (see managedRemoveIsFatal): warn, but keep
		// going so the invoking user's per-user integration is still removed.
		fmt.Fprintf(os.Stderr, "warning: could not remove %s (%v); %s. Continuing with per-user cleanup.\n",
			claudemanaged.Path(), err, elevationHintFor(runtime.GOOS, "waired claude disable"))
	}
	// Also clean up any retired MITM artifacts an upgrader may still carry.
	legacycleanup.Run(stateDir, stderrLogger())

	// Remove the /waired-route slash command and the routing statusline we
	// installed on enable (#580). The Stop hook was already dropped by
	// claudemanaged.Remove above (when it had permission).
	removeRouteSkillForInvoker()
	removeStatuslineForInvoker()
	// #407: drop the /model picker cache too. The reader only checks that its
	// baseUrl matches the live one, so leaving it behind is a documented way to
	// end up with a picker full of entries that route nowhere.
	removeModelsCacheForInvoker()

	switch {
	case err != nil:
		// Managed-settings file left for the elevated phase; nothing to report.
	case removed:
		fmt.Printf("Removed waired ANTHROPIC_BASE_URL from %s\n", claudemanaged.Path())
	default:
		fmt.Printf("No waired-managed ANTHROPIC_BASE_URL present in %s (nothing to do)\n", claudemanaged.Path())
	}
	return nil
}

// claudeWindowStatusLine renders the `waired claude status` line comparing the
// context window Claude Code will be STARTED with (the managed-settings
// CLAUDE_CODE_MAX_CONTEXT_TOKENS, frozen at its process start) against the one
// local inference actually serves right now.
//
// The two can legitimately disagree: only an elevated process may write managed
// settings (docs/decisions/20260728/1444-…, waired#935), so changing the serving
// model leaves the value behind until the next `waired claude enable` / init.
// Before #408 the value was a static 250000 and the disagreement was permanent
// AND invisible. It stays visible here until waired#1031 fixes the window as an
// advertised contract and the drift stops existing.
//
// Returns "" when neither number is known — an un-routed host has nothing to
// say here, and a status command should not manufacture a line to fill.
func claudeWindowStatusLine(goos, managed string, live int) string {
	const label = "local window:      "
	fix := fmt.Sprintf("re-run `%s`", elevatedCmdline(goos, "waired claude enable"))
	switch {
	case live <= 0 && managed == "":
		return ""
	case live <= 0:
		// Can't verify: report what Claude Code will use and say why we cannot
		// vouch for it, rather than implying agreement.
		return fmt.Sprintf("%s unknown (agent not answering)  (managed settings: %s)", label, managed)
	case managed == "":
		return fmt.Sprintf("%s %d  (managed settings: not set)", label, live)
	case managed == strconv.Itoa(live):
		return fmt.Sprintf("%s %d  (managed settings: %s)", label, live, managed)
	default:
		return fmt.Sprintf("%s %d  (managed settings: %s — STALE, Claude Code is being told the wrong window; %s)",
			label, live, managed, fix)
	}
}

func runClaudeStatus(stateDir string) error {
	baseURL, port := claudeBaseURL(stateDir)
	path, present, current := claudemanaged.View()

	fmt.Printf("managed settings:  %s (%s)\n", path, existsLabel(path))
	if present {
		if current == "" {
			fmt.Println("ANTHROPIC_BASE_URL: (not set)")
		} else {
			fmt.Printf("ANTHROPIC_BASE_URL: %s\n", current)
		}
	}
	fmt.Printf("expected base URL:  %s\n", baseURL)
	fmt.Printf("gateway listener:   127.0.0.1:%d (%s)\n", port, listenerLabel(port))
	if line := claudeWindowStatusLine(runtime.GOOS,
		claudemanaged.MaxContextTokensAt(path), claudeLocalContextWindow(stateDir)); line != "" {
		fmt.Println(line)
	}
	fmt.Printf("fallback hook:      %s\n", installedLabel(claudemanaged.StopHookInstalled()))
	if legacycleanup.Present(stateDir) {
		// Retired MITM proxy artifacts still on disk (a stale api.anthropic.com
		// hosts redirect / orphaned CA) silently break Claude Code — warn and
		// point at enable, which sweeps them while keeping managed settings
		// (waired#750).
		fmt.Printf("legacy proxy:       DETECTED — run `%s` to remove the retired MITM proxy (CA + hosts redirect)\n",
			elevatedCmdline(runtime.GOOS, "waired claude enable"))
	}
	printClaudeStatuslineStatus()
	printClaudeRouteStatus(defaultMgmtAddr)
	return nil
}

// printClaudeStatuslineStatus reports the invoking user's Claude Code statusline
// integration state, best-effort. Besides the user-scope install state it
// resolves the statusLine actually effective for the CURRENT directory (#599):
// a project-scope statusLine shadows the user-scope waired segment entirely,
// and reporting "installed" alone would hide exactly that failure mode.
func printClaudeStatuslineStatus() {
	home, _, _ := invokerHome()
	if home == "" {
		return
	}
	kind, existing, err := claudecode.DetectStatusLine(home)
	if err != nil {
		return
	}
	if kind == claudecode.StatusLineOurs || kind == claudecode.StatusLineWrapped {
		cwd, _ := os.Getwd()
		eff, effErr := claudecode.DetectEffectiveStatusLine(home, cwd, claudemanaged.Path())
		if effErr == nil && eff.Shadowed() {
			fmt.Printf("statusline:         installed but shadowed here by %s (%s scope)\n", eff.Path, eff.Scope)
			fmt.Printf("                    to show routing in that statusline, append:  %s\n", statuslineSnippet)
			return
		}
	}
	switch kind {
	case claudecode.StatusLineOurs:
		fmt.Println("statusline:         waired segment installed")
	case claudecode.StatusLineWrapped:
		fmt.Println("statusline:         wrapping your existing statusLine")
	case claudecode.StatusLineForeign:
		fmt.Printf("statusline:         not waired (custom: %s) — `waired claude statusline install --wrap` to add\n", existing)
	default:
		fmt.Println("statusline:         not installed")
	}
}

func installedLabel(b bool) string {
	if b {
		return "installed"
	}
	return "not installed"
}

// printClaudeRouteStatus appends the live per-class routing policy +
// last-fallback to `waired claude status`, best-effort: it needs a running
// agent (the boot-level routing controller lives in the daemon), so an
// unreachable agent degrades to a single informational line rather than an
// error.
func printClaudeRouteStatus(mgmt string) {
	body, err := httpGet(claudeRouteURL(mgmt))
	if err != nil {
		fmt.Println("routing:            (agent not reachable)")
		return
	}
	var st management.ClaudeRoutingState
	if err := json.Unmarshal(body, &st); err != nil {
		return
	}
	pol := st.Policy
	if pol.Main == "" {
		pol.Main = state.ClaudeRouteAuto
	}
	fmt.Printf("main conversation:  %s\n", pol.Main)
	fmt.Printf("subagents:          %s\n", claudeSubDisplay(pol))
	if st.LastFallback != nil {
		fmt.Printf("last fallback:      %s\n", claudeFallbackDisplay(st.LastFallback))
	}
}

// listenerLabel reports whether something is accepting connections on the
// loopback Claude gateway port.
func listenerLabel(port int) string {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
	if err != nil {
		return "not listening"
	}
	_ = conn.Close()
	return "listening"
}

func existsLabel(p string) string {
	if p == "" {
		return "unsupported OS"
	}
	if _, err := os.Stat(p); err == nil {
		return "present"
	}
	return "absent"
}

// stderrLogger builds a quiet text logger for the best-effort legacy cleanup so
// its progress lands on stderr without polluting the command's stdout output.
func stderrLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Writer(os.Stderr), &slog.HandlerOptions{Level: slog.LevelInfo}))
}
