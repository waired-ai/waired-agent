package main

import (
	"bufio"
	"context"
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
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
	"github.com/waired-ai/waired-agent/internal/integration/claudemanaged"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/platform/paths"
	"github.com/waired-ai/waired-agent/internal/proxy/legacycleanup"
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
are preserved. Each turn runs where the model you picked in /model says: a
Waired entry runs it on your computers, an Anthropic model runs it on your
Claude subscription. Waired never moves a turn to the other side on its own
- when it cannot answer, it says so. No MITM CA, /etc/hosts edit, or shell
env needed.

  %s     (also done by 'waired init')
  %s
  waired claude status`,
		elevatedCmdline(runtime.GOOS, "waired claude enable"),
		elevatedCmdline(runtime.GOOS, "waired claude disable"))
}

func newClaudeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "claude",
		Short: "Claude Code integration via managed settings (enable / disable / status)",
		Long:  claudeLongText(),
		RunE:  namespaceRunE,
	}
	cmd.AddCommand(newClaudeEnableCmd(), newClaudeDisableCmd(), newClaudeStatusCmd(),
		newClaudeRouteShimCmd(), newClaudeNodeShimCmd(), newClaudeFallbackShimCmd(),
		newClaudeRouteSkillCmd(), newClaudePickerCmd(), newClaudeModelDefaultCmd(),
		newClaudeStatuslineCmd(), newClaudeSubagentsCmd())
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
		Short: "Write Claude Code managed settings (ANTHROPIC_BASE_URL → local gateway)",
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
		Short: "Remove waired's ANTHROPIC_BASE_URL from Claude Code managed settings",
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
		Short: "Show the Claude Code managed-settings state and gateway listener",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return runClaudeStatus(stateDir) },
	}
	claudeStateDirFlag(cmd, &stateDir)
	return cmd
}

// claudeBaseURL resolves the loopback Anthropic base URL waired serves. The
// derivation lives in claudemanaged so this command and the management API
// answer the same question with the same string (waired-agent#1032).
func claudeBaseURL(stateDir string) (string, int) {
	return claudemanaged.ExpectedBaseURL(stateDir)
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
	local := claudeLocalContextWindow(stateDir)
	opts := claudemanaged.WriteOptions{
		ModelRouteDirectives: c.Inference.ClaudeModelRouteDirectives,
		LocalContextWindow:   local,
		ModelPeerEntries:     c.Inference.ClaudeModelPeerEntries,
	}
	// Only when there is no engine here. A host that serves already has the
	// exact number for the row most people use, and reading the mesh for a
	// fallback it would not use is a round trip for nothing.
	if local == 0 {
		opts.PeerContextWindow = claudeReachableContextWindow(defaultMgmtAddr)
	}
	return opts
}

// claudeReachableContextWindow is the SMALLEST input-token window among the
// computers this one can currently reach, or 0 when it can reach none
// (waired-agent#1246).
//
// Smallest, because this number sizes one session and the rows it covers are
// several computers: declaring more than the smallest means a turn is
// compacted only after the gateway has already refused it, which is the one
// outcome the variable exists to avoid.
//
// Every failure collapses to 0 and writes nothing, the same rule
// claudeLocalWindowFromModels follows: this decides what an elevated process
// tells Claude Code about a window, so declining beats guessing.
func claudeReachableContextWindow(mgmtAddr string) int {
	ctx, cancel := context.WithTimeout(context.Background(), pickerMeshTimeout)
	defer cancel()
	snap, err := fetchMeshSnapshotCtx(ctx, mgmtAddr)
	if err != nil || snap == nil {
		return 0
	}
	smallest := 0
	for _, p := range snap.Peers {
		if !inferencemesh.PeerServing(p) || p.InferenceState == nil {
			continue
		}
		w := p.InferenceState.ContextWindow
		if w <= 0 {
			continue
		}
		if smallest == 0 || w < smallest {
			smallest = w
		}
	}
	return smallest
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
		// waired-agent#1188: an organisation manages this machine's Claude
		// Code. Not an error to work around — the write would switch off the
		// policy that organisation delivers — so say what was found and what
		// the person can do that does not touch the machine-wide file.
		var org *claudemanaged.ErrOrgManaged
		if errors.As(err, &org) {
			printOrgManagedRefusal(org)
			// The refusal is the whole message and it is several lines, so
			// returning the error too would print a one-line summary of it
			// underneath. A non-zero exit still comes from the error itself
			// once it reaches main; this one is deliberately terse.
			return fmt.Errorf("waired claude enable: settings left unchanged")
		}
		if os.IsPermission(err) {
			return fmt.Errorf("waired claude enable: %w\n  (writing %s needs elevation — %s)", err, claudemanaged.Path(), elevationHintFor(runtime.GOOS, "waired claude enable"))
		}
		return fmt.Errorf("waired claude enable: %w", err)
	}
	fmt.Fprintf(stdout, "Claude Code managed settings written: %s\n", path)
	fmt.Fprintf(stdout, "  ANTHROPIC_BASE_URL = %s  (no credential — subscription / auto-mode preserved)\n", baseURL)
	fmt.Fprintln(stdout, "  Restart any running `claude` session (or open a new shell) to pick it up.")
	fmt.Fprintln(stdout, "  In a Claude Code session, /model picks where a turn runs: a Waired entry for your")
	fmt.Fprintln(stdout, "  computers, an Anthropic model for your Claude subscription.")
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

// leftoverContextWindow returns the CLAUDE_CODE_MAX_CONTEXT_TOKENS a scrub
// has just left behind, or "" when nothing was left.
//
// Only when the window was unknown: with a known window the scrub either
// recognised the value as ours and removed it, or recognised it as an
// operator's and kept it on purpose (waired-agent#1174).
func leftoverContextWindow(window int) string {
	if window > 0 {
		return ""
	}
	return claudemanaged.MaxContextTokensAt(claudemanaged.Path())
}

func runClaudeDisable(stateDir string) error {
	// The window lets the scrub recognise a host-derived
	// CLAUDE_CODE_MAX_CONTEXT_TOKENS as ours (#408). Best-effort by design:
	// disable frequently runs with the agent already stopped, and a 0 here
	// only means one inert key may survive — see RemoveOptions.
	opts := claudemanaged.RemoveOptions{LocalContextWindow: claudeLocalContextWindow(stateDir)}
	if opts.LocalContextWindow == 0 {
		// The engine-less host wrote a reachable window rather than none
		// (waired-agent#1246), so the scrub has to be able to recognise that
		// number too.
		opts.PeerContextWindow = claudeReachableContextWindow(defaultMgmtAddr)
	}
	window := opts.DeclaredContextWindow()
	removed, err := claudemanaged.RemoveWithOptions(opts)
	// waired-agent#1174: with the window unknown the scrub cannot tell our
	// own value from an operator's, so it keeps it — and the file is
	// rewritten carrying that one key. On a machine Waired is being removed
	// from, a stale window goes on steering every Claude Code session that
	// starts there. Say so here rather than leaving it to be found later:
	// the uninstall transcript is where someone would look.
	if left := leftoverContextWindow(window); left != "" {
		fmt.Fprintf(stderr, "Warning: left %s=%s in %s — waired-agent could not be reached, "+
			"so this value could not be confirmed as ours. Remove it by hand if you did not set it.\n",
			claudemanaged.MaxContextTokensKey, left, claudemanaged.Path())
	}
	if managedRemoveIsFatal(err) {
		return fmt.Errorf("waired claude disable: %w", err)
	}
	if err != nil {
		// Tolerated permission error (see managedRemoveIsFatal): warn, but keep
		// going so the invoking user's per-user integration is still removed.
		fmt.Fprintf(stderr, "Warning: could not remove %s (%v); %s. Continuing with per-user cleanup.\n",
			claudemanaged.Path(), err, elevationHintFor(runtime.GOOS, "waired claude disable"))
	}
	// Also clean up any retired MITM artifacts an upgrader may still carry.
	legacycleanup.Run(stateDir, stderrLogger())

	// Remove the retired /waired-route slash command and the statusline we
	// installed on enable (#580). The Stop hook was already dropped by
	// claudemanaged.Remove above (when it had permission).
	removeRouteSkillForInvoker()
	removeStatuslineForInvoker()
	// #407: drop the /model picker cache too. The reader only checks that its
	// baseUrl matches the live one, so leaving it behind is a documented way to
	// end up with a picker full of entries that route nowhere.
	removePickerRowsForInvoker()
	// waired-agent#1037: and the default model, which points at an id that no
	// longer routes anywhere once the gateway is out of the picture. Only ours
	// is dropped; a model the operator picked themselves stays.
	removeModelDefaultForInvoker()
	// waired-agent#1186: and the subagent switch, for the same reason — it
	// points every subagent at a Waired id, and with the gateway gone that id
	// reaches nothing. A value the operator set themselves stays.
	removeSubagentPlacementForInvoker()

	switch {
	case err != nil:
		// Managed-settings file left for the elevated phase; nothing to report.
	case removed:
		fmt.Fprintf(stdout, "Removed waired ANTHROPIC_BASE_URL from %s\n", claudemanaged.Path())
	default:
		fmt.Fprintf(stdout, "No waired-managed ANTHROPIC_BASE_URL present in %s (nothing to do)\n", claudemanaged.Path())
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
// reachable is the window this host would declare when it has no engine of its
// own — the smallest one it can reach (waired-agent#1246). Without it this line
// reported "unknown (agent not answering)" on a perfectly healthy engine-less
// host, which is both a false diagnosis and a different number from the one
// `waired claude enable` writes there.
//
// Returns "" when neither number is known — an un-routed host has nothing to
// say here, and a status command should not manufacture a line to fill.
func claudeWindowStatusLine(goos, managed string, live, reachable int) string {
	const label = "local window:      "
	fix := fmt.Sprintf("re-run `%s`", elevatedCmdline(goos, "waired claude enable"))
	if live <= 0 && reachable > 0 {
		// The label stays "local window" because that is what the line is
		// called on every other host and in the docs; the value says where
		// the number came from, which is the part that differs.
		got := fmt.Sprintf("%s none here — %d from another computer", label, reachable)
		switch {
		case managed == "":
			return got + "  (managed settings: not set)"
		case managed == strconv.Itoa(reachable):
			return got + fmt.Sprintf("  (managed settings: %s)", managed)
		default:
			return got + fmt.Sprintf("  (managed settings: %s — STALE, Claude Code is being told the wrong window; %s)",
				managed, fix)
		}
	}
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

// claudeStatusLabel is the width `waired claude status` aligns its labels to,
// and claudeStatusIndent the continuation under one of them.
const (
	claudeStatusLabel  = 20
	claudeStatusIndent = "                    "
)

// claudeShellFormNote is the continuation printed under a waired command written
// for a shell this computer may not have — the pre-waired-agent#787 POSIX
// one-liners on Windows. Printed only when there is something to say, so a
// healthy status gains no line.
//
// fix arrives as a whole sentence, already spelled for this OS: on Windows the
// managed-settings rewrite needs an Administrator prompt and no sudo, which is
// elevationHintFor's job (waired#752).
func claudeShellFormNote(fix string) string {
	return claudeStatusIndent + "written for a Unix shell — Claude Code runs it here only when Git Bash\n" +
		claudeStatusIndent + "is installed. To rewrite it:\n" +
		claudeStatusIndent + "  " + fix + "\n"
}

// claudeRefreshHookStatusRows is the same row for the SessionStart hook that
// keeps the /model picker entries current (waired-agent#830). Separate row
// rather than a combined one: the two can be in different states — the
// refresh hook is only installed when the directives feature is on — and a
// single "hooks: ok" would hide which of them is not.
func claudeRefreshHookStatusRows(goos, hookCommand string) string {
	return hookStatusRow(goos, "/model refresh:", hookCommand, claudemanaged.RefreshHookRunsOn)
}

// hookStatusRow renders one hook's state from the command actually recorded,
// not from its presence — presence alone is what let a Windows host report a
// hook it could not run (waired-agent#787).
func hookStatusRow(goos, label, hookCommand string, runsOn func(string, string) bool) string {
	l := fmt.Sprintf("%-*s", claudeStatusLabel, label)
	switch {
	case hookCommand == "":
		return l + "not installed\n"
	case runsOn(goos, hookCommand):
		return l + "installed\n"
	default:
		return l + "installed, but not in the form this computer runs\n" +
			claudeShellFormNote(elevationHintFor(goos, "waired claude enable"))
	}
}

func runClaudeStatus(stateDir string) error {
	baseURL, port := claudeBaseURL(stateDir)
	path := claudemanaged.Path()
	present, current, readErr := claudemanaged.ViewDetailAt(path)

	fmt.Fprintf(stdout, "managed settings:  %s (%s)\n", path, existsLabel(path))
	if present {
		switch {
		case readErr != nil:
			// Not the same thing as "(not set)", and the difference matters:
			// Claude Code reads a file waired cannot, so routing can be live
			// while every line below reports it as absent. Say which of the
			// two it is (waired-agent#1067).
			fmt.Fprintln(stdout, "ANTHROPIC_BASE_URL: UNREADABLE — this file is not JSON waired can parse.")
			fmt.Fprintln(stdout, "                    Claude Code may still be routed by it. Re-write it with")
			fmt.Fprintln(stdout, "                    `waired claude enable`, or save it as UTF-8 without a byte-order mark.")
		case current == "":
			fmt.Fprintln(stdout, "ANTHROPIC_BASE_URL: (not set)")
		default:
			fmt.Fprintf(stdout, "ANTHROPIC_BASE_URL: %s\n", current)
		}
	}
	fmt.Fprintf(stdout, "expected base URL:  %s\n", baseURL)
	fmt.Fprintf(stdout, "gateway listener:   127.0.0.1:%d (%s)\n", port, listenerLabel(port))
	// The reachable window is only resolved when there is no local one, the
	// same order `waired claude enable` writes in — otherwise status would
	// pay for a mesh read on every host that does not need it.
	live := claudeLocalContextWindow(stateDir)
	reachable := 0
	if live == 0 {
		reachable = claudeReachableContextWindow(defaultMgmtAddr)
	}
	if line := claudeWindowStatusLine(runtime.GOOS,
		claudemanaged.MaxContextTokensAt(path), live, reachable); line != "" {
		fmt.Fprintln(stdout, line)
	}
	fmt.Fprint(stdout, claudeRefreshHookStatusRows(runtime.GOOS, claudemanaged.RefreshHookCommandAt(path)))
	if legacycleanup.Present(stateDir) {
		// Retired MITM proxy artifacts still on disk (a stale api.anthropic.com
		// hosts redirect / orphaned CA) silently break Claude Code — warn and
		// point at enable, which sweeps them while keeping managed settings
		// (waired#750).
		fmt.Fprintf(stdout, "legacy proxy:       DETECTED — run `%s` to remove the retired MITM proxy (CA + hosts redirect)\n",
			elevatedCmdline(runtime.GOOS, "waired claude enable"))
	}
	printClaudeStatuslineStatus()
	// The live value managed settings carry, not the expected one: the client
	// compares the cache against what it is actually pointed at.
	printClaudePickerStatus(current)
	printClaudeDefaultModelStatus()
	printClaudeSubagentStatusForInvoker()
	printClaudeRouteStatus(defaultMgmtAddr)
	return nil
}

// printClaudeDefaultModelStatus reports the model id new sessions start on. It
// is the line that explains an idle computer: a default that names a real
// Anthropic model sends every untouched session to the real API, and nothing
// else on this screen would say so (waired-agent#1037).
func printClaudeDefaultModelStatus() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	kind, id, err := claudecode.DetectModelSetting(home)
	if err != nil {
		return
	}
	switch kind {
	case claudecode.ModelSettingOurs:
		fmt.Fprintf(stdout, "default model:      %s  (new sessions; change it with /model)\n", id)
	case claudecode.ModelSettingForeign:
		if id == "" {
			return
		}
		fmt.Fprintf(stdout, "default model:      %s — new sessions go to the real Anthropic API\n", id)
		fmt.Fprintln(stdout, "                    pick a Waired entry in /model to use your own computers")
	default:
		fmt.Fprintln(stdout, "default model:      not set — Claude Code uses its own, which is a real Anthropic model")
		fmt.Fprintln(stdout, "                    pick a Waired entry in /model to use your own computers")
	}
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
			fmt.Fprintf(stdout, "statusline:         installed but shadowed here by %s (%s scope)\n", eff.Path, eff.Scope)
			fmt.Fprintf(stdout, "                    to show routing in that statusline, append:  %s\n", statuslineSnippet)
			return
		}
	}
	switch kind {
	case claudecode.StatusLineOurs:
		fmt.Fprintln(stdout, "statusline:         waired segment installed")
	case claudecode.StatusLineWrapped:
		fmt.Fprintln(stdout, "statusline:         wrapping your existing statusLine")
	case claudecode.StatusLineForeign:
		fmt.Fprintf(stdout, "statusline:         not waired (custom: %s) — `waired claude statusline install --wrap` to add\n", existing)
	default:
		fmt.Fprintln(stdout, "statusline:         not installed")
	}
	// Same question the hook row answers, one surface over: an entry written
	// for another OS's shell is installed and still does nothing
	// (waired-agent#787). Printed under the row rather than folded into it so
	// the row's wording — which docs-site quotes — does not move.
	if (kind == claudecode.StatusLineOurs || kind == claudecode.StatusLineWrapped) &&
		!claudecode.StatusLineRunsOn(runtime.GOOS, kind, existing) {
		fmt.Fprint(stdout, claudeShellFormNote("re-run `waired claude statusline install`"))
	}
}

// printClaudeRouteStatus appends what the Claude surface last did to
// `waired claude status`, best-effort: it needs a running agent (the record
// lives in the daemon), so an unreachable agent degrades to a single
// informational line rather than an error.
//
// There is no routing policy left to print. A turn runs where its model id
// says, so the two lines that answer "where is my work going" are what the
// last turn asked for and what answered it
// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md).
func printClaudeRouteStatus(mgmt string) {
	body, err := httpGet(claudeRouteURL(mgmt))
	if err != nil {
		fmt.Fprintln(stdout, "last turn:          (agent not reachable)")
		return
	}
	var st management.ClaudeRoutingState
	if err := json.Unmarshal(body, &st); err != nil {
		return
	}
	if line := claudeLastRequestDisplay(st); line != "" {
		fmt.Fprintf(stdout, "last request:       %s\n", line)
	}
	if st.LastLocalModel != "" || !st.LastServedAt.IsZero() {
		fmt.Fprintf(stdout, "last served:        %s\n",
			claudeServedDisplay(st, claudePeerNameLookup(mgmt, st.LastServedBy)))
	}
	// Which of your computers answers a turn addressed to Waired. It is the
	// `waired worker` preference, and the one remaining choice on this page:
	// the side is the model id's to decide, the node is this.
	if line := claudeWairedNodeLine(mgmt); line != "" {
		fmt.Fprintf(stdout, "waired node:        %s\n", line)
	}
}

// claudeLastRequestDisplay names the model the last main-conversation turn
// carried and which side that id named — the line waired-agent#1036 asked
// for. It is what makes a surprise legible: a session that started on
// claude-opus-5 is answered by the real Anthropic API, and nothing else on
// this host would say so. Empty until a turn has been seen, and on an agent
// predating the field.
func claudeLastRequestDisplay(st management.ClaudeRoutingState) string {
	if st.LastRequestModel == "" {
		return ""
	}
	line := st.LastRequestModel
	if st.LastRequestRoute != "" {
		line += " → " + claudeRouteDestination(st.LastRequestRoute)
	}
	if !st.LastRequestAt.IsZero() {
		line += "   (" + humanAge(time.Now(), st.LastRequestAt) + ")"
	}
	return line
}

// claudeRouteDestination names the side a model id sent the turn to, in the
// words the status line already uses.
func claudeRouteDestination(route string) string {
	if route == claudecode.RouteAnthropic {
		return "the real Anthropic API"
	}
	return "Waired"
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
	return slog.New(slog.NewTextHandler(io.Writer(stderr), &slog.HandlerOptions{Level: slog.LevelInfo}))
}
