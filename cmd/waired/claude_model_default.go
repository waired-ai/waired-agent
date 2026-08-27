package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
)

// Claude Code's default model, per user (waired-agent#1037).
//
// A model id now decides where a turn runs, so the id a session starts on
// decides where it runs by default — and Claude Code's own default is a real
// Anthropic model. Without a Waired default recorded, every untouched session
// would go to the real API and this computer's hardware would sit idle.
//
// Like the picker cache and the statusline, the file is USER-owned
// (~/.claude/settings.json) while the routing it completes is machine-wide and
// written as root, so it rides the same sudo hop: applyClaudeRoute is elevated,
// and a root-written file in the user's home is a support ticket.
//
// Best-effort throughout. Routing is what enable exists to do; a missing
// default leaves Claude Code on its own model, which is the state every host
// was in before this shipped.

func newClaudeModelDefaultCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "_model-default <write|remove>",
		Short:  "Internal: record or drop this user's Claude Code default model.",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("waired claude _model-default: resolve home: %w", err)
			}
			switch args[0] {
			case "write":
				res, err := claudecode.EnsureModelSetting(home)
				if err != nil {
					return fmt.Errorf("waired claude _model-default write: %w", err)
				}
				printModelDefaultResult(res)
				return nil
			case "remove":
				if err := claudecode.RemoveModelSetting(home); err != nil {
					return fmt.Errorf("waired claude _model-default remove: %w", err)
				}
				return nil
			default:
				return fmt.Errorf("waired claude _model-default: unknown action %q (want write|remove)", args[0])
			}
		},
	}
}

// printModelDefaultResult reports what the write found. The foreign case is the
// one worth words: the operator has chosen a model, that choice now decides
// where their turns run, and saying nothing would leave them wondering why
// their own hardware is idle.
func printModelDefaultResult(res claudecode.ModelSettingResult) {
	switch {
	case res.Wrote != "":
		fmt.Printf("Claude Code default model: %s (change it any time with /model)\n", res.Wrote)
	case res.Kind == claudecode.ModelSettingForeign && res.Existing != "":
		fmt.Printf("Claude Code is set to %s, so its turns go to the real Anthropic API.\n", res.Existing)
		fmt.Println("  Pick a Waired entry in /model to use your own computers instead; left as it is.")
	case res.Kind == claudecode.ModelSettingForeign:
		fmt.Println("Claude Code already records a default model; left as it is.")
	}
}

// installModelDefaultForInvoker records the default for the user who invoked
// the elevated command, hopping to them when this process is root via sudo.
func installModelDefaultForInvoker() {
	if hoppedModelDefault([]string{"claude", "_model-default", "write"}, "write") {
		return
	}
	home, ok := invokerHomeFor("write")
	if !ok {
		return
	}
	res, err := claudecode.EnsureModelSetting(home)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: record Claude Code default model: %v\n", err)
		return
	}
	printModelDefaultResult(res)
}

func removeModelDefaultForInvoker() {
	if hoppedModelDefault([]string{"claude", "_model-default", "remove"}, "remove") {
		return
	}
	home, ok := invokerHomeFor("remove")
	if !ok {
		return
	}
	if err := claudecode.RemoveModelSetting(home); err != nil {
		fmt.Fprintf(os.Stderr, "warning: drop Claude Code default model: %v\n", err)
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
	if err := runLinkAllAsUser(ctx, sudoUser, childArgs, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %s Claude Code default model for user %q failed: %v\n",
			action, sudoUser, err)
	}
	return true
}

// invokerHomeFor resolves this process's own home for the no-hop case, warning
// instead of failing when it is unknown.
func invokerHomeFor(action string) (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %s Claude Code default model: resolve home: %v\n", action, err)
		return "", false
	}
	return home, true
}
