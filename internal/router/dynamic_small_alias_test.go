package router

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// smallAliasManifest builds one ollama manifest that owns no waired/*
// alias — the state the catalog is in after #521 retired the static
// namespace, and the state that makes waired/small reach the dynamic
// path at all.
func smallAliasManifest(id string, ramGB int, manualOnly string) catalog.Manifest {
	return catalog.Manifest{
		ModelID:       id,
		ContextLength: 8192,
		Capabilities:  []string{"chat", "json_mode"},
		Runtime:       catalog.RuntimePolicy{Preferred: catalog.RuntimeOllama},
		ManualOnly:    manualOnly,
		Variants: []catalog.Variant{{
			VariantID:      "q4-gguf",
			Format:         catalog.FormatOllamaTag,
			RuntimeSupport: []string{catalog.RuntimeOllama},
			MinRAMGB:       ramGB,
			Source:         catalog.VariantSource{Type: "ollama", Tag: id + ":q4_K_M"},
		}},
	}
}

func smallAliasState(sizes map[string]int64) catalog.State {
	st := catalog.State{
		Version:   catalog.StateVersion,
		Models:    map[string]catalog.ModelState{},
		Endpoints: map[string]catalog.EndpointState{},
	}
	for id, size := range sizes {
		st.Models[id] = catalog.ModelState{
			VariantID: "q4-gguf",
			OllamaTag: id + ":q4_K_M",
			State:     catalog.ModelStateReady,
			SizeBytes: size,
			PulledAt:  time.Now(),
		}
	}
	return st
}

// TestSmallAlias_ResolvesToTheSmallestLocalModel is the product contract
// for #521's half of the alias retirement: waired/small stopped being a
// static pointer at a 3B model and became a role. openclaw writes the
// name into every user's config, so what it resolves to has to keep
// being a real answer on every host.
func TestSmallAlias_ResolvesToTheSmallestLocalModel(t *testing.T) {
	manifests := []catalog.Manifest{
		smallAliasManifest("big-model", 12, ""),
		smallAliasManifest("small-model", 4, ""),
	}

	cases := []struct {
		name  string
		state catalog.State
		deflt string
		want  string
		// wantNotReady expects the name to RESOLVE and the failure to
		// come from the serving layer instead: a host with nothing on
		// disk owes a 503 naming the model it is fetching, not a 404
		// saying the name means nothing. waired/default answers the
		// same way on the same host.
		wantNotReady bool
	}{
		{
			// The case the role exists for: two models on disk, the
			// smaller download wins regardless of which one is the
			// coding default.
			name:  "picks the smaller download",
			state: smallAliasState(map[string]int64{"big-model": 9_000_000_000, "small-model": 2_000_000_000}),
			deflt: "big-model",
			want:  "small-model",
		},
		{
			// The common case: one model on the host. "The smallest
			// thing here" and "the coding default" are the same
			// answer, and the alias must not 404 for want of a
			// choice.
			name:  "one model is still an answer",
			state: smallAliasState(map[string]int64{"big-model": 9_000_000_000}),
			deflt: "big-model",
			want:  "big-model",
		},
		{
			// Fail-soft. A host that has pulled nothing yet still
			// has a config full of waired/small; answering with the
			// coding default is the difference between a slow first
			// request and a broken integration.
			name:         "nothing local falls back to the coding default",
			state:        smallAliasState(nil),
			deflt:        "big-model",
			want:         "big-model",
			wantNotReady: true,
		},
		{
			// A model the product would never choose is not the
			// product's answer to "give me something small" either
			// (#521) — even though a person may still name it.
			name: "a manual_only model is not the small one",
			state: smallAliasState(map[string]int64{
				"big-model": 9_000_000_000, "withheld-small": 1_000_000_000}),
			deflt: "big-model",
			want:  "big-model",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ms := append([]catalog.Manifest{}, manifests...)
			if _, ok := tc.state.Models["withheld-small"]; ok {
				ms = append(ms, smallAliasManifest("withheld-small", 2, "test fixture: never chosen automatically"))
			}
			s := NewSelector(Inputs{
				Manifests:      ms,
				LocalState:     tc.state,
				Hardware:       goodHardware(),
				Runtimes:       registryWithOllama(),
				DefaultModelID: tc.deflt,
			})
			out, err := s.Select(context.Background(), Request{Model: "waired/small"})
			if errors.Is(err, ErrModelNotFound) {
				t.Fatalf("Select(waired/small) = ErrModelNotFound — the name is in " +
					"every openclaw config and must never stop meaning something")
			}
			if tc.wantNotReady {
				if !errors.Is(err, ErrModelNotReady) {
					t.Fatalf("Select(waired/small) = %v, want ErrModelNotReady", err)
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Errorf("error %q does not name %q, so the alias did not resolve to it", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatalf("Select(waired/small): %v", err)
			}
			if out.ModelID != tc.want {
				t.Errorf("waired/small resolved to %q, want %q", out.ModelID, tc.want)
			}
		})
	}
}

// TestSmallAlias_IsNotStaticallyOwned pins the mechanism rather than the
// answer: if a manifest ever claims waired/small again, LookupByAlias
// would answer before the dynamic branch runs and the role would
// silently revert to a pointer.
func TestSmallAlias_IsNotStaticallyOwned(t *testing.T) {
	ms, err := catalog.BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatalf("BundledManifestsIncludingInternal: %v", err)
	}
	for _, name := range DynamicAliases() {
		if m, ok := catalog.LookupByAlias(name, ms); ok {
			t.Errorf("%q is statically owned by %q; the router resolves it at request "+
				"time and a static claim shadows that (#632, #521)", name, m.ModelID)
		}
	}
}

// TestDynamicAliases_AreAdvertised: a name no manifest owns is invisible
// to a model list built from the catalog, so the gateway has to add
// these explicitly. Adding waired/small to the dynamic set without
// adding it to what gets advertised would leave openclaw's picker
// showing two of the three references it allowlists.
func TestDynamicAliases_AreAdvertised(t *testing.T) {
	got := DynamicAliases()
	for _, want := range []string{"waired/default", "waired/coding", "waired/small"} {
		found := false
		for _, g := range got {
			if g == want {
				found = true
			}
		}
		if !found {
			t.Errorf("DynamicAliases() = %v, missing %q", got, want)
		}
	}
}
