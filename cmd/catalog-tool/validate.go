package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

func init() {
	subcommands["validate"] = subcommand{run: runValidate, summary: "validate manifest(s) + catalog-wide tier uniqueness"}
}

func runValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	file := fs.String("file", "", "path to a manifest JSON to validate")
	all := fs.Bool("all", false, "validate the bundled catalog (Validate each + catalog-wide tier uniqueness)")
	againstBundled := fs.Bool("against-bundled", true, "when --file is given, also check the file's tiers don't collide with the bundled catalog")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*all && *file == "" {
		return fmt.Errorf("validate: one of --file or --all is required")
	}

	bundled, err := catalog.BundledManifests()
	if err != nil {
		return fmt.Errorf("validate: load bundled catalog: %w", err)
	}

	if *all {
		for _, m := range bundled {
			if err := m.Validate(); err != nil {
				return fmt.Errorf("validate: %w", err)
			}
		}
		if err := catalog.CheckTierUniqueness(bundled); err != nil {
			return err
		}
		fmt.Printf("ok: %d bundled manifests valid, quality_tier unique catalog-wide\n", len(bundled))
		return nil
	}

	data, err := os.ReadFile(*file)
	if err != nil {
		return fmt.Errorf("validate: read --file: %w", err)
	}
	var m catalog.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("validate: parse --file: %w", err)
	}
	if err := m.Validate(); err != nil {
		return fmt.Errorf("validate: %w", err)
	}
	if *againstBundled {
		// A new/updated manifest must not duplicate a tier already in use.
		// Drop any bundled manifest with the same model_id first (this is an
		// in-place update of that model), then check the merged set.
		merged := make([]catalog.Manifest, 0, len(bundled)+1)
		for _, b := range bundled {
			if b.ModelID != m.ModelID {
				merged = append(merged, b)
			}
		}
		merged = append(merged, m)
		if err := catalog.CheckTierUniqueness(merged); err != nil {
			return fmt.Errorf("validate: %s would collide with the bundled catalog: %w", m.ModelID, err)
		}
		fmt.Printf("ok: %s valid; quality_tier unique against bundled catalog\n", m.ModelID)
		return nil
	}
	fmt.Printf("ok: %s valid\n", m.ModelID)
	return nil
}
