package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
	"github.com/waired-ai/waired-agent/internal/integration/claudemanaged"
)

// The Waired rows of Claude Code's /model picker, per user
// (waired-agent#1185, superseding #332 / #407).
//
// They used to be written into Claude Code's own discovery cache,
// ~/.claude/cache/gateway-models.json, because under subscription OAuth
// Claude Code never fetches GET /v1/models — the fetch is credential-gated and
// waired holds no credential by design (#488) — while the picker read whatever
// cache was on disk. They go into the documented `modelPicker` setting now, in
// the same user's ~/.claude/settings.json.
//
// The file is USER-owned while the routing it complements is machine-wide and
// written as root. That split is why this rides the same sudo hop as the
// statusline: applyClaudeRoute is elevated, and a root-written file in the
// user's home is a support ticket. runLinkAllAsUser drops to the invoking user
// and re-enters this hidden subcommand there, so ownership comes out right.
//
// One thing the move loses, deliberately. The cache was read AFTER SessionStart
// hooks ran, so a hook could refresh it for the session about to start.
// Settings are read BEFORE the watch that would notice a hook's write is armed,
// so a row written by the hook first appears in the NEXT session (measured on
// Claude Code 2.1.261, 2026-09-06: a synchronous hook write and writes at 1 s,
// 2 s and 3 s are missed; 6 s and 15 s land — a race, not a contract, so the
// hook does not try to win it). Only the per-peer rows and the presence of the
// public and 1M rows move with the mesh, so the cost is one relaunch after the
// fleet changes, and the docs say so.

func newClaudePickerCmd() *cobra.Command {
	var baseURL string
	var peerEntries int
	var fromManaged bool
	cmd := &cobra.Command{
		Use:    "_picker <write|remove>",
		Short:  "Internal: write or remove this user's Waired rows in Claude Code's /model picker",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("waired claude _picker: resolve home: %w", err)
			}
			switch args[0] {
			case "write":
				if fromManaged {
					// The SessionStart hook has no parent to hand it the
					// values, so it reads them where the enable path put them.
					_, _, baseURL = claudemanaged.View()
				}
				path, changed, err := writePickerRows(home, baseURL, peerEntries)
				if err != nil {
					return err
				}
				if fromManaged {
					// Silence. Claude Code reads a hook's stdout as session
					// context, so a success line here would be pasted into
					// the user's session.
					return nil
				}
				if !changed {
					fmt.Fprintf(stdout, "Claude Code /model rows already current: %s\n", path)
					return nil
				}
				fmt.Fprintf(stdout, "Wrote Claude Code /model rows: %s\n", path)
				return nil
			case "remove":
				if _, err := claudecode.RemoveRetiredCache(
					claudecode.ClaudeConfigDir(), home, baseURLFromManagedSettings()); err != nil {
					fmt.Fprintf(stderr, "Warning: %v\n", err)
				}
				_, err := claudecode.RemovePickerLineup(claudecode.SettingsPath(home))
				return err
			default:
				return fmt.Errorf("waired claude _picker: unknown action %q (write|remove)", args[0])
			}
		},
	}
	cmd.Flags().StringVar(&baseURL, "base-url", "", "the exact ANTHROPIC_BASE_URL managed settings carry")
	cmd.Flags().IntVar(&peerEntries, "peer-entries", 0, "how many per-computer rows to include (0 = none)")
	cmd.Flags().BoolVar(&fromManaged, "from-managed", false,
		"read the base URL from managed settings and stay silent (for the SessionStart hook)")
	return cmd
}

// pickerWriteGuard decides whether the rows may be written, from the
// managed-settings facts alone — a pure function so every refusal is table
// testable without a root-owned file (CLAUDE.md §Test discipline).
//
// The check is not redundant with applyClaudeRoute having just written managed
// settings: the subcommand is reachable on its own, and the failure it prevents
// is silent. Rows naming a gateway this machine is not routed at leave the
// picker offering entries that go nowhere.
//
// The base URLs are compared by exact string. Managed settings are the one
// place that says where Claude Code is pointed, and a value that differs by a
// trailing slash is a different pointing.
func pickerWriteGuard(present bool, current, want, managedPath string) error {
	if want == "" {
		return fmt.Errorf("waired claude _picker write: --base-url is required")
	}
	if !present {
		return fmt.Errorf("waired claude _picker write: %s isn't present. Run `waired claude enable` first", managedPath)
	}
	if current != want {
		return fmt.Errorf("waired claude _picker write: managed settings carry ANTHROPIC_BASE_URL=%q, not %q. Refusing to offer rows this computer isn't routed at",
			current, want)
	}
	return nil
}

// writePickerRows publishes the rows after re-checking, in the user context,
// that this machine really is routed at the given base URL. changed is false
// when the lineup already said exactly this: the SessionStart refresh runs on
// every `claude` launch and the rows are usually the same ones, so rewriting
// the file each time would be churn and would race two launches starting
// together.
func writePickerRows(home, baseURL string, peerEntries int) (path string, changed bool, err error) {
	_, present, current := claudemanaged.View()
	if err := pickerWriteGuard(present, current, baseURL, claudemanaged.Path()); err != nil {
		return "", false, err
	}
	path = claudecode.SettingsPath(home)
	changed, err = claudecode.WritePickerLineup(path, pickerRows(defaultMgmtAddr, peerEntries))
	if err != nil {
		return path, changed, err
	}
	// The upgrade path. Until waired-agent#1185 the rows reached the picker
	// through Claude Code's own discovery cache, which waired wrote and
	// nothing removed. The flag that makes the picker read it is scrubbed by
	// the next root `waired claude enable`, but until that happens the
	// picker shows the stale rows AND these ones — measured on 2.1.261,
	// 2026-09-06, three old names beside the new ones. This half needs no
	// elevation and runs on every launch, so it is the half that closes the
	// window.
	if gone, err := claudecode.RemoveRetiredCache(
		claudecode.ClaudeConfigDir(), home, baseURL); err != nil {
		fmt.Fprintf(stderr, "Warning: %v\n", err)
	} else if gone {
		changed = true
	}
	return path, changed, err
}

// installPickerRowsForInvoker / removePickerRowsForInvoker (un)write the rows
// for the human user, hopping to them under sudo — the same shape as
// installStatuslineForInvoker.
//
// Best-effort, like the other per-user extras: the managed-settings write is
// what routing depends on, and a missing picker row is the state every OAuth
// host was in before #52 shipped. It must not turn a good enable into a failed
// one.
func installPickerRowsForInvoker(baseURL string, directives bool, peerEntries int) {
	if !directives {
		// Opt-out: the ids are not offered, so listing them would be a lie.
		// Clear instead of write.
		removePickerRowsForInvoker()
		return
	}
	// The peer cap travels as an argument for the reason baseURL does: the
	// child runs as a different user, and re-reading agent.json there is how
	// a hop ends up disagreeing with the parent about what it is writing.
	if hoppedPickerWrite([]string{
		"claude", "_picker", "write",
		"--base-url", baseURL,
		"--peer-entries", strconv.Itoa(peerEntries),
	}, "write") {
		return
	}
	home, ok := invokerPickerHome("write")
	if !ok {
		return
	}
	path, _, err := writePickerRows(home, baseURL, peerEntries)
	if err != nil {
		fmt.Fprintf(stderr, "Warning: %v\n", err)
		return
	}
	fmt.Fprintf(stdout, "Claude Code /model rows: %s\n", path)
}

func removePickerRowsForInvoker() {
	if hoppedPickerWrite([]string{"claude", "_picker", "remove"}, "remove") {
		return
	}
	home, ok := invokerPickerHome("remove")
	if !ok {
		return
	}
	if _, err := claudecode.RemovePickerLineup(claudecode.SettingsPath(home)); err != nil {
		fmt.Fprintf(stderr, "Warning: %v\n", err)
	}
}

// hoppedPickerWrite runs childArgs as the sudo-invoking user and reports
// whether it handled the work. false means there is no hop target — an
// unelevated run, a real root login, or Windows, where UAC keeps the same user
// and this process's home is already the right one.
func hoppedPickerWrite(childArgs []string, action string) bool {
	sudoUser, isSudo := invokingSudoUser()
	if !isSudo {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// ascii: a child process's streams. It is `waired` again, run as the
	// invoking user, and it folds its own output.
	if err := runLinkAllAsUser(ctx, sudoUser, childArgs, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(stderr, "Warning: %s Claude Code /model rows for user %q failed: %v\n",
			action, sudoUser, err)
	}
	return true
}

// invokerPickerHome resolves this process's own home for the no-hop case,
// warning instead of failing when it is unknown.
func invokerPickerHome(action string) (home string, ok bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(stderr, "Warning: couldn't find the home directory to %s Claude Code /model rows: %v\n", action, err)
		return "", false
	}
	return home, true
}

// baseURLFromManagedSettings is the live ANTHROPIC_BASE_URL, which is what a
// retired cache has to name to be one of ours. Read here rather than passed
// in: `_picker remove` runs from `waired claude disable`, which has no base
// URL to hand it, and the file is the same one the reader compares against.
func baseURLFromManagedSettings() string {
	_, _, baseURL := claudemanaged.View()
	return baseURL
}
