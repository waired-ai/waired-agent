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

// Measured (waired-ai/waired#1432): the window VLLMMaxModelLenFor sizes
// for Qwen3.5-4B bf16 is one the engine starts with. vLLM 0.29.0 on an
// RTX PRO 4000 Blackwell (24,467 MiB), fp8 KV, prefix caching, one start
// per row; the pool is the engine's "GPU KV cache size" line, and the
// engine refuses to start when it is below --max-model-len.
//
// At util 0.85 every row is capped by the model's 262,144-token window. The
// rows at util 0.5664 put 0.85 × 16,303 MiB inside the same card, the
// budget of a 16 GB card, where the estimate is what binds: the windows
// 84,992 / 53,248 / 46,080 started there with pools of 135,791 / 93,184 /
// 82,106 tokens. Without the draft priced, the 2-token row would have asked
// for 84,992 tokens against a pool of 82,106 and not started.
func TestVLLMMaxModelLenForStartsOnTheMeasuredPools(t *testing.T) {
	v := catalog.Variant{VariantID: "bf16", Format: catalog.FormatSafetensors, EstimatedWeightGB: 9.32,
		KVBytesPerTokenFP16: 32768, MTPLayers: 1, MTPKVBytesPerTokenFP16: 4096}
	const nativeWindow = 262144
	for _, c := range []struct {
		util  float64
		draft int
		pool  int
	}{
		{0.85, 0, 567464}, {0.85, 1, 479581}, {0.85, 2, 469207}, {0.85, 3, 463793}, {0.85, 4, 456474},
		{0.5664, 0, 135791}, {0.5664, 1, 93184}, {0.5664, 2, 82106},
	} {
		window := min(VLLMMaxModelLenFor(v, c.draft, 1, c.util, VLLMKVFactorFP8, rtxPro4000), nativeWindow)
		if window <= 0 || window > c.pool {
			t.Errorf("util %.4f draft %d: window %d, measured pool %d", c.util, c.draft, window, c.pool)
		}
	}
	if unpriced := VLLMMaxModelLenFor(v, 0, 1, 0.5664, VLLMKVFactorFP8, rtxPro4000); unpriced <= 82106 {
		t.Errorf("fixture no longer shows why the draft is priced: the draft-free window %d already fits the 2-token pool", unpriced)
	}
}

// The reserve covers what each measured draft took from the memory the
// profiler left for the KV cache ("Available KV cache memory", GiB, draft 0
// minus draft N, same card and settings as above). The pool rows above
// cannot pin it on their own: the other terms of the estimate leave the
// 4B enough slack to start with no reserve at all, and a smaller build does
// not have that slack.
func TestVLLMMTPReserveCoversTheMeasuredCost(t *testing.T) {
	for _, c := range []struct {
		build string
		draft int
		gib   float64
	}{
		{"qwen3.5-4b", 1, 8.90 - 8.56}, {"qwen3.5-4b", 2, 8.90 - 8.47}, {"qwen3.5-4b", 3, 8.90 - 8.47}, {"qwen3.5-4b", 4, 8.90 - 8.44},
		{"qwen3.5-2b", 1, 13.26 - 13.01}, {"qwen3.5-2b", 2, 13.26 - 12.93},
		{"qwen3.5-0.8b", 1, 16.20 - 16.05}, {"qwen3.5-0.8b", 2, 16.20 - 15.94},
	} {
		if reserve := vllmMTPReserveMB + float64(c.draft)*vllmMTPReservePerDraftTokenMB; reserve < c.gib*1024 {
			t.Errorf("%s draft %d: reserve %.0f MiB, measured %.0f MiB", c.build, c.draft, reserve, c.gib*1024)
		}
	}
}

// A draft the product writes onto a tag runs only where the load with it
// fits the device; a draft the tag publishes runs everywhere, because
// ollama reads it from the tag (waired-ai/waired#1433).
func TestOllamaDraftTokensWritesTheDraftOnlyWhereItFits(t *testing.T) {
	q2 := bundledVariantForTest(t, "qwen3.8-27b", "q2-gguf")
	if q2.GGUF == nil || q2.GGUF.NextNLayers == 0 || q2.GGUF.DraftMaxTokens != 0 {
		t.Skipf("fixture changed: %+v", q2.GGUF)
	}
	stamped := q2
	stamped.MTPDraftTokens = 2
	published := q2
	pg := *q2.GGUF
	pg.DraftMaxTokens = 2
	published.GGUF = &pg

	// The two Macs M9 ran on, as their profilers report them.
	mac16 := Host{RAMTotalGB: 16, GPUCount: 1, UnifiedMemory: true, UsableVRAMMB: 12288, VRAM0MB: 16384, GPUVendor: "apple"}
	mac48 := Host{RAMTotalGB: 48, GPUCount: 1, UnifiedMemory: true, UsableVRAMMB: 36864, VRAM0MB: 49152, GPUVendor: "apple"}
	cpu := Host{RAMTotalGB: 64}
	const window = 200704
	for _, c := range []struct {
		name string
		v    catalog.Variant
		h    Host
		want int
	}{
		// Measured on the 16 GB Mac: 38/66 layers and 4 tokens/s without
		// the draft, 24/66 and 0.11 tokens/s with it.
		{"written, 16 GB Mac, spills", stamped, mac16, 0},
		// Measured on the 48 GB Mac: 66/66 layers with the draft.
		{"written, 48 GB Mac, fits", stamped, mac48, 2},
		{"written, no GPU", stamped, cpu, 0},
		{"published, 16 GB Mac", published, mac16, 2},
		{"none", q2, mac48, 0},
	} {
		got := OllamaDraftTokens(c.v, c.h, catalog.KVCacheQ4_0, window, 1)
		if got != c.want {
			t.Errorf("%s: draft %d, want %d", c.name, got, c.want)
		}
		mem := OllamaEstimateMemory(c.v, c.h, catalog.KVCacheQ4_0, window, 1)
		if (mem.DraftMB > 0) != (c.want > 0) {
			t.Errorf("%s: estimate prices a draft of %d MiB for a draft of %d tokens", c.name, mem.DraftMB, c.want)
		}
	}
	// Where the written draft does not run, the load is priced as the tag
	// without one, number for number.
	if a, b := OllamaEstimateMemory(stamped, mac16, catalog.KVCacheQ4_0, window, 1), OllamaEstimateMemory(q2, mac16, catalog.KVCacheQ4_0, window, 1); a != b {
		t.Errorf("16 GB Mac: withheld draft priced %+v, no draft %+v", a, b)
	}
}
