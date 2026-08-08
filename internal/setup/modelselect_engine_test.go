package setup

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
)

// The install pick must not lose local inference to an engine choice.
//
// Product contract, ratified in waired-agent#522 (owner decision
// 2026-08-08): the engine auto-pick requires a model the engine can serve.
// SelectBundledModel is where that matters — PickEngine names the engine
// and SelectInstallModel judges every model against it, with no way back.
//
// This test exists because the term is opt-in at the call site
// (EnginePickInput.Catalog): dropping the field here compiles, passes
// every other test in the tree, and silently restores the defect. It is
// the guard on the wiring, not on the rule — TestPickEngine_RequiresAModelTheEngineCanServe
// covers the rule.
func TestSelectBundledModel_EngineMustBeFeedable(t *testing.T) {
	// A host that clears every hardware term of the vLLM auto-pick, and a
	// catalog whose only vLLM variant needs twice its VRAM. Ollama has a
	// model it fits comfortably.
	hw := hardware.Profile{
		OS:         "linux",
		Arch:       "x86_64",
		RAMTotalGB: 32,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", Model: "RTX 4060 Ti", VRAMTotalMB: 16000}},
	}
	manifests := []catalog.Manifest{
		{
			ModelID: "vllm-only-too-big", DisplayName: "vLLM Only Too Big",
			ContextLength: 262144,
			Variants: []catalog.Variant{{
				VariantID: "awq-int4", Format: catalog.FormatSafetensors,
				RuntimeSupport: []string{catalog.RuntimeVLLM},
				MinVRAMMB:      32000, QualityTier: 80,
				Source: catalog.VariantSource{Type: catalog.SourceHuggingFace, RepoID: "example/big"},
			}},
		},
		{
			ModelID: "ollama-fits-here", DisplayName: "Ollama Fits Here",
			ContextLength: 262144,
			Variants: []catalog.Variant{{
				VariantID: "q4-gguf", Format: catalog.FormatOllamaTag,
				RuntimeSupport: []string{catalog.RuntimeOllama},
				MinRAMGB:       12, QualityTier: 52,
				Source: catalog.VariantSource{Type: catalog.SourceOllama, Tag: "fits:q4"},
			}},
		},
	}

	in := baseInputs(hw, manifests)
	in.Inference.BundledModelID = "" // nothing pinned: exercise the auto path
	in.FreeDiskBytes = fixedDisk(500)

	sel, err := SelectBundledModel(in)
	if err != nil {
		t.Fatalf("SelectBundledModel: %v", err)
	}
	if sel.BelowRecommendedSpec {
		t.Fatalf("host reported below the recommended spec, but %q fits it on ollama — "+
			"the engine pick was made without asking whether the catalog could feed it "+
			"(notes: %v)", "ollama-fits-here", sel.Notes)
	}
	if sel.ModelID != "ollama-fits-here" {
		t.Errorf("ModelID = %q, want ollama-fits-here", sel.ModelID)
	}
	if !sel.EnableInference {
		t.Errorf("EnableInference = false, want true: a model was selected")
	}
}
