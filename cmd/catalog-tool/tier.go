package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

func init() {
	subcommands["tier"] = subcommand{run: runTier, summary: "derive catalog-wide quality_tier from benchmarks (#133)"}
}

// stringSlice is a repeatable string flag (--manifest a.json --manifest b.json).
type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func runTier(args []string) error {
	fs := flag.NewFlagSet("tier", flag.ContinueOnError)
	var extra stringSlice
	fs.Var(&extra, "manifest", "additional manifest JSON to merge into the bundled catalog before tiering (repeatable; a same-model_id file replaces the bundled one)")
	rerank := fs.Bool("rerank", false, "full re-rank of every variant (default: freeze — keep existing tiers, only slot tier-0 variants)")
	format := fs.String("format", "text", "output format: text | json")
	if err := fs.Parse(args); err != nil {
		return err
	}

	manifests, err := catalog.BundledManifests()
	if err != nil {
		return fmt.Errorf("tier: load bundled catalog: %w", err)
	}
	for _, path := range extra {
		m, err := readManifest(path)
		if err != nil {
			return err
		}
		manifests = mergeManifest(manifests, m)
	}
	bench, err := catalog.Benchmarks()
	if err != nil {
		return fmt.Errorf("tier: load benchmarks: %w", err)
	}

	res, err := catalog.AssignTiers(manifests, bench, *rerank)
	if err != nil {
		return err
	}

	switch *format {
	case "json":
		return printJSON(res)
	case "text":
		printTierReport(res)
		return nil
	default:
		return fmt.Errorf("tier: unknown --format %q (want text|json)", *format)
	}
}

func readManifest(path string) (catalog.Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return catalog.Manifest{}, fmt.Errorf("tier: read %s: %w", path, err)
	}
	var m catalog.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return catalog.Manifest{}, fmt.Errorf("tier: parse %s: %w", path, err)
	}
	if m.ModelID == "" {
		return catalog.Manifest{}, fmt.Errorf("tier: %s: model_id required", path)
	}
	return m, nil
}

// mergeManifest replaces a bundled manifest with the same model_id, else appends.
func mergeManifest(ms []catalog.Manifest, m catalog.Manifest) []catalog.Manifest {
	for i := range ms {
		if ms[i].ModelID == m.ModelID {
			ms[i] = m
			return ms
		}
	}
	return append(ms, m)
}

// printTierReport writes the human-readable diff a draft PR embeds verbatim.
func printTierReport(res catalog.TierResult) {
	mode := "freeze"
	if res.Reranked {
		mode = "rerank"
	}
	changed := res.Changes()
	fmt.Printf("quality_tier assignment (%s): %d variants, %d changed\n\n", mode, len(res.Assignments), len(changed))
	fmt.Printf("%-5s %-5s %-6s %-7s %-46s %s\n", "TIER", "OLD", "Δ", "SWE", "MODEL/VARIANT", "CONF")
	for _, a := range res.Assignments {
		delta := ""
		if a.Changed() {
			delta = "*"
		}
		swe := ""
		if a.SWEBench > 0 {
			swe = fmt.Sprintf("%.1f", a.SWEBench)
		}
		conf := a.Confidence
		if a.Overridden {
			conf = "override"
		}
		fmt.Printf("%-5d %-5d %-6s %-7s %-46s %s\n", a.NewTier, a.OldTier, delta, swe, a.Key(), conf)
	}
}
