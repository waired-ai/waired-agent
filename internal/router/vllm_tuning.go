package router

import (
	"strconv"
	"strings"

	"github.com/waired-ai/waired-agent/internal/catalog/scoring"
	"github.com/waired-ai/waired-agent/internal/hardware"
)

// VLLMTensorParallelSize returns the --tensor-parallel-size the agent
// should pass to vLLM on this host: the largest power of two not
// exceeding the number of identical NVIDIA GPUs, and 1 whenever the
// host has zero or one NVIDIA GPU or a heterogeneous mix.
//
// vLLM is the engine we reserve for NVIDIA multi-parallel tiers
// (decisions.md "推論エンジンを Ollama に集約し vLLM を NVIDIA 高並列
// ティアに限定する", 20260527), so multi-GPU hosts should shard by
// default instead of leaving every device past GPUs[0] idle.
//
// Identical means the same (Model, VRAMTotalMB) pair: tensor parallelism
// splits weights and KV evenly, so a mixed pool runs every shard at the
// slowest/smallest device and can OOM the small one — the same marketing
// name with different VRAM (RTX 3080 10G vs 12G) is still a mismatch.
// The power-of-two constraint is the safe universal choice: vLLM
// requires the attention-head count to be divisible by the TP size, and
// every head count in the catalog divides by 2/4/8 while odd sizes
// (3, 5, 6, 7) routinely fail.
func VLLMTensorParallelSize(hw hardware.Profile) int {
	var count int
	var first hardware.GPU
	for _, g := range hw.GPUs {
		if g.Vendor != "nvidia" {
			continue
		}
		if count == 0 {
			first = g
		} else if g.Model != first.Model || g.VRAMTotalMB != first.VRAMTotalMB {
			return 1
		}
		count++
	}
	if count <= 1 {
		return 1
	}
	tp := 1
	for tp*2 <= count {
		tp *= 2
	}
	return tp
}

const (
	// vllmPerGPUOverheadMB is the per-device non-torch reservation: the
	// CUDA context plus (under TP) the NCCL communicator buffers, which
	// repeat on every device. vLLM's memory profiler charges non-torch
	// memory AGAINST the --gpu-memory-utilization fraction (measured on
	// L4/cu129/vLLM 0.24: util×VRAM 13.28 GB, weights+activation
	// ~11.4 GB, engine-reported available KV only 1.13 GiB — the
	// ~0.69 GB gap is the CUDA context), so both the TP>1 selection
	// aggregate and the max-model-len budget subtract it per device.
	// 1024 MiB errs high (measured ~700 MiB single-GPU; NCCL adds more
	// under TP, unmeasured — dev-waired's GPU quota is 1 device, #686):
	// too large only under-sizes, too small recommends a model whose
	// weights don't fit, which the serve-time clamp cannot repair.
	vllmPerGPUOverheadMB = 1024

	// vllmWeightOverhead pads raw weights with the +15% activation /
	// framework-state allowance (scoring report §2.4, same figure as
	// scoring.VRAMGB) when sizing against the utilization budget —
	// vLLM's profiler charges the activation peak inside the
	// utilization fraction too. Unlike the ollama tuning (flat engine
	// overhead subtracted from the budget, weights counted raw), the
	// vLLM convention is weights ×1.15 plus the flat per-GPU non-torch
	// reservation above. Calibration point (L4, qwen3-14b-awq,
	// util=0.55): formula 4096 tokens vs engine-measured 7392 — the
	// flat term dominates at low utilization, at the 0.85 default the
	// margin is ~5%. The engine.log KV-capacity read-back (#675)
	// records the per-host truth.
	vllmWeightOverhead = 1.15

	// DefaultVLLMGPUMemoryUtilization mirrors agentconfig's default for
	// vllm_gpu_memory_utilization. Selection-time callers (the #624
	// context-floor gate) have no agent config in hand, so they size
	// against the default; an operator's custom utilization affects
	// serving only. The same asymmetry applies to fp8 KV
	// (VLLMUsesFP8KV / vllm_disable_fp8_kv): selection sizes against the
	// Ada+ default-on, an operator opt-out affects serving only.
	DefaultVLLMGPUMemoryUtilization = 0.85

	// vllmFP8KVMinComputeCap is the CUDA compute capability at/above
	// which vLLM's `--kv-cache-dtype fp8` (e4m3) has hardware support:
	// 8.9 = Ada Lovelace (L4, RTX 40xx); Hopper (9.0) and Blackwell
	// (10.x/12.x) are higher. Below this (Ampere 8.0/8.6 and older) fp8
	// KV either falls back to an emulated path or runs without a matching
	// attention kernel, so KV stays fp16 (#676).
	vllmFP8KVMinComputeCap = 8.9
)

// parseComputeCap parses an nvidia-smi compute-capability string ("8.9")
// into a float. ok is false for empty or malformed values, which the
// fp8 gate treats as "no fp8" (fail-safe: non-CUDA and pre-detection
// GPUs leave GPU.ComputeCap empty).
func parseComputeCap(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// VLLMUsesFP8KV reports whether the vLLM serve path runs KV cache in fp8
// (e4m3) on this host by default: every NVIDIA serving GPU parses a
// compute capability ≥ vllmFP8KVMinComputeCap (Ada/Hopper+). It fails
// closed — a single sub-Ada or unknown-capability NVIDIA GPU, or a host
// with no NVIDIA GPU at all, returns false so fp8 is never forced onto
// hardware that cannot accelerate it. An operator opt-out
// (vllm_disable_fp8_kv) is applied by the caller, not here; selection
// sizing calls this directly to size against the Ada+ default.
func VLLMUsesFP8KV(hw hardware.Profile) bool {
	seen := false
	for _, g := range hw.GPUs {
		if g.Vendor != "nvidia" {
			continue
		}
		seen = true
		cap, ok := parseComputeCap(g.ComputeCap)
		if !ok || cap < vllmFP8KVMinComputeCap {
			return false
		}
	}
	return seen
}

// VLLMKVFactor is the scoring KV factor the vLLM sizing math should use
// on this host by default: KVFactorFP8 (0.5) when VLLMUsesFP8KV, else
// KVFactorF16 (1.0). Selection surfaces (VLLMServesContextFloor) use it
// directly; the serve path derives its own factor from the same gate
// plus the operator opt-out.
func VLLMKVFactor(hw hardware.Profile) float64 {
	if VLLMUsesFP8KV(hw) {
		return scoring.KVFactorFP8
	}
	return scoring.KVFactorF16
}

// VLLMVRAMBudgetMB is the VRAM budget (MB) model selection compares
// against Variant.MinVRAMMB on the vLLM path (#678). With tensor
// parallelism (VLLMTensorParallelSize > 1) weights and KV shard evenly
// across the identical devices, so the budget aggregates them, minus a
// per-device overhead reservation:
//
//	TP × (perGPU_VRAMTotalMB − vllmPerGPUOverheadMB)
//
// At TP == 1 it returns Profile.EffectiveVRAMMB() unchanged — MinVRAMMB
// thresholds were authored against raw single-GPU totals, so the
// single-GPU behaviour must stay bit-identical (no overhead deduction).
// The aggregate never drops below that single-GPU figure either: TP=1
// serving is always available, so a degenerate aggregate (overhead
// swallowing tiny devices) must not shrink the budget.
//
// Selection always uses the AUTO tensor-parallel rule; an operator's
// vllm_tensor_parallel override affects serving only (recommendation
// surfaces — CLI init, catalog UI, FamilyBestFit — don't have agent
// config in hand).
func VLLMVRAMBudgetMB(hw hardware.Profile) int {
	single := hw.EffectiveVRAMMB()
	tp := VLLMTensorParallelSize(hw)
	if tp <= 1 {
		return single
	}
	for _, g := range hw.GPUs {
		if g.Vendor != "nvidia" {
			continue
		}
		// TP > 1 guarantees the NVIDIA devices are identical; the first
		// one is representative.
		if agg := tp * (g.VRAMTotalMB - vllmPerGPUOverheadMB); agg > single {
			return agg
		}
		return single
	}
	return single
}

// VLLMMaxModelLen returns the largest --max-model-len whose KV cache
// fits alongside the activation-padded weights within the vLLM memory
// budget (#675):
//
//	budget_GB = tp × (gpuMemUtil × perGPU_VRAM_GB − perGPU_overhead_GB)
//	max_len   = largest L with weightGB×1.15 + kv_bytes×kvFactor×L/1e9 ≤ budget_GB
//
// The per-GPU overhead is subtracted INSIDE the utilization fraction —
// vLLM's profiler charges non-torch memory (CUDA context, NCCL)
// against it (see vllmPerGPUOverheadMB).
//
// tp is the RESOLVED tensor-parallel size (operator override included —
// callers must not re-derive it from hw, an override changes the shard
// budget). perGPU VRAM is the smallest VRAMTotalMB among the first tp
// NVIDIA devices, conservative under a clamped heterogeneous override.
// kvFactor scales the fp16 per-token KV bytes for the serving KV dtype
// (scoring.KVFactorF16 for `--kv-cache-dtype auto`, scoring.KVFactorFP8
// for fp8 e4m3 on Ada+ — #676). Callers pass it explicitly (like tp and
// gpuMemUtil): serving must reflect the operator opt-out, selection
// passes VLLMKVFactor(hw), the Ada+ default.
//
// Returns 0 when any sizing input is unknown or the padded weights
// alone exceed the budget; callers then keep the manifest window
// (pre-#675 behaviour). The result is 1024-aligned via
// scoring.MaxContextTokens; note the weights are pre-padded ×1.15 here
// (see vllmWeightOverhead) whereas that helper's ollama callers pass
// them raw against an overhead-reduced budget.
func VLLMMaxModelLen(weightGB float64, kvBytesPerTokFP16 int, tp int, gpuMemUtil float64, kvFactor float64, hw hardware.Profile) int {
	if weightGB <= 0 || kvBytesPerTokFP16 <= 0 || gpuMemUtil <= 0 || kvFactor <= 0 {
		return 0
	}
	if tp < 1 {
		tp = 1
	}
	perGPU, seen := 0, 0
	for _, g := range hw.GPUs {
		if g.Vendor != "nvidia" || seen >= tp {
			continue
		}
		if perGPU == 0 || g.VRAMTotalMB < perGPU {
			perGPU = g.VRAMTotalMB
		}
		seen++
	}
	if perGPU <= 0 {
		return 0
	}
	const mib = float64(1 << 20)
	perGPUBudgetGB := (gpuMemUtil*float64(perGPU) - vllmPerGPUOverheadMB) * mib / 1e9
	if perGPUBudgetGB <= 0 {
		return 0
	}
	budgetGB := float64(tp) * perGPUBudgetGB
	return scoring.MaxContextTokens(weightGB*vllmWeightOverhead, kvBytesPerTokFP16, kvFactor, budgetGB)
}
