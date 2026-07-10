// #675: vLLM context/VRAM sizing — the vLLM counterpart of the ollama
// serve tuning (inference_ollama_tuning.go). vLLM sizes its KV-cache
// pool from --gpu-memory-utilization after loading the weights and
// ABORTS startup when the pool cannot hold one --max-model-len request;
// there is no ollama-style spill degradation. So instead of forwarding
// the manifest window verbatim, compute the largest window the budget
// fits (router.VLLMMaxModelLen) and clamp, with a user-visible warning
// on the same ModelTuning surface the ollama tuning uses.
//
// Kept free of build tags so the sizing logic is unit-tested on every
// platform even though only the linux vLLM path calls it.
package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/catalog/scoring"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/router"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// computeVLLMTuning sizes --max-model-len for one (manifest, variant,
// host, tp, util) combination. tp is the RESOLVED tensor-parallel size
// (resolveVLLMTensorParallel — operator override included). Returns the
// value to pass as VLLMConfig.MaxModelLen plus the ModelTuning record
// for the status/doctor surfaces.
//
// Unknown sizing inputs keep the manifest window with no warning
// (pre-#675 behaviour: never guess). Known inputs whose padded weights
// alone exceed the budget also keep the manifest window — a shorter
// window cannot save that case — but carry a startup-will-likely-fail
// warning so the abort is diagnosable before it happens.
func computeVLLMTuning(m catalog.Manifest, v catalog.Variant, hw hardware.Profile, tp int, gpuMemUtil float64, kvFactor float64) (int, infruntime.ModelTuning) {
	mt := infruntime.ModelTuning{ModelID: m.ModelID, VariantID: v.VariantID}
	est := router.VLLMMaxModelLen(v.EstimatedWeightGB, v.KVBytesPerTokenFP16, tp, gpuMemUtil, kvFactor, hw)
	if est <= 0 {
		mt.ContextLength = m.ContextLength
		if v.EstimatedWeightGB > 0 && v.KVBytesPerTokenFP16 > 0 && gpuMemUtil > 0 && hasNVIDIAGPU(hw) {
			// All inputs known ⇒ the padded weights alone overflow the
			// utilization budget.
			mt.Warning = fmt.Sprintf(
				"model weights (~%.1f GB plus activations) exceed the vLLM GPU memory budget at gpu-memory-utilization=%.2f, TP=%d; engine startup will likely fail — see engine.log",
				v.EstimatedWeightGB, gpuMemUtil, tp)
		}
		return m.ContextLength, mt
	}
	if m.ContextLength > 0 && est >= m.ContextLength {
		mt.ContextLength = m.ContextLength
		return m.ContextLength, mt
	}

	mt.ContextLength = est
	native := "unknown"
	if m.ContextLength > 0 {
		native = strconv.Itoa(m.ContextLength)
	}
	mt.Warning = fmt.Sprintf(
		"context window clamped to %d tokens (model native %s) so the KV cache fits GPU memory at gpu-memory-utilization=%.2f, TP=%d",
		est, native, gpuMemUtil, tp)
	if router.MeetsNativeContextFloor(m) && est < router.EffectiveContextFloor(m) {
		// Same tone as the ollama sub-floor note: informational — a
		// clamped window is a working configuration, not an error.
		mt.Warning += "; below the ~200k coding-agent context target — long sessions will truncate or compact"
	}
	return est, mt
}

func hasNVIDIAGPU(hw hardware.Profile) bool {
	for _, g := range hw.GPUs {
		if g.Vendor == "nvidia" {
			return true
		}
	}
	return false
}

// vllmKVCacheDType maps the resolved fp8 decision to the VLLMConfig
// KVCacheDType value: "fp8" (e4m3) when fp8 KV is engaged, else "" which
// omits --kv-cache-dtype and leaves vLLM's `auto` = model dtype (fp16).
// The decision (router.VLLMUsesFP8KV && !vllm_disable_fp8_kv) is made by
// the caller; this only formats it (#676).
func vllmKVCacheDType(useFP8 bool) string {
	if useFP8 {
		return "fp8"
	}
	return ""
}

// resolveVLLMKVCache decides the serving KV cache dtype for this host:
// fp8 (e4m3) when the GPUs support it (Ada+, router.VLLMUsesFP8KV) AND
// the operator has not opted out (vllm_disable_fp8_kv), else fp16
// (#676). It returns both the VLLMConfig.KVCacheDType string and the
// scoring KV factor the #675 max-model-len sizing must use so serving
// and sizing agree — an fp8 engine sized with an f16 factor would leave
// half its KV capacity unused; the reverse would abort at startup.
func resolveVLLMKVCache(hw hardware.Profile, disableFP8 bool) (kvCacheDType string, kvFactor float64) {
	if router.VLLMUsesFP8KV(hw) && !disableFP8 {
		return vllmKVCacheDType(true), scoring.KVFactorFP8
	}
	return vllmKVCacheDType(false), scoring.KVFactorF16
}

// vllmNgramSpeculativeConfig is the --speculative-config vLLM receives
// when vllm_speculative_ngram is enabled (#677). ngram (prompt-lookup)
// speculation needs no draft model — it proposes tokens by matching the
// recent context against earlier n-grams, a strong fit for coding where
// the model re-emits identifiers, imports and code already present in
// the prompt. num_speculative_tokens=5 with a 2–4 token match window is
// vLLM's documented starting point for single-stream decode; coding
// agents run effectively single-stream so the speculation rarely
// competes with batched requests.
const vllmNgramSpeculativeConfig = `{"method":"ngram","num_speculative_tokens":5,"prompt_lookup_max":4,"prompt_lookup_min":2}`

// vllmSpeculativeConfigJSON returns the VLLMConfig SpeculativeConfig
// value for the ngram toggle: the ngram config JSON when enabled, else
// "" which omits --speculative-config (no speculation).
func vllmSpeculativeConfigJSON(ngramEnabled bool) string {
	if ngramEnabled {
		return vllmNgramSpeculativeConfig
	}
	return ""
}

// vllmKVCapacityRe matches vLLM V1's post-profiling KV pool report,
// e.g. "GPU KV cache size: 152,192 tokens" (kv_cache_utils.py; the
// count carries thousands separators).
var vllmKVCapacityRe = regexp.MustCompile(`GPU KV cache size:\s*([0-9][0-9,]*)\s*tokens`)

// parseVLLMKVCapacityTokens extracts the engine-measured KV-cache
// capacity (tokens) from an engine.log, 0 when absent. The last
// occurrence wins: the log is truncated per spawn but a retry loop can
// write several startups into one file.
func parseVLLMKVCapacityTokens(log string) int {
	ms := vllmKVCapacityRe.FindAllStringSubmatch(log, -1)
	if len(ms) == 0 {
		return 0
	}
	n, err := strconv.Atoi(strings.ReplaceAll(ms[len(ms)-1][1], ",", ""))
	if err != nil {
		return 0
	}
	return n
}

// applyVLLMTuningVerification is the post-start read-back (the ollama
// /api/ps verify analogue): once the engine is ready, read the actual
// KV capacity it reported in engine.log and mark the tuning Verified.
// vLLM refuses to start when the pool is smaller than max-model-len, so
// a capacity below ContextLength should be impossible — flag it anyway
// rather than trust the estimate silently. An absent capacity line is
// inconclusive and changes nothing.
func applyVLLMTuningVerification(mt infruntime.ModelTuning, engineLog string) infruntime.ModelTuning {
	capacity := parseVLLMKVCapacityTokens(engineLog)
	if capacity <= 0 {
		return mt
	}
	mt.Verified = true
	if mt.ContextLength > 0 && capacity < mt.ContextLength {
		note := fmt.Sprintf("engine reports a KV cache of only %d tokens (below the exported %d-token window)", capacity, mt.ContextLength)
		if mt.Warning != "" {
			mt.Warning += "; " + note
		} else {
			mt.Warning = note
		}
	}
	return mt
}
