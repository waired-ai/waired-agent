package catalog

import (
	"testing"

	protocatalog "github.com/waired-ai/waired-agent/proto/catalog"
)

func baseVariant() Variant {
	return Variant{
		VariantID:         "q4-gguf",
		Format:            "gguf",
		Quantization:      "Q4_K_M",
		DType:             "",
		RuntimeSupport:    []string{"ollama"},
		EstimatedWeightGB: 5.4,
		MinRAMGB:          16,
		MinVRAMMB:         0,
		QualityTier:       72,
		ParamCount:        8_000_000_000,
		QuantizationTier:  4,
		Source: VariantSource{
			Type: "ollama",
			Tag:  "qwen3:8b-q4_K_M",
		},
	}
}

func TestVariantSHA_StableForIdenticalInput(t *testing.T) {
	v := baseVariant()
	first := VariantSHA(v)
	second := VariantSHA(v)
	if first != second {
		t.Fatalf("two calls with the same variant produced different digests: %s vs %s", first, second)
	}

	fromScratch1 := VariantSHA(baseVariant())
	fromScratch2 := VariantSHA(baseVariant())
	if fromScratch1 != fromScratch2 {
		t.Fatalf("two calls building the same variant from scratch produced different digests: %s vs %s", fromScratch1, fromScratch2)
	}
	if first != fromScratch1 {
		t.Fatalf("variant built two ways produced different digests: %s vs %s", first, fromScratch1)
	}
}

func TestVariantSHA_ChangesWhenIdentityChanges(t *testing.T) {
	base := VariantSHA(baseVariant())

	cases := []struct {
		name   string
		mutate func(*Variant)
	}{
		{"VariantID", func(v *Variant) { v.VariantID = "q5-gguf" }},
		{"Format", func(v *Variant) { v.Format = "safetensors" }},
		{"Quantization", func(v *Variant) { v.Quantization = "Q5_K_M" }},
		{"DType", func(v *Variant) { v.DType = "bfloat16" }},
		{"Source.Type", func(v *Variant) { v.Source.Type = "huggingface" }},
		{"Source.Tag", func(v *Variant) { v.Source.Tag = "qwen3:8b-q5_K_M" }},
		{"Source.RepoID", func(v *Variant) { v.Source.RepoID = "Qwen/Qwen3-8B" }},
		{"Source.Revision", func(v *Variant) { v.Source.Revision = "abc123" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := baseVariant()
			tc.mutate(&v)
			if got := VariantSHA(v); got == base {
				t.Fatalf("changing %s did not change digest (still %s)", tc.name, got)
			}
		})
	}
}

func TestVariantSHA_StableUnderMutableMetadataChange(t *testing.T) {
	base := VariantSHA(baseVariant())

	cases := []struct {
		name   string
		mutate func(*Variant)
	}{
		{"QualityTier", func(v *Variant) { v.QualityTier = 99 }},
		{"EstimatedWeightGB", func(v *Variant) { v.EstimatedWeightGB = 9.9 }},
		{"MinRAMGB", func(v *Variant) { v.MinRAMGB = 64 }},
		{"MinVRAMMB", func(v *Variant) { v.MinVRAMMB = 24576 }},
		{"ParamCount", func(v *Variant) { v.ParamCount = 70_000_000_000 }},
		{"QuantizationTier", func(v *Variant) { v.QuantizationTier = 8 }},
		{"RuntimeSupport", func(v *Variant) { v.RuntimeSupport = []string{"vllm"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := baseVariant()
			tc.mutate(&v)
			if got := VariantSHA(v); got != base {
				t.Fatalf("changing %s changed digest unexpectedly: %s -> %s", tc.name, base, got)
			}
		})
	}
}

func TestVariantSHA_HexEncoded64Chars(t *testing.T) {
	got := VariantSHA(baseVariant())
	if len(got) != 64 {
		t.Fatalf("expected 64-char hex digest, got %d chars: %q", len(got), got)
	}
	for _, c := range got {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Fatalf("non-hex character %q in digest %q", c, got)
		}
	}
}

// TestVariantSHAMatchesProto: this digest is spelled twice — here and in
// proto/catalog — and nothing compared them.
//
// The proto copy's own comment says why that matters: "A second spelling
// of this hash would not fail loudly — it would simply never match, and
// the measurements would look absent (waired-agent#784, #970)." Both
// spellings are load-bearing on measurement keys: this one keys the
// request-shape store and its baseline exemptions, the proto one keys
// modelrank. A drift between them makes a recorded variant read as
// unmeasured, which is a gap the operator cannot close by measuring
// again.
//
// Driven over the real catalog rather than a fixture, so the cases are
// the variants actually shipped.
func TestVariantSHAMatchesProto(t *testing.T) {
	manifests, err := BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatalf("BundledManifestsIncludingInternal: %v", err)
	}
	checked := 0
	for _, m := range manifests {
		for _, v := range m.Variants {
			checked++
			if got, want := VariantSHA(v), protocatalog.VariantSHA(v); got != want {
				t.Errorf("%s/%s: internal/catalog says %s, proto/catalog says %s — "+
					"the two spellings of this digest have drifted, and every record keyed "+
					"by one of them now reads as absent to the other",
					m.ModelID, v.VariantID, got, want)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no variants in the bundled catalog — this guard is checking nothing")
	}
}
