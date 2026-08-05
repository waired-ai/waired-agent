package router

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// A name a user pinned before an entry was retired keeps arriving long
// after the entry is gone — out of agent.json, out of a coding-agent
// config, out of a script. resolveModel is the ONE serve-path resolver
// (OpenAI, Anthropic, mesh hops and `waired infer` all reach it through
// SelectK step 1), so migrating the name here migrates every caller at
// once. Pins waired-ai/waired-agent#200.

// successorManifest mirrors qwen()'s shape under the id the shipped
// retirement table points at, so these tests exercise the real table
// rather than a fixture of one.
func successorManifest() catalog.Manifest {
	return catalog.Manifest{
		ModelID:       "qwen3.5-0.8b",
		ContextLength: 8192,
		Capabilities:  []string{"chat", "json_mode"},
		Runtime:       catalog.RuntimePolicy{Preferred: catalog.RuntimeOllama},
		Variants: []catalog.Variant{{
			VariantID:      "q8-gguf",
			Format:         catalog.FormatOllamaTag,
			RuntimeSupport: []string{catalog.RuntimeOllama},
			MinRAMGB:       2,
			Source:         catalog.VariantSource{Type: "ollama", Tag: "qwen3.5:0.8b-q8_0"},
		}},
	}
}

func successorReadyState() catalog.State {
	return catalog.State{
		Version: catalog.StateVersion,
		Models: map[string]catalog.ModelState{
			"qwen3.5-0.8b": {
				VariantID: "q8-gguf",
				OllamaTag: "qwen3.5:0.8b-q8_0",
				State:     catalog.ModelStateReady,
				PulledAt:  time.Now(),
			},
		},
		Endpoints: map[string]catalog.EndpointState{},
	}
}

// PRODUCT CONTRACT (#200): every name a retired entry answered to
// resolves to its successor, not just the model id. A user who pinned the
// short form is exactly as migrated as one who pinned the id — the
// aliases died with the manifest that declared them, so the table is the
// only thing that still knows them.
func TestResolveModel_RetiredNameServesSuccessor(t *testing.T) {
	sel := NewSelector(Inputs{Manifests: []catalog.Manifest{qwen(), successorManifest()}})

	for _, name := range []string{
		"qwen2.5-coder-0.5b-instruct",
		"qwen2.5-coder-0.5b",
		"Qwen/Qwen2.5-Coder-0.5B-Instruct",
	} {
		t.Run(name, func(t *testing.T) {
			var reasons []string
			m, ok := sel.resolveModel(name, &reasons)
			if !ok {
				t.Fatalf("retired name %q did not resolve — the request 404s instead of migrating", name)
			}
			if m.ModelID != "qwen3.5-0.8b" {
				t.Fatalf("resolved to %q, want the successor qwen3.5-0.8b", m.ModelID)
			}
			// The trace has to name BOTH: which name the caller used and
			// what actually answered. One without the other cannot explain
			// a support question about why the response says a different
			// model than the request did.
			if len(reasons) == 0 {
				t.Fatal("substituted with no reason recorded")
			}
			last := reasons[len(reasons)-1]
			if !strings.Contains(last, name) || !strings.Contains(last, "qwen3.5-0.8b") {
				t.Errorf("reason %q must name the requested and the served model", last)
			}
			if !strings.Contains(last, "retired") {
				t.Errorf("reason %q does not say the name was retired", last)
			}
		})
	}
}

// PRODUCT CONTRACT (#200): the retirement stage runs LAST, so it can only
// add resolutions and never redirect one. Unreachable against the shipped
// catalog — a proto test forbids a live manifest from claiming a retired
// name — but the ordering is what makes that test's guarantee the only
// thing standing between the two, so pin it here too.
func TestResolveModel_LiveCatalogWinsOverRetirement(t *testing.T) {
	shadow := catalog.Manifest{
		ModelID:       "shadow",
		ModelAliases:  []string{"qwen2.5-coder-0.5b"},
		ContextLength: 4096,
		Runtime:       catalog.RuntimePolicy{Preferred: catalog.RuntimeOllama},
		Variants: []catalog.Variant{{
			VariantID:      "v",
			Format:         catalog.FormatOllamaTag,
			RuntimeSupport: []string{catalog.RuntimeOllama},
			Source:         catalog.VariantSource{Type: "ollama", Tag: "shadow:latest"},
		}},
	}
	sel := NewSelector(Inputs{Manifests: []catalog.Manifest{shadow, successorManifest()}})

	var reasons []string
	m, ok := sel.resolveModel("qwen2.5-coder-0.5b", &reasons)
	if !ok {
		t.Fatal("resolveModel missed")
	}
	if m.ModelID != "shadow" {
		t.Fatalf("retirement overrode a live alias: resolved to %q", m.ModelID)
	}
	if len(reasons) > 0 && strings.Contains(reasons[len(reasons)-1], "retired") {
		t.Errorf("took the retirement path when the catalog answered: %v", reasons)
	}
}

// PRODUCT CONTRACT (#200): retirement is not a wildcard. A name nobody
// ever shipped still misses, and the caller still gets
// ErrModelNotFound — the table migrates what we withdrew, it does not
// absorb typos.
func TestResolveModel_UnknownIsStillNotFound(t *testing.T) {
	sel := NewSelector(Inputs{Manifests: []catalog.Manifest{qwen(), successorManifest()}})

	var reasons []string
	for _, name := range []string{"", "qwen2.5-coder-0.4b", "no-such-model"} {
		if m, ok := sel.resolveModel(name, &reasons); ok {
			t.Errorf("unknown name %q resolved to %q", name, m.ModelID)
		}
	}
}

// The behaviour, not the helper: a request naming a retired model is
// SERVED. This is the whole point of #200 — before it, the same request
// came back 404 model_not_found with nothing to act on.
func TestSelectK_RetiredNameIsServedByTheSuccessor(t *testing.T) {
	sel := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{successorManifest()},
		LocalState:     successorReadyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		MeshSnapshotFn: nil,
	})

	cands, err := sel.SelectK(context.Background(), Request{Model: "qwen2.5-coder-0.5b-instruct"}, 1)
	if err != nil {
		if errors.Is(err, ErrModelNotFound) {
			t.Fatalf("a retired model id is still answered with ErrModelNotFound — " +
				"the retirement map is not reaching the serve path")
		}
		t.Fatalf("SelectK: %v", err)
	}
	if len(cands) == 0 {
		t.Fatal("no candidate for a retired model id")
	}
	if cands[0].ModelID != "qwen3.5-0.8b" {
		t.Fatalf("served %q, want the successor qwen3.5-0.8b", cands[0].ModelID)
	}
	if cands[0].ExecutionMode != "local" {
		t.Fatalf("execution mode = %q, want local", cands[0].ExecutionMode)
	}
}

// The shipped-catalog end of the same claim: every retired name resolves
// against the manifests the daemon actually hands the router
// (BundledManifestsIncludingInternal — resolution takes the complete
// set). A retirement whose successor was itself withheld or removed would
// pass every fixture test above and still 404 in production.
func TestResolveModel_EveryRetiredNameResolvesOnTheShippedCatalog(t *testing.T) {
	manifests, err := catalog.BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatalf("BundledManifestsIncludingInternal: %v", err)
	}
	sel := NewSelector(Inputs{Manifests: manifests})

	checked := 0
	for _, r := range catalog.Retirements() {
		for _, name := range r.Names {
			checked++
			var reasons []string
			m, ok := sel.resolveModel(name, &reasons)
			if !ok {
				t.Errorf("retired name %q does not resolve on the shipped catalog", name)
				continue
			}
			if m.ModelID != r.SuccessorModelID {
				t.Errorf("retired name %q resolved to %q, want %q", name, m.ModelID, r.SuccessorModelID)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no retired names in the table — this test is asserting nothing")
	}
}
