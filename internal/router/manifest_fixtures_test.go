package router

import (
	"github.com/waired-ai/waired-agent/internal/catalog"
)

// bigQwen is a second, stronger manifest, so a test can put a peer on a
// different catalog model than the requester's own.
func bigQwen() catalog.Manifest {
	return catalog.Manifest{
		ModelID:       "qwen3-32b-instruct",
		ContextLength: 32768,
		Capabilities:  []string{"chat"},
		Runtime:       catalog.RuntimePolicy{Preferred: catalog.RuntimeOllama},
		Variants: []catalog.Variant{{
			VariantID:        "q4-gguf",
			Format:           catalog.FormatOllamaTag,
			RuntimeSupport:   []string{catalog.RuntimeOllama},
			ParamCount:       32,
			QuantizationTier: 4,
			Source:           catalog.VariantSource{Type: "ollama", Tag: "qwen3:32b-q4_K_M"},
		}},
	}
}
