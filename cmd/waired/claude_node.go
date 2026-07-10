package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newClaudeNodeShimCmd retires `waired claude node` (#645/#665): node choice
// folded into the unified `waired claude route` and node SELECTION (this
// device vs a mesh peer) now lives in `waired worker`. The shim points users
// at the replacements rather than silently vanishing.
func newClaudeNodeShimCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "node",
		Short:  "(removed) folded into `waired claude route` + `waired worker`.",
		Hidden: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return fmt.Errorf("`waired claude node` was removed: use `waired claude route " +
				"[auto|waired|anthropic] --subagents ...` to choose where Claude runs, and " +
				"`waired worker` to choose which Waired node serves it")
		},
	}
}

// newClaudeFallbackShimCmd retires `waired claude fallback [on|off]` (#580):
// the privacy opt-out is now the "waired" route (never contacts Anthropic).
func newClaudeFallbackShimCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "fallback",
		Short:  "(removed) use `waired claude route waired` for the never-Anthropic option.",
		Hidden: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return fmt.Errorf("`waired claude fallback` was removed: to keep Claude strictly on " +
				"Waired (never contact Anthropic) use `waired claude route waired`; the default " +
				"`auto` falls back to Anthropic on failure")
		},
	}
}
