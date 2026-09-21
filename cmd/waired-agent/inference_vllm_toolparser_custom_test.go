package main

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// A custom model's vLLM parsers come from its own variant, set at import
// and checked there against the pinned vLLM's registry
// (waired-ai/waired#1480); it loads safetensors only. The host override
// still wins for the tool parser, as for every model. A record of today's
// behaviour.
func TestCustomModelVLLMFlags(t *testing.T) {
	custom := catalog.Manifest{
		ModelID: "custom-qwen3-8b-awq-0123abcd", Provenance: catalog.ProvenanceCustom,
		Variants: []catalog.Variant{{VariantID: "awq", VLLMToolCallParser: "hermes", VLLMReasoningParser: "qwen3"}},
	}
	if got := resolveVLLMToolParser(custom, ""); got != "hermes" {
		t.Errorf("tool parser %q, want the variant's", got)
	}
	if got := resolveVLLMToolParser(custom, "openai"); got != "openai" {
		t.Errorf("tool parser %q, want the host override", got)
	}
	if got := resolveVLLMReasoningParser(custom); got != "qwen3" {
		t.Errorf("reasoning parser %q", got)
	}
	if got := vllmLoadFormat(custom); got != "safetensors" {
		t.Errorf("load format %q", got)
	}
	bundled := catalog.Manifest{ModelID: "gpt-oss-20b", Variants: []catalog.Variant{{VLLMToolCallParser: "hermes"}}}
	if resolveVLLMReasoningParser(bundled) != "" || vllmLoadFormat(bundled) != "" {
		t.Error("a catalog model took a custom model's flags")
	}
	if got := resolveVLLMToolParser(bundled, ""); got != vllmToolParserByModelID["gpt-oss-20b"] {
		t.Errorf("a catalog model's tool parser came from its variant: %q", got)
	}
}
