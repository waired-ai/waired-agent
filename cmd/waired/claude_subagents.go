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

// `waired claude subagents follow|waired` — where Claude Code's subagents run
// (waired-agent#1186).
//
// One switch with two values, because there are only two answers a person can
// act on. Either subagents follow whatever Claude Code resolves for them (the
// main conversation's model, or one their own definition pins), or they all
// run on Waired. "Main on Waired, subagents on Anthropic" is not a third
// value: someone who wants that pins a real Anthropic model in the agent
// definition, which is the documented way and already works
// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md
// decision 6).
//
// It writes the invoking user's own settings.json, so it needs no elevation —
// and under sudo it hops to that user the way the status line and the /model
// rows do, or the file lands in root's home.

func newClaudeSubagentsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "subagents [follow|waired]",
		Short: "Choose where Claude Code's subagents run",
		Long: "Choose where Claude Code's subagents run.\n\n" +
			"  follow   each subagent runs where its own model says: the main\n" +
			"           conversation's model, or one its definition pins\n" +
			"  waired   every subagent runs on your computers\n\n" +
			"With no argument, reports the current setting. To run subagents on\n" +
			"Anthropic while the main conversation is on Waired, pin a real\n" +
			"Anthropic model in the agent's own definition.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("waired claude subagents: resolve home: %w", err)
			}
			if len(args) == 0 {
				printClaudeSubagentStatus(claudecode.SettingsPath(home))
				return nil
			}
			var want claudecode.SubagentPlacement
			switch args[0] {
			case "follow":
				want = claudecode.SubagentFollow
			case "waired":
				want = claudecode.SubagentWaired
			default:
				return fmt.Errorf("waired claude subagents: unknown value %q (follow|waired)", args[0])
			}
			return applySubagentPlacement(want)
		},
	}
	cmd.AddCommand(&cobra.Command{
		Use:    "_set <follow|waired>",
		Short:  "Internal: write the subagent placement for this user",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("waired claude subagents _set: resolve home: %w", err)
			}
			want := claudecode.SubagentFollow
			if args[0] == "waired" {
				want = claudecode.SubagentWaired
			}
			return writeSubagentPlacement(home, want)
		},
	})
	return cmd
}

// applySubagentPlacement writes the switch for the human user, hopping to them
// under sudo.
func applySubagentPlacement(want claudecode.SubagentPlacement) error {
	value := "follow"
	if want == claudecode.SubagentWaired {
		value = "waired"
	}
	if sudoUser, isSudo := invokingSudoUser(); isSudo {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		child := []string{"claude", "subagents", "_set", value}
		// ascii: a child process's streams. It is `waired` again, run as the
		// invoking user, and it folds its own output.
		return runLinkAllAsUser(ctx, sudoUser, child, os.Stdout, os.Stderr)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("waired claude subagents: resolve home: %w", err)
	}
	return writeSubagentPlacement(home, want)
}

func writeSubagentPlacement(home string, want claudecode.SubagentPlacement) error {
	path := claudecode.SettingsPath(home)
	// The any-node row is what "on Waired" means: Waired picks the computer,
	// and says so when none can answer. Naming one computer here would make
	// every subagent wait on that machine.
	changed, err := claudecode.SetSubagentPlacement(path, want, claudecode.DirectiveModelAny)
	if err != nil {
		return err
	}
	if !changed {
		fmt.Fprint(stdout, claudeSubagentStatusLine(want, ""))
		return nil
	}
	if want == claudecode.SubagentWaired {
		fmt.Fprintf(stdout, "Subagents run on Waired (%s).\n", path)
	} else {
		fmt.Fprintf(stdout, "Subagents follow their own model (%s).\n", path)
	}
	fmt.Fprint(stdout, "Restart any running `claude` session to pick it up.\n")
	return nil
}

// printClaudeSubagentStatus is the row `waired claude status` and the bare
// command both print.
func printClaudeSubagentStatus(path string) {
	got, value := claudecode.DetectSubagentPlacement(path)
	fmt.Fprint(stdout, claudeSubagentStatusLine(got, value))
}

// claudeSubagentStatusLine renders the row. Pure so every state is table
// testable without a filesystem.
func claudeSubagentStatusLine(p claudecode.SubagentPlacement, value string) string {
	switch p {
	case claudecode.SubagentWaired:
		return "subagents:          on Waired\n"
	case claudecode.SubagentForeign:
		return fmt.Sprintf("subagents:          left alone. CLAUDE_CODE_SUBAGENT_MODEL=%s isn't Waired's\n", value)
	case claudecode.SubagentUnreadable:
		return "subagents:          unreadable. These settings aren't JSON Waired can read\n"
	default:
		return "subagents:          follow their own model\n"
	}
}

// printOrgManagedRefusal explains why waired did not write this machine's
// Claude Code settings, and what is still available to the person in front of
// it. The alternatives matter: refusing without them reads as "waired does
// not work here", when in fact everything except the machine-wide redirect
// still does.
func printOrgManagedRefusal(org *claudemanaged.ErrOrgManaged) {
	fmt.Fprintf(stderr, "Claude Code on this computer is managed by your organisation, so Waired did not\n")
	fmt.Fprintf(stderr, "change its settings. Found in %s:\n", org.Path)
	for _, s := range org.Signals {
		fmt.Fprintf(stderr, "  %s\n", s)
	}
	fmt.Fprintf(stderr, "\nPointing ANTHROPIC_BASE_URL at Waired would also switch off the settings your\n")
	fmt.Fprintf(stderr, "organisation delivers to every session on this computer, which is not Waired's\n")
	fmt.Fprintf(stderr, "call to make. Ask whoever manages this computer, or use Waired from another\n")
	fmt.Fprintf(stderr, "coding tool. `waired link` sets those up per user and touches nothing\n")
	fmt.Fprintf(stderr, "machine-wide.\n")
}

// removeSubagentPlacementForInvoker undoes the switch on `waired claude
// disable`, best-effort like the other per-user extras. A value the operator
// set themselves is left alone by SetSubagentPlacement's ownership check.
func removeSubagentPlacementForInvoker() {
	if err := applySubagentPlacement(claudecode.SubagentFollow); err != nil {
		fmt.Fprintf(stderr, "Warning: %v\n", err)
	}
}

// printClaudeSubagentStatusForInvoker reads the INVOKING user's settings, not
// this process's: `waired claude status` is usually run under sudo, and
// looking in /root would report every host as unset.
func printClaudeSubagentStatusForInvoker() {
	home, _, _ := invokerHome()
	if home == "" {
		return
	}
	printClaudeSubagentStatus(claudecode.SettingsPath(home))
}
