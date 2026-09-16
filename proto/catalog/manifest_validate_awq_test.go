package catalog

import "testing"

// Product contract: a quantization published outside the model's own org
// validates (owner, 2026-09-16, waired-ai/waired#1427: 「有志の量子化も
// 認める」). Validate used to refuse an AWQ repository outside Qwen/.
// What stays is that an AWQ build is a Hugging Face repository.
func TestValidate_AWQFromOutsideTheModelsOrg(t *testing.T) {
	base := func() Manifest {
		return Manifest{
			ModelID:        "m",
			ContextLength:  262144,
			DefaultVariant: map[string]string{RuntimeVLLM: "awq"},
			Variants: []Variant{{
				VariantID: "awq", Format: FormatSafetensors, Quantization: "AWQ-int4",
				RuntimeSupport: []string{RuntimeVLLM},
				QualityTier:    50, ParamCount: 2e9, QuantizationTier: 4,
				Source: VariantSource{Type: SourceHuggingFace, RepoID: "someone/M-AWQ-4bit",
					Revision: "718fd9b52c5ca16f1f856f6c4e4209129802a6a2"},
			}},
		}
	}
	if m := base(); m.Validate() != nil {
		t.Errorf("an AWQ build published outside the model's org must validate, got %v", m.Validate())
	}

	m := base()
	m.Variants[0].Format = FormatOllamaTag
	m.Variants[0].RuntimeSupport = []string{RuntimeOllama}
	m.DefaultVariant = map[string]string{RuntimeOllama: "awq"}
	m.Variants[0].Source = VariantSource{Type: SourceOllama, Tag: "m:awq"}
	if err := m.Validate(); err == nil {
		t.Error("an AWQ variant sourced from the ollama registry validated; AWQ is a Hugging Face format")
	}
}
