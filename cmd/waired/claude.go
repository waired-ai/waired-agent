package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"runtime"
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
		newClaudeRouteSkillCmd(),
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

func runClaudeEnable(stateDir string, noStatusline bool) error {
	baseURL, _ := claudeBaseURL(stateDir)

	// Sweep any retired MITM proxy artifacts first: a stale api.anthropic.com
	// hosts redirect would otherwise break the new gateway's passthrough leg.
	legacycleanup.Run(stateDir, stderrLogger())

	// Write also installs the Stop hook (managed-settings hooks.Stop) so a
	// post-dispatch fallback is visible in the Claude Code TUI (#580).
	path, err := claudemanaged.Write(baseURL)
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

	// Install the /waired-route in-session escape hatch (#580) into the
	// invoking user's ~/.claude/skills/. Best-effort — the managed-settings
	// write above is the core of enable.
	installRouteSkillForInvoker()
	fmt.Println("  In a Claude Code session, /waired-route switches routing (auto | waired | anthropic) live.")

	// Add the routing statusline segment to the invoking user's Claude Code
	// footer (#580). Absent ⇒ injected; a pre-existing one ⇒ ask before wrapping.
	installStatuslineForInvoker(noStatusline, true)
	return nil
}

func runClaudeDisable(stateDir string) error {
	removed, err := claudemanaged.Remove()
	if err != nil {
		if os.IsPermission(err) {
			return fmt.Errorf("waired claude disable: %w\n  (editing %s needs elevation — %s)", err, claudemanaged.Path(), elevationHintFor(runtime.GOOS, "waired claude disable"))
		}
		return fmt.Errorf("waired claude disable: %w", err)
	}
	// Also clean up any retired MITM artifacts an upgrader may still carry.
	legacycleanup.Run(stateDir, stderrLogger())

	// Remove the /waired-route slash command and the routing statusline we
	// installed on enable (#580). The Stop hook was already dropped by
	// claudemanaged.Remove above.
	removeRouteSkillForInvoker()
	removeStatuslineForInvoker()

	if removed {
		fmt.Printf("Removed waired ANTHROPIC_BASE_URL from %s\n", claudemanaged.Path())
	} else {
		fmt.Printf("No waired-managed ANTHROPIC_BASE_URL present in %s (nothing to do)\n", claudemanaged.Path())
	}
	return nil
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
