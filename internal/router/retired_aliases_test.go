package router

import (
	"context"
	"errors"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// TestRetiredAliasesDoNotResolve is the product contract for #521's
// alias half: the waired/* namespace is one name, and the retired ones
// are gone rather than quietly aliased to something else.
//
// The failure this guards against is not a 404 — it is the opposite. A
// retired name kept alive "just in case" is a name nobody ever removes
// from a config, and waired/coding in particular resolved to the same
// model waired/default did, so keeping it would preserve exactly the
// duplicate the retirement removed. openclaw prunes both from a user's
// config on re-link (legacyModelRefs), which is how waired/auto was
// retired in #422/#478.
//
// Ratifying source:
// docs/decisions/20260805/1806-waired-aliases-are-dynamic-or-internal.md
func TestRetiredAliasesDoNotResolve(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	s := NewSelector(Inputs{
		Manifests:      manifests,
		LocalState:     readyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		DefaultModelID: "qwen3.5-9b",
	})

	// Every name #521 retired, static and dynamic alike.
	retired := []string{
		"waired/coding", "waired/small", "waired/medium", "waired/flagship",
		"waired/oss-small", "waired/moe-small", "waired/moe-mid", "waired/moe-large",
		"waired/moe-mit", "waired/moe-coding", "waired/moe-dual-gpu",
		"waired/moe-frontier", "waired/dense-large",
	}
	for _, name := range retired {
		t.Run(name, func(t *testing.T) {
			if m, ok := catalog.LookupByAlias(name, manifests); ok {
				t.Errorf("%q still resolves statically, to %q", name, m.ModelID)
			}
			_, err := s.Select(context.Background(), Request{Model: name})
			if !errors.Is(err, ErrModelNotFound) {
				t.Errorf("Select(%q) = %v, want ErrModelNotFound", name, err)
			}
		})
	}

	// And the one that survives still does. A test that only checked
	// for absences would pass with the whole namespace deleted.
	if len(DynamicCodingAliases) != 1 || DynamicCodingAliases[0] != "waired/default" {
		t.Fatalf("DynamicCodingAliases = %v, want exactly [waired/default]", DynamicCodingAliases)
	}
	if _, err := s.Select(context.Background(), Request{Model: "waired/default"}); errors.Is(err, ErrModelNotFound) {
		t.Error("waired/default stopped resolving; it is the one name a client config is told to use")
	}
}
