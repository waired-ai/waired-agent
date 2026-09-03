package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// This file holds the shims for `waired claude` subcommands that no longer
// exist. Each points at what replaced it rather than silently vanishing.

// newClaudeNodeShimCmd retires `waired claude node` (#645/#665): node
// SELECTION (this device vs a mesh peer) lives in `waired worker`.
func newClaudeNodeShimCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "node",
		Short:  "(removed) use /model to choose a side and `waired worker` to choose a node.",
		Hidden: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return fmt.Errorf("`waired claude node` was removed: pick a model in Claude Code's " +
				"/model to choose whether a turn runs on Waired or on the Anthropic API, and use " +
				"`waired worker` to choose which Waired node serves it")
		},
	}
}

// newClaudeFallbackShimCmd retires `waired claude fallback [on|off]` (#580).
// There is nothing to turn off: a turn runs where its model id says and
// waired never carries it to the other side
// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md).
func newClaudeFallbackShimCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "fallback",
		Short:  "(removed) Waired never sends a turn to Anthropic on its own.",
		Hidden: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return fmt.Errorf("`waired claude fallback` was removed, and so was the fallback: a turn " +
				"picked in /model runs where that model says, and Waired never carries it to the " +
				"Anthropic API on its own — it tells you it could not answer instead")
		},
	}
}

// newClaudeRouteShimCmd retires `waired claude route [auto|waired|anthropic]`
// (#580). It set a machine-wide per-class route, and there is no route left to
// set: the model id a session carries decides where its turns run
// (docs/decisions/20260828/0252-the-model-you-pick-is-where-the-turn-runs.md,
// docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md).
func newClaudeRouteShimCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "route",
		Short:  "(removed) pick where a turn runs in Claude Code's /model.",
		Hidden: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return fmt.Errorf("`waired claude route` was removed: a turn runs where its model says, " +
				"so choose in Claude Code's /model — a Waired entry to run it on your computers, an " +
				"Anthropic model to run it on your Claude subscription. `waired claude status` shows " +
				"what the last turn did")
		},
	}
}
