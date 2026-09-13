package hostfit

import (
	"testing"

	"github.com/waired-ai/waired-agent/proto/catalog"
)

func bundledVariantForTest(t *testing.T, model, variant string) catalog.Variant {
	t.Helper()
	ms, err := catalog.BundledManifests()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		if m.ModelID != model {
			continue
		}
		for _, v := range m.Variants {
			if v.VariantID == variant {
				return v
			}
		}
	}
	t.Fatalf("the bundled catalog has no %s/%s", model, variant)
	return catalog.Variant{}
}

// TestOllamaEstimateMemoryMatchesTheFit holds the estimate to what
// llama.cpp's fit measured on real loads (ollama 0.33.3), term by term
// in the aggregate: the device requirement fit compares with free memory
// is its "projected to use" figure plus its free-memory target. The
// estimate adds the process context before fit measures, so that term is
// taken off here, and so is the MTP draft head's setup, which lowers the
// free figure rather than adding to the projection.
//
// A record of measured behaviour (waired-ai/waired#1357,
// waired-ai/waired-agent#1337), not a contract with the engine: a pin
// move that changes these buffers changes these rows. The bound is
// asymmetric on purpose. An estimate under the fit recommends a load
// that spills, so it may fall short by no more than rounding (64 MiB);
// one over it only recommends a lighter build.
func TestOllamaEstimateMemoryMatchesTheFit(t *testing.T) {
	cuda := Host{RAMTotalGB: 121, GPUCount: 1, VRAM0MB: 24467, GPUVendor: "nvidia"}
	metal := Host{RAMTotalGB: 48, GPUCount: 1, UnifiedMemory: true, UsableVRAMMB: 38338, GPUVendor: "apple"}
	vulkan := Host{RAMTotalGB: 128, GPUCount: 1, UnifiedMemory: true, UsableVRAMMB: 98304, GPUVendor: "amd"}

	mtp27 := bundledVariantForTest(t, "qwen3.8-27b", "mtp-q4-gguf")
	ud3 := bundledVariantForTest(t, "qwen3.8-27b", "q3-gguf")
	moe35 := bundledVariantForTest(t, "qwen3.6-35b-a3b", "mtp-q4-gguf")
	// unsloth's UD-Q4_K_M of the same model is not in the catalog; its
	// header reads 15,691.23 MiB of tensors and 13,679.8 MiB of repeating
	// blocks, with the same geometry and token_embd type as the MTP build.
	ud4 := bundledVariantForTest(t, "qwen3.8-27b", "q3-gguf")
	ud4Layout := *ud4.GGUF
	ud4Layout.TensorBytes, ud4Layout.RepeatingBytes = 16453449523, 14344153907
	ud4.GGUF, ud4.HostResidentWeightGB = &ud4Layout, 0.715
	// qwen3.8-flash-next carries no layout in the catalog until its size is
	// corrected (#1305); these are its header facts, with the KV figure the
	// engine allocates today (attention + both halves of the indexer cache).
	flashNext := catalog.Variant{
		EstimatedWeightGB: 79.78, KVBytesPerTokenFP16: 33792, HostResidentWeightGB: 29.237,
		GGUF: &catalog.GGUFLayout{
			BlockCount: 48, FullAttentionLayers: 12, RecurrentStateBytes: 118040000,
			TensorBytes: 78858074931, ProjectorBytes: 907542528,
			RepeatingBytes: 49255563264, ExpertBytes: 46083686400,
		},
	}

	for _, tc := range []struct {
		name      string
		v         catalog.Variant
		h         Host
		kv        string
		window    int
		slots     int
		ubatch    int // the -ub ollama chose for the logged load
		projected int // fit's "projected to use" + its target, MiB
		overMiB   int // how far over the fit the estimate may run
	}{
		{"CUDA dense 27B MTP, q4_0, 200k", mtp27, cuda, catalog.KVCacheQ4_0, 200704, 1, 512, 21723 + 1936, 128},
		{"CUDA dense 27B MTP, q4_0, 32k", mtp27, cuda, catalog.KVCacheQ4_0, 32768, 1, 512, 17161 + 1936, 128},
		{"CUDA dense 27B MTP, f16, 41k", mtp27, cuda, catalog.KVCacheF16, 40960, 1, 512, 19107 + 1936, 128},
		{"CUDA dense 27B MTP, q8_0, 200k", mtp27, cuda, catalog.KVCacheQ8_0, 200704, 1, 512, 24859 + 1936, 128},
		{"CUDA dense 27B MTP, q8_0, 32k", mtp27, cuda, catalog.KVCacheQ8_0, 32768, 1, 512, 17673 + 1936, 128},
		{"CUDA dense 27B UD-Q3, q8_0, 200k", ud3, cuda, catalog.KVCacheQ8_0, 200704, 1, 512, 19545 + 1936, 128},
		{"CUDA dense 27B UD-Q3, q4_0, 200k", ud3, cuda, catalog.KVCacheQ4_0, 200704, 1, 512, 16409 + 1936, 128},
		{"CUDA dense 27B UD-Q2, q8_0, 200k", bundledVariantForTest(t, "qwen3.8-27b", "q2-gguf"), cuda, catalog.KVCacheQ8_0, 200704, 1, 512, 16504 + 1936, 128},
		{"CUDA dense 27B UD-Q4, q8_0, 200k", ud4, cuda, catalog.KVCacheQ8_0, 200704, 1, 512, 22548 + 1936, 128},
		{"CUDA dense 27B UD-Q4, q4_0, 32k", ud4, cuda, catalog.KVCacheQ4_0, 32768, 1, 512, 15640 + 1936, 128},
		{"CUDA dense qwen3.6 27B MTP, q8_0, 200k", bundledVariantForTest(t, "qwen3.6-27b", "mtp-q4-gguf"), cuda, catalog.KVCacheQ8_0, 200704, 1, 512, 24735 + 1936, 128},
		{"CUDA MoE 35B-A3B MTP, q8_0, 32k", moe35, cuda, catalog.KVCacheQ8_0, 32768, 1, 512, 21248 + 1909, 128},
		{"CUDA MoE 35B-A3B MTP, q8_0, 200k", moe35, cuda, catalog.KVCacheQ8_0, 200704, 1, 512, 23967 + 1909, 128},
		{"CUDA MoE 35B-A3B MTP, q4_0, 200k", moe35, cuda, catalog.KVCacheQ4_0, 200704, 1, 512, 22987 + 1909, 128},
		{"CUDA MoE 35B-A3B UD-Q3, q8_0, 200k", bundledVariantForTest(t, "qwen3.6-35b-a3b", "mtp-q3-gguf"), cuda, catalog.KVCacheQ8_0, 200704, 1, 512, 18284 + 1909, 128},
		{"CUDA dense 9B, inline vision, q8_0, 200k", bundledVariantForTest(t, "qwen3.5-9b", "q4-gguf"), cuda, catalog.KVCacheQ8_0, 200704, 1, 2048, 9891 + 1919, 256},
		{"CUDA dense 9B, inline vision, q4_0, 200k", bundledVariantForTest(t, "qwen3.5-9b", "q4-gguf"), cuda, catalog.KVCacheQ4_0, 200704, 1, 2048, 8323 + 1919, 256},
		{"CUDA dense 4B, inline vision, q8_0, 200k", bundledVariantForTest(t, "qwen3.5-4b", "q4-gguf"), cuda, catalog.KVCacheQ8_0, 200704, 1, 2048, 7664 + 1685, 256},
		// The base of the compute buffer is charged per ubatch token at the
		// 27B's rate; the smallest model's is lower, so its rows run over.
		{"CUDA dense 0.8B, inline vision, q8_0, 200k", bundledVariantForTest(t, "qwen3.5-0.8b", "q8-gguf"), cuda, catalog.KVCacheQ8_0, 200704, 1, 2048, 3304 + 1241, 384},
		{"Metal MoE 35B-A3B MTP, q8_0, 200k", moe35, metal, catalog.KVCacheQ8_0, 200704, 1, 1024, 24877 + 2994, 256},
		{"Metal dense 4B, inline vision, q8_0, 200k", bundledVariantForTest(t, "qwen3.5-4b", "q4-gguf"), metal, catalog.KVCacheQ8_0, 200704, 1, 2048, 7697 + 2622, 256},
		{"Metal dense 0.8B, inline vision, q8_0, 44k", bundledVariantForTest(t, "qwen3.5-0.8b", "q8-gguf"), metal, catalog.KVCacheQ8_0, 44288, 1, 2048, 1431 + 1601, 384},

		{"Vulkan dense 0.8B, q8_0, 44k", bundledVariantForTest(t, "qwen3.5-0.8b", "q8-gguf"), vulkan, catalog.KVCacheQ8_0, 44288, 1, 2048, 1392 + 1241, 384},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := ollamaEstimateMemoryAt(tc.v, tc.h, tc.kv, tc.window, tc.slots, tc.ubatch)
			got := e.DeviceMB() - e.FixedMB
			if g := tc.v.GGUF; g != nil && g.DraftMaxTokens > 0 {
				if tc.h.UnifiedMemory {
					got -= ollamaDraftContextUnifiedMB
				} else {
					got -= ollamaDraftContextMB
				}
			}
			if got < tc.projected-64 || got > tc.projected+tc.overMiB {
				t.Errorf("device requirement = %d MiB, fit measured %d; want within [-64, +%d] (weights %d, KV %d, recurrent %d, compute %d, draft %d, target %d)",
					got, tc.projected, tc.overMiB, e.DeviceWeightsMB, e.KVCacheMB, e.RecurrentStateMB, e.ComputeMB, e.DraftMB, e.FitTargetMB)
			}
		})
	}
	// qwen3.8-flash-next's indexer (its sparse attention's token scorer)
	// holds a compute buffer the estimate has no term for: 566 MiB short at
	// two 200k slots, about 1.5 KB per cell. The model carries no layout in
	// the catalog (waired-agent#1305), so nothing prices it this way today;
	// this pins the gap so a layout for it arrives with the missing term.
	e := ollamaEstimateMemoryAt(flashNext, vulkan, catalog.KVCacheQ8_0, 200704, 2, 512)
	if gap := 56538 + 1914 - (e.DeviceMB() - e.FixedMB); gap < 400 || gap > 700 {
		t.Errorf("flash-next estimate is %d MiB under the fit, want the known 400–700 MiB indexer gap", gap)
	}
}

// TestOllamaEstimateMemoryTerms pins the terms that are engineering facts
// against the engine's own buffer lines, to the MiB: ggml's KV block sizes,
// the recurrent state and its per-draft copies, the f16 draft KV, and the
// input layer kept in system RAM.
func TestOllamaEstimateMemoryTerms(t *testing.T) {
	cuda := Host{RAMTotalGB: 121, GPUCount: 1, VRAM0MB: 24467, GPUVendor: "nvidia"}
	mtp27 := bundledVariantForTest(t, "qwen3.8-27b", "mtp-q4-gguf")
	for _, tc := range []struct {
		kv          string
		window      int
		kvMiB, rMiB int
	}{
		{catalog.KVCacheQ8_0, 200704, 6664, 749},
		{catalog.KVCacheQ4_0, 200704, 3528, 749},
		{catalog.KVCacheQ8_0, 32768, 1088, 749},
		{catalog.KVCacheQ4_0, 32768, 576, 749},
		{catalog.KVCacheF16, 40960, 2560, 749},
	} {
		e := OllamaEstimateMemory(mtp27, cuda, tc.kv, tc.window, 1)
		if e.KVCacheMB != tc.kvMiB || e.RecurrentStateMB != tc.rMiB {
			t.Errorf("%s @%d: KV %d / recurrent %d MiB, want %d / %d", tc.kv, tc.window, e.KVCacheMB, e.RecurrentStateMB, tc.kvMiB, tc.rMiB)
		}
	}
	if e := OllamaEstimateMemory(mtp27, cuda, catalog.KVCacheQ8_0, 200704, 1); e.HostWeightsMB != 682 {
		t.Errorf("input layer in system RAM = %d MiB, want 682 (CPU_Mapped model buffer)", e.HostWeightsMB)
	}
	// No draft in the tag: one copy of the recurrent state, no draft context.
	ud3 := bundledVariantForTest(t, "qwen3.8-27b", "q3-gguf")
	if e := OllamaEstimateMemory(ud3, cuda, catalog.KVCacheQ8_0, 200704, 1); e.RecurrentStateMB != 150 || e.DraftMB != 0 {
		t.Errorf("UD-Q3: recurrent %d, draft %d; want 150 and 0", e.RecurrentStateMB, e.DraftMB)
	}
	if got := OllamaKVCacheFactor(catalog.KVCacheQ8_0); got != 0.53125 {
		t.Errorf("q8_0 factor = %v", got)
	}
	if got := OllamaKVCacheFactor("q5_1"); got != 1 {
		t.Errorf("an unknown type must price at f16, got %v", got)
	}
}

// TestOllamaPredictPlacementMatchesTheFit holds the layer prediction to
// llama.cpp's "offloaded N/M layers" on the loads a 24 GB card measured
// (budget = the free figure the product reads before the engine starts).
// A record of measured behaviour, like the test above.
func TestOllamaPredictPlacementMatchesTheFit(t *testing.T) {
	cuda := Host{RAMTotalGB: 121, GPUCount: 1, VRAM0MB: 24467, VRAMAvailable0MB: 23997, GPUVendor: "nvidia"}
	mtp27 := bundledVariantForTest(t, "qwen3.8-27b", "mtp-q4-gguf")
	ud4 := bundledVariantForTest(t, "qwen3.8-27b", "q3-gguf")
	ud4Layout := *ud4.GGUF
	ud4Layout.TensorBytes, ud4Layout.RepeatingBytes = 16453449523, 14344153907
	ud4.GGUF, ud4.HostResidentWeightGB = &ud4Layout, 0.715
	for _, tc := range []struct {
		name string
		v    catalog.Variant
		kv   string
		gpu  int
	}{
		{"MTP-Q4 q8_0", mtp27, catalog.KVCacheQ8_0, 53},
		{"MTP-Q4 q4_0", mtp27, catalog.KVCacheQ4_0, 63},
		{"UD-Q4 q8_0", ud4, catalog.KVCacheQ8_0, 61},
		{"UD-Q3 q8_0", bundledVariantForTest(t, "qwen3.8-27b", "q3-gguf"), catalog.KVCacheQ8_0, 66},
		{"UD-Q2 q8_0", bundledVariantForTest(t, "qwen3.8-27b", "q2-gguf"), catalog.KVCacheQ8_0, 66},
	} {
		p := OllamaPredictPlacement(tc.v, cuda, tc.kv, 200704, 1)
		if p.TotalLayers != 66 || p.GPULayers < tc.gpu-1 || p.GPULayers > tc.gpu {
			t.Errorf("%s: predicted %d/%d layers on the GPU, llama.cpp placed %d/66 (want the same or one fewer)", tc.name, p.GPULayers, p.TotalLayers, tc.gpu)
		}
	}
}
