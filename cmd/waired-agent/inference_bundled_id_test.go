package main

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
)

// aliasedManifest returns a shipped manifest together with one alias that
// differs from its model_id. Derived from the catalog rather than written
// down, because the alias→id mapping is data other work repoints (#200
// re-points `waired/medium`), and a test that pinned one would fail for a
// reason that has nothing to do with resolution.
func aliasedManifest(t *testing.T) (alias, modelID string) {
	t.Helper()
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	for _, m := range manifests {
		for _, a := range m.ModelAliases {
			if a != "" && a != m.ModelID {
				return a, m.ModelID
			}
		}
	}
	t.Fatal("no bundled manifest carries an alias that differs from its model_id")
	return "", ""
}

func providerWithBundled(t *testing.T, configured string) *agentInferenceProvider {
	t.Helper()
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	return &agentInferenceProvider{
		cfg:       agentconfig.InferenceConfig{BundledModelID: configured},
		manifests: manifests,
	}
}

// TestBundledModelID resolves the operator's configured value — which
// accepts ANY catalog alias — to the canonical catalog id (#380).
func TestBundledModelID(t *testing.T) {
	alias, modelID := aliasedManifest(t)

	cases := []struct {
		name       string
		configured string
		want       string
	}{
		{name: "canonical id is returned unchanged", configured: modelID, want: modelID},
		{name: "alias resolves to the id", configured: alias, want: modelID},
		{name: "unknown id passes through", configured: "not-a-catalog-model", want: "not-a-catalog-model"},
		{name: "unset stays unset", configured: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := providerWithBundled(t, tc.configured).bundledModelID(); got != tc.want {
				t.Fatalf("bundledModelID() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestIsBundledModel is the predicate behind the first-run activation guard.
//
// Its three call sites used to compare the RAW config value against a
// model id the pull path had already resolved. An operator who pinned
// `--inference-bundled-model-id <alias>` got a pull that registered under
// the canonical id, a comparison that was false, and
// activateBundledIfUnset that never ran — silently, with no log line. On a
// fresh install that leaves state.Active nil: EngineReady() false, the
// boot benchmark 400s, /inference/benchmark 425s, Capacity falls back to 1
// and Status() reports awaiting_model, on a host whose weights are on disk.
//
// PRODUCT CONTRACT: an alias pin and the id it names are the same model.
func TestIsBundledModel(t *testing.T) {
	alias, modelID := aliasedManifest(t)

	cases := []struct {
		name       string
		configured string
		incoming   string
		want       bool
	}{
		{name: "id matches id", configured: modelID, incoming: modelID, want: true},
		{name: "alias matches the id it names", configured: alias, incoming: modelID, want: true},
		{name: "a different model does not match", configured: modelID, incoming: "granite4-350m"},
		// Nothing is the bundled model when none is configured — without
		// this guard an empty config would claim every unnamed pull.
		{name: "unset matches nothing", configured: "", incoming: modelID},
		{name: "unset does not match an empty id either", configured: "", incoming: ""},
		{name: "unknown id still matches itself", configured: "not-a-catalog-model", incoming: "not-a-catalog-model", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := providerWithBundled(t, tc.configured).isBundledModel(tc.incoming); got != tc.want {
				t.Fatalf("isBundledModel(%q) with %q configured = %v, want %v",
					tc.incoming, tc.configured, got, tc.want)
			}
		})
	}
}
