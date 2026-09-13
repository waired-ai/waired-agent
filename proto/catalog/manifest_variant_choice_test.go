package catalog

import (
	"encoding/json"
	"strings"
	"testing"
)

// The wire for a user-chosen build and KV-cache type
// (waired-ai/waired-agent#1346; decisions 1, 4 and 6 of
// docs/decisions/20260913/2355-catalog-variant-kv-and-residency-rulings.md).
// Validate refuses what a manifest author could get wrong: a KV-cache type
// the variant's engine does not have, a default build the engine cannot
// serve, and a host-resident part larger than the whole model.
func TestValidate_VariantChoiceFields(t *testing.T) {
	base := func() Manifest {
		return Manifest{
			ModelID:       "m",
			ContextLength: 262144,
			Variants: []Variant{
				{
					VariantID: "q4-gguf", Format: FormatOllamaTag,
					RuntimeSupport: []string{RuntimeOllama},
					QualityTier:    50, ParamCount: 1e9, QuantizationTier: 4,
					EstimatedWeightGB: 3.4,
					Source:            VariantSource{Type: SourceOllama, Tag: "m:q4"},
				},
				{
					VariantID: "fp8", Format: FormatSafetensors,
					RuntimeSupport: []string{RuntimeVLLM},
					QualityTier:    51, ParamCount: 1e9, QuantizationTier: 8,
					Source: VariantSource{Type: SourceHuggingFace, RepoID: "org/m"},
				},
			},
		}
	}
	if m := base(); m.Validate() != nil {
		t.Fatalf("the base fixture must validate, got %v", m.Validate())
	}

	ok := base()
	ok.DefaultVariant = map[string]string{RuntimeOllama: "q4-gguf", RuntimeVLLM: "fp8"}
	ok.Variants[0].KVCacheTypes = []string{KVCacheQ4_0, KVCacheQ8_0, KVCacheF16}
	ok.Variants[0].HostResidentWeightGB = 0.521
	ok.Variants[1].KVCacheTypes = []string{KVCacheFP8, KVCacheFP16}
	if err := ok.Validate(); err != nil {
		t.Fatalf("a manifest using every new field correctly must validate, got %v", err)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*Manifest)
		want   string
	}{
		{"an ollama variant offering a vLLM cache type", func(m *Manifest) {
			m.Variants[0].KVCacheTypes = []string{KVCacheFP8}
		}, "kv_cache_types"},
		{"a vLLM variant offering an ollama cache type", func(m *Manifest) {
			m.Variants[1].KVCacheTypes = []string{KVCacheQ4_0}
		}, "kv_cache_types"},
		{"a default for an engine that does not exist", func(m *Manifest) {
			m.DefaultVariant = map[string]string{"mlx": "q4-gguf"}
		}, "unknown engine"},
		{"a default naming no variant", func(m *Manifest) {
			m.DefaultVariant = map[string]string{RuntimeOllama: "q3-gguf"}
		}, "names no variant"},
		{"a default the engine cannot serve", func(m *Manifest) {
			m.DefaultVariant = map[string]string{RuntimeOllama: "fp8"}
		}, "names no variant"},
		{"a host-resident part larger than the model", func(m *Manifest) {
			m.Variants[0].HostResidentWeightGB = 4
		}, "host_resident_weight_gb"},
		{"a negative host-resident part", func(m *Manifest) {
			m.Variants[0].HostResidentWeightGB = -0.1
		}, "host_resident_weight_gb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := base()
			tc.mutate(&m)
			err := m.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate() = %v, want an error mentioning %q", err, tc.want)
			}
		})
	}
}

// Every new field is omitempty, so a manifest that uses none of them
// marshals byte-identically to one written before they existed — which is
// what lets a control plane on an older proto tag round-trip today's
// bundled catalog unchanged.
func TestVariantChoiceFields_AbsentWhenUnset(t *testing.T) {
	m := Manifest{ModelID: "m", Variants: []Variant{{VariantID: "q4-gguf"}}}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"default_variant", "kv_cache_types", "host_resident_weight_gb"} {
		if strings.Contains(string(b), `"`+key+`"`) {
			t.Errorf("unset %s is on the wire: %s", key, b)
		}
	}
}
