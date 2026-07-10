package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

func loadManifestsT(t *testing.T) []catalog.Manifest {
	t.Helper()
	ms, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	if len(ms) == 0 {
		t.Fatal("BundledManifests returned no manifests")
	}
	return ms
}

// renderCatalogBlock must be a pure function of the bundled catalog: same input,
// byte-identical output. A timestamp or map-iteration leak here would make every
// regeneration churn the docs and defeat the `--check` freshness gate.
func TestRenderCatalogBlock_Deterministic(t *testing.T) {
	ms := loadManifestsT(t)
	a := renderCatalogBlock(ms)
	b := renderCatalogBlock(ms)
	if a != b {
		t.Fatal("renderCatalogBlock is not deterministic")
	}
	for _, want := range []string{
		"qwen2.5-coder-7b-instruct", // a known bundled model
		"waired/default",            // the renamed default alias (#422/#478)
		"### 固定エイリアス",
		"### ファミリ概要",
		"### 全バリアント（数値）",
		"#### Ollama 経路（Mac / Windows / CPU / 内蔵・低VRAM GPU）",
		"#### vLLM 経路（NVIDIA / AMD GPU サーバ）",
		"**Dense**",
		"**MoE（総 / 活性）**",
		"glm-4.5-air-106b-a12b", // a vLLM-only family
		"qwen3.5-9b",            // an Ollama-only family
	} {
		if !strings.Contains(a, want) {
			t.Errorf("generated block missing %q", want)
		}
	}
	if strings.Contains(a, "waired/auto") {
		t.Error("generated block still references the renamed alias waired/auto")
	}
}

// TestRenderCatalogBlock_EngineArchSplit locks the two-axis layout: the catalog
// is grouped by engine (Ollama / vLLM) then architecture (Dense before MoE), and
// a family is listed only under an engine it actually ships a build for. The
// vLLM-only GLM family must never appear in an Ollama section; the dual-engine
// Qwen coder must appear under both.
func TestRenderCatalogBlock_EngineArchSplit(t *testing.T) {
	block := renderCatalogBlock(loadManifestsT(t))

	famOverview := between(t, block, "### ファミリ概要", "### 全バリアント（数値）")
	ollama := between(t, famOverview, "#### Ollama 経路", "#### vLLM 経路")
	vllm := famOverview[strings.Index(famOverview, "#### vLLM 経路"):]

	// Dense is listed before MoE within an engine section.
	if i, j := strings.Index(ollama, "**Dense**"), strings.Index(ollama, "**MoE"); i < 0 || j < 0 || i > j {
		t.Errorf("Ollama section must list Dense before MoE (dense=%d moe=%d)", i, j)
	}
	// A vLLM-only family must not leak into the Ollama section, and must appear
	// under vLLM.
	if strings.Contains(ollama, "glm-4.5-air-106b-a12b") {
		t.Error("vLLM-only glm-4.5-air leaked into the Ollama family section")
	}
	if !strings.Contains(vllm, "glm-4.5-air-106b-a12b") {
		t.Error("vLLM-only glm-4.5-air missing from the vLLM family section")
	}
	// A dual-engine family appears under both engines.
	if !strings.Contains(ollama, "qwen2.5-coder-7b-instruct") || !strings.Contains(vllm, "qwen2.5-coder-7b-instruct") {
		t.Error("dual-engine qwen2.5-coder-7b-instruct should appear under both engines")
	}
}

// between returns the substring of s strictly between the first occurrence of
// start and the first occurrence of end after it; it fails the test if either
// marker is missing.
func between(t *testing.T, s, start, end string) string {
	t.Helper()
	_, rest, ok := strings.Cut(s, start)
	if !ok {
		t.Fatalf("marker %q not found", start)
	}
	mid, _, ok := strings.Cut(rest, end)
	if !ok {
		t.Fatalf("marker %q not found after %q", end, start)
	}
	return mid
}

func TestSpliceGeneratedBlock_Idempotent(t *testing.T) {
	doc := []byte("intro\n" + docsBeginMarker + "\nstale\n" + docsEndMarker + "\noutro\n")
	block := "FRESH BODY"
	once, err := spliceGeneratedBlock(doc, block)
	if err != nil {
		t.Fatalf("splice: %v", err)
	}
	twice, err := spliceGeneratedBlock(once, block)
	if err != nil {
		t.Fatalf("re-splice: %v", err)
	}
	if string(once) != string(twice) {
		t.Fatalf("splice not idempotent:\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
	s := string(once)
	if !strings.Contains(s, "FRESH BODY") || strings.Contains(s, "stale") {
		t.Fatalf("splice did not replace the block region:\n%s", s)
	}
	if !strings.HasPrefix(s, "intro\n") || !strings.HasSuffix(s, "outro\n") {
		t.Fatalf("splice did not preserve text outside the markers:\n%s", s)
	}
}

func TestSpliceGeneratedBlock_MissingMarkers(t *testing.T) {
	if _, err := spliceGeneratedBlock([]byte("no markers here"), "x"); err == nil {
		t.Fatal("expected error when markers are absent")
	}
	// End before begin must also error.
	bad := []byte(docsEndMarker + "\n" + docsBeginMarker)
	if _, err := spliceGeneratedBlock(bad, "x"); err == nil {
		t.Fatal("expected error when end marker precedes begin marker")
	}
}

// TestModelCatalogPageFresh guards the checked-in dev-docs page against drift:
// if a bundled manifest changed but reference/models.md was not regenerated,
// this fails in the ordinary `unit` test job (no extra CI wiring needed). It
// mirrors `catalog-tool docs --check`. Skips only if the page is absent (e.g. a
// sparse checkout), since that is not a staleness signal.
func TestModelCatalogPageFresh(t *testing.T) {
	rel := filepath.Join("..", "..", filepath.FromSlash(docsDefaultFile))
	existing, err := os.ReadFile(rel)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skipf("dev-docs model page not present at %s; skipping freshness guard", rel)
		}
		t.Fatalf("read %s: %v", rel, err)
	}
	block := renderCatalogBlock(loadManifestsT(t))
	updated, err := spliceGeneratedBlock(existing, block)
	if err != nil {
		t.Fatalf("splice %s: %v", rel, err)
	}
	if string(existing) != string(updated) {
		t.Fatalf("%s is stale — run `make catalog-docs` (or `catalog-tool docs`) and commit the result", docsDefaultFile)
	}
}
