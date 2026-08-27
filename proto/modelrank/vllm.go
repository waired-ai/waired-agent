package modelrank

import (
	"github.com/waired-ai/waired-agent/proto/hostfit"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// The vLLM sizing arithmetic MOVED to proto/hostfit (waired-agent#1061).
//
// It had to: hostfit.VLLMRecommendModel could not ask "would this host
// actually serve the coding window" — the answer is VLLMMaxModelLen, and
// this package imports hostfit, not the other way round. So a vLLM row was
// demoted for a model whose own window is too small and NOT demoted for one
// this particular host would clamp below the coding window, while the agent
// knew and said so in its tuning warning.
//
// Everything below is a delegating wrapper, kept because these names are
// published proto API and the additive-only guard forbids removing them
// (scripts/ci/protoguard). New callers should use the hostfit spellings.
// The three consts stay written out rather than aliased for the same guard:
// it compares the const's VALUE AS WRITTEN, so `= hostfit.X` reads as a
// changed value even when the number is identical. TestVLLMConstsMatchHostfit
// pins the two copies together.

// KV-cache quantization factors relative to FP16.
//
// KVFactorFP8 is the vLLM `--kv-cache-dtype fp8` (e4m3) analogue of
// ollama's q8_0: 1 B/elem vs fp16's 2, so it halves KV. Kept a distinct
// name from the ollama spelling because the two are different formats on
// different engines (waired-agent#676).
//
// Mirrors hostfit.VLLMKVFactorF16 / hostfit.VLLMKVFactorFP8.
const (
	KVFactorF16 = 1.0
	KVFactorFP8 = 0.5
)

// DefaultVLLMGPUMemoryUtilization mirrors the agent config default for
// vllm_gpu_memory_utilization, and hostfit.DefaultVLLMGPUMemoryUtilization.
// Selection-time callers have no agent config in hand, so they size against
// the default; an operator's custom utilization affects serving only.
const DefaultVLLMGPUMemoryUtilization = 0.85

// VLLMTensorParallelSize is hostfit.VLLMTensorParallelSize.
func VLLMTensorParallelSize(gpus []signer.HardwareGPUSummary) int {
	return hostfit.VLLMTensorParallelSize(gpus)
}

// VLLMUsesFP8KV is hostfit.VLLMUsesFP8KV.
func VLLMUsesFP8KV(gpus []signer.HardwareGPUSummary) bool {
	return hostfit.VLLMUsesFP8KV(gpus)
}

// VLLMKVFactor is hostfit.VLLMKVFactor.
func VLLMKVFactor(gpus []signer.HardwareGPUSummary) float64 {
	return hostfit.VLLMKVFactor(gpus)
}

// VLLMVRAMBudgetMB is hostfit.VLLMVRAMBudgetMB.
func VLLMVRAMBudgetMB(host hostfit.Host, gpus []signer.HardwareGPUSummary) int {
	return hostfit.VLLMVRAMBudgetMB(host, gpus)
}

// VLLMMaxModelLen is hostfit.VLLMMaxModelLen.
func VLLMMaxModelLen(
	weightGB float64, kvBytesPerTokFP16, tp int, gpuMemUtil, kvFactor float64,
	gpus []signer.HardwareGPUSummary,
) int {
	return hostfit.VLLMMaxModelLen(weightGB, kvBytesPerTokFP16, tp, gpuMemUtil, kvFactor, gpus)
}
