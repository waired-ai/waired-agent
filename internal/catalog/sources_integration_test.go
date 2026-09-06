//go:build integration

// Source resolution against the real registries (waired-agent#824).
//
// Behind `integration` because it reaches the network, so it cannot live
// in the required unit job. Run it with:
//
//	go test -tags integration -count=1 ./internal/catalog/...
//
// CI runs it from .github/workflows/catalog-sources.yml, which triggers
// on proto/catalog/bundled/** — the PR that introduces a bad id is the
// one that goes red.
//
// What this exists for: every check on a source used to be a check on
// the STRING. Manifest.Validate requires safetensors variants to carry a
// non-empty source.repo_id, and AWQ variants to carry one starting
// "Qwen/". Qwen/Qwen3.6-27B-AWQ satisfies both and does not exist; it
// shipped in the catalog for months, and the failure surfaced as a pull
// error on the machine of whoever picked the model, while they waited.
package catalog

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog/hfclient"
	"github.com/waired-ai/waired-agent/internal/catalog/ollamaregistry"
)

// TestBundledSourcesResolve asks each registry whether the thing the
// catalog names is there.
//
// The COMPLETE set, not the offered one: a withheld model is still
// shipped, and its source is still what a host would try to pull.
func TestBundledSourcesResolve(t *testing.T) {
	manifests, err := BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatalf("BundledManifestsIncludingInternal: %v", err)
	}
	if len(manifests) == 0 {
		t.Fatal("no bundled manifests — this test is checking nothing")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	hf := &hfclient.Client{}
	ollama := &ollamaregistry.Client{}

	checked := 0
	for _, m := range manifests {
		for _, v := range m.Variants {
			name := m.ModelID + "/" + v.VariantID
			switch v.Source.Type {
			case SourceHuggingFace:
				if v.Source.RepoID == "" {
					continue // Validate's business, and it already fails on this.
				}
				checked++
				t.Run(name, func(t *testing.T) {
					ok, err := hf.RepoExists(ctx, v.Source.RepoID, v.Source.Revision)
					if err != nil {
						// Could not ask. Not evidence of absence, and
						// reporting it as such would delete a live entry
						// from the catalog on a bad afternoon.
						t.Skipf("could not reach the Hub for %s: %v", v.Source.RepoID, err)
					}
					if !ok {
						t.Errorf("source.repo_id %q does not resolve on Hugging Face. "+
							"A repo id that satisfies every string check and does not exist is "+
							"exactly how Qwen/Qwen3.6-27B-AWQ shipped (#824)", v.Source.RepoID)
					}
				})
			case SourceOllama:
				if v.Source.Tag == "" {
					continue
				}
				checked++
				t.Run(name, func(t *testing.T) {
					ok, err := ollama.TagExists(ctx, v.Source.Tag)
					if err != nil {
						t.Skipf("could not reach the ollama registry for %s: %v", v.Source.Tag, err)
					}
					if !ok {
						t.Errorf("source.tag %q does not resolve in the ollama registry", v.Source.Tag)
						return
					}
					// Existing is not enough. A tag that brings neither a
					// built-in renderer nor a template layer is served
					// through the chat template baked into the model file,
					// and those are written for the vendor's own API:
					// waired-agent#1192 measured one answering 500 to a
					// trailing system turn, to a system turn after a tool
					// round-trip, and to a developer turn — the shapes
					// Claude Code sends, and the same red #1035 / #1095
					// found. That model pulled, loaded and passed the
					// agent-harness grade first; the cause was legible in
					// its manifest the whole time.
					//
					// NECESSARY, NOT SUFFICIENT. A tag can carry a
					// renderer and still refuse a shape, so this never
					// stands in for `catalog-tool shapes
					// --require-accepted`. It moves one registry-visible
					// cause of that red from the end of a GPU run to the
					// seconds a manifest fetch takes.
					//
					// A manifest may also supply the renderer itself, and
					// then the published tag is not required to carry
					// one: the pull path rewrites the local manifest with
					// it (download.Puller.Stamp), so what the engine
					// serves does name a renderer even though the
					// registry copy does not. That is not a loophole
					// around this check — it is the only way to ship a
					// GGUF from a community namespace at all, because no
					// GGUF publisher sets the field (24 tags across six
					// namespaces carry this model; none declares one).
					r, err := ollama.TagRendering(ctx, v.Source.Tag)
					if err != nil {
						t.Skipf("could not read what %s renders with: %v", v.Source.Tag, err)
					}
					switch {
					case r.Renders():
						// The tag names one itself; nothing to stamp.
					case v.Renderer != "":
						t.Logf("source.tag %q brings %s; the manifest stamps renderer %q on pull",
							v.Source.Tag, r, v.Renderer)
					default:
						t.Errorf("source.tag %q brings %s and the manifest names no renderer "+
							"either, so ollama will render it with the model file's own chat "+
							"template — which is how a tag that pulls, loads and grades still "+
							"fails the request-shape gate (#1192). Fix it by naming a renderer "+
							"on the variant, or by choosing a tag that carries one",
							v.Source.Tag, r)
					}
				})
			}
		}
	}

	// A sweep that resolved nothing is not a clean sweep. Without this,
	// a rename of the source-type constants would empty the switch and
	// this file would go green having asked no registry anything.
	if checked == 0 {
		t.Fatal("no variant carried a resolvable source — this test is checking nothing")
	}
	t.Logf("resolved %d source(s) across %d manifest(s)", checked, len(manifests))
}

// TestSourceCheckersReportAbsence is the other half, and the one that
// makes the sweep above mean something: a checker that answered "yes" to
// everything would make TestBundledSourcesResolve pass no matter what
// the catalog said.
//
// These two ids are chosen to be permanently absent rather than merely
// absent today.
func TestSourceCheckersReportAbsence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	t.Run("huggingface", func(t *testing.T) {
		hf := &hfclient.Client{}
		ok, err := hf.RepoExists(ctx, "waired-ai/no-such-model-waired-824", "")
		if err != nil {
			t.Skipf("could not reach the Hub: %v", err)
		}
		if ok {
			t.Error("the Hub checker reports a nonexistent repo as present; " +
				"every row in the sweep above is then meaningless")
		}
	})

	t.Run("ollama", func(t *testing.T) {
		reg := &ollamaregistry.Client{}
		ok, err := reg.TagExists(ctx, "waired-no-such-model-824:0b")
		if err != nil {
			t.Skipf("could not reach the ollama registry: %v", err)
		}
		if ok {
			t.Error("the registry checker reports a nonexistent tag as present; " +
				"every row in the sweep above is then meaningless")
		}
	})

	// ErrNotFound stays the sentinel FetchConfig callers expect; this
	// test must not be read as changing that contract.
	if !errors.Is(hfclient.ErrNotFound, hfclient.ErrNotFound) {
		t.Fatal("unreachable")
	}
}
