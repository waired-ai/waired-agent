package catalog

import (
	"encoding/json"
	"strings"
	"testing"
)

// Both fields must serialise away when unset, so a consumer built against an
// older proto tag sees a byte-identical variant. Required by the
// additive-only proto contract (CLAUDE.md §Modules) and not otherwise
// observable from Go.
func TestRendererAndParserAreAbsentWhenUnset(t *testing.T) {
	b, err := json.Marshal(Variant{VariantID: "v", Format: FormatOllamaTag})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, field := range []string{"renderer", "parser"} {
		if strings.Contains(string(b), `"`+field+`"`) {
			t.Errorf("%s must be omitempty; got %s", field, b)
		}
	}
	// The whole struct, not just these fields: adding them must not have
	// disturbed anything else that was already omitempty.
	const want = `{"variant_id":"v","format":"ollama-tag","runtime_support":null,` +
		`"quality_tier":0,"param_count":0,"quantization_tier":0,"source":{"type":""}}`
	if got := string(b); got != want {
		t.Errorf("empty variant no longer serialises as before:\n got %s\nwant %s", got, want)
	}
}

// The digest deliberately does not cover Renderer/Parser: its payload is
// frozen, and widening it would make every persisted measurement on every
// host stop matching (see VariantSHA's doc comment). This test exists so
// that a future edit which "fixes" the omission fails loudly here, next to
// the reasoning, instead of silently emptying the measurement stores.
func TestVariantSHA_IgnoresRendererAndParser(t *testing.T) {
	base := Variant{
		VariantID:    "q2-gguf",
		Format:       FormatOllamaTag,
		Quantization: "Q2_K",
		Source:       VariantSource{Type: SourceOllama, Tag: "ns/model:tag"},
	}
	stamped := base
	stamped.Renderer = "qwen3.8"
	stamped.Parser = "qwen3.5"

	if got, want := VariantSHA(stamped), VariantSHA(base); got != want {
		t.Errorf("VariantSHA changed when a renderer was stamped:\n got %s\nwant %s\n"+
			"the payload is frozen; carry the distinction in VariantID and guard it "+
			"in the measurement stores instead", got, want)
	}
}
