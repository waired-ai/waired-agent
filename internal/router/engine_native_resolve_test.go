package router

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// The mesh puts an ENGINE-NATIVE identifier on the wire, not a catalog
// alias: buildMeshCandidates matches a peer's advertised
// InferenceState.Models against Source.Tag / Source.RepoID,
// makeMeshCandidate carries the matched name as Selection.EngineModel,
// and the gateway rewrites the proxied body's `model` field to it. The
// SERVING agent therefore resolves an engine-native name — which
// LookupByAlias (model_id / model_aliases only) cannot do. These tests
// pin the fallback that closes that gap (#107).

func TestResolveModel_EngineNativeOllamaTag(t *testing.T) {
	sel := NewSelector(Inputs{
		Manifests:  []catalog.Manifest{qwen()},
		LocalState: readyState(),
		Hardware:   goodHardware(),
		Runtimes:   registryWithOllama(),
	})

	var reasons []string
	m, ok := sel.resolveModel("qwen3:8b-q4_K_M", &reasons)
	if !ok {
		t.Fatal("engine-native ollama tag did not resolve; a peer hop would 404")
	}
	if m.ModelID != "qwen3-8b-instruct" {
		t.Fatalf("resolved to %q, want qwen3-8b-instruct", m.ModelID)
	}
	if len(reasons) == 0 || !strings.Contains(reasons[0], "engine-native") {
		t.Fatalf("reason trace does not record the engine-native path: %v", reasons)
	}
}

func TestResolveModel_AliasWinsOverEngineNative(t *testing.T) {
	// A manifest whose ModelID collides with another manifest's engine
	// tag must still resolve by alias first — the fallback may never
	// change an existing resolution.
	decoy := catalog.Manifest{
		ModelID:       "qwen3:8b-q4_K_M",
		ContextLength: 4096,
		Runtime:       catalog.RuntimePolicy{Preferred: catalog.RuntimeOllama},
		Variants: []catalog.Variant{{
			VariantID:      "decoy",
			Format:         catalog.FormatOllamaTag,
			RuntimeSupport: []string{catalog.RuntimeOllama},
			Source:         catalog.VariantSource{Type: "ollama", Tag: "decoy:latest"},
		}},
	}
	sel := NewSelector(Inputs{Manifests: []catalog.Manifest{qwen(), decoy}})

	var reasons []string
	m, ok := sel.resolveModel("qwen3:8b-q4_K_M", &reasons)
	if !ok {
		t.Fatal("resolveModel missed")
	}
	if m.ModelID != "qwen3:8b-q4_K_M" {
		t.Fatalf("alias/model_id lookup lost priority: resolved to %q", m.ModelID)
	}
	if len(reasons) == 0 || strings.Contains(reasons[0], "engine-native") {
		t.Fatalf("took the engine-native path when an alias matched: %v", reasons)
	}
}

func TestResolveModel_UnknownStillMisses(t *testing.T) {
	sel := NewSelector(Inputs{Manifests: []catalog.Manifest{qwen()}})

	var reasons []string
	if _, ok := sel.resolveModel("no-such-model:latest", &reasons); ok {
		t.Fatal("unknown model resolved; the fallback must not be a wildcard")
	}
	if _, ok := sel.resolveModel("", &reasons); ok {
		t.Fatal("empty model resolved")
	}
}

func TestLookupByEngineModel_RespectsRuntimeSupport(t *testing.T) {
	// Source.Tag addresses a variant only when it supports ollama, and
	// Source.RepoID only when it supports vLLM — the same rule
	// variantWantSets applies when the consumer matches a peer.
	m := catalog.Manifest{
		ModelID: "mixed",
		Variants: []catalog.Variant{{
			VariantID:      "gguf",
			RuntimeSupport: []string{catalog.RuntimeOllama},
			Source:         catalog.VariantSource{Tag: "mixed:q4", RepoID: "org/Mixed-GGUF"},
		}, {
			VariantID:      "awq",
			RuntimeSupport: []string{catalog.RuntimeVLLM},
			Source:         catalog.VariantSource{Tag: "mixed:awq", RepoID: "org/Mixed-AWQ"},
		}},
	}
	manifests := []catalog.Manifest{m}

	for _, name := range []string{"mixed:q4", "org/Mixed-AWQ"} {
		if _, ok := lookupByEngineModel(name, manifests); !ok {
			t.Errorf("%q did not resolve", name)
		}
	}
	// The ollama-only variant's RepoID and the vLLM-only variant's Tag
	// are not addressable — no peer ever advertises them.
	for _, name := range []string{"org/Mixed-GGUF", "mixed:awq"} {
		if _, ok := lookupByEngineModel(name, manifests); ok {
			t.Errorf("%q resolved but its variant does not support that engine", name)
		}
	}
}

func TestLookupByEngineModel_DeterministicOnCollision(t *testing.T) {
	// Two manifests advertising the same tag is a catalog defect, but
	// resolution must still be deterministic regardless of slice order:
	// strongest (ParamCount x QuantizationTier) wins, ModelID breaks ties.
	weak := catalog.Manifest{
		ModelID: "weak",
		Variants: []catalog.Variant{{
			VariantID:        "v",
			RuntimeSupport:   []string{catalog.RuntimeOllama},
			ParamCount:       1_000_000_000,
			QuantizationTier: 4,
			Source:           catalog.VariantSource{Tag: "shared:q4"},
		}},
	}
	strong := catalog.Manifest{
		ModelID: "strong",
		Variants: []catalog.Variant{{
			VariantID:        "v",
			RuntimeSupport:   []string{catalog.RuntimeOllama},
			ParamCount:       8_000_000_000,
			QuantizationTier: 4,
			Source:           catalog.VariantSource{Tag: "shared:q4"},
		}},
	}
	for _, tc := range []struct {
		name      string
		manifests []catalog.Manifest
	}{
		{"weak first", []catalog.Manifest{weak, strong}},
		{"strong first", []catalog.Manifest{strong, weak}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, ok := lookupByEngineModel("shared:q4", tc.manifests)
			if !ok {
				t.Fatal("did not resolve")
			}
			if m.ModelID != "strong" {
				t.Fatalf("resolved to %q, want strong", m.ModelID)
			}
		})
	}
}

// TestBundledCatalog_EngineNamesResolve is the regression test for the
// bug's real-world shape: every engine-native identifier the SHIPPED
// catalog can put on the wire must resolve on the serving side. Before
// #107 not one of the ollama tags did.
func TestBundledCatalog_EngineNamesResolve(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	if len(manifests) == 0 {
		t.Fatal("bundled catalog is empty")
	}
	sel := NewSelector(Inputs{Manifests: manifests})

	checked := 0
	for _, m := range manifests {
		for _, v := range m.Variants {
			for _, name := range engineNamesOf(v) {
				checked++
				var reasons []string
				if _, ok := sel.resolveModel(name, &reasons); !ok {
					t.Errorf("%s/%s: engine name %q does not resolve — a peer serving it 404s",
						m.ModelID, v.VariantID, name)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no engine-native identifiers found in the bundled catalog")
	}
	t.Logf("checked %d engine-native identifiers across %d manifests", checked, len(manifests))
}

// engineNamesOf returns the identifiers a peer running this variant can
// advertise, mirroring variantWantSets.
func engineNamesOf(v catalog.Variant) []string {
	var out []string
	if supports(v.RuntimeSupport, catalog.RuntimeOllama) && v.Source.Tag != "" {
		out = append(out, v.Source.Tag)
	}
	if supports(v.RuntimeSupport, catalog.RuntimeVLLM) && v.Source.RepoID != "" {
		out = append(out, v.Source.RepoID)
	}
	return out
}

// TestSelectK_ServingLegAcceptsEngineModel drives the full serving-side
// entry point the way a peer hop does: SelectK with the exact string
// makeMeshCandidate would have put in Selection.EngineModel, against a
// local-only Selector (MeshSnapshotFn nil, AllowExternal false) — the
// overlay posture from cmd/waired-agent's overlayDeps.
func TestSelectK_ServingLegAcceptsEngineModel(t *testing.T) {
	consumer := NewSelector(Inputs{
		Manifests:  []catalog.Manifest{qwen()},
		LocalState: readyState(),
		Hardware:   goodHardware(),
		Runtimes:   registryWithOllama(),
	})
	sent, err := consumer.Select(context.Background(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("consumer Select: %v", err)
	}
	if sent.EngineModel == "" {
		t.Fatal("consumer produced no EngineModel")
	}

	serving := NewSelector(Inputs{
		Manifests:  []catalog.Manifest{qwen()},
		LocalState: readyState(),
		Hardware:   goodHardware(),
		Runtimes:   registryWithOllama(),
		// Overlay posture: no mesh recursion, no external fallback.
		MeshSnapshotFn: nil,
		AllowExternal:  false,
	})
	cands, err := serving.SelectK(context.Background(), Request{Model: sent.EngineModel}, 1)
	if err != nil {
		if errors.Is(err, ErrModelNotFound) {
			t.Fatalf("serving leg rejected the consumer's own EngineModel %q with ErrModelNotFound", sent.EngineModel)
		}
		t.Fatalf("serving SelectK: %v", err)
	}
	if len(cands) == 0 {
		t.Fatal("serving leg produced no candidate")
	}
	if cands[0].ExecutionMode != "local" {
		t.Fatalf("serving leg execution mode = %q, want local", cands[0].ExecutionMode)
	}
}
