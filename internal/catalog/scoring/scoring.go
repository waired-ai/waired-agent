package scoring

import (
	"math"

	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// weightOverhead is the +15% activation/buffer/framework-state allowance the
// scoring report §2.4 adds on top of raw weight to estimate live VRAM.
const weightOverhead = 1.15

// bytesKVFP16 is the per-element KV-cache size in bytes for FP16/BF16 KV
// (scoring report §2.2: bytes_kv = 2).
const bytesKVFP16 = 2

// KVBytesPerTokenFP16 returns the per-token KV-cache footprint in bytes,
// assuming FP16 KV, after the hybrid-mamba / sliding-window correction:
//
//	kv_bytes_per_tok = 2 × n_full_attn_layers × n_kv_heads × head_dim × bytes_kv
//
// Only full-attention layers are counted — linear/Mamba layers carry constant
// state (independent of context) and sliding-window layers cap their KV at the
// window, both negligible per-token contributions (scoring report §2.2).
//
// This is the ATTENTION cache alone. A model may carry a second per-token
// cache beside it; KVBytesPerTokenFP16ForConfig is the total, and is what a
// manifest's kv_bytes_per_token_fp16 should carry.
func KVBytesPerTokenFP16(fullAttnLayers, nKVHeads, headDim int) int {
	if fullAttnLayers <= 0 || nKVHeads <= 0 || headDim <= 0 {
		return 0
	}
	return 2 * fullAttnLayers * nKVHeads * headDim * bytesKVFP16
}

// indexerKeyBytesPerTokenFP16 returns the per-token cost of a block-sparse
// attention indexer's KEY cache:
//
//	idx_bytes_per_tok = n_full_attn_layers × indexer_kv_heads × indexer_head_dim × bytes_kv
//
// There is no ×2 for K+V because there is no V. The model carries no value
// projection for the indexer — Qwen3.8-Flash-Next's layer struct has
// index_q_proj, index_k_proj, index_q_norm and index_k_norm and nothing else
// — and the graph issues only cpy_k/get_k on that cache.
//
// llama.cpp allocates a V half anyway: the indexer reuses the general
// llama_kv_cache, which sets has_v = !is_mla, and the indexer is not MLA. Its
// constructor overrides only the KEY width, so the V half is allocated at the
// MODEL's head_dim rather than the indexer's — 6144 B/token on Flash-Next,
// 1,536 MiB at a 262,144 window, never written and never read. That is
// ggml-org/llama.cpp#28330, open at b10760 (the version 0.33.3 vendors),
// which is why it is not modelled here: it is an upstream over-allocation
// rather than a cost of the model, and annotating around it would bake the
// bug into the catalog and then be wrong again when the fix lands.
//
// The indexer runs on the same layers as full attention, so the caller's
// fullAttnLayers is the right multiplier (llama.cpp builds its layer filter
// from the same predicate: "QSA runs on the dense-attention layers only").
func indexerKeyBytesPerTokenFP16(fullAttnLayers, indexerKVHeads, indexerHeadDim int) int {
	if fullAttnLayers <= 0 || indexerKVHeads <= 0 || indexerHeadDim <= 0 {
		return 0
	}
	return fullAttnLayers * indexerKVHeads * indexerHeadDim * bytesKVFP16
}

// KVBytesPerTokenFP16ForConfig returns the TOTAL per-token KV footprint for a
// config in bytes at FP16: the attention cache, plus a block-sparse indexer's
// key cache when the model has one.
//
// The caller resolves fullAttnLayers and headDim itself (FullAttnLayers,
// ResolvedHeadDim) because it also has to report whether either had to be
// inferred. The sum lives here so the two terms cannot drift apart across the
// several places that compute a manifest's kv_bytes_per_token_fp16.
//
// Models without an indexer are unaffected: the second term is zero whenever
// the config declares no indexer_kv_heads / indexer_head_dim, which is every
// entry in the catalog except qwen3.8-flash-next.
func KVBytesPerTokenFP16ForConfig(c ArchConfig, fullAttnLayers, headDim int) int {
	return KVBytesPerTokenFP16(fullAttnLayers, c.NumKeyValueHeads, headDim) +
		indexerKeyBytesPerTokenFP16(fullAttnLayers, c.IndexerKVHeads, c.IndexerHeadDim)
}

// WeightGB returns the estimated quantized weight size in GB (decimal,
// /1e9) for a model of totalParams parameters at quant q:
//
//	weight_GB = total_params × bpw / 8 / 1e9
//
// For MoE models pass the TOTAL parameter count (all experts are resident in
// memory; only compute scales with active params — scoring report §2.4).
//
// NOTE: for partially-quantized formats (AWQ / GPTQ / MXFP4) this is a LOWER
// BOUND. Embeddings, attention, router and lm_head weights stay at higher
// precision, so real on-disk size runs higher (e.g. gpt-oss-20b MXFP4: formula
// ~11.1 GB vs ~14 GB on disk). Prefer a measured artifact size when available;
// compute() emits a warning for these formats.
func WeightGB(totalParams int64, q Quant) float64 {
	if totalParams <= 0 || q.BPW <= 0 {
		return 0
	}
	return float64(totalParams) * q.BPW / 8.0 / 1e9
}

// KVGB returns the KV-cache size in GB for a given per-token footprint and
// context length: kv_GB(L) = kv_bytes_per_tok × L / 1e9 (scoring report §2.4).
func KVGB(kvBytesPerTok, contextLen int) float64 {
	if kvBytesPerTok <= 0 || contextLen <= 0 {
		return 0
	}
	return float64(kvBytesPerTok) * float64(contextLen) / 1e9
}

// VRAMGB returns the estimated live VRAM at context length contextLen:
//
//	VRAM_GB(L) = weight_GB × 1.15 + kv_GB(L)
//
// (scoring report §2.4). weightGB is the value from WeightGB (or a measured
// override).
func VRAMGB(weightGB float64, kvBytesPerTok, contextLen int) float64 {
	return weightGB*weightOverhead + KVGB(kvBytesPerTok, contextLen)
}

// DecodeFLOPsPerTok returns the batch-1, KV-hit decode FLOPs per token:
// 2 × active_params (scoring report §2.1). For dense models active==total.
func DecodeFLOPsPerTok(activeParams int64) int64 {
	if activeParams <= 0 {
		return 0
	}
	return 2 * activeParams
}

// SuggestMinVRAMMB suggests a min_vram_mb threshold (vLLM/GPU runtimes) for the
// given VRAM-at-context estimate, rounded UP to the next 1 GB boundary then
// expressed in MB, to leave headroom. It is a suggestion the manifest author
// reviews against the full VRAM curve, not a hard truth.
func SuggestMinVRAMMB(vramGB float64) int {
	if vramGB <= 0 {
		return 0
	}
	return int(math.Ceil(vramGB)) * 1024
}

// SuggestMinRAMGB suggests a min_ram_gb threshold (Ollama/CPU runtimes): VRAM
// at context plus a 2 GB OS/runtime headroom, rounded UP to the next whole GB.
func SuggestMinRAMGB(vramGB float64) int {
	if vramGB <= 0 {
		return 0
	}
	return int(math.Ceil(vramGB + 2))
}

// KV-cache quantization factors relative to FP16, matching Ollama's
// OLLAMA_KV_CACHE_TYPE options (f16 / q8_0 / q4_0). q8_0 is near-lossless
// and halves the KV footprint; q4_0 quarters it but degrades long-context
// recall.
//
// KVFactorFP8 is the vLLM `--kv-cache-dtype fp8` (e4m3) analogue: 1 B/elem
// vs fp16's 2, so it halves KV just like q8_0. It is numerically equal to
// KVFactorQ8_0 but kept a distinct name because the two are different
// formats on different engines (#676).
const (
	KVFactorF16  = 1.0
	KVFactorQ8_0 = 0.5
	KVFactorFP8  = 0.5
	KVFactorQ4_0 = 0.25
)

// MaxContextTokens returns the largest context length L such that
//
//	weight_GB + kv_bytes_per_tok_fp16 × kvFactor × L / 1e9 ≤ budget_GB
//
// i.e. the biggest window whose weights + KV cache fit the given memory
// budget without spilling. Weights are counted RAW (no ×1.15) because this
// pairs with an engine-overhead reservation the caller already subtracted
// from the budget (router.OllamaVRAMOverheadMB) — the same convention the
// router's ollamaFitsVRAM gate was calibrated with; applying both would
// double-count the overhead. The result is rounded DOWN to a multiple of
// 1024 so the exported engine setting stays tidy and slightly conservative.
// Returns 0 when the weights alone don't fit or any input is unknown
// (non-positive). Callers cap the result at the manifest context_length.
//
// The formula moved to proto/hostfit when the window sizing became a
// shared decision: the control plane has to reach the same answer about a
// host as the host does (waired-ai/waired#1056 decision 3). This stays as
// the catalog-scoring spelling of it.
func MaxContextTokens(weightGB float64, kvBytesPerTokFP16 int, kvFactor, budgetGB float64) int {
	return hostfit.MaxContextTokens(weightGB, kvBytesPerTokFP16, kvFactor, budgetGB)
}
