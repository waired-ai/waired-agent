package scoring

import "math"

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
func KVBytesPerTokenFP16(fullAttnLayers, nKVHeads, headDim int) int {
	if fullAttnLayers <= 0 || nKVHeads <= 0 || headDim <= 0 {
		return 0
	}
	return 2 * fullAttnLayers * nKVHeads * headDim * bytesKVFP16
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
func MaxContextTokens(weightGB float64, kvBytesPerTokFP16 int, kvFactor, budgetGB float64) int {
	if weightGB <= 0 || kvBytesPerTokFP16 <= 0 || kvFactor <= 0 || budgetGB <= 0 {
		return 0
	}
	leftoverGB := budgetGB - weightGB
	if leftoverGB <= 0 {
		return 0
	}
	tokens := leftoverGB * 1e9 / (float64(kvBytesPerTokFP16) * kvFactor)
	return int(tokens/1024) * 1024
}
