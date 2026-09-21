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
	"github.com/waired-ai/waired-agent/internal/version"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// computeVLLMTuning sizes --max-model-len for one (manifest, variant,
// host, tp, util) combination. tp is the RESOLVED tensor-parallel size
// (resolveVLLMTensorParallel — operator override included). Returns the
// value to pass as VLLMConfig.MaxModelLen plus the ModelTuning record
// for the status/doctor surfaces. spec is the speculative decoding the
// engine will run (router.VLLMSpeculative); an MTP draft's layers and
// memory come out of the window (waired-ai/waired#1432).
//
// The window is one of the two this product serves, and nothing between
// them (owner decision 2026-09-16, waired-agent#1396; #1434):
//
//   - 1,048,576 when the model's own window reaches it and the KV pool
//     holds it;
//   - 200,704 when the model's own window and the KV pool reach that;
//   - otherwise the pool's estimate, and the host cannot serve the model:
//     ModelTuning.WindowFits is false, the warning says so, and
//     DeclaredContextWindow declares nothing, so no Waired row routes
//     here. Recommendation does not pick such a build
//     (modelrank's vLLM window gate); this is the defensive answer for one
//     that got here anyway, an explicit pin or a changed GPU.
//
// It used to serve the model's native window whenever the pool covered it
// (262,144 for most models) and any 1024-aligned clamp below that, and the
// mesh saw those numbers as they were.
//
// Unknown sizing inputs serve 200,704 — capped at the model's own window
// for one below it, which only CI's internal_only model is — rather than
// the native window: never guess upward. Known inputs whose padded weights
// alone exceed the budget serve the same and carry a
// startup-will-likely-fail warning, so the abort is diagnosable before it
// happens.
func computeVLLMTuning(m catalog.Manifest, v catalog.Variant, hw hardware.Profile, tp int, gpuMemUtil float64, kvFactor float64, spec router.VLLMSpeculation) (int, infruntime.ModelTuning) {
	mt := infruntime.ModelTuning{ModelID: m.ModelID, VariantID: v.VariantID,
		SpeculativeMethod: spec.Method, SpeculativeTokens: spec.Tokens}
	est := router.VLLMMaxModelLenFor(v, spec.DraftTokens(), tp, gpuMemUtil, kvFactor, hw)
	if est <= 0 {
		// Unknown sizing inputs are not evidence against the host —
		// permissive, like VLLMServesContextFloor. The exception is the
		// warning branch: with every input known, est<=0 means the
		// padded weights alone overflow the budget.
		win := vllmUnsizedWindow(m)
		mt.ContextLength = win
		mt.WindowFits = true
		if v.EstimatedWeightGB > 0 && v.KVBytesPerTokenFP16 > 0 && gpuMemUtil > 0 && hasNVIDIAGPU(hw) {
			mt.WindowFits = false
			mt.WeightsOverBudget = true
			mt.Warning = fmt.Sprintf(
				"model weights (~%.1f GB plus activations) exceed the vLLM GPU memory budget at gpu-memory-utilization=%.2f, TP=%d; engine startup will likely fail — see engine.log",
				v.EstimatedWeightGB, gpuMemUtil, tp)
		}
		return win, mt
	}
	if win := vllmTierWindow(m, est); win > 0 {
		// A window the KV-pool arithmetic says this host holds (vLLM
		// clamps rather than spills), so a proven one.
		mt.ContextLength = win
		mt.WindowFits = true
		return win, mt
	}

	// The pool cannot hold the 200k session.
	mt.ContextLength = est
	mt.WindowFits = false
	mt.Warning = fmt.Sprintf(vllmBelowTierWarning, est, gpuMemUtil, tp)
	return est, mt
}

// vllmBelowTierWarning is the ModelTuning warning for a build whose KV pool
// holds less than the 200k session on this host. Shown as an engine notice
// on `waired status` and in `waired runtimes ls`. Written with the doc-writer
// skill (owner pre-approval, 2026-09-17).
const vllmBelowTierWarning = "KV cache holds a context window of %d tokens for this model at gpu-memory-utilization=%.2f, TP=%d, under the 200,704 a coding agent needs, so Waired won't send one to this computer — pick a smaller model or raise gpu-memory-utilization"

// vllmTierWindow is the tier this build serves given the KV pool's estimate:
// 1048576 or 200704, capped by the model's own window, or 0 when the pool
// cannot hold the model at 200704. A model whose own window is under 200704 —
// only CI's internal_only one — is served at its own window when the pool
// holds it (owner answer 2026-09-16 on waired-agent#1396).
func vllmTierWindow(m catalog.Manifest, est int) int {
	native := m.ContextLength
	switch {
	case native >= hostfit.ServingWindow1M && est >= hostfit.ServingWindow1M:
		return hostfit.ServingWindow1M
	case (native <= 0 || native >= hostfit.ServingWindow200k) && est >= hostfit.ServingWindow200k:
		return hostfit.ServingWindow200k
	case native > 0 && native < hostfit.ServingWindow200k && est >= native:
		return native
	}
	return 0
}

// vllmUnsizedWindow is the window a build is served at when the KV pool
// cannot be estimated: the 200k tier, or the model's own window when that is
// smaller, and 0 (the engine's default) when the manifest names none.
func vllmUnsizedWindow(m catalog.Manifest) int {
	if m.ContextLength > 0 && m.ContextLength < hostfit.ServingWindow200k {
		return m.ContextLength
	}
	if m.ContextLength <= 0 {
		return 0
	}
	return hostfit.ServingWindow200k
}

// vllmServeFlagsSupported reports whether the installed venv is new
// enough for the serve flags this build emits (waired-agent#885).
//
// The floor is VLLMPinnedVersion itself, not a per-flag introduction
// version. That is the one release whose flag set has been read and
// verified, and guessing when upstream added each flag is exactly the
// mistake this gate exists to prevent: vLLM exits with argparse code 2
// on an unrecognised flag, bootstrapVLLM's three retries then all fail,
// and the only trace is one log line saying local inference is
// unavailable until restart.
//
// Fails closed on an empty or unparseable version, the same rule
// router.engineVersionSatisfies applies to model floors: an engine whose
// version cannot be read is not evidence that it is current. This
// matters because Active() returns whatever venv the "current" symlink
// points at, which may have been installed by an older agent build.
func vllmServeFlagsSupported(activeVersion string) bool {
	if activeVersion == "" {
		return false
	}
	return version.AtLeast(activeVersion, infruntime.VLLMPinnedVersion)
}

// The vLLM serve-flag derivations (prefill chunking and KV offloading,
// waired-agent#887) live in internal/router alongside VLLMMaxModelLen and
// the rest of the sizing, because the GPU e2e lane has to call the SAME
// functions this daemon calls. They were unexported here in package main,
// which no test outside this binary can reach, so `make e2e-vllm` ran with
// vLLM's own defaults for the two settings #891 introduced and could not
// observe them at all (waired-agent#955).

// vllmStartupDiagnosis turns an engine log into a named cause and the
// setting to change, or "" when it recognises nothing (waired-agent#887).
//
// It exists because bootstrapVLLM's only report on a failed start is one
// line saying local inference is unavailable until restart: an
// unrecognised flag, a KV pool that did not fit, and an invalid
// tool-call parser all present identically. That was tolerable while
// nothing changed the argv; it stops being tolerable in the change that
// starts tuning memory.
//
// addr is the loopback address the engine was told to bind, for the arm
// that reports a busy port. It is passed in rather than parsed out of the
// log because the config is what the engine was TOLD, and the log line the
// port arm matches (a Python OSError) does not name the address at all.
//
// Deliberately silent on anything it does not recognise. A wrong hint on
// a startup failure is worse than none — it sends someone to change a
// setting that was never the problem.
//
// It reads ONE spawn's output, not the whole engine.log — see
// vllmStartupHint, which is what the bootstrap calls. engine.log now
// holds every attempt of a retry loop (waired-agent#878), and scanning
// all of them would let the first matching arm below win regardless of
// which attempt it came from: a transient first failure would outrank
// the reason the loop actually gave up on. A human reading the file
// still sees all three attempts, which is the point of keeping them.
func vllmStartupDiagnosis(engineLog, addr string) string {
	switch {
	case strings.Contains(engineLog, "unrecognized arguments"),
		strings.Contains(engineLog, "error: unrecognized"):
		return "the vLLM venv rejected a start-up flag, so it is probably older than this build expects" +
			" — run `waired runtimes install vllm` to rebuild it"
	case strings.Contains(engineLog, "No available memory for the cache blocks"),
		strings.Contains(engineLog, "to increase KV cache size"),
		strings.Contains(engineLog, "CUDA out of memory"):
		return "the KV cache did not fit in the GPU memory budget" +
			" — lower inference.vllm_max_num_batched_tokens, then inference.vllm_gpu_memory_utilization"
	case strings.Contains(engineLog, "Could not find nvcc"):
		return "vLLM compiles kernels at engine start and found no CUDA compiler" +
			" — install a CUDA toolkit on this host (Debian/Ubuntu: apt-get install cuda-toolkit-13-1)." +
			" Do not point CUDA_HOME at the venv's bundled CUDA: it has no lib64 and no libcudart.so"
	case strings.Contains(engineLog, "cc1plus"):
		return "the CUDA compiler could not run the host C++ compiler" +
			" — gcc alone is not enough, install g++ (Debian/Ubuntu: apt-get install g++)"
	case strings.Contains(engineLog, "CUDA compiler and CUDA toolkit headers are incompatible"):
		return "the CUDA compiler and the headers it was given are different versions" +
			" — this is what happens when CUDA_HOME points at the venv's bundled CUDA;" +
			" unset it so the host toolkit is used"
	case strings.Contains(engineLog, "invalid tool call parser"):
		return "vLLM does not register the configured tool-call parser" +
			" — clear or correct inference.vllm_tool_parser"
	case strings.Contains(engineLog, "Address already in use"),
		strings.Contains(engineLog, "address already in use"),
		strings.Contains(engineLog, "[Errno 98]"):
		// waired-agent#1026. The one arm whose cause is not about vLLM at
		// all: something else on the machine owns the port, and until this
		// existed the whole event was invisible — the API server binds
		// before it does anything else, so the log holds one OSError and no
		// vLLM diagnostics, and the bootstrap's three attempts all fail
		// identically. It says which address, the way the local gateway's
		// own bind failure does (inference.go): "address already in use"
		// with no number is the least useful thing to hand someone.
		//
		// The spellings are Linux's, and that is the complete set: this
		// engine runs on linux alone (internal/runtime/vllm.go and
		// inference_vllm_linux.go are //go:build linux, and the other two
		// OSes get vllm_stub_*.go), so no vLLM engine.log is ever written
		// on Windows or macOS. A `WinError 10048` arm stood here until
		// waired-agent#1085 went looking for evidence behind it and found
		// the file it would read cannot exist. Errno 98 is EADDRINUSE on
		// Linux; the bare-text spellings cover the CPython versions that
		// render it without the number.
		return enginePortBusyDiagnosis(addr, "inference.vllm_port")
	}
	return ""
}

// vllmStartupHint is what the bootstrap calls with the raw engine.log
// after its retry loop gives up: the diagnosis of the attempt the loop
// ended on. The scoping is the whole point of the wrapper — dropping it
// and passing the raw file back would silently re-open the ambiguity
// #878 closed, so it is a named function with its own test rather than
// a composition at the call site.
func vllmStartupHint(engineLog string, port int) string {
	return vllmStartupDiagnosis(infruntime.LastEngineLogSpawn(engineLog),
		fmt.Sprintf("127.0.0.1:%d", port))
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

// vllmKVCapacityRe matches vLLM V1's post-profiling KV pool report,
// e.g. "GPU KV cache size: 152,192 tokens" (kv_cache_utils.py; the
// count carries thousands separators).
var vllmKVCapacityRe = regexp.MustCompile(`GPU KV cache size:\s*([0-9][0-9,]*)\s*tokens`)

// parseVLLMKVCapacityTokens extracts the engine-measured KV-cache
// capacity (tokens) that the RUNNING engine reported, 0 when its spawn
// did not report one.
//
// It reads only the most recent spawn's section of the log
// (waired-agent#878: engine.log now holds several). A capacity line from
// an earlier spawn was measured under a configuration that is no longer
// loaded — sizing, gpu-memory-utilization and the model itself can all
// have changed since — and this figure is what marks the tuning
// Verified, so an out-of-date one would be a stale number presented as a
// measurement. Absent is the correct answer there, and it is the one
// applyVLLMTuningVerification already treats as inconclusive.
//
// Within that section the last occurrence still wins, unchanged.
func parseVLLMKVCapacityTokens(log string) int {
	ms := vllmKVCapacityRe.FindAllStringSubmatch(infruntime.LastEngineLogSpawn(log), -1)
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
//
// The figure is now KEPT rather than only checked (waired-agent#1126):
// divided by the served window it is how many conversations this host
// holds warm, which is what Capacity means. An absent line therefore
// leaves KVCapacityTokens at 0 and the host advertises "one at a time
// until I know" — the fail-safe that matters, because the line's
// wording and value both belong to the pinned release.
func applyVLLMTuningVerification(mt infruntime.ModelTuning, engineLog string) infruntime.ModelTuning {
	capacity := parseVLLMKVCapacityTokens(engineLog)
	if capacity <= 0 {
		return mt
	}
	mt.Verified = true
	mt.KVCapacityTokens = capacity
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
