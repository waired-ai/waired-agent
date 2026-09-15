package hostfit

import (
	"slices"

	"github.com/waired-ai/waired-agent/proto/catalog"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// The KV-cache type ladders, lowest precision first. The owner decision
// (decision 2 of
// docs/decisions/20260913/2355-catalog-variant-kv-and-residency-rulings.md)
// is "q4_0 by default; where q4_0 cannot be used, the smallest type above
// it", so the first type a host and a build both allow is the default,
// and a type a user asked for that is not allowed falls to that same
// first rung.
var (
	ollamaKVLadder = []string{catalog.KVCacheQ4_0, catalog.KVCacheQ8_0, catalog.KVCacheF16}
	vllmKVLadder   = []string{catalog.KVCacheFP8, catalog.KVCacheFP16}
)

// OllamaDefaultKVCacheType is the KV-cache type the serve tuning exports
// when nothing pins one. The recommendation and the capacity gate price
// the cache with it, because the recommendation reads the type the tuning
// actually serves (decision 1 of
// docs/decisions/20260913/2355-catalog-variant-kv-and-residency-rulings.md).
// It is q8_0 today; waired-ai/waired-agent#1348 moves the default to q4_0,
// and that change belongs here and in the tuning together. The build-aware
// answer the tuning will read, KVCacheChoices and ResolveKVCacheType below,
// lands first and is not yet what anything prices with.
//
// The host is a parameter although nothing reads it yet: the default
// ladder (#1348) depends on the hardware, and the signature is published.
func OllamaDefaultKVCacheType(_ Host) string {
	return catalog.KVCacheQ8_0
}

// KVCacheChoices lists the KV-cache types a user may choose for variant v
// served by engine on this host, lowest precision first — the order the
// default is taken from. It is the one answer a console greys its KV
// buttons by and the serve tuning resolves a request with, so the two
// cannot disagree (waired-ai/waired-agent#1348, waired-ai/waired#1387).
//
// The set is the engine's ladder narrowed three ways:
//
//   - by the build: v.KVCacheTypes; an empty list is what the engines
//     served before the list existed (f16 and q8_0 on ollama, fp16 and
//     fp8 on vLLM);
//   - by the host on ollama: without GPU-addressable memory only f16;
//   - by the GPU on vLLM: fp8 only where VLLMUsesFP8KV (Ada and newer).
//
// The unquantised type (f16 / fp16) is always offered: it is what an
// engine falls back to when a quantised cache cannot start, so no list
// can take it away. Unknown engines get nil.
func KVCacheChoices(engine string, v catalog.Variant, h Host, gpus []signer.HardwareGPUSummary) []string {
	var ladder, unset []string
	var full string
	switch engine {
	case catalog.RuntimeOllama:
		if !h.HasGPU() {
			return []string{catalog.KVCacheF16}
		}
		ladder, full = ollamaKVLadder, catalog.KVCacheF16
		unset = []string{catalog.KVCacheQ8_0, catalog.KVCacheF16}
	case catalog.RuntimeVLLM:
		ladder, full = vllmKVLadder, catalog.KVCacheFP16
		unset = []string{catalog.KVCacheFP8, catalog.KVCacheFP16}
	default:
		return nil
	}
	allowed := v.KVCacheTypes
	if len(allowed) == 0 {
		allowed = unset
	}
	out := make([]string, 0, len(ladder))
	for _, t := range ladder {
		if t != full && !slices.Contains(allowed, t) {
			continue
		}
		if t == catalog.KVCacheFP8 && !VLLMUsesFP8KV(gpus) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// ResolveKVCacheType is the KV-cache type variant v is served with on this
// host when want was asked for: want itself if KVCacheChoices allows it,
// otherwise the first allowed rung — the default. An empty want is "no
// instruction" and resolves to that default. Unknown engines get "".
func ResolveKVCacheType(engine string, v catalog.Variant, h Host, gpus []signer.HardwareGPUSummary, want string) string {
	choices := KVCacheChoices(engine, v, h, gpus)
	if len(choices) == 0 {
		return ""
	}
	if want != "" && slices.Contains(choices, want) {
		return want
	}
	return choices[0]
}
