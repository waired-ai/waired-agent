// Command catalog-tool is the deterministic helper behind the model-catalog
// refresh pipeline (issue #413). It turns published model facts into the
// numeric fields a bundled manifest needs — so manifest authoring (and the
// catalog-radar automation) never does the VRAM/KV/FLOPs arithmetic by hand
// and every number is re-derivable by a reviewer.
//
// Subcommands:
//
//	compute   config.json + quant + context -> footprint fields (JSON)
//	tier      catalog-wide quality_tier from benchmarks (#133)
//	draft     assemble a full manifest JSON from inputs + computed fields
//	validate  Manifest.Validate() + catalog-wide quality_tier uniqueness
//	radar     poll HuggingFace for new candidate models (discovery)
//
// All formulas come from docs/reports/20260516-coding-model-scoring.md.
package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

type subcommand struct {
	run     func(args []string) error
	summary string
}

// subcommands is populated by each subcommand file's init(). Splitting it this
// way lets milestones add commands (tier in #133, radar in discovery) without
// touching this file.
var subcommands = map[string]subcommand{}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "catalog-tool:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return fmt.Errorf("no subcommand given")
	}
	cmd := args[0]
	if cmd == "-h" || cmd == "--help" || cmd == "help" {
		usage()
		return nil
	}
	sc, ok := subcommands[cmd]
	if !ok {
		usage()
		return fmt.Errorf("unknown subcommand %q", cmd)
	}
	return sc.run(args[1:])
}

func usage() {
	var b strings.Builder
	b.WriteString("catalog-tool — deterministic model-catalog helper (#413)\n\n")
	b.WriteString("usage: catalog-tool <subcommand> [flags]\n\nsubcommands:\n")
	names := make([]string, 0, len(subcommands))
	for n := range subcommands {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(&b, "  %-9s %s\n", n, subcommands[n].summary)
	}
	fmt.Fprint(os.Stderr, b.String())
}
