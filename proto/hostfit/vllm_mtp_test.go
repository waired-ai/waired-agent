package hostfit

import (
	"testing"

	"github.com/waired-ai/waired-agent/proto/catalog"
	"github.com/waired-ai/waired-agent/proto/signer"
)

var rtxPro4000 = []signer.HardwareGPUSummary{{Vendor: "nvidia", Model: "NVIDIA RTX PRO 4000 Blackwell", VRAMTotalMB: 24467, ComputeCap: "12.0"}}

// Without a draft the variant-aware sizing is the published one, number for
// number, for every vLLM build in the catalog: adding MTP must not move a
// window anybody is served today. The published function's signature is
// frozen by the proto guard, which is why the new one exists at all.
func TestVLLMMaxModelLenForWithoutADraftIsVLLMMaxModelLen(t *testing.T) {
	ms, err := catalog.BundledManifests()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		for _, v := range m.Variants {
			if v.Format != catalog.FormatSafetensors {
				continue
			}
			for _, kf := range []float64{VLLMKVFactorF16, VLLMKVFactorFP8} {
				old := VLLMMaxModelLen(v.EstimatedWeightGB, v.KVBytesPerTokenFP16, 1, DefaultVLLMGPUMemoryUtilization, kf, rtxPro4000)
				if got := VLLMMaxModelLenFor(v, 0, 1, DefaultVLLMGPUMemoryUtilization, kf, rtxPro4000); got != old {
					t.Errorf("%s/%s kv×%.1f: VLLMMaxModelLenFor(draft 0) = %d, VLLMMaxModelLen = %d", m.ModelID, v.VariantID, kf, got, old)
				}
			}
		}
	}
}

// A longer draft never buys a longer window, and a build without MTP layers
// is priced as if no draft were asked for (vLLM would not run one).
func TestVLLMMaxModelLenForDraftCosts(t *testing.T) {
	v := catalog.Variant{VariantID: "bf16", Format: catalog.FormatSafetensors, EstimatedWeightGB: 9.32,
		KVBytesPerTokenFP16: 32768, MTPLayers: 1, MTPKVBytesPerTokenFP16: 4096}
	prev := VLLMMaxModelLenFor(v, 0, 1, DefaultVLLMGPUMemoryUtilization, VLLMKVFactorFP8, rtxPro4000)
	if prev <= 0 {
		t.Fatalf("no window at draft 0: %d", prev)
	}
	for n := 1; n <= 4; n++ {
		got := VLLMMaxModelLenFor(v, n, 1, DefaultVLLMGPUMemoryUtilization, VLLMKVFactorFP8, rtxPro4000)
		if got <= 0 || got >= prev {
			t.Errorf("draft %d: window %d, want 0 < window < %d (draft %d)", n, got, prev, n-1)
		}
		prev = got
	}
	noLayers := v
	noLayers.MTPLayers, noLayers.MTPKVBytesPerTokenFP16 = 0, 0
	if a, b := VLLMMaxModelLenFor(noLayers, 3, 1, DefaultVLLMGPUMemoryUtilization, VLLMKVFactorFP8, rtxPro4000),
		VLLMMaxModelLenFor(noLayers, 0, 1, DefaultVLLMGPUMemoryUtilization, VLLMKVFactorFP8, rtxPro4000); a != b {
		t.Errorf("a build without MTP layers priced a draft: %d vs %d", a, b)
	}
}

// The ollama estimate reads the draft that actually runs: a draft the
// product stamps onto a tag (Variant.MTPDraftTokens) prices exactly like the
// same draft published in the tag's own parameters.
func TestOllamaEstimateMemoryPricesAStampedDraftLikeTheTagsOwn(t *testing.T) {
	v := bundledVariantForTest(t, "qwen3.6-35b-a3b", "mtp-q2-gguf")
	if v.GGUF == nil || v.GGUF.NextNLayers == 0 || v.GGUF.DraftMaxTokens != 0 {
		t.Skipf("fixture changed: %+v", v.GGUF)
	}
	h := Host{RAMTotalGB: 120, GPUCount: 1, VRAM0MB: 24467, UsableVRAMMB: 24467, VRAMPoolMB: 24467, GPUVendor: "nvidia"}
	own := v
	g := *v.GGUF
	g.DraftMaxTokens = 2
	own.GGUF = &g
	stamped := v
	stamped.MTPDraftTokens = 2
	if a, b := OllamaEstimateMemory(own, h, catalog.KVCacheQ4_0, 200704, 1), OllamaEstimateMemory(stamped, h, catalog.KVCacheQ4_0, 200704, 1); a != b {
		t.Errorf("stamped draft priced %+v, the tag's own draft %+v", b, a)
	}
	if a, b := OllamaEstimateMemory(v, h, catalog.KVCacheQ4_0, 200704, 1).DeviceMB(), OllamaEstimateMemory(stamped, h, catalog.KVCacheQ4_0, 200704, 1).DeviceMB(); b <= a {
		t.Errorf("a draft added no device memory: %d vs %d", b, a)
	}
}
