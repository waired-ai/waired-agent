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
}

// linkLongText builds the `waired link` help blurb with a platform-correct
// elevated-command spelling — a bare `sudo …` was wrong on Windows
// (waired#752).
func linkLongText() string {
	return fmt.Sprintf(`Set up the per-user coding-agent integration: the Claude Code skills
(~/.claude/skills/) and the OpenCode plugin (~/.config/opencode/plugin/
waired.js). Pass an agent name to target one; --force applies even when the
agent is not installed yet.

Claude REQUEST ROUTING is handled separately by Claude Code managed settings
('waired init', or '%s'), NOT by link.`, elevatedCmdline(runtime.GOOS, "waired claude enable"))
}

func newLinkCmd() *cobra.Command {
	o := &linkOpts{gatewayBaseURL: defaultGatewayURL}
	cmd := &cobra.Command{
		Use:   "link [agent]",
		Short: "Set up the per-user coding-agent integration (Claude Code skills, OpenCode/OpenClaw plugins).",
		Long:  linkLongText(),
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLinkWith(o, false, args)
		},
	}
	addStateDirFlag(cmd, &o.stateDir, "directory holding identity / secrets / integrations ledger")
	cmd.Flags().BoolVar(&o.dryRun, "dry-run", false, "print what would change but do not write")
	cmd.Flags().StringVar(&o.gatewayBaseURL, "gateway-base-url", defaultGatewayURL,
		"Local Gateway base URL (the OpenCode/OpenClaw plugins derive their data-plane URL from this)")
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
		Short: "Remove the coding-agent integration (ledger-based, surgical).",
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
			if err := setup.UninstallAll(ctx, opts); err != nil {
				return err
			}
			cleanupShellResidue(homeDir)
			fmt.Println("Coding-agent integration removed.")
			return nil
		}
		// Undetected agents are no longer a silent skip: --force applies
		// them outright, and an interactive run asks once (default Yes)
		// so the integration activates when the agent gets installed.
		opts.Force = resolveLinkForce(os.Stdin, os.Stdout,
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
				return fmt.Errorf("integration: %s: %w", ar.Agent, ar.Err)
			}
		}
		printSetupHelper(target, helperOpts, os.Stdout, os.Stdin)
		return nil
	case "claude-code", "opencode", "openclaw":
		id := integration.AgentID(target)
		if uninstall {
			if err := setup.UninstallOne(ctx, id, opts); err != nil {
				return err
			}
			if id == integration.AgentClaudeCode {
				cleanupShellResidue(homeDir)
			}
			fmt.Printf("%s integration removed.\n", id)
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
		printSetupHelper(target, helperOpts, os.Stdout, os.Stdin)
		return nil
	default:
		return fmt.Errorf("unknown agent %q (expected: all | claude-code | openclaw | opencode)", target)
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
		fmt.Printf("removed waired claude alias from %d rc file(s)\n", changed)
	}
	if changed, err := setup.SweepLegacyManagedBlocks(homeDir); err == nil && len(changed) > 0 {
		fmt.Printf("removed legacy waired sentinel block from %d rc file(s)\n", len(changed))
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
	fmt.Printf("Plan: %s coding-agent integration (%s)\n", verb, target)
	fmt.Printf("  $HOME              = %s\n", home)
	fmt.Printf("  state directory    = %s\n", state)
	fmt.Printf("  gateway base URL   = %s\n", baseURL)
	if force && !uninstall {
		fmt.Println("  force              = apply even when the agent is not detected")
	}
	if !uninstall {
		switch target {
		case "all", "":
			fmt.Println("  agents             = claude-code (skills only), opencode (plugin), openclaw (plugin + openclaw.json)")
		default:
			fmt.Printf("  agents             = %s\n", target)
		}
	} else {
		fmt.Println("  removes agent-managed skill / command files,")
		fmt.Println("  the OpenCode plugin (~/.config/opencode/plugin/waired.js),")
		fmt.Println("  the OpenClaw plugin (~/.openclaw/plugins/waired/) and its")
		fmt.Println("  openclaw.json keys, the v2 `waired-claude alias` block from rc")
		fmt.Println("  files, and any residual v1 `# >>> waired managed` block (best-effort).")
	}
	fmt.Println("\nRun without --dry-run to apply.")
	return nil
}

func printIntegrationSummary(res *setup.IntegrationResult) {
	if res == nil {
		return
	}
	for _, ar := range res.Agents {
		switch {
		case ar.Skipped:
			fmt.Printf("%-12s skipped (not detected — run `waired link --force` to set up anyway)\n", ar.Agent+":")
		case ar.Err != nil:
			fmt.Printf("%-12s FAILED: %v\n", ar.Agent+":", ar.Err)
		case ar.Applied:
			fmt.Printf("%-12s configured\n", ar.Agent+":")
		}
	}
}
