package hostfit_test

import (
	"testing"

	"github.com/waired-ai/waired-agent/proto/catalog"
	"github.com/waired-ai/waired-agent/proto/hostfit"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// A custom model whose own window is below 200,704 tokens (waired-ai/waired#1481).
// On real hardware, a 0.6B model with a 40,960-token window was labelled
// "window exceeds memory" on a 16 GB Mac and the picker said the computer
// couldn't hold the window — false on both counts: the model's own window
// was the limit, and memory held all of it.
//
// PRODUCT CONTRACT (#1473 ruling 10, "no minimum window"; #1481): such a model
// is judged for memory at its own window, and the reason a surface shows is
// the window, never memory, unless memory cannot hold even the model's own
// window.

// qwen3-0.6b's shape: 28 layers × 8 KV heads × (128 + 128) × 2 bytes.
const smallKVPerToken = 28 * 8 * 256 * 2

func shortModel() (catalog.Manifest, catalog.Variant) {
	v := catalog.Variant{EstimatedWeightGB: 0.48, KVBytesPerTokenFP16: smallKVPerToken}
	return catalog.Manifest{ModelID: "custom-qwen3-0.6b-0123abcd", ContextLength: 40960, Variants: []catalog.Variant{v}}, v
}

func TestOllamaRecommend_ShortOwnWindowIsItsOwnReason(t *testing.T) {
	m, v := shortModel()
	for name, h := range map[string]hostfit.Host{
		"24 GB card":   hostFromWire(t, wireRTX4090),
		"16 GB Mac":    hostFromWire(t, wireMac16),
		"CPU, 16 GB":   {RAMTotalGB: 16},
		"CPU, 128 GB":  {RAMTotalGB: 128},
		"48 GB M4 Max": hostFromWire(t, wireMac48M4Max),
	} {
		kv := hostfit.ResolveKVCacheType(catalog.RuntimeOllama, v, h, nil, "")
		got := hostfit.OllamaRecommendModelFor(m, v, h, kv)
		if got.Fits || got.Reason != hostfit.ReasonModelWindowShort {
			t.Errorf("%s: %+v, want not recommended for %q", name, got, hostfit.ReasonModelWindowShort)
		}
		if got.NeedMB != 0 || got.HaveMB != 0 {
			t.Errorf("%s: NeedMB/HaveMB = %d/%d: this reason is about the window, not memory", name, got.NeedMB, got.HaveMB)
		}
	}
}

func TestOllamaRecommend_ShortOwnWindowStillReportsMemory(t *testing.T) {
	// 4 MiB of KV per token at FP16: the model's own 40,960 tokens need
	// 160 GiB of cache (80 GiB even at q8_0, which the 24 GB card resolves),
	// which neither a 24 GB card nor a 16 GB CPU host holds. Memory IS the
	// limit here, so the memory reason stands.
	m, v := shortModel()
	v.KVBytesPerTokenFP16 = 4 << 20
	m.Variants = []catalog.Variant{v}
	for name, h := range map[string]hostfit.Host{
		"24 GB card": hostFromWire(t, wireRTX4090),
		"CPU, 16 GB": {RAMTotalGB: 16},
	} {
		kv := hostfit.ResolveKVCacheType(catalog.RuntimeOllama, v, h, nil, "")
		if got := hostfit.OllamaRecommendModelFor(m, v, h, kv); got.Reason != hostfit.ReasonWindowExceedsMemory {
			t.Errorf("%s: reason %q, want %q", name, got.Reason, hostfit.ReasonWindowExceedsMemory)
		}
	}
}

func TestOllamaRecommend_ShortOwnWindowWithUnknownKV(t *testing.T) {
	// A custom model whose KV cache the import could not price (MLA,
	// sliding window) carries 0. The window is still what there is to say.
	m, v := shortModel()
	v.KVBytesPerTokenFP16 = 0
	m.Variants = []catalog.Variant{v}
	h := hostFromWire(t, wireRTX4090)
	kv := hostfit.ResolveKVCacheType(catalog.RuntimeOllama, v, h, nil, "")
	if got := hostfit.OllamaRecommendModelFor(m, v, h, kv); got.Reason != hostfit.ReasonModelWindowShort {
		t.Errorf("reason %q, want %q", got.Reason, hostfit.ReasonModelWindowShort)
	}
}

func TestOllamaRecommend_WeightsSpillOutranksAShortWindow(t *testing.T) {
	// Weights that do not fit the card are the more useful thing to say:
	// more memory would fix them, and nothing fixes the window.
	m, v := shortModel()
	v.EstimatedWeightGB = 40
	m.Variants = []catalog.Variant{v}
	h := hostFromWire(t, wireRTX4090)
	kv := hostfit.ResolveKVCacheType(catalog.RuntimeOllama, v, h, nil, "")
	if got := hostfit.OllamaRecommendModelFor(m, v, h, kv); got.Reason != hostfit.ReasonWeightsSpill {
		t.Errorf("reason %q, want %q", got.Reason, hostfit.ReasonWeightsSpill)
	}
}

func TestOllamaRecommend_CodingWindowModelsUnchanged(t *testing.T) {
	// The same shape with a 262,144-token window is a bundled-class model:
	// recommended where it was before.
	m, v := shortModel()
	m.ContextLength = 262144
	for name, h := range map[string]hostfit.Host{
		"24 GB card": hostFromWire(t, wireRTX4090),
		"CPU, 64 GB": {RAMTotalGB: 64},
	} {
		kv := hostfit.ResolveKVCacheType(catalog.RuntimeOllama, v, h, nil, "")
		if got := hostfit.OllamaRecommendModelFor(m, v, h, kv); !got.Fits {
			t.Errorf("%s: %+v, want recommended", name, got)
		}
	}
}

func TestVLLMRecommend_ShortOwnWindowIsItsOwnReason(t *testing.T) {
	m, v := shortModel()
	got := hostfit.VLLMRecommendModelOnHost(m, v, hostfit.Host{}, []signer.HardwareGPUSummary{l4})
	if got.Fits || got.Reason != hostfit.ReasonModelWindowShort {
		t.Errorf("small model on one L4: %+v, want %q", got, hostfit.ReasonModelWindowShort)
	}

	// Memory still wins when the pool cannot hold even the model's own
	// window: 14 GB of weights and 1 MiB per token on one L4.
	wide := catalog.Variant{EstimatedWeightGB: 14.0, KVBytesPerTokenFP16: 1 << 20}
	if got := hostfit.VLLMRecommendModelOnHost(m, wide, hostfit.Host{}, []signer.HardwareGPUSummary{l4}); got.Reason != hostfit.ReasonWindowExceedsMemory {
		t.Errorf("wide model on one L4: reason %q, want %q", got.Reason, hostfit.ReasonWindowExceedsMemory)
	}

	// A 262,144-token model is judged at 200,704 as before: one L4 cannot
	// hold that much of this cache, two can.
	m.ContextLength = 262144
	if got := hostfit.VLLMRecommendModelOnHost(m, v, hostfit.Host{}, []signer.HardwareGPUSummary{l4}); got.Reason != hostfit.ReasonWindowExceedsMemory {
		t.Errorf("a 262,144-token model on one L4: %+v, want %q as before", got, hostfit.ReasonWindowExceedsMemory)
	}
	if got := hostfit.VLLMRecommendModelOnHost(m, v, hostfit.Host{}, []signer.HardwareGPUSummary{l4, l4}); !got.Fits {
		t.Errorf("a 262,144-token model on two L4s: %+v, want recommended as before", got)
	}
}
