package router

import (
	"fmt"
	"math"
	"strings"

	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/proto/modelrank"
)

// VLLMTensorParallelSize returns the --tensor-parallel-size the agent
// should pass to vLLM on this host: the largest power of two not
// exceeding the number of identical NVIDIA GPUs, and 1 whenever the
// host has zero or one NVIDIA GPU or a heterogeneous mix.
//
// vLLM is the engine we reserve for NVIDIA multi-parallel tiers
// (waired/docs/decisions/ "推論エンジンを Ollama に集約し vLLM を NVIDIA 高並列
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
	return modelrank.VLLMTensorParallelSize(hw.GPUSummaries())
}

const (

	// DefaultVLLMGPUMemoryUtilization mirrors agentconfig's default for
	// vllm_gpu_memory_utilization. Selection-time callers (the #624
	// context-floor gate) have no agent config in hand, so they size
	// against the default; an operator's custom utilization affects
	// serving only. The same asymmetry applies to fp8 KV
	// (VLLMUsesFP8KV / vllm_disable_fp8_kv): selection sizes against the
	// Ada+ default-on, an operator opt-out affects serving only.
	DefaultVLLMGPUMemoryUtilization = 0.85
)

// VLLMUsesFP8KV reports whether the vLLM serve path runs KV cache in fp8
// (e4m3) on this host by default: every NVIDIA serving GPU parses a
// compute capability ≥ vllmFP8KVMinComputeCap (Ada/Hopper+). It fails
// closed — a single sub-Ada or unknown-capability NVIDIA GPU, or a host
// with no NVIDIA GPU at all, returns false so fp8 is never forced onto
// hardware that cannot accelerate it. An operator opt-out
// (vllm_disable_fp8_kv) is applied by the caller, not here; selection
// sizing calls this directly to size against the Ada+ default.
func VLLMUsesFP8KV(hw hardware.Profile) bool {
	return modelrank.VLLMUsesFP8KV(hw.GPUSummaries())
}

// VLLMKVFactor is the scoring KV factor the vLLM sizing math should use
// on this host by default: KVFactorFP8 (0.5) when VLLMUsesFP8KV, else
// KVFactorF16 (1.0). Selection surfaces (VLLMServesContextFloor) use it
// directly; the serve path derives its own factor from the same gate
// plus the operator opt-out.
func VLLMKVFactor(hw hardware.Profile) float64 {
	return modelrank.VLLMKVFactor(hw.GPUSummaries())
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
	return modelrank.VLLMVRAMBudgetMB(hw.HostFit(), hw.GPUSummaries())
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
	return modelrank.VLLMMaxModelLen(weightGB, kvBytesPerTokFP16, tp, gpuMemUtil, kvFactor, hw.GPUSummaries())
}

// --- vLLM serve flags (waired-agent#887) ---
//
// Moved here from cmd/waired-agent so the GPU e2e lane can call the same
// derivations the daemon calls rather than re-typing their numbers. In
// package main they were unreachable from any test outside that binary,
// which is why internal/e2e/inference built its own VLLMConfig, left both
// settings at zero, and ran the engine on vLLM's own defaults for the whole
// life of the feature (waired-agent#955).

// Prefill chunking (waired-agent#887).
//
// vLLM prefills a prompt in scheduler steps of max_num_batched_tokens,
// and its own default for the OpenAI API server is 2048 on every GPU
// under 70 GiB and 8192 above (arg_utils.py, still true at 0.28.0).
// Every card waired can serve on is under that line, so a 30k-token
// coding-agent prompt is ~15 sequential passes on a value nobody chose
// — upstream's
// smaller default exists to protect aggregate throughput on A100-class
// cards, which is not the profile a single developer's agent presents.
//
// 4096 rather than 8192 because the cost is not free and does not land
// where it looks. vLLM V1 profiles peak activation with a dummy forward
// pass sized at this value and subtracts that peak from the
// gpu-memory-utilization budget BEFORE sizing the KV pool, so raising
// the chunk shrinks the pool; if the pool then holds fewer tokens than
// --max-model-len, the engine aborts at start-up rather than degrading.
// vllmWeightOverhead (1.15) is what absorbs activation memory in
// the #675 sizing and was calibrated against today's chunk with a thin
// margin. Doubling stays inside that margin while still halving the
// number of passes; quadrupling is a measurement away, not an argument
// away.
const (
	vllmDefaultBatchedTokens = 4096
	vllmBigGPUBatchedTokens  = 8192
	// vllmBigGPUVRAMMB mirrors upstream's own 70 GiB threshold. Below
	// it upstream picks 2048 and we raise; above it upstream already
	// picks 8192, and passing a flat 4096 there would LOWER the chunk —
	// a regression introduced by a performance change.
	//
	// Re-read against the 0.28.0 pin: the sub-70 GiB default is still
	// 2048 and the 70 GiB branch still 8192, so both halves of this
	// constant still say what they claim. Upstream did grow a THIRD tier
	// above it — >= 160 GiB (B200/B300 class) now defaults to 16384 —
	// where passing 8192 would lower the chunk the way this comment warns
	// about. No card in the catalog is there, so no new rung is added
	// until one is; the moment a >= 160 GiB host appears, this needs a
	// third step rather than a wider top one.
	vllmBigGPUVRAMMB = 70 * 1024
	// vllmMinBatchedTokens is vLLM's own max_num_seqs default, which
	// config/scheduler.py requires max_num_batched_tokens to reach or
	// exceed (it raises a ValueError otherwise).
	//
	// 256 confirmed against the 0.28.0 pin as installed, not from prose:
	// arg_utils.py's tier table gives OPENAI_API_SERVER 256 for every
	// card under 70 GiB (1024 above). Later upstream V1 documentation
	// says 1024 unconditionally, which is the claim #1126 needed settled
	// — it is wrong for the cards this product serves on.
	vllmMinBatchedTokens = 256
)

// VLLMMaxNumBatchedTokens returns the value to pass, or 0 to omit the
// flag entirely. override wins when set; otherwise the value is derived
// from the smallest serving GPU and clamped to maxModelLen, because a
// chunk larger than the window is budget spent on a batch that can never
// be filled.
func VLLMMaxNumBatchedTokens(maxModelLen int, hw hardware.Profile, override int) int {
	if override > 0 {
		return override
	}
	want := vllmDefaultBatchedTokens
	if smallestServingGPUVRAMMB(hw) >= vllmBigGPUVRAMMB {
		want = vllmBigGPUBatchedTokens
	}
	if maxModelLen > 0 && maxModelLen < want {
		want = maxModelLen
	}
	if want < vllmMinBatchedTokens {
		want = vllmMinBatchedTokens
	}
	return want
}

// smallestServingGPUVRAMMB is the smallest NVIDIA card's VRAM, or 0 when
// none is visible. The smallest rather than the first: tensor
// parallelism spreads one model across all of them, so the tightest card
// is what the budget has to fit.
func smallestServingGPUVRAMMB(hw hardware.Profile) int {
	smallest := 0
	for _, g := range hw.GPUs {
		if !strings.EqualFold(g.Vendor, "nvidia") || g.VRAMTotalMB <= 0 {
			continue
		}
		if smallest == 0 || g.VRAMTotalMB < smallest {
			smallest = g.VRAMTotalMB
		}
	}
	return smallest
}

// KV offloading (waired-agent#887).
//
// vLLM's native backend spills evicted KV blocks to HOST RAM inside the
// engine process. It is not persistence: it does not reach disk and does
// not survive a restart (only the lmcache backend reaches disk, and that
// wheel is not in the pin set). What it buys is the case where another
// conversation evicts your prefix from the GPU pool — measured on the
// other engine as a full re-prefill.
//
// Opt-in, because it spends whole GiB of a machine that is usually also
// somebody's workstation, no fleet host runs vLLM yet, and the only
// measurement waired has of "spend host RAM to keep prefixes warm" is a
// null result (waired-agent#866 / #883).
const (
	// vllmKVOffloadRAMShare caps the buffer at a quarter of the host's
	// standing available-memory figure. A share rather than a constant
	// because the same request is reasonable on a 128 GB server and
	// reckless on a 16 GB laptop.
	vllmKVOffloadRAMShare = 4
	// vllmKVOffloadMinGiB is the smallest buffer worth allocating; a
	// fraction of a GiB holds too little of a coding-agent prefix to
	// change an outcome.
	vllmKVOffloadMinGiB = 1.0
)

// VLLMKVOffloadingGiB clamps an operator's requested buffer against host
// RAM. Returns the value to pass (0 = omit the flags) and a non-empty
// note when the request was not honoured verbatim, so the reason reaches
// the log rather than being silently absorbed.
func VLLMKVOffloadingGiB(request float64, hw hardware.Profile) (float64, string) {
	if request <= 0 {
		return 0, ""
	}
	// The standing figure, not the live one: a live reading counts the
	// resident model against the host serving it (profiler.go).
	ramGB := hw.RAMAvailableAtInstallGB
	if ramGB <= 0 {
		ramGB = hw.RAMTotalGB
	}
	if ramGB <= 0 {
		return 0, "no host RAM measurement; KV offloading not enabled"
	}
	ceiling := float64(ramGB) / vllmKVOffloadRAMShare
	if request > ceiling {
		return roundedGiB(ceiling), fmt.Sprintf(
			"KV offloading buffer clamped from %.1f to %.1f GiB (a quarter of %d GB host RAM)",
			request, roundedGiB(ceiling), ramGB)
	}
	if request < vllmKVOffloadMinGiB {
		return 0, fmt.Sprintf(
			"KV offloading buffer of %.1f GiB is below the %.1f GiB floor; not enabled",
			request, vllmKVOffloadMinGiB)
	}
	return request, ""
}

// roundedGiB trims a computed ceiling to one decimal so the argv and the
// log agree with what an operator would read back.
func roundedGiB(v float64) float64 {
	return math.Floor(v*10) / 10
}
