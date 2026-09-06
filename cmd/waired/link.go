package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/setup"
)

// runLink implements `waired link [agent]` and, with uninstall=true,
// `waired unlink [agent]`. This is the gcloud-auth-login-equivalent:
// re-run only the integration slice that `waired init` already runs at
// install time.
//
// Subcommand patterns:
//
//	waired link                      # = waired link all
//	waired link all
//	waired link claude-code
//	waired link opencode
//	waired link openclaw
//	waired link --force all          # apply even when the agent is not detected
//	waired unlink                    # remove everything for every agent
//	waired unlink claude-code        # remove a single agent
//	waired link --dry-run            # show what would happen, no writes
//
// `waired link` configures only the per-user integration: the Claude
// Code skills (~/.claude/skills/), the OpenCode plugin
// (~/.config/opencode/plugin/waired.js), and the OpenClaw plugin
// (~/.openclaw/plugins/waired/ + a small openclaw.json merge). It never
// edits shell rc files or IDE settings. Claude request routing is handled by
// Claude Code managed settings (`sudo waired claude enable`, also done by
// `waired init`), not by this command; printSetupHelper points the user at it.

// linkOpts holds the `waired link` / `waired unlink` flag values. unlink
// registers only stateDir + dryRun (the apply-only flags are absent, so
// `waired unlink --force` is a parse error — the desired ergonomics).
type linkOpts struct {
	stateDir       string
	dryRun         bool
	gatewayBaseURL string
	noPrompt       bool
	force          bool
	// mgmtURL is where the finished repair is reported (waired-agent#791).
	// Not a flag: `waired link` gains no CLI surface from this, and an
	// operator naming somebody else's daemon here would be reporting a
	// repair about the wrong machine. Set once in newLinkCmd; tests point
	// it at a stub.
	mgmtURL string
}

// linkLongText builds the `waired link` help blurb with a platform-correct
// elevated-command spelling — a bare `sudo …` was wrong on Windows
// (waired#752).
func linkLongText() string {
	return fmt.Sprintf(`Set up the per-user coding-agent integration: the Claude Code skills
(~/.claude/skills/), the OpenCode plugin (~/.config/opencode/plugin/
waired.js) and the OpenClaw plugin (~/.openclaw/plugins/waired/). Pass an
agent name to target one; --force applies even when the agent is not
installed yet.

Claude REQUEST ROUTING is handled separately by Claude Code managed settings
('waired init', or '%s'), NOT by link.`, elevatedCmdline(runtime.GOOS, "waired claude enable"))
}

func newLinkCmd() *cobra.Command {
	o := &linkOpts{gatewayBaseURL: defaultGatewayURL, mgmtURL: defaultMgmtURL}
	cmd := &cobra.Command{
		Use:   "link [agent]",
		Short: "Set up the per-user coding-agent integration (Claude Code skills, OpenCode/OpenClaw plugins)",
		Long:  linkLongText(),
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			o.gatewayBaseURL = resolveGatewayBaseURL(cmd, o.mgmtURL, o.stateDir, o.gatewayBaseURL)
			return runLinkWith(o, false, args)
		},
	}
	addStateDirFlag(cmd, &o.stateDir, "directory holding identity / secrets / integrations ledger")
	cmd.Flags().BoolVar(&o.dryRun, "dry-run", false, "print what would change but do not write")
	cmd.Flags().StringVar(&o.gatewayBaseURL, "gateway-base-url", defaultGatewayURL,
		"local gateway base URL (the OpenCode/OpenClaw plugins point at this); defaults to this host's configured port")
	cmd.Flags().BoolVar(&o.noPrompt, "no-prompt", false,
		"do not prompt the user for setup-helper choices (used in CI / scripts)")
	cmd.Flags().BoolVar(&o.force, "force", false,
		"apply the integration even when the coding agent is not detected (it activates once the agent is installed)")
	return cmd
}

func newUnlinkCmd() *cobra.Command {
	o := &linkOpts{gatewayBaseURL: defaultGatewayURL}
	cmd := &cobra.Command{
		Use:   "unlink [agent]",
		Short: "Remove the coding-agent integration (ledger-based, surgical)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLinkWith(o, true, args)
		},
	}
	addStateDirFlag(cmd, &o.stateDir, "directory holding identity / secrets / integrations ledger")
	cmd.Flags().BoolVar(&o.dryRun, "dry-run", false, "print what would change but do not write")
	return cmd
}

func runLinkWith(o *linkOpts, uninstall bool, posArgs []string) error {
	name := "link"
	if uninstall {
		name = "unlink"
	}
	stateDir := &o.stateDir
	dryRun := &o.dryRun
	gatewayBaseURL := &o.gatewayBaseURL
	noPrompt := &o.noPrompt
	force := &o.force

	rest := posArgs
	target := "all"
	if len(rest) > 0 {
		target = rest[0]
	}

	homeDir, _ := os.UserHomeDir()
	if homeDir == "" {
		return fmt.Errorf("waired %s: cannot resolve $HOME", name)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if *dryRun {
		return printLinkPlan(target, uninstall, *force, homeDir, *stateDir, *gatewayBaseURL)
	}

	opts := setup.IntegrationOptions{
		HomeDir:        homeDir,
		StateDir:       *stateDir,
		GatewayBaseURL: *gatewayBaseURL,
		NonInteractive: !isTerminal(os.Stdin),
		WiredBinary:    wairedBinaryPath(),
	}

	helperOpts := helperPrintOptions{
		HomeDir:     homeDir,
		WiredBinary: wairedBinaryPath(),
		Interactive: !*noPrompt && isTerminal(os.Stdin),
	}

	switch target {
	case "all", "":
		if uninstall {
			// Read before the removal: Uninstall deletes the ledger rows
			// this reads from.
			kept := recordedConfigBackups(*stateDir, allIntegrationIDs())
			if err := setup.UninstallAll(ctx, opts); err != nil {
				return err
			}
			cleanupShellResidue(homeDir)
			fmt.Fprintln(stdout, "Coding-agent integration removed.")
			printKeptConfigBackups(stdout, kept)
			return nil
		}
		// Undetected agents are no longer a silent skip: --force applies
		// them outright, and an interactive run asks once (default Yes)
		// so the integration activates when the agent gets installed.
		opts.Force = resolveLinkForce(os.Stdin, stdout,
			*force, *noPrompt, isTerminal(os.Stdin),
			detectIntegrationAgents(ctx, homeDir))
		res, err := setup.Integration(ctx, opts)
		if err != nil {
			return err
		}
		printIntegrationSummary(res)
		// fail-fast contract: any per-agent error → non-zero exit.
		for _, ar := range res.Agents {
			if ar.Err != nil {
				// Reported before returning so a partial run is not
				// claimed as a repair: linkIntegrationReport refuses this
				// result anyway, and calling it here keeps the one place
				// that decides in the one place that is reached.
				reportLinkIntegrations(o.mgmtURL, linkIntegrationReport(target, uninstall, res, ar.Err))
				return fmt.Errorf("integration: %s: %w", ar.Agent, ar.Err)
			}
		}
		// waired-agent#791: the coding-tools row can be red, and this is
		// the command its warning names. Say so, best-effort — after the
		// outcome is known and before anything that reads stdin.
		reportLinkIntegrations(o.mgmtURL, linkIntegrationReport(target, uninstall, res, nil))
		printSetupHelper(target, helperOpts, stdout, os.Stdin)
		return nil
	case "claude-code", "opencode", "openclaw":
		id := integration.AgentID(target)
		if uninstall {
			kept := recordedConfigBackups(*stateDir, []integration.AgentID{id})
			if err := setup.UninstallOne(ctx, id, opts); err != nil {
				return err
			}
			if id == integration.AgentClaudeCode {
				cleanupShellResidue(homeDir)
			}
			fmt.Fprintf(stdout, "%s integration removed.\n", id)
			printKeptConfigBackups(stdout, kept)
			return nil
		}
		res, err := setup.IntegrationOne(ctx, id, opts)
		if err != nil {
			return err
		}
		printIntegrationSummary(res)
		for _, ar := range res.Agents {
			if ar.Err != nil {
				return fmt.Errorf("integration: %s: %w", ar.Agent, ar.Err)
			}
		}
		printSetupHelper(target, helperOpts, stdout, os.Stdin)
		return nil
	default:
		return fmt.Errorf("unknown agent %q (expected: all | claude-code | opencode | openclaw)", target)
	}
}

// allIntegrationIDs is the set `link all` / `unlink all` drives, in the
// order the summary prints them.
func allIntegrationIDs() []integration.AgentID {
	return []integration.AgentID{
		integration.AgentClaudeCode,
		integration.AgentOpenCode,
		integration.AgentOpenClaw,
	}
}

// recordedConfigBackups returns, per agent, the config backup the ledger
// records — the copy taken before waired first changed a config file the
// user owns. Only the OpenClaw adapter takes one today.
//
// unlink leaves that copy in place: the removal puts the keys back but not
// the user's original key order or formatting, so the backup is the only
// copy of the file as they wrote it. Naming it is what stops it from being
// unexplained residue. Best-effort: a missing or unreadable ledger is not
// an error worth failing an otherwise clean removal over.
//
// Must be called BEFORE Uninstall, which deletes the rows it reads.
func recordedConfigBackups(stateDir string, ids []integration.AgentID) map[integration.AgentID]string {
	paths, err := integration.PathsFor(stateDir)
	if err != nil {
		return nil
	}
	ledger, err := integration.LoadLedger(paths.Ledger)
	if err != nil {
		return nil
	}
	out := map[integration.AgentID]string{}
	for _, id := range ids {
		rec, ok := ledger.Get(id)
		if !ok || rec.BackupPath == "" {
			continue
		}
		if _, err := os.Stat(rec.BackupPath); err != nil {
			continue
		}
		out[id] = rec.BackupPath
	}
	return out
}

// printKeptConfigBackups reports the backups unlink deliberately left behind.
func printKeptConfigBackups(w io.Writer, kept map[integration.AgentID]string) {
	for _, id := range allIntegrationIDs() {
		if path := kept[id]; path != "" {
			_, _ = fmt.Fprintf(w, "Your %s config as it was before waired changed it is kept at %s\n", id, path)
		}
	}
}

// cleanupShellResidue removes both the v2 alias-block (best-effort) and
// the v1 `# >>> waired managed` sentinel block from the user's rc
// files. Running this on every `waired unlink` is what closes
// the original silent-breakage class for users still carrying a
// dotfile from a v1 install: their next uninstall scrubs the residue
// even though no v2 component ever wrote it.
func cleanupShellResidue(homeDir string) {
	if changed := bestEffortUninstallShellAlias(homeDir); changed > 0 {
		fmt.Fprintf(stdout, "removed waired claude alias from %d rc file(s)\n", changed)
	}
	if changed, err := setup.SweepLegacyManagedBlocks(homeDir); err == nil && len(changed) > 0 {
		fmt.Fprintf(stdout, "removed legacy waired sentinel block from %d rc file(s)\n", len(changed))
	}
}

// resolveLinkForce decides whether `waired link all` applies adapters
// whose Detect() is negative. --force wins outright; otherwise an
// interactive TTY run (no --no-prompt) with at least one undetected
// agent asks once, default Yes, so the integration activates the moment
// the agent gets installed. Non-interactive runs stay Detect-gated for
// script compatibility — pass --force to override.
func resolveLinkForce(in io.Reader, out io.Writer, forceFlag, noPrompt, interactive bool, dets []agentDetection) bool {
	if forceFlag {
		return true
	}
	if noPrompt || !interactive {
		return false
	}
	undetected := false
	for _, d := range dets {
		if !d.Found {
			undetected = true
			break
		}
	}
	if !undetected {
		return false // everything detected; plain Detect-gated apply hits all of them anyway
	}
	printAgentDetections(out, dets)
	sc := bufio.NewScanner(in)
	return ynPrompt(out, sc,
		"Set up the integration anyway so it activates once installed?", true)
}

// printLinkPlan renders a short, human-readable plan when --dry-run is
// used. Real apply / uninstall paths print their own per-agent summary.
func printLinkPlan(target string, uninstall, force bool, home, state, baseURL string) error {
	verb := "apply"
	if uninstall {
		verb = "remove"
	}
	fmt.Fprintf(stdout, "Plan: %s coding-agent integration (%s)\n", verb, target)
	fmt.Fprintf(stdout, "  $HOME              = %s\n", home)
	fmt.Fprintf(stdout, "  state directory    = %s\n", state)
	fmt.Fprintf(stdout, "  gateway base URL   = %s\n", baseURL)
	if force && !uninstall {
		fmt.Fprintln(stdout, "  force              = apply even when the agent is not detected")
	}
	if !uninstall {
		switch target {
		case "all", "":
			fmt.Fprintln(stdout, "  agents             = claude-code (skills only), opencode (plugin + commands), openclaw (plugin + openclaw.json)")
		default:
			fmt.Fprintf(stdout, "  agents             = %s\n", target)
		}
	} else {
		fmt.Fprintln(stdout, "  removes agent-managed skill / command files,")
		fmt.Fprintln(stdout, "  the OpenCode plugin (~/.config/opencode/plugin/waired.js) and its command files,")
		fmt.Fprintln(stdout, "  the OpenClaw plugin (~/.openclaw/plugins/waired/) and its")
		fmt.Fprintln(stdout, "  openclaw.json keys, the v2 `waired-claude alias` block from rc")
		fmt.Fprintln(stdout, "  files, and any residual v1 `# >>> waired managed` block (best-effort).")
		fmt.Fprintln(stdout, "  keeps the copy of openclaw.json taken before waired first")
		fmt.Fprintln(stdout, "  changed it (openclaw.json.waired-bak-<timestamp>), and prints where it is.")
	}
	fmt.Fprintln(stdout, "\nRun without --dry-run to apply.")
	return nil
}

func printIntegrationSummary(res *setup.IntegrationResult) {
	if res == nil {
		return
	}
	for _, ar := range res.Agents {
		switch {
		case ar.Skipped:
			fmt.Fprintf(stdout, "%-12s skipped (not detected — run `waired link --force` to set up anyway)\n", ar.Agent+":")
		case ar.Err != nil:
			fmt.Fprintf(stdout, "%-12s FAILED: %v\n", ar.Agent+":", ar.Err)
		case ar.Applied:
			fmt.Fprintf(stdout, "%-12s configured\n", ar.Agent+":")
		}
	}
}
