package router

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/internal/version"
)

// TestBundledEngineFloorsNeverExceedThePin is the release-engineering
// half of waired-agent#836: a shipped engine-version floor must be a
// version waired's own engine already IS.
//
// v0.0.3-rc2 is what asked for it. The catalog entry and the pin move
// landed together — that part was right — but the converge that brings
// an installed engine onto the pin shipped one tag later (#826), so for
// the length of that window the model was in the catalog and unusable
// on every host. Converge closed the window; this closes the case
// converge cannot reach, because converge follows the PIN. A floor
// ABOVE the pin describes an engine waired never installs anywhere, so
// no amount of converging produces it and the family is dark on the
// whole fleet from the day it ships.
//
// The check runs against the ollama floors only. vLLM's version comes
// from a venv the installer records rather than from this constant, and
// its variants carry no min_engine_version today; when one does, this
// test should grow the matching pin rather than silently skip it.
func TestBundledEngineFloorsNeverExceedThePin(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	pin := runtime.OllamaPinnedVersion
	seen := 0
	for _, m := range manifests {
		for _, v := range m.Variants {
			if v.MinEngineVersion == "" || !engineSupports(v, catalog.RuntimeOllama) {
				continue
			}
			seen++
			if !version.AtLeast(pin, v.MinEngineVersion) {
				t.Errorf("%s/%s: min_engine_version %s is above the bundled pin %s — "+
					"raise OllamaPinnedVersion in the same change or lower the floor. "+
					"The engine converge (#826) follows the pin, so a floor above it "+
					"is an engine no host ever runs and the family is dark fleet-wide",
					m.ModelID, v.VariantID, v.MinEngineVersion, pin)
			}
		}
	}
	if seen == 0 {
		t.Fatalf("no floored ollama variant in the bundled catalog — this test now "+
			"asserts nothing; either the field went away or the guard did (pin %s)", pin)
	}
}
