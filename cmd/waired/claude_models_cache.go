package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
	"github.com/waired-ai/waired-agent/internal/integration/claudemanaged"
)

// Claude Code /model picker cache, per user (#332 / #407).
//
// Under subscription OAuth Claude Code never fetches GET /v1/models — the
// fetch is credential-gated and waired holds no credential by design (#488) —
// but the picker still reads whatever cache is on disk. So the agent writes it.
//
// The file is USER-owned (~/.claude/cache/gateway-models.json), while the
// routing it complements is machine-wide and written as root. That split is
// why this rides the same sudo hop as the /waired-route skill and the
// statusline: applyClaudeRoute is elevated, and a root-written file in the
// user's home is a support ticket. runLinkAllAsUser drops to the invoking user
// and re-enters this hidden subcommand there, so ownership comes out right.
//
// The base URL travels as an argument rather than being re-derived in the
// child. The reader compares baseUrl against the live ANTHROPIC_BASE_URL by
// exact string, so the value written has to be the very one the parent just
// put in managed settings — re-deriving it in a different process with a
// different state-dir view is precisely how a trailing slash or a stale port
// gets in and silently disables the whole cache.

func newClaudeModelsCacheCmd() *cobra.Command {
	var baseURL string
	cmd := &cobra.Command{
		Use:    "_models-cache <write|remove>",
		Short:  "Internal: write or remove this user's Claude Code /model picker cache.",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("waired claude _models-cache: resolve home: %w", err)
			}
			configDir := claudecode.ClaudeConfigDir()
			switch args[0] {
			case "write":
				path, err := writeModelsCache(configDir, home, baseURL)
				if err != nil {
					return err
				}
				fmt.Printf("Wrote Claude Code /model picker entries: %s\n", path)
				return nil
			case "remove":
				return claudecode.RemoveGatewayCache(configDir, home)
			default:
				return fmt.Errorf("waired claude _models-cache: unknown action %q (write|remove)", args[0])
			}
		},
	}
	cmd.Flags().StringVar(&baseURL, "base-url", "", "the exact ANTHROPIC_BASE_URL managed settings carry")
	return cmd
}

// modelsCacheGuard decides whether the picker cache may be written, from the
// managed-settings facts alone — a pure function so every refusal is table
// testable without a root-owned file (CLAUDE.md §Test discipline).
//
// The check is not redundant with applyClaudeRoute having just written managed
// settings: the subcommand is reachable on its own, and the failure it prevents
// is silent. A cache naming a gateway this machine is not routed at leaves the
// picker offering entries that go nowhere, and the only way a user clears one is
// by deleting a file nothing told them about.
//
// The base URLs are compared by exact string on purpose. That is how the READER
// compares them, so anything short of byte equality here would let us write a
// file Claude Code then ignores — a success message and no picker entries.
func modelsCacheGuard(present bool, current, want, managedPath string) error {
	if want == "" {
		return fmt.Errorf("waired claude _models-cache write: --base-url is required")
	}
	if !present {
		return fmt.Errorf("waired claude _models-cache write: %s is not present — run `waired claude enable` first", managedPath)
	}
	if current != want {
		return fmt.Errorf("waired claude _models-cache write: managed settings carry ANTHROPIC_BASE_URL=%q, not %q — refusing to advertise a gateway this machine is not routed at",
			current, want)
	}
	return nil
}

// writeModelsCache writes the picker cache after re-checking, in the user
// context, that this machine really is routed at the given base URL.
func writeModelsCache(configDir, home, baseURL string) (string, error) {
	_, present, current := claudemanaged.View()
	if err := modelsCacheGuard(present, current, baseURL, claudemanaged.Path()); err != nil {
		return "", err
	}
	return claudecode.WriteGatewayCache(configDir, home, baseURL, claudecode.DirectiveCacheModels(), time.Now)
}

// installModelsCacheForInvoker / removeModelsCacheForInvoker (un)write the
// picker cache for the human user, hopping to them under sudo — the same shape
// as manageRouteSkillForInvoker.
//
// Best-effort, like the other two per-user extras: the managed-settings write
// is what routing depends on, and a missing picker entry is the state every
// OAuth host has been in since #52 shipped. It must not turn a good enable into
// a failed one.
func installModelsCacheForInvoker(baseURL string, directives bool) {
	if !directives {
		// Opt-out: the ids are not advertised, so offering them in the picker
		// would be a lie. Clear instead of write.
		removeModelsCacheForInvoker()
		return
	}
	if hoppedModelsCache([]string{"claude", "_models-cache", "write", "--base-url", baseURL}, "write") {
		return
	}
	home, configDir, ok := invokerCacheTarget("write")
	if !ok {
		return
	}
	path, err := writeModelsCache(configDir, home, baseURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
		return
	}
	fmt.Printf("Claude Code /model picker entries: %s\n", path)
}

func removeModelsCacheForInvoker() {
	if hoppedModelsCache([]string{"claude", "_models-cache", "remove"}, "remove") {
		return
	}
	home, configDir, ok := invokerCacheTarget("remove")
	if !ok {
		return
	}
	if err := claudecode.RemoveGatewayCache(configDir, home); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}
}

// hoppedModelsCache runs childArgs as the sudo-invoking user and reports
// whether it handled the work. false means there is no hop target — an
// unelevated run, a real root login, or Windows, where UAC keeps the same user
// and this process's home is already the right one.
func hoppedModelsCache(childArgs []string, action string) bool {
	sudoUser, isSudo := invokingSudoUser()
	if !isSudo {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := runLinkAllAsUser(ctx, sudoUser, childArgs, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %s Claude Code /model picker entries for user %q failed: %v\n",
			action, sudoUser, err)
	}
	return true
}

// invokerCacheTarget resolves this process's own home and Claude config dir for
// the no-hop case, warning instead of failing when the home is unknown.
func invokerCacheTarget(action string) (home, configDir string, ok bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: cannot resolve home to %s Claude Code /model picker entries: %v\n", action, err)
		return "", "", false
	}
	return home, claudecode.ClaudeConfigDir(), true
}
