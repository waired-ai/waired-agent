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
// This is a record of today's catalog, not a contract — and #522 is the
// diff it was written to catch. The 8 GB and 16 GB rows used to answer
// vllm, because qwen2.5-coder's awq-int4 variants sat at 8000 and
// 16000 MB of VRAM. Those retired with the rest of the 2025 generation,
// and every vLLM variant under 24 GB went with them: the smallest one
// left is qwen3.6-27b/awq-int4 at 24000 MB, the pinned generation's only
// safetensors build.
//
// So every NVIDIA card between MinVLLMVRAMMB and 24 GB now serves on
// ollama. That is a fallback, not coverage — #575 tracks adding vLLM
// builds for the qwen3.5 line, and closing it moves these rows back.
//
// It is also the whole reason the catalog term landed in #572 first.
// Without it these hosts would have lost local inference outright
// instead of changing engine.
//
// #823 moved the line again, from 24 GB to 40 GB, and the 24576 row with
// it. The build that used to sit at 24000 MB was qwen3.6-27b/awq-int4,
// sourced from Qwen/Qwen3.6-27B-AWQ — a repository that is not on
// Hugging Face and, going by the Qwen org's 27B listing, never was. The
// row is repointed at Qwen/Qwen3.6-27B-FP8, which exists, and FP8 is
// 30.9 GB of weights rather than 17, so its floor is 38912 MB. The 27B
// band is where the smallest vLLM build lives, so the whole line moved.
//
// The rows below are therefore not a regression against what a host
// could do yesterday: a 24 GB card auto-picking vLLM yesterday resolved
// to weights it could not fetch. They are the same fallback as before,
// now told truthfully, and #575 is still where the coverage gap lives.
func TestPickEngine_ShippedCatalog_TodaysVerdicts(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}

	cases := []struct {
		vramMB int
		want   string
	}{
		// Below the smallest remaining vLLM build (38912 MB).
		{vramMB: 8192, want: catalog.RuntimeOllama},
		{vramMB: 16000, want: catalog.RuntimeOllama},
		{vramMB: 24576, want: catalog.RuntimeOllama},
		// At and above it.
		{vramMB: 40960, want: catalog.RuntimeVLLM},
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
