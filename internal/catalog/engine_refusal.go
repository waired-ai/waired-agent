package catalog

import (
	"fmt"
	"slices"
	"strings"
)

// VariantEngines lists the engines m has a build for, in manifest order.
func VariantEngines(m Manifest) []string {
	var out []string
	for _, v := range m.Variants {
		for _, r := range v.RuntimeSupport {
			if !slices.Contains(out, r) {
				out = append(out, r)
			}
		}
	}
	return out
}

// NoBuildForEngine is the sentence for a model that has no build for
// engine, the engine this computer runs, and false when it has one. It
// names the model by the name a person gave or knows it by, the engine it
// runs on, and what to do; a custom model has exactly one engine, the one
// it was imported for. The pull and the model switch say the same thing
// (waired-ai/waired#1480): the sentence the pull used to end with, "this
// device cannot fetch this model's files", read as a download fault.
func NoBuildForEngine(m Manifest, engine string) (string, bool) {
	engines := VariantEngines(m)
	if slices.Contains(engines, engine) {
		return "", false
	}
	name := m.ModelID
	if m.DisplayName != "" && m.DisplayName != m.ModelID {
		name = m.DisplayName + " (" + m.ModelID + ")"
	}
	on := strings.Join(engines, ", ")
	if IsCustomModelID(m.ModelID) {
		return fmt.Sprintf(
			"%s was imported for %s, and this computer runs %s, so it can't run here — choose a model for %s, or import this one again for %s in the Waired console's Custom models tab",
			name, on, engine, engine, engine), true
	}
	return fmt.Sprintf(
		"%s has no build for %s, the engine this computer runs; it runs on %s — choose a model that has a build for %s",
		name, engine, on, engine), true
}
