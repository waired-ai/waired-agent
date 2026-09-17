//go:build integration

// The catalog's build limits against the registries and the pinned ollama
// source (waired-ai/waired-agent#1423). Behind `integration` for the same
// reason as sources_integration_test.go: it reaches the network. CI runs it
// from .github/workflows/catalog-sources.yml, which triggers on the bundled
// manifests and on the ollama pin.
package catalog

import (
	"context"
	"io"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog/ollamaregistry"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// TestBundledMaxParallelFollowsOllamaFamilies reads each bundled ollama
// tag's model family — the value ollama's scheduler compares — and holds the
// catalog to it: a build of a one-slot family carries max_parallel 1, and
// any other build carries none.
func TestBundledMaxParallelFollowsOllamaFamilies(t *testing.T) {
	manifests, err := BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatalf("BundledManifestsIncludingInternal: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	reg := &ollamaregistry.Client{}

	checked := 0
	for _, m := range manifests {
		for _, v := range m.Variants {
			if v.Source.Type != SourceOllama || v.Source.Tag == "" {
				continue
			}
			name := m.ModelID + "/" + v.VariantID
			family, err := reg.TagModelFamily(ctx, v.Source.Tag)
			if err != nil {
				// An outage is not a verdict about the catalog, and
				// TestBundledSourcesResolve already fails a tag that is gone.
				t.Logf("%s: could not read the family of %s: %v", name, v.Source.Tag, err)
				continue
			}
			checked++
			switch single := slices.Contains(ollamaSingleRequestFamilies, family); {
			case family == "":
				t.Errorf("%s: %s names no model_family; decide its max_parallel by hand and record why", name, v.Source.Tag)
			case single && v.MaxParallel != 1:
				t.Errorf("%s: family %q is one ollama serves one request at a time, but max_parallel is %d; want 1",
					name, family, v.MaxParallel)
			case !single && v.MaxParallel != 0:
				t.Errorf("%s: family %q is not on ollama's one-slot list, but max_parallel is %d; want it unset",
					name, family, v.MaxParallel)
			}
		}
	}
	if checked == 0 {
		t.Fatal("read no family from any registry — this test checked nothing")
	}
}

// TestOllamaSingleRequestFamiliesMatchThePin re-reads the one-slot family
// list from the ollama release this repository pins, so a pin bump that
// adds or lifts a family fails here instead of leaving the catalog's limits
// describing an engine nobody installs.
func TestOllamaSingleRequestFamiliesMatchThePin(t *testing.T) {
	url := "https://raw.githubusercontent.com/ollama/ollama/v" + infruntime.OllamaPinnedVersion + "/server/sched.go"
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Skipf("could not fetch %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, resp.StatusCode)
	}
	src, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	got, err := parseSchedSingleRequestFamilies(string(src))
	if err != nil {
		t.Fatalf("ollama v%s server/sched.go: %v", infruntime.OllamaPinnedVersion, err)
	}
	if !slices.Equal(got, ollamaSingleRequestFamilies) {
		t.Errorf("ollama v%s starts these families with one slot:\n  %v\nthe copy in ollama_single_request_families_test.go says:\n  %v\n"+
			"update the copy, then re-run TestBundledMaxParallelFollowsOllamaFamilies to see which builds' max_parallel must change",
			infruntime.OllamaPinnedVersion, got, ollamaSingleRequestFamilies)
	}
}
