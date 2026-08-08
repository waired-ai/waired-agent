package router

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
)

// nvidiaLinuxHost is a host that clears every hardware term of the vLLM
// auto-pick: NVIDIA vendor, Linux, and VRAM at or above MinVLLMVRAMMB.
// Only the catalog term is left to decide.
func nvidiaLinuxHost(vramMB int) hardware.Profile {
	return hardware.Profile{
		OS:         "linux",
		RAMTotalGB: 32,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", Model: "RTX 4060 Ti", VRAMTotalMB: vramMB}},
	}
}

// vllmVariant is a safetensors variant needing minVRAMMB, i.e. one only
// vLLM can serve. ollamaVariant is its gguf counterpart. The 262144
// context length keeps both above the #624 native coding floor so that
// gate is not what decides these cases.
func vllmVariant(id string, minVRAMMB, tier int) catalog.Manifest {
	return catalog.Manifest{
		ModelID: id, DisplayName: id, ContextLength: 262144,
		Variants: []catalog.Variant{{
			VariantID: "awq-int4", Format: catalog.FormatSafetensors,
			RuntimeSupport: []string{catalog.RuntimeVLLM},
			MinVRAMMB:      minVRAMMB, QualityTier: tier,
			Source: catalog.VariantSource{Type: catalog.SourceHuggingFace, RepoID: "example/" + id},
		}},
	}
}

func ollamaVariant(id string, minRAMGB, tier int) catalog.Manifest {
	return catalog.Manifest{
		ModelID: id, DisplayName: id, ContextLength: 262144,
		Variants: []catalog.Variant{{
			VariantID: "q4-gguf", Format: catalog.FormatOllamaTag,
			RuntimeSupport: []string{catalog.RuntimeOllama},
			MinRAMGB:       minRAMGB, QualityTier: tier,
			Source: catalog.VariantSource{Type: catalog.SourceOllama, Tag: id + ":q4"},
		}},
	}
}

// The catalog term of the auto-pick rule.
//
// Product contract, ratified in waired-agent#522 (owner decision
// 2026-08-08): PickEngine does not name an engine that no catalog variant
// fits on this host. Every consumer of the pick judges models against the
// engine it names and none of them can revisit it, so naming an unfeedable
// one costs the host its local inference on hardware the other engine
// would serve.
func TestPickEngine_RequiresAModelTheEngineCanServe(t *testing.T) {
	const vram = 16000

	cases := []struct {
		name       string
		manifests  []catalog.Manifest
		preference string
		want       string
		wantReason string
	}{
		{
			name: "no fitting vllm variant falls back to ollama",
			manifests: []catalog.Manifest{
				vllmVariant("too-big-for-this-card", 24000, 72),
				ollamaVariant("fits-on-ram", 12, 52),
			},
			want:       catalog.RuntimeOllama,
			wantReason: "no vllm variant in the catalog fits this host",
		},
		{
			name: "a fitting vllm variant keeps vllm",
			manifests: []catalog.Manifest{
				vllmVariant("fits-this-card", 12000, 72),
				ollamaVariant("fits-on-ram", 12, 52),
			},
			want:       catalog.RuntimeVLLM,
			wantReason: "auto: vllm",
		},
		{
			name: "a vllm-servable catalog with nothing else still keeps vllm",
			manifests: []catalog.Manifest{
				vllmVariant("fits-this-card", 12000, 72),
			},
			want: catalog.RuntimeVLLM,
		},
		{
			// The documented empty-catalog case: the caller has nothing to
			// judge against, so the hardware terms decide alone. This is
			// what every caller got before the term existed, and it is why
			// the field can be added without auditing each one.
			name:      "an empty catalog leaves the hardware rule alone",
			manifests: nil,
			want:      catalog.RuntimeVLLM,
		},
		{
			// Preference bypasses auto-detection entirely, including this
			// term: an operator asking for vllm has an external reason, the
			// same contract the VRAM and OS terms already honour.
			name: "an explicit preference is honoured even with nothing to serve",
			manifests: []catalog.Manifest{
				vllmVariant("too-big-for-this-card", 24000, 72),
			},
			preference: catalog.RuntimeVLLM,
			want:       catalog.RuntimeVLLM,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pick, err := PickEngine(EnginePickInput{
				Hardware:   nvidiaLinuxHost(vram),
				Preference: tc.preference,
				Catalog:    tc.manifests,
			})
			if err != nil {
				t.Fatalf("PickEngine: %v", err)
			}
			if pick.Engine != tc.want {
				t.Fatalf("Engine = %q, want %q (reasons: %v)", pick.Engine, tc.want, pick.Reasons)
			}
			if tc.wantReason == "" {
				return
			}
			if !strings.Contains(strings.Join(pick.Reasons, " | "), tc.wantReason) {
				t.Errorf("reasons %v do not explain the pick; want one containing %q",
					pick.Reasons, tc.wantReason)
			}
		})
	}
}

// The shipped catalog today, so the diff that changes it is visible.
//
// This is a record of today's catalog, not a contract. The 8 GB and 16 GB
// NVIDIA Linux rows pass only because qwen2.5-coder's awq-int4 variants sit
// at 8000 and 16000 MB of VRAM — and waired-agent#522 retires that
// generation. When it does, these two rows move to ollama and the retiring
// PR must say so: the smallest remaining vLLM variant needs 24000 MB, so
// every NVIDIA card between the vLLM threshold and 24 GB changes engine.
//
// That is the whole reason this term landed first. Before it, the same
// change would have taken local inference away from those hosts instead.
func TestPickEngine_ShippedCatalog_TodaysVerdicts(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}

	cases := []struct {
		vramMB int
		want   string
	}{
		{vramMB: 8192, want: catalog.RuntimeVLLM},
		{vramMB: 16000, want: catalog.RuntimeVLLM},
		{vramMB: 24576, want: catalog.RuntimeVLLM},
		{vramMB: 81920, want: catalog.RuntimeVLLM},
	}

	for _, tc := range cases {
		hw := nvidiaLinuxHost(tc.vramMB)
		pick, err := PickEngine(EnginePickInput{Hardware: hw, Catalog: manifests})
		if err != nil {
			t.Fatalf("PickEngine(%d MB): %v", tc.vramMB, err)
		}
		if pick.Engine != tc.want {
			t.Errorf("%d MB VRAM: Engine = %q, want %q (reasons: %v)",
				tc.vramMB, pick.Engine, tc.want, pick.Reasons)
		}
	}
}
