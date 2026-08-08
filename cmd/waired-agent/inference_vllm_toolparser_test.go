package main

import (
	"slices"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

func TestResolveVLLMToolParser(t *testing.T) {
	tests := []struct {
		name     string
		modelID  string
		override string
		want     string
	}{
		// The qwen2.5-coder (hermes), qwen3-coder (qwen3_xml) and
		// glm-4.5-air (glm45) rows went with #522's retirement, and their
		// map entries went with them — an entry for a model the catalog
		// no longer ships is dead code that reads as coverage.
		{"qwen3.6 ships the XML dialect", "qwen3.6-27b", "", vllmParserQwen3XML},
		{"gpt-oss ships harmony", "gpt-oss-20b", "", vllmParserOpenAI},
		{"deepseek v4 flash", "deepseek-v4-flash", "", vllmParserDeepSeekV4},

		// An unestablished template must NOT be guessed into a
		// plausible neighbour: "" means "emit no flags", which leaves
		// the model's output untouched for the gateway to recover
		// (#409) instead of letting a mismatched parser mangle it.
		{"unmapped model emits no flags", "some-model-nobody-mapped", "", ""},
		{"empty model id emits no flags", "", "", ""},

		// The override is the escape hatch for a parser vLLM registered
		// after this binary was built, so it is passed through
		// unvalidated and outranks the table.
		{"override wins over a mapped model", "qwen3.6-27b", "llama3_json", "llama3_json"},
		{"override supplies a parser for an unmapped model", "glm-5.2", "glm47", "glm47"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveVLLMToolParser(catalog.Manifest{ModelID: tt.modelID}, tt.override)
			if got != tt.want {
				t.Errorf("resolveVLLMToolParser(%q, %q) = %q, want %q",
					tt.modelID, tt.override, got, tt.want)
			}
		})
	}
}

// Every value in the table must be one of the constants read out of
// vLLM's own registry. vLLM validates --tool-call-parser at start-up and
// refuses to boot on an unknown name, so a raw-string typo in a new row
// would cost the whole engine rather than just tool calling — and no
// unit test that only exercises the mapped models would notice.
func TestVLLMToolParserTableUsesRegisteredNames(t *testing.T) {
	// hermes and glm45 have no row in the table since #522 retired the
	// models that used them. They stay declared: these are vLLM's
	// registered parser names read out of its source at the pinned
	// version, not names we invented, and the next model of either
	// lineage needs them back. The check below is over the table's
	// VALUES, so an unused constant costs nothing here.
	registered := map[string]bool{
		vllmParserHermes:     true,
		vllmParserQwen3XML:   true,
		vllmParserOpenAI:     true,
		vllmParserGLM45:      true,
		vllmParserDeepSeekV4: true,
	}
	for modelID, parser := range vllmToolParserByModelID {
		if !registered[parser] {
			t.Errorf("model %q maps to %q, which is not one of the verified vLLM parser constants",
				modelID, parser)
		}
	}
}

// vllmToolParserUnestablished lists bundled models that CAN be served by
// vLLM but whose chat template has not been established against vLLM's
// parser registry. They serve without tool calling rather than with a
// guessed parser.
//
// This is the price of keeping the mapping in the agent instead of on
// the catalog variant (see inference_vllm_toolparser.go): adding a vLLM
// variant has to be paired with a decision here. Deleting an entry is
// the fix — measure the model, add the row.
var vllmToolParserUnestablished = map[string]string{
	// qwen3-coder-next-80b-a3b-instruct sat here — its id said "coder"
	// while the variant loaded the general Instruct model, so neither
	// parser was established — until #522 retired it. Dropped rather than
	// kept: the guard below iterates the shipped manifests, so an
	// exemption for a model nobody ships excuses nothing.
	//
	// vLLM 0.24.0 documents glm45 for GLM-4.5/4.6 and glm47 for GLM-4.7.
	// zai-org/GLM-5.2 is a later major version with neither listed.
	"glm-5.2": "no vLLM parser documented for the GLM-5 line",
}

// A bundled model that vLLM can serve either resolves to a parser or is
// declared unestablished above. Without this, adding a vLLM variant to
// the catalog ships a model that silently cannot make tool calls — the
// exact defect #410 reports, reintroduced by a data change.
func TestResolveVLLMToolParser_bundledCatalogCovered(t *testing.T) {
	manifests, err := catalog.BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatalf("BundledManifestsIncludingInternal: %v", err)
	}

	servesViaVLLM := func(m catalog.Manifest) bool {
		for _, v := range m.Variants {
			if slices.Contains(v.RuntimeSupport, catalog.RuntimeVLLM) {
				return true
			}
		}
		return false
	}

	seen := 0
	for _, m := range manifests {
		if !servesViaVLLM(m) {
			if _, declared := vllmToolParserUnestablished[m.ModelID]; declared {
				t.Errorf("%s is declared unestablished but has no vLLM variant; drop the entry", m.ModelID)
			}
			if _, mapped := vllmToolParserByModelID[m.ModelID]; mapped {
				t.Errorf("%s maps to a vLLM parser but has no vLLM variant; drop the row", m.ModelID)
			}
			continue
		}
		seen++
		parser := resolveVLLMToolParser(m, "")
		_, declared := vllmToolParserUnestablished[m.ModelID]
		switch {
		case parser == "" && !declared:
			t.Errorf("%s can be served by vLLM but resolves to no --tool-call-parser: "+
				"add a row to vllmToolParserByModelID, or declare it in vllmToolParserUnestablished with a reason",
				m.ModelID)
		case parser != "" && declared:
			t.Errorf("%s resolves to %q yet is still declared unestablished; drop the declaration",
				m.ModelID, parser)
		}
	}
	if seen == 0 {
		t.Fatal("no bundled manifest declares a vLLM variant; the guard is asserting nothing")
	}
	t.Logf("%d bundled models can be served by vLLM; %d declared unestablished",
		seen, len(vllmToolParserUnestablished))
}
