package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
)

// waired does not set Claude Code's default model (owner ruling, 2026-09-03).
//
// It used to: `waired claude enable` wrote claude-waired-auto into the user's
// ~/.claude/settings.json so an untouched session started on Waired
// (waired-agent#1037, docs/decisions/20260828/0252 §4). That was safe while a
// Waired turn could still be carried to the real Anthropic API when nothing
// here could answer it. It is not safe now: a turn fails closed
// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md),
// so on a computer with no engine and no peer the written default would make
// every turn of every session fail from the moment routing was enabled.
//
// The ruling is unconditional — waired does not change the user's default at
// all — and the file stays theirs: a value an earlier build wrote is left
// where it is, and only `waired claude disable` removes one, because that
// value IS waired's.
//
// What remains here is that removal, which keeps the same USER ownership as
// the picker cache and the statusline: ~/.claude/settings.json is the user's
// file while the routing it completes is machine-wide and written as root, so
// it rides the same sudo hop.

func newClaudeModelDefaultCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "_model-default remove",
		Short:  "Internal: drop the Waired default model waired wrote for this user",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("waired claude _model-default: resolve home: %w", err)
			}
			if args[0] != "remove" {
				return fmt.Errorf("waired claude _model-default: unknown action %q (want remove)", args[0])
			}
			if err := claudecode.RemoveModelSetting(home); err != nil {
				return fmt.Errorf("waired claude _model-default remove: %w", err)
			}
			return nil
		},
	}
}

// removeModelDefaultForInvoker drops the default for the user who invoked the
// elevated command, hopping to them when this process is root via sudo.
func removeModelDefaultForInvoker() {
	if hoppedModelDefault([]string{"claude", "_model-default", "remove"}, "remove") {
		return
	}
	home, ok := invokerHomeFor("remove")
	if !ok {
		return
	}
	if err := claudecode.RemoveModelSetting(home); err != nil {
		fmt.Fprintf(stderr, "Warning: drop Claude Code default model: %v\n", err)
	}
}

// hoppedModelDefault runs childArgs as the sudo-invoking user and reports
// whether it handled the work. false means there is no hop target — an
// unelevated run, a real root login, or Windows, where UAC keeps the same user.
func hoppedModelDefault(childArgs []string, action string) bool {
	sudoUser, isSudo := invokingSudoUser()
	if !isSudo {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// ascii: a child process's streams. It is `waired` again, run as the
	// invoking user, and it folds its own output.
	if err := runLinkAllAsUser(ctx, sudoUser, childArgs, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(stderr, "Warning: %s Claude Code default model for user %q failed: %v\n",
			action, sudoUser, err)
	}
	return true
}

// invokerHomeFor resolves this process's own home for the no-hop case, warning
// instead of failing when it is unknown.
func invokerHomeFor(action string) (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(stderr, "Warning: %s Claude Code default model: resolve home: %v\n", action, err)
		return "", false
	}
	return home, true
}
