//go:build e2e && gpu

// vLLM e2e (Step 2.13): the GPU-only sibling of inference_test.go.
// Drives the full Step-2 path:
//
//   - Verifies a vLLM venv exists (Step 8 result; the operator must
//     have run `waired runtimes install vllm` beforehand).
//   - Verifies the host has at least one NVIDIA GPU with VRAM the
//     test model fits into.
//   - Pulls the test model from Hugging Face via huggingface-cli
//     (caches under XDG_DATA_HOME so subsequent runs are fast).
//   - Spawns vLLMAdapter against the real venv + GPU.
//   - Hits /v1/chat/completions through the production gateway.
//
// Run with:
//
//	make e2e-vllm-quick   # smoke (Qwen2.5-0.5B, ~3 min)
//	make e2e-vllm         # smoke + AWQ realistic (Qwen3-14B-AWQ, ~30 min)
//
// REQUIRED before any release made from a GPU-equipped host. CI must
// include a GPU lane that runs `make e2e-vllm` — see
// docs/decisions.md "GPU test mandate" entry.
package inference_e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog/scoring"
	"github.com/waired-ai/waired-agent/internal/download"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/platform/paths"
	"github.com/waired-ai/waired-agent/internal/router"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// smokeRepo / smokeModelName: cheapest Qwen safetensors release that
// vLLM can actually serve. Keep tiny so the test stays under
// `testing.Short()` budget.
const (
	smokeRepo      = "Qwen/Qwen2.5-0.5B-Instruct"
	smokeModelName = "qwen2.5-0.5b-instruct"

	awqRepo      = "Qwen/Qwen3-14B-AWQ"
	awqModelName = "qwen3-14b-instruct"
)

func TestVLLMGatewayE2E(t *testing.T) {
	requireNVIDIAGPU(t)
	venvPath := requireVLLMVenv(t)
	hfCLI := filepath.Join(venvPath, "bin", "hf")
	if _, err := os.Stat(hfCLI); err != nil {
		// Fall back to the legacy name for venvs built against
		// huggingface_hub < 1.0.
		hfCLI = filepath.Join(venvPath, "bin", "huggingface-cli")
		if _, err := os.Stat(hfCLI); err != nil {
			t.Fatalf("HF CLI not found in venv (tried hf and huggingface-cli); reinstall vllm so hf_transfer co-installs")
		}
	}

	t.Run("Smoke_Qwen0.5B", func(t *testing.T) {
		runVLLMSmoke(t, venvPath, smokeRepo, smokeModelName, 1)
	})

	// Multi-GPU lane: only runs on hosts with ≥2 NVIDIA GPUs (e.g. GCP
	// g2-standard-24, 2×L4), so the single-GPU CI/dev lane is unaffected.
	t.Run("TP2_Qwen0.5B", func(t *testing.T) {
		if n := nvidiaGPUCount(t); n < 2 {
			t.Skipf("host has %d NVIDIA GPU(s); tensor-parallel lane needs ≥2", n)
		}
		runVLLMSmoke(t, venvPath, smokeRepo, smokeModelName, 2)
	})

	if testing.Short() {
		t.Log("AWQ realistic test skipped (-short); run `make e2e-vllm` for the full run")
		return
	}

	t.Run("AWQ_Qwen14B", func(t *testing.T) {
		runVLLMSmoke(t, venvPath, awqRepo, awqModelName, 1)
	})
}

// vllmSmokeOpts parameterizes runVLLMSmokeOpts beyond the default
// smoke shape (#675: the max-model-len clamp lanes need a custom
// window, a low utilization, and an expected-abort mode).
type vllmSmokeOpts struct {
	tensorParallel int
	maxModelLen    int     // 0 → 4096
	gpuMemUtil     float64 // 0 → 0.85
	// kvCacheDType maps to VLLMConfig.KVCacheDType — "" (auto/fp16) or
	// "fp8" (e4m3, #676). speculativeConfig maps to
	// VLLMConfig.SpeculativeConfig — "" or an ngram JSON (#677).
	kvCacheDType      string
	speculativeConfig string
	// benchTokens, when > 0, replaces the one-word smoke prompt with a
	// timed coding-style generation of this many output tokens; the
	// resulting decode tok/s is returned for the #677 ngram comparison.
	benchTokens int
	// expectAbort inverts the readiness assertion: the engine must FAIL
	// to become ready (e.g. a KV cache that cannot hold one
	// max-model-len request); the chat step is skipped.
	expectAbort bool
}

// vllmSmokeResult carries what the clamp/fp8/ngram lanes read back after
// a run: the engine.log directory and, for bench lanes, the measured
// decode throughput.
type vllmSmokeResult struct {
	logDir          string
	decodeTokPerSec float64
}

func runVLLMSmoke(t *testing.T, venvPath, repo, modelName string, tensorParallel int) {
	t.Helper()
	runVLLMSmokeOpts(t, venvPath, repo, modelName, vllmSmokeOpts{tensorParallel: tensorParallel})
}

// runVLLMSmokeOpts drives download → spawn → (chat) and returns a result
// carrying the LogDir holding engine.log (so callers can inspect the
// engine's own sizing report) plus, for bench lanes, the measured decode
// throughput.
func runVLLMSmokeOpts(t *testing.T, venvPath, repo, modelName string, opts vllmSmokeOpts) vllmSmokeResult {
	t.Helper()
	if opts.maxModelLen == 0 {
		opts.maxModelLen = 4096
	}
	if opts.gpuMemUtil == 0 {
		opts.gpuMemUtil = 0.85
	}
	if opts.tensorParallel == 0 {
		opts.tensorParallel = 1
	}
	cacheRoot := xdgDataHome() + "/waired/models/" + modelName
	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}

	// Step 1: HF download (idempotent — re-runs are fast cache hits).
	t.Logf("hf download %s → %s", repo, cacheRoot)
	dlCtx, dlCancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer dlCancel()
	hfBin, err := download.ResolveHFCLI(filepath.Join(venvPath, "bin", "hf"))
	if err != nil {
		hfBin, err = download.ResolveHFCLI(filepath.Join(venvPath, "bin", "huggingface-cli"))
		if err != nil {
			t.Fatalf("resolve HF CLI: %v", err)
		}
	}
	puller := download.NewHFPuller(hfBin, download.DefaultHFRunner{})
	if err := puller.Pull(dlCtx, repo, download.HFPullOpts{
		LocalDir:     cacheRoot,
		FastTransfer: true,
	}, func(p download.Progress) {
		if p.State == download.StatePulling && p.Percent >= 0 && p.Percent%10 == 0 {
			t.Logf("hf download: %d%% — %s", p.Percent, p.Message)
		}
	}); err != nil {
		t.Fatalf("hf-cli pull %s: %v", repo, err)
	}

	// Step 2: spawn vLLM adapter against a free port. LogDir captures
	// the engine's stdout/stderr (#587) so a startup failure here is
	// diagnosable; the path is logged below and dumped on failure.
	port := freePort(t)
	logDir := t.TempDir()
	a := infruntime.NewVLLMAdapter(infruntime.VLLMConfig{
		Python:               filepath.Join(venvPath, "bin", "python"),
		Host:                 "127.0.0.1",
		Port:                 port,
		Model:                cacheRoot,
		ServedModelName:      modelName,
		MaxModelLen:          opts.maxModelLen,
		GPUMemoryUtilization: opts.gpuMemUtil,
		TensorParallelSize:   opts.tensorParallel,
		KVCacheDType:         opts.kvCacheDType,
		SpeculativeConfig:    opts.speculativeConfig,
		LogDir:               logDir,
		Spawner:              infruntime.DefaultSpawner{},
		HealthInterval:       2 * time.Second,
		HealthSuccess:        2,
		// 5 min budget: the FIRST engine start on a host also runs the
		// flashinfer JIT compile (nvcc via ninja/g++), which alone can
		// exceed the old 3-min budget on an L4 (observed ~4 min cold,
		// <1 min warm).
		HealthMaxFails: 150,
		StopTimeout:    10 * time.Second,
	})
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := a.Stop(stopCtx); err != nil {
			t.Logf("vllm stop returned: %v", err)
		}
	})

	startCtx, startCancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer startCancel()
	if err := a.EnsureRunning(startCtx); err != nil {
		if raw, rerr := os.ReadFile(filepath.Join(logDir, "engine.log")); rerr == nil {
			tail := string(raw)
			if len(tail) > 4096 {
				tail = tail[len(tail)-4096:]
			}
			t.Logf("engine.log tail:\n%s", tail)
		}
		if opts.expectAbort {
			t.Logf("engine aborted as expected: %v", err)
			return vllmSmokeResult{logDir: logDir}
		}
		t.Fatalf("vllm EnsureRunning: %v", err)
	}
	if opts.expectAbort {
		t.Fatalf("engine became ready but the lane expected a startup abort (max_model_len=%d, util=%.2f)",
			opts.maxModelLen, opts.gpuMemUtil)
	}

	// Under tensor parallelism the engine must actually be sharded:
	// nvidia-smi should report compute processes on tensorParallel
	// distinct GPUs (not one busy device plus idle siblings).
	if opts.tensorParallel > 1 {
		if got := gpusWithComputeProcs(t); got < opts.tensorParallel {
			t.Errorf("compute processes on %d GPU(s), want ≥ %d (tensor parallelism not sharding)", got, opts.tensorParallel)
		}
	}

	// Step 3: call /v1/chat/completions and assert a non-empty response.
	// benchTokens > 0 switches to a timed coding-style generation whose
	// decode tok/s is returned (#677 ngram comparison); otherwise a
	// one-word smoke prompt.
	prompt := "Reply with one word: hello"
	maxTokens := 16
	if opts.benchTokens > 0 {
		prompt = "Write a complete, idiomatic Go function `func MergeSort(xs []int) []int` " +
			"that returns a new sorted slice using recursive merge sort. Include the merge helper. Code only."
		maxTokens = opts.benchTokens
	}
	postCtx, postCancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer postCancel()
	reqBody, _ := json.Marshal(map[string]any{
		"model":       modelName,
		"messages":    []map[string]string{{"role": "user", "content": prompt}},
		"max_tokens":  maxTokens,
		"temperature": 0,
	})
	req, err := http.NewRequestWithContext(postCtx, http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", port), strings.NewReader(string(reqBody)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/chat/completions: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode completion: %v", err)
	}
	if len(parsed.Choices) == 0 || strings.TrimSpace(parsed.Choices[0].Message.Content) == "" {
		t.Fatalf("empty completion content")
	}
	res := vllmSmokeResult{logDir: logDir}
	if opts.benchTokens > 0 {
		elapsed := time.Since(start).Seconds()
		if parsed.Usage.CompletionTokens > 0 && elapsed > 0 {
			res.decodeTokPerSec = float64(parsed.Usage.CompletionTokens) / elapsed
		}
		t.Logf("bench: %d completion tokens in %.2fs → %.1f tok/s (wall, incl. prefill)",
			parsed.Usage.CompletionTokens, elapsed, res.decodeTokPerSec)
		t.Logf("sample output (first 200 chars): %.200s", parsed.Choices[0].Message.Content)
	}
	t.Logf("vLLM e2e ok for model=%s on port=%d", modelName, port)
	return res
}

// TestVLLMMaxModelLenClamp (#675): on one (model, GPU, utilization)
// tuple, prove BOTH sides of the clamp: a --max-model-len whose KV
// cache does not fit the utilization budget aborts engine startup (the
// failure computeVLLMTuning exists to prevent), and the window
// router.VLLMMaxModelLen estimates for the same tuple boots and serves.
// A deliberately low gpu-memory-utilization reproduces the mismatch on
// a 24 GB L4 with the AWQ realistic model; the engine's own KV-capacity
// report is logged for calibrating the ×1.15 weight-padding convention.
//
// Run with: make e2e-vllm-clamp
func TestVLLMMaxModelLenClamp(t *testing.T) {
	requireNVIDIAGPU(t)
	venvPath := requireVLLMVenv(t)
	if testing.Short() {
		t.Skip("needs the ~9 GB AWQ model; run without -short (make e2e-vllm-clamp)")
	}

	hw := nvidiaProfile(t)
	const (
		util = 0.55
		// Qwen3-14B-AWQ sizing: ~9.9 GB on disk; 40 layers × 8 KV heads
		// × 128 head-dim × 2 B/elem × 2 (K+V) = 163840 B/token fp16 KV.
		weightGB = 9.9
		kvBytes  = 163840
		// Well under Qwen3-14B's max_position_embeddings, big enough
		// that its KV overflows util×24 GB after the weights.
		window = 32768
	)
	est := router.VLLMMaxModelLen(weightGB, kvBytes, 1, util, scoring.KVFactorF16, hw)
	t.Logf("router.VLLMMaxModelLen(weight=%.1f GB, kv=%d B/tok, tp=1, util=%.2f, f16) = %d", weightGB, kvBytes, util, est)
	if est <= 0 || est >= window {
		t.Skipf("host does not reproduce the clamp condition (est=%d, control window=%d)", est, window)
	}

	t.Run("NativeWindowAborts", func(t *testing.T) {
		runVLLMSmokeOpts(t, venvPath, awqRepo, awqModelName, vllmSmokeOpts{
			maxModelLen: window, gpuMemUtil: util, expectAbort: true,
		})
	})
	t.Run("ClampedWindowServes", func(t *testing.T) {
		res := runVLLMSmokeOpts(t, venvPath, awqRepo, awqModelName, vllmSmokeOpts{
			maxModelLen: est, gpuMemUtil: util,
		})
		raw, err := os.ReadFile(filepath.Join(res.logDir, "engine.log"))
		if err != nil {
			t.Logf("engine.log unreadable (calibration skipped): %v", err)
			return
		}
		m := regexp.MustCompile(`GPU KV cache size:\s*([0-9][0-9,]*)\s*tokens`).FindAllStringSubmatch(string(raw), -1)
		if len(m) == 0 {
			t.Log("engine.log carries no KV-capacity line (vLLM log format changed?)")
			return
		}
		capTok, _ := strconv.Atoi(strings.ReplaceAll(m[len(m)-1][1], ",", ""))
		t.Logf("calibration: exported max_model_len=%d, engine-measured KV capacity=%d tokens (headroom %.1f%%)",
			est, capTok, 100*float64(capTok-est)/float64(est))
		if capTok < est {
			t.Errorf("engine KV capacity %d < exported window %d — estimator not conservative enough", capTok, est)
		}
	})
}

// TestVLLMFP8KVCache (#676): on an Ada+ host (compute_cap ≥ 8.9), fp8
// (e4m3) KV cache must roughly double the engine-reported KV pool versus
// the default fp16 KV at the same weights / utilization / window — and
// still boot with prefix caching pinned and serve a real completion. The
// coding-quality of fp8 output is spot-checked manually on the VM (the
// sample output is logged); this test asserts the capacity gain that is
// the point of the feature.
//
// Run with: make e2e-vllm-fp8
func TestVLLMFP8KVCache(t *testing.T) {
	requireNVIDIAGPU(t)
	venvPath := requireVLLMVenv(t)
	if testing.Short() {
		t.Skip("needs the ~9 GB AWQ model; run without -short (make e2e-vllm-fp8)")
	}
	hw := nvidiaProfile(t)
	if !router.VLLMUsesFP8KV(hw) {
		t.Skipf("host GPUs are not fp8-KV capable (need compute_cap ≥ 8.9): %s", gpuCaps(hw))
	}
	// A small window both dtypes can seat, so both boot and the only
	// variable is the KV pool size the profiler reports.
	const window = 8192
	const util = 0.85

	// The two lanes MUST run one at a time: each engine reserves util× of
	// VRAM, so a second spawn while the first is alive OOMs. Each lane is
	// its own subtest so runVLLMSmokeOpts's t.Cleanup stops its engine
	// before the next lane; requireGPUIdle then waits for the CUDA context
	// to actually release the VRAM (a just-killed engine's residue would
	// otherwise shrink the next engine's reported KV pool — the #689
	// pollution note).
	var f16Cap, fp8Cap int
	t.Run("fp16", func(t *testing.T) {
		requireGPUIdle(t)
		res := runVLLMSmokeOpts(t, venvPath, awqRepo, awqModelName, vllmSmokeOpts{
			maxModelLen: window, gpuMemUtil: util, kvCacheDType: "",
		})
		f16Cap = parseKVCapacity(t, res.logDir)
	})
	t.Run("fp8", func(t *testing.T) {
		requireGPUIdle(t)
		res := runVLLMSmokeOpts(t, venvPath, awqRepo, awqModelName, vllmSmokeOpts{
			maxModelLen: window, gpuMemUtil: util, kvCacheDType: "fp8",
		})
		fp8Cap = parseKVCapacity(t, res.logDir)
	})
	if f16Cap == 0 || fp8Cap == 0 {
		t.Fatalf("missing KV-capacity report (f16=%d, fp8=%d) — cannot verify the fp8 gain", f16Cap, fp8Cap)
	}
	ratio := float64(fp8Cap) / float64(f16Cap)
	t.Logf("KV pool: fp16=%d tokens, fp8=%d tokens (%.2f×)", f16Cap, fp8Cap, ratio)
	// Ideal is 2.0×; the fixed non-torch/activation overhead inside the
	// utilization budget only shifts once, so the pool nearly doubles.
	// 1.7 leaves margin for that shift without passing a no-op.
	if ratio < 1.7 {
		t.Errorf("fp8 KV pool only %.2f× fp16 (%d vs %d); expected ≈2× — fp8 may not be engaged", ratio, fp8Cap, f16Cap)
	}
}

// TestVLLMSpeculativeNgram (#677): ngram speculative decoding must boot
// (with prefix caching pinned) and serve a coding-style generation. It
// logs the wall-clock decode tok/s with and without speculation for the
// single-stream comparison the #677 default decision rests on — the
// numbers land in the PR/report, this lane only guards "it serves".
//
// Run with: make e2e-vllm-spec
func TestVLLMSpeculativeNgram(t *testing.T) {
	requireNVIDIAGPU(t)
	venvPath := requireVLLMVenv(t)
	if testing.Short() {
		t.Skip("needs the ~9 GB AWQ model; run without -short (make e2e-vllm-spec)")
	}
	const window = 8192
	// The exact ngram config the agent ships (keep in sync with
	// cmd/waired-agent/inference_vllm_tuning.go vllmNgramSpeculativeConfig).
	const ngramCfg = `{"method":"ngram","num_speculative_tokens":5,"prompt_lookup_max":4,"prompt_lookup_min":2}`

	// One engine at a time (see TestVLLMFP8KVCache): per-lane subtest so
	// each engine is stopped and its VRAM released before the next spawns.
	var baseTps, specTps float64
	t.Run("baseline", func(t *testing.T) {
		requireGPUIdle(t)
		res := runVLLMSmokeOpts(t, venvPath, awqRepo, awqModelName, vllmSmokeOpts{
			maxModelLen: window, benchTokens: 256,
		})
		baseTps = res.decodeTokPerSec
	})
	t.Run("ngram", func(t *testing.T) {
		requireGPUIdle(t)
		res := runVLLMSmokeOpts(t, venvPath, awqRepo, awqModelName, vllmSmokeOpts{
			maxModelLen: window, benchTokens: 256, speculativeConfig: ngramCfg,
		})
		specTps = res.decodeTokPerSec
	})
	if baseTps > 0 && specTps > 0 {
		t.Logf("decode tok/s: baseline=%.1f, ngram=%.1f (%.2f×)",
			baseTps, specTps, specTps/baseTps)
	}
}

// parseKVCapacity reads the last "GPU KV cache size: N tokens" figure
// vLLM V1 logs during startup from engine.log; 0 when absent.
func parseKVCapacity(t *testing.T, logDir string) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(logDir, "engine.log"))
	if err != nil {
		t.Logf("engine.log unreadable: %v", err)
		return 0
	}
	m := regexp.MustCompile(`GPU KV cache size:\s*([0-9][0-9,]*)\s*tokens`).FindAllStringSubmatch(string(raw), -1)
	if len(m) == 0 {
		return 0
	}
	n, _ := strconv.Atoi(strings.ReplaceAll(m[len(m)-1][1], ",", ""))
	return n
}

// gpuCaps renders the compute capabilities for a skip message.
func gpuCaps(hw hardware.Profile) string {
	var b strings.Builder
	for i, g := range hw.GPUs {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s cc=%q", g.Model, g.ComputeCap)
	}
	return b.String()
}

// nvidiaProfile builds the hardware.Profile the router sizing functions
// need, from nvidia-smi (name + total memory + compute capability per
// device). compute_cap mirrors the production detectNvidia query so
// router.VLLMUsesFP8KV resolves the same fp8 gate the agent would (#676).
func nvidiaProfile(t *testing.T) hardware.Profile {
	t.Helper()
	out, err := exec.Command("nvidia-smi",
		"--query-gpu=name,memory.total,compute_cap", "--format=csv,noheader,nounits").Output()
	if err != nil {
		t.Fatalf("nvidia-smi profile query: %v", err)
	}
	var hw hardware.Profile
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, ",")
		if len(fields) < 3 {
			continue
		}
		vram, err := strconv.Atoi(strings.TrimSpace(fields[1]))
		if err != nil {
			t.Fatalf("parse nvidia-smi memory.total %q: %v", fields[1], err)
		}
		hw.GPUs = append(hw.GPUs, hardware.GPU{
			Vendor:      "nvidia",
			Model:       strings.TrimSpace(fields[0]),
			VRAMTotalMB: vram,
			ComputeCap:  strings.TrimSpace(fields[2]),
		})
	}
	if len(hw.GPUs) == 0 {
		t.Fatal("nvidia-smi reported zero GPUs")
	}
	return hw
}

// --- helpers ---

func requireNVIDIAGPU(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("nvidia-smi"); err != nil {
		t.Fatalf("nvidia-smi not on PATH; vLLM e2e requires an NVIDIA GPU host")
	}
	out, err := exec.Command("nvidia-smi", "--query-gpu=memory.free", "--format=csv,noheader,nounits").Output()
	if err != nil {
		t.Fatalf("nvidia-smi: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 {
		t.Fatal("nvidia-smi reported zero GPUs")
	}
	t.Logf("GPU memory free: %s MB (first GPU)", strings.TrimSpace(lines[0]))
}

// nvidiaGPUCount returns the number of GPUs nvidia-smi reports.
func nvidiaGPUCount(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("nvidia-smi", "--query-gpu=uuid", "--format=csv,noheader").Output()
	if err != nil {
		t.Fatalf("nvidia-smi gpu count: %v", err)
	}
	return len(strings.Fields(strings.TrimSpace(string(out))))
}

// requireGPUIdle waits until GPU 0's used memory drops below ~1.5 GB, so
// a comparison lane starts on a clean GPU. A just-stopped engine's CUDA
// context releases its VRAM asynchronously; measuring the next engine's
// KV pool before that completes would undercount it (the #689 pollution
// note). Best-effort: logs and returns after ~60 s rather than failing.
func requireGPUIdle(t *testing.T) {
	t.Helper()
	const maxUsedMB = 1500
	deadline := 30 // × 2 s = 60 s
	for i := 0; i < deadline; i++ {
		out, err := exec.Command("nvidia-smi",
			"--query-gpu=memory.used", "--format=csv,noheader,nounits", "--id=0").Output()
		if err != nil {
			t.Logf("requireGPUIdle: nvidia-smi failed (%v); continuing", err)
			return
		}
		used, _ := strconv.Atoi(strings.TrimSpace(strings.Split(string(out), "\n")[0]))
		if used < maxUsedMB {
			if i > 0 {
				t.Logf("GPU idle after %ds (used=%d MB)", i*2, used)
			}
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Logf("requireGPUIdle: GPU still busy after %ds; proceeding anyway", deadline*2)
}

// gpusWithComputeProcs returns how many distinct GPUs currently host at
// least one compute process (by gpu_uuid in --query-compute-apps).
func gpusWithComputeProcs(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("nvidia-smi",
		"--query-compute-apps=gpu_uuid", "--format=csv,noheader").Output()
	if err != nil {
		t.Fatalf("nvidia-smi compute-apps: %v", err)
	}
	uuids := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if u := strings.TrimSpace(line); u != "" {
			uuids[u] = true
		}
	}
	return len(uuids)
}

func requireVLLMVenv(t *testing.T) string {
	t.Helper()
	// Resolve the venv from <state-dir>/runtimes/vllm — the same path the
	// CLI installer now writes (#525). WAIRED_STATE_DIR overrides; otherwise
	// AutoDetect yields the per-user state dir on Linux dev/CI hosts.
	baseDir := filepath.Join(paths.StateDir(paths.AutoDetect), "runtimes", "vllm")
	inst := infruntime.NewVLLMInstallerAt(baseDir)
	res, ok := inst.Active()
	if !ok {
		t.Fatalf("vLLM venv not active at %s; run `waired runtimes install vllm` first "+
			"(see make e2e-vllm-quick output for guidance)", baseDir)
	}
	t.Logf("using vllm venv: %s (version %s)", res.VenvPath, res.Version)
	return res.VenvPath
}

func xdgDataHome() string {
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return x
	}
	return os.Getenv("HOME") + "/.local/share"
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
