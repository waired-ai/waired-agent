package catalog

import "testing"

// A manifest with no context_length must not validate.
//
// It is not a hole anyone can see: hostfit.OllamaCeilingWindow returns 0
// for it and OllamaPlannedWindow's cap is guarded on ceiling > 0, so the
// window sizing silently stops being capped rather than failing. The
// auto-selection path is protected by MeetsNativeContextFloor needing
// >= 200000, which a zero-window model cannot meet — but a PIN skips
// that, and a pin is the one input that arrives from outside.
//
// Record of a rule, not of today's catalog: every bundled entry has a
// window, which is why nothing caught this until the arithmetic was
// re-read (#552 review, closed in #522).
func TestValidate_RequiresAContextLength(t *testing.T) {
	base := func() Manifest {
		return Manifest{
			ModelID:       "m",
			ContextLength: 262144,
			Variants: []Variant{{
				VariantID: "q4-gguf", Format: FormatOllamaTag,
				RuntimeSupport: []string{RuntimeOllama},
				QualityTier:    50, ParamCount: 1e9, QuantizationTier: 4,
				Source: VariantSource{Type: SourceOllama, Tag: "m:q4"},
			}},
		}
	}
	if m := base(); m.Validate() != nil {
		t.Fatalf("the base fixture must validate, got %v", m.Validate())
	}
	for _, ctx := range []int{0, -1} {
		m := base()
		m.ContextLength = ctx
		if err := m.Validate(); err == nil {
			t.Errorf("context_length %d validated; a zero window disables the "+
				"window cap instead of failing", ctx)
		}
	}
}
