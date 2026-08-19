//go:build linux

// vLLM ships only as a Linux CUDA build in the upstream wheels we pin
// (see VLLMPinnedVersion). Windows gets vllm_stub_windows.go and
// macOS gets vllm_stub_darwin.go; both shim the cmd/waired and
// cmd/waired-agent call sites so the binaries compile but every
// install / spawn call returns the platform-specific
// "vLLM is not supported" error.
// Decision: waired/docs/decisions/
//   - "Windows 版 waired-agent の方針" (20260514)
//   - "Mac 版 waired-agent の推論エンジン方針" (20260517)

package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// VLLMConfig wires a VLLMAdapter. The Python interpreter must come
// from the venv built by VLLMInstaller; bare host Python won't have
// vllm importable.
type VLLMConfig struct {
	// Python is the absolute path to the venv's bin/python (= the
	// VenvPath/bin/python returned by VLLMInstaller.Active).
	Python string
	// Host is the loopback address (always 127.0.0.1 in production).
	Host string
	// Port is the listening port (default 8000 from InferenceConfig).
	Port int

	// Model is the absolute on-disk path the engine loads (= the
	// LocalPath recorded in state.json by the HF download). vLLM
	// accepts either an HF repo id or a local directory; we always
	// pass a local dir so a flaky network during start-up never
	// reaches huggingface.co.
	Model string
	// ServedModelName is the alias the OpenAI-compat API exposes
	// (= the manifest model_id; gateway proxy binds this to
	// `/v1/models[].id`).
	ServedModelName string
	// MaxModelLen caps the context window (= manifest.context_length).
	MaxModelLen int
	// DType is "auto" / "float16" / "bfloat16" (matches manifest).
	// Quantized checkpoints (AWQ etc.) need no flag: vLLM reads the
	// quantization method from the HF config, so there is no
	// --quantization field here.
	DType string
	// GPUMemoryUtilization is the fraction of VRAM vllm may reserve
	// (matches InferenceConfig.VLLMGPUMemoryUtilization, default 0.85).
	// Under tensor parallelism the fraction applies to EACH device.
	GPUMemoryUtilization float64
	// TensorParallelSize shards the model across N identical NVIDIA
	// GPUs (vLLM --tensor-parallel-size). 0 and 1 both mean single-GPU
	// (the flag is omitted so the argv is unchanged from before TP
	// support). Callers resolve the value via
	// router.VLLMTensorParallelSize / the vllm_tensor_parallel config.
	TensorParallelSize int

	// KVCacheDType sets vLLM's --kv-cache-dtype. Empty (default) omits
	// the flag, leaving vLLM's `auto` = model dtype (fp16/bf16, 2 B/elem).
	// "fp8" selects e4m3 (1 B/elem), halving KV to roughly double usable
	// context — hardware-accelerated on Ada (compute_cap ≥ 8.9) and
	// Hopper+. The caller gates this on the GPU capability and the
	// operator opt-out (router.VLLMUsesFP8KV, vllm_disable_fp8_kv, #676).
	KVCacheDType string

	// SpeculativeConfig, when non-empty, is passed verbatim as vLLM's
	// --speculative-config (a JSON object). The agent builds the ngram
	// config here (no draft weights) to accelerate single-stream decode
	// for coding agents (#677); empty omits the flag.
	SpeculativeConfig string

	// ToolCallParser selects vLLM's --tool-call-parser, the parser that
	// turns the model's own tool-call syntax into structured
	// `tool_calls` on the OpenAI-compat response. Empty omits it AND
	// --enable-auto-tool-choice, which is what shipped before #410 and
	// means the server never populates tool_calls for any model — the
	// call stays in `content` as prose and a coding agent stalls.
	//
	// The right value is a property of the MODEL's chat template
	// (hermes for Qwen2.5's <tool_call>{json}</tool_call>, qwen3_xml for
	// Qwen3-Coder's <function=…> dialect, …), so the caller resolves it
	// per model — see resolveVLLMToolParser in cmd/waired-agent.
	//
	// It must be a name vLLM actually registers: an unknown value is
	// rejected at start-up (vLLM 0.24 cli_args.py validate / api_server
	// "invalid tool call parser"), so a typo here costs the whole engine
	// rather than just tool calling. The resolver therefore emits only
	// verified names and falls back to "" — no tool calls, but a
	// daemon that still serves.
	ToolCallParser string

	// EnablePromptTokensDetails asks the server to report
	// usage.prompt_tokens_details.cached_tokens — how much of the prompt
	// the prefix cache served instead of prefilling (waired-agent#885).
	// Without the flag vLLM returns the whole block as null, so the field
	// is unavailable rather than zero.
	//
	// Set from a version gate rather than from config: there is nothing an
	// operator would want to turn off (it adds two integers to a usage
	// object, logs nothing, and carries no content), but an older venv
	// that does not know the flag exits with argparse code 2 and takes the
	// engine with it.
	EnablePromptTokensDetails bool

	// ExtraEnv augments the env passed to the subprocess.
	ExtraEnv []string

	// LogDir, when non-empty, is where the spawned engine's merged
	// stdout+stderr is captured (<LogDir>/engine.log, truncated per
	// spawn, size-capped) — same contract as OllamaConfig.LogDir.
	// Empty discards the output (tests). Without it a failed vLLM
	// start-up (CUDA OOM at max-model-len, NCCL init under TP>1,
	// missing host prereqs) leaves no trail (#587).
	LogDir string

	// Spawner / HTTPClient / health knobs mirror OllamaAdapter.
	Spawner    Spawner
	HTTPClient *http.Client

	// HealthInterval defaults to 2s (vLLM health flips fast once
	// model load completes).
	HealthInterval time.Duration
	// HealthSuccess defaults to 3 (consecutive 200s on /health).
	HealthSuccess int
	// HealthMaxFails defaults to 60 — vLLM startup is dominated by
	// VRAM weight load (10–60s for typical Step 2 sizes), so the
	// total budget is roughly HealthInterval × HealthMaxFails = 120s.
	// With TensorParallelSize > 1 the default rises to 90 (~180s):
	// NCCL init plus per-worker flashinfer JIT stack on top of the
	// weight load.
	HealthMaxFails int
	// StopTimeout defaults to 10s (process-group SIGTERM gives vLLM
	// a chance to release CUDA contexts before SIGKILL).
	StopTimeout time.Duration
}

// VLLMAdapter is a single-subprocess vLLM engine that exposes vLLM's
// OpenAI-compatible API. Mirrors OllamaAdapter's lifecycle so the
// management/router layer treats both engines uniformly.
type VLLMAdapter struct {
	cfg VLLMConfig

	mu      sync.Mutex
	proc    RunningProcess
	state   Health
	baseURL string
	// logFile is the open <LogDir>/engine.log handle for the current
	// spawn (nil when LogDir is unset or the open failed).
	logFile *os.File
	// appliedTuning records the per-model sizing exported to the engine
	// (#675: the clamped --max-model-len and its user-visible warning),
	// mirroring OllamaAdapter so the status/doctor surfaces treat both
	// engines uniformly.
	appliedTuning ModelTuning
	// ensuring is non-nil while one EnsureRunning is in flight; later
	// callers join it instead of erroring (#280) — the old hard error
	// became a 503 runtime_unhealthy for every request that arrived
	// during a start (ollama analogue: waired-agent#29 / #279).
	// Guarded by mu.
	ensuring chan struct{}
	// ensureErr is the in-flight EnsureRunning's result, published to
	// joiners when ensuring closes. Guarded by mu.
	ensureErr error
}

// SetAppliedTuning records the sizing exported to the engine and (after
// the engine.log read-back) its verification outcome (#675).
func (a *VLLMAdapter) SetAppliedTuning(t ModelTuning) {
	a.mu.Lock()
	a.appliedTuning = t
	a.mu.Unlock()
}

// AppliedTuning returns the recorded per-model sizing, or the zero
// value before any tuning has been computed.
func (a *VLLMAdapter) AppliedTuning() ModelTuning {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.appliedTuning
}

// NewVLLMAdapter constructs an adapter with sensible defaults.
func NewVLLMAdapter(cfg VLLMConfig) *VLLMAdapter {
	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}
	if cfg.Port == 0 {
		cfg.Port = 8000
	}
	if cfg.GPUMemoryUtilization == 0 {
		cfg.GPUMemoryUtilization = 0.85
	}
	if cfg.HealthInterval <= 0 {
		cfg.HealthInterval = 2 * time.Second
	}
	if cfg.HealthSuccess <= 0 {
		cfg.HealthSuccess = 3
	}
	if cfg.HealthMaxFails <= 0 {
		if cfg.TensorParallelSize > 1 {
			cfg.HealthMaxFails = 90
		} else {
			cfg.HealthMaxFails = 60
		}
	}
	if cfg.StopTimeout <= 0 {
		cfg.StopTimeout = 10 * time.Second
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 3 * time.Second}
	}
	return &VLLMAdapter{
		cfg:     cfg,
		state:   Health{State: StateNotStarted},
		baseURL: fmt.Sprintf("http://%s:%d", cfg.Host, cfg.Port),
	}
}

// Name returns "vllm".
func (a *VLLMAdapter) Name() string { return "vllm" }

// BaseURL returns http://Host:Port.
func (a *VLLMAdapter) BaseURL() string { return a.baseURL }

// Health returns a snapshot of the engine's current state.
func (a *VLLMAdapter) Health(_ context.Context) Health {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state
}

// EnsureRunning starts the vLLM subprocess (if not already running)
// and blocks until either the engine is StateReady or the readiness
// probe gives up. Already-ready returns immediately; a call that
// arrives while another is starting joins it and returns its result.
func (a *VLLMAdapter) EnsureRunning(ctx context.Context) (err error) {
	a.mu.Lock()
	if a.state.State == StateReady {
		a.mu.Unlock()
		return nil
	}
	if ch := a.ensuring; ch != nil {
		// Join the in-flight start rather than erroring (#280). This
		// used to be a hard error that the gateway mapped to 503
		// runtime_unhealthy for every concurrent request (the ollama
		// analogue was fixed the same way, waired-agent#29 / #279).
		a.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err() // the joiner's own deadline still applies
		}
		a.mu.Lock()
		jerr := a.ensureErr
		a.mu.Unlock()
		return jerr
	}
	done := make(chan struct{})
	a.ensuring = done
	a.state = Health{State: StateStarting}
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.ensureErr = err
		a.ensuring = nil
		a.mu.Unlock()
		close(done) // off-lock, so a woken joiner never contends with the closer
	}()

	args := a.commandArgs()
	env := a.processEnv()
	logW := a.openEngineLog()
	proc, err := a.cfg.Spawner.Spawn(ctx, a.cfg.Python, args, env, logW)
	if err != nil {
		a.closeEngineLog()
		a.setState(Health{State: StateFailed, LastErr: err.Error()})
		return fmt.Errorf("vllm: spawn: %w", err)
	}
	a.mu.Lock()
	a.proc = proc
	a.mu.Unlock()

	if err := a.waitReady(ctx, proc); err != nil {
		_ = a.stopProcess(context.Background())
		if p := a.engineLogPath(); p != "" {
			err = fmt.Errorf("%w (see %s)", err, p)
		}
		a.setState(Health{State: StateFailed, LastErr: err.Error()})
		return err
	}
	a.setState(Health{State: StateReady, LastOK: time.Now()})
	return nil
}

// commandArgs builds the `python -m vllm.entrypoints.openai.api_server ...`
// argv. Order matches plan §3.1: -m first so the args after it are
// the entry-point's flags.
//
// The single --model below is also how this adapter satisfies
// MaxResidentModels: one api_server process holds one model, so vLLM has no
// equivalent of ollama's OLLAMA_MAX_LOADED_MODELS to set.
func (a *VLLMAdapter) commandArgs() []string {
	args := []string{
		"-m", "vllm.entrypoints.openai.api_server",
		"--host", a.cfg.Host,
		"--port", strconv.Itoa(a.cfg.Port),
		"--model", a.cfg.Model,
		"--gpu-memory-utilization", strconv.FormatFloat(a.cfg.GPUMemoryUtilization, 'f', 2, 64),
		// vllm 0.24 removed --disable-log-requests in favour of the
		// --[no-]enable-log-requests pair (argparse exits 2 on the old
		// spelling); stay explicit so a future default flip upstream
		// cannot silently start logging prompts.
		"--no-enable-log-requests",
		// Prefix caching is the V1 engine default, but coding agents
		// depend on it (stable system prompt + growing conversation
		// re-sent every turn), so pin it explicitly — an upstream
		// default flip must not silently reintroduce full prefills.
		"--enable-prefix-caching",
	}
	if a.cfg.TensorParallelSize > 1 {
		args = append(args, "--tensor-parallel-size", strconv.Itoa(a.cfg.TensorParallelSize))
	}
	if a.cfg.ServedModelName != "" {
		args = append(args, "--served-model-name", a.cfg.ServedModelName)
	}
	if a.cfg.MaxModelLen > 0 {
		args = append(args, "--max-model-len", strconv.Itoa(a.cfg.MaxModelLen))
	}
	if a.cfg.DType != "" {
		args = append(args, "--dtype", a.cfg.DType)
	}
	if a.cfg.KVCacheDType != "" {
		args = append(args, "--kv-cache-dtype", a.cfg.KVCacheDType)
	}
	if a.cfg.SpeculativeConfig != "" {
		args = append(args, "--speculative-config", a.cfg.SpeculativeConfig)
	}
	if a.cfg.EnablePromptTokensDetails {
		args = append(args, "--enable-prompt-tokens-details")
	}
	// Both flags or neither (#410). vLLM rejects --enable-auto-tool-choice
	// without --tool-call-parser outright, and --tool-call-parser alone is
	// inert: the server keeps `"tool_choice": "auto"` unimplemented and
	// never populates tool_calls. Emitting only one is therefore either a
	// start-up failure or a silent no-op, never the thing we want.
	if a.cfg.ToolCallParser != "" {
		args = append(args, "--enable-auto-tool-choice", "--tool-call-parser", a.cfg.ToolCallParser)
	}
	return args
}

// processEnv returns the env passed to the venv python. We inherit the
// parent env so HF_TOKEN / NCCL_* / HF_HOME etc. flow through, prepend
// the venv's bin dir to PATH — vllm 0.24's flashinfer JIT shells out
// to `ninja` (shipped in the venv) at engine start-up, and the spawned
// python never activates the venv — then layer ExtraEnv on top.
func (a *VLLMAdapter) processEnv() []string {
	venvBin := filepath.Dir(a.cfg.Python)
	if venvBin == "." {
		venvBin = ""
	}
	// Route the venv-bin PATH prepend through the shared ChildBaseEnv so
	// every engine adapter assembles its launch env one way (#22 parity).
	// No HOME fallback: vLLM is linux-only and runs under systemd's User=
	// (HOME already set), so "" tells ChildBaseEnv never to fabricate one.
	out := ChildBaseEnv(runtime.GOOS, os.Environ(), "", venvBin, string(os.PathListSeparator))
	out = append(out, a.cfg.ExtraEnv...)
	return out
}

// waitReady polls /health on HealthInterval cadence. After
// HealthSuccess consecutive 200s it makes one /v1/models call to
// confirm the served-model-name is what we asked for; only then does
// it transition to ready. The model-name check protects against the
// rare bug where vLLM serves something other than what was requested
// (e.g. wrong --served-model-name flag). The child is passed in rather
// than read from a.proc so the leader-only access is structural — only
// the EnsureRunning leader ever waits, and it waits on the process it
// just spawned.
func (a *VLLMAdapter) waitReady(ctx context.Context, proc RunningProcess) error {
	healthURL := a.baseURL + "/health"
	consecOK, consecFail := 0, 0
	tick := time.NewTicker(a.cfg.HealthInterval)
	defer tick.Stop()
	for {
		ok := a.probeOnce(ctx, healthURL)
		if ok {
			consecOK++
			consecFail = 0
			if consecOK >= a.cfg.HealthSuccess {
				if err := a.verifyServedModelName(ctx); err != nil {
					return fmt.Errorf("vllm: served model name mismatch: %w", err)
				}
				return nil
			}
		} else {
			consecFail++
			consecOK = 0
			if consecFail >= a.cfg.HealthMaxFails {
				return fmt.Errorf("vllm: not ready after %d failed probes", consecFail)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-proc.Done():
			return startupExitError("vllm", a.engineLogPath(), proc.Err())
		case <-tick.C:
		}
	}
}

func (a *VLLMAdapter) probeOnce(ctx context.Context, url string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := a.cfg.HTTPClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// verifyServedModelName fetches /v1/models and asserts the returned
// data array contains the configured ServedModelName. Only validates
// when ServedModelName is non-empty.
func (a *VLLMAdapter) verifyServedModelName(ctx context.Context) error {
	if a.cfg.ServedModelName == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL+"/v1/models", nil)
	if err != nil {
		return err
	}
	resp, err := a.cfg.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return fmt.Errorf("decode /v1/models: %w", err)
	}
	for _, m := range body.Data {
		if m.ID == a.cfg.ServedModelName {
			return nil
		}
	}
	return fmt.Errorf("/v1/models did not return %q", a.cfg.ServedModelName)
}

// Stop terminates the vLLM subprocess gracefully.
func (a *VLLMAdapter) Stop(ctx context.Context) error {
	a.mu.Lock()
	if a.proc == nil {
		a.state = Health{State: StateStopped}
		a.mu.Unlock()
		return nil
	}
	a.mu.Unlock()
	if err := a.stopProcess(ctx); err != nil {
		a.setState(Health{State: StateFailed, LastErr: err.Error()})
		return err
	}
	a.setState(Health{State: StateStopped})
	return nil
}

// stopProcess mirrors OllamaAdapter.stopProcess, including its
// commit-to-kill rule: once the stop has begun, a cancelled caller must
// not leave the child alive holding GPU memory (#316).
func (a *VLLMAdapter) stopProcess(ctx context.Context) error {
	a.mu.Lock()
	proc := a.proc
	a.mu.Unlock()
	if proc == nil {
		return nil
	}
	// Send SIGTERM to the *process group* (DefaultSpawner sets
	// Setpgid:true) so vLLM's multiprocessing children also receive
	// it. proc.Signal forwards to the leader; the OS broadcasts
	// because we used setpgid.
	//
	// vLLM is Linux-only, so ErrSignalUnsupported cannot occur here
	// today; it is handled anyway so the two adapters do not drift.
	if err := proc.Signal(syscall.SIGTERM); errors.Is(err, ErrSignalUnsupported) {
		return a.killAndReap(proc)
	}
	select {
	case <-proc.Done():
		a.closeEngineLog()
		return nil
	case <-time.After(a.cfg.StopTimeout):
		return a.killAndReap(proc)
	case <-ctx.Done():
		slog.Warn("vllm: stop caller cancelled; killing the engine rather than abandoning it",
			"pid", proc.PID(), "err", ctx.Err())
		return a.killAndReap(proc)
	}
}

// killAndReap force-kills proc and waits (bounded by StopTimeout) for the
// process group to be reaped, closing the engine log on every path.
func (a *VLLMAdapter) killAndReap(proc RunningProcess) error {
	if err := proc.Kill(); err != nil {
		a.closeEngineLog()
		return fmt.Errorf("vllm: kill: %w", err)
	}
	select {
	case <-proc.Done():
		a.closeEngineLog()
		return nil
	case <-time.After(a.cfg.StopTimeout):
		a.closeEngineLog()
		return fmt.Errorf("vllm: process %d did not exit within %s of being killed", proc.PID(), a.cfg.StopTimeout)
	}
}

// openEngineLog opens (truncating) <LogDir>/engine.log for the next
// spawn and returns a size-capped writer for the child's merged
// stdout+stderr — same best-effort contract as OllamaAdapter's
// (capture must never block bringing the engine up). Returns nil when
// LogDir is unset or the file can't be opened.
func (a *VLLMAdapter) openEngineLog() io.Writer {
	if a.cfg.LogDir == "" {
		return nil
	}
	a.closeEngineLog()
	if err := os.MkdirAll(a.cfg.LogDir, 0o755); err != nil {
		return nil
	}
	f, err := os.OpenFile(a.engineLogPath(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil
	}
	a.mu.Lock()
	a.logFile = f
	a.mu.Unlock()
	return &cappedWriter{w: f, max: engineLogMaxBytes}
}

// closeEngineLog closes the current engine.log handle if open.
func (a *VLLMAdapter) closeEngineLog() {
	a.mu.Lock()
	f := a.logFile
	a.logFile = nil
	a.mu.Unlock()
	if f != nil {
		_ = f.Close()
	}
}

// engineLogPath is <LogDir>/engine.log (or "" when LogDir is unset).
func (a *VLLMAdapter) engineLogPath() string {
	if a.cfg.LogDir == "" {
		return ""
	}
	return filepath.Join(a.cfg.LogDir, "engine.log")
}

func (a *VLLMAdapter) setState(h Health) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state = h
}

// CheckGPUOrphans inspects `nvidia-smi --query-compute-apps=pid,used_memory --format=csv`
// for PIDs that look like leftover vLLM workers (a previous agent
// crashed without cleanup) so the bootstrap can warn about — or
// optionally kill — them before starting a fresh adapter. Caller
// supplies the runner so unit tests don't shell out.
type GPUOrphanInfo struct {
	PID         int
	UsedMemMiB  int
	ProcessName string
}

// FindGPUOrphans runs nvidia-smi and decides which compute-apps
// processes look like vLLM (cmdline contains "vllm" or
// "vllm.entrypoints"). Returns nil + nil error when nvidia-smi is
// unavailable; "no orphans" is reported as an empty slice.
func FindGPUOrphans(ctx context.Context) ([]GPUOrphanInfo, error) {
	if _, err := exec.LookPath("nvidia-smi"); err != nil {
		return nil, nil
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx,
		"nvidia-smi",
		"--query-compute-apps=pid,used_memory,process_name",
		"--format=csv,noheader,nounits",
	).Output()
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi compute-apps: %w", err)
	}
	return parseGPUOrphans(string(out)), nil
}

// parseGPUOrphans handles the CSV from nvidia-smi --query-compute-apps.
// Lines without "vllm" / "python" in process_name are skipped; we
// only flag things that look like our engine.
func parseGPUOrphans(s string) []GPUOrphanInfo {
	var out []GPUOrphanInfo
	for _, raw := range strings.Split(s, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) < 3 {
			continue
		}
		for i := range fields {
			fields[i] = strings.TrimSpace(fields[i])
		}
		name := strings.ToLower(fields[2])
		if !strings.Contains(name, "vllm") && !strings.Contains(name, "python") {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		mb, _ := strconv.Atoi(fields[1])
		out = append(out, GPUOrphanInfo{PID: pid, UsedMemMiB: mb, ProcessName: fields[2]})
	}
	return out
}
