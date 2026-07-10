package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// OllamaConfig wires an OllamaAdapter. All time-related fields fall
// back to spec-recommended defaults when zero, so production code only
// sets Binary / Host / Port / (real) Spawner / HTTPClient.
type OllamaConfig struct {
	// Binary is the absolute path to the ollama executable. May be empty
	// at construction time when ollama is not yet installed; in that case
	// BinaryResolver is consulted lazily on the first EnsureRunning.
	Binary string
	// BinaryResolver lazily resolves the ollama binary path when Binary
	// is empty. It lets an agent that booted before ollama was installed
	// pick up a freshly installed binary on the next EnsureRunning,
	// without an agent restart (#188). Production wires this to
	// download.ResolveBinary; leaving it nil keeps the old "Binary must
	// be set" behaviour for tests.
	BinaryResolver func() (string, error)

	// Borrowed selects "reuse" mode (#188): the agent does NOT spawn or
	// own an ollama process — it expects one already listening at
	// Host:Port (a user's `ollama serve` or system service) and merely
	// probes it. EnsureRunning never spawns and Stop is a no-op (we must
	// not kill an engine we didn't start). Used when the operator chose
	// to reuse their existing Ollama instead of waired's bundled one.
	Borrowed bool
	// Host is the loopback address the engine binds to (always
	// 127.0.0.1 in production; unit tests point at httptest).
	Host string
	// Port is the engine's listening port. Bundled mode binds the
	// waired-owned port (agentconfig.DefaultOllamaBundledPort) so it
	// never contends with a user's system ollama on 11434.
	Port int
	// ModelsDir, when non-empty, is exported as OLLAMA_MODELS to the
	// spawned engine so blobs live in a waired-owned directory
	// (<state-dir>/runtimes/ollama/models, bundled mode). Empty keeps
	// the engine's own default (borrowed mode / tests).
	ModelsDir string
	// ExpectedVersion is the exact version (GET /api/version) an
	// EADDRINUSE survivor on our port must report to be adopted as an
	// orphan of a previous waired run. Bundled mode wires the pinned
	// version; empty disables adoption entirely. Any other survivor is
	// a foreign engine and EnsureRunning fails loudly instead of
	// silently serving from an unpinned engine.
	ExpectedVersion string
	// ExtraEnv augments the env passed to the subprocess. Useful in
	// tests; production callers leave it empty.
	ExtraEnv []string
	// BackendEnv holds GPU-backend selection overrides (e.g.
	// "OLLAMA_VULKAN=1" or "HSA_OVERRIDE_GFX_VERSION=11.5.1") chosen for
	// this host by ResolveOllamaBackend (#290). Unlike ExtraEnv it is
	// production-set and may be replaced at runtime via SetBackendEnv
	// (the Strix Halo ROCm->Vulkan probe). Any inherited env var with a
	// matching key is dropped so these win over the parent environment.
	BackendEnv []string

	// Spawner abstracts the subprocess starter (DefaultSpawner{} in
	// production, a fake in unit tests).
	Spawner Spawner
	// HTTPClient is used for health probes and (later) proxying.
	HTTPClient *http.Client

	// HealthInterval is the polling interval for the readiness probe
	// (default 5s, spec §8.4).
	HealthInterval time.Duration
	// HealthSuccess is the number of consecutive successful probes
	// required to declare ready (default 3).
	HealthSuccess int
	// HealthMaxFails is the number of consecutive failed probes
	// before declaring failed (default 3). It bounds BORROWED-mode and
	// steady-state probing only — for a SPAWNED engine the adapter owns
	// the child and supervises its liveness directly (see
	// StartupReadyTimeout), so a slow cold start is not mistaken for a
	// crash and the not-yet-ready child is not killed prematurely.
	HealthMaxFails int
	// StartupReadyTimeout bounds how long the FIRST readiness wait of a
	// spawned engine may take before giving up (default 150s). Ollama's
	// first `ollama serve` cold start on a fresh host can take far longer
	// than HealthMaxFails*HealthInterval (~10s) — Windows Defender
	// scanning a freshly-extracted 1.4 GB install, CPU-runner init, model
	// store build — so the spawned path waits up to this deadline while
	// the child is alive instead of bailing after HealthMaxFails probes.
	// A real crash is still caught immediately via the process-exit
	// channel; this only changes the "alive but still warming up" case.
	StartupReadyTimeout time.Duration
	// LogDir, when non-empty, is where the spawned engine's merged
	// stdout+stderr is captured (<LogDir>/engine.log, truncated per
	// spawn, size-capped). Empty discards the output (borrowed mode /
	// tests). Without it a failed `ollama serve` leaves no trail and
	// "not ready" is undiagnosable in the field.
	LogDir string
	// StopTimeout is how long Stop waits after SIGTERM before
	// SIGKILL (default 5s).
	StopTimeout time.Duration
}

// ErrEngineParked is returned by EnsureRunning when the engine has been
// administratively parked (hard-stopped via the engine power axis, #186).
// The gateway maps it to a 503 so request traffic does NOT resurrect an
// engine the operator explicitly stopped to free memory.
var ErrEngineParked = errors.New("ollama: engine parked (stopped by operator)")

// ErrEngineBorrowed is returned by Park when the adapter is in reuse mode
// (#188): the agent does not own the process, so it cannot free the user's
// memory and must not signal their `ollama serve`.
var ErrEngineBorrowed = errors.New("ollama: engine is reused, not managed by waired")

// ErrEngineNotOwned is returned by Park when the engine was adopted as
// an orphan of a previous run: there is no process handle to signal, so
// waired cannot free its memory.
var ErrEngineNotOwned = errors.New("ollama: engine adopted from a previous run, not stoppable by waired")

// EngineMode describes who owns the serving engine process.
type EngineMode string

const (
	// EngineModeSpawned: the engine is waired's own supervised child
	// (the normal bundled outcome).
	EngineModeSpawned EngineMode = "spawned"
	// EngineModeBorrowed: reuse mode (#188) — the user's engine,
	// probed but never spawned/stopped.
	EngineModeBorrowed EngineMode = "borrowed"
	// EngineModeAdopted: an exact-pin orphan from a previous waired
	// run answered on our port; serving from it, but with no process
	// handle (Stop/Park cannot signal it).
	EngineModeAdopted EngineMode = "adopted"
)

// ollamaKeepAlive is the idle time a loaded model stays in (V)RAM
// (exported as OLLAMA_KEEP_ALIVE). 60m rather than ollama's 5m default:
// coding-agent sessions routinely pause longer than 5 minutes between
// requests, and an unload there costs a ~20 GB model reload on the next
// turn. Truly idle hosts still release VRAM after the hour.
const ollamaKeepAlive = "60m"

// OllamaAdapter is a single-subprocess Ollama engine.
type OllamaAdapter struct {
	cfg OllamaConfig

	mu      sync.Mutex
	proc    RunningProcess
	state   Health
	baseURL string
	// backendEnv is the live GPU-backend env override set (seeded from
	// cfg.BackendEnv, swappable via SetBackendEnv for the Strix Halo
	// ROCm->Vulkan probe, #290). Guarded by mu; read by processEnv at
	// each spawn.
	backendEnv []string
	// modelEnvProvider, when set, is consulted at each spawn that has
	// no explicit modelEnv yet: it resolves the serving target and
	// returns its tuning env fresh (#624). This closes the boot-order
	// gap where the engine becomes viable only after the boot-time
	// engine decision (fresh install pulls the binary mid-bootstrap)
	// and the one-shot SetModelEnv wiring never ran — the engine then
	// served untuned at its 32k default. An explicit SetModelEnv
	// (boot compute, verify-degrade) always wins; ok=false leaves the
	// spawn untuned. Guarded by mu.
	modelEnvProvider func() ([]string, ModelTuning, bool)
	// modelEnv is the per-model serve tuning env (OLLAMA_CONTEXT_LENGTH,
	// OLLAMA_KV_CACHE_TYPE, OLLAMA_NUM_PARALLEL, OLLAMA_FLASH_ATTENTION —
	// #621), computed by the agent from the target manifest and host
	// memory, swappable via SetModelEnv across a Stop / re-EnsureRunning
	// cycle. Guarded by mu; read by processEnv at each spawn.
	modelEnv []string
	// appliedTuning records the tuning actually exported to the engine
	// and the post-load verification outcome (#621), for the doctor /
	// inference status and the Claude intercept's window advertisement
	// (#623). Zero value until set. Guarded by mu.
	appliedTuning ModelTuning
	// resolvedBackend is the GPU backend the engine ended up on after the
	// #290 selection (and, for Strix Halo Linux, the engagement probe).
	// Surfaced by the doctor / inference status so a CPU fallback is
	// never silent. "" until set. Guarded by mu.
	resolvedBackend OllamaBackend
	// parked is the engine power axis (#186): when true the engine has
	// been hard-stopped by the operator and EnsureRunning refuses to
	// (re)spawn until Unpark clears it. Live-only state — not persisted,
	// so a daemon restart returns to normal config-driven startup.
	// Guarded by mu (EnsureRunning/Stop already hold it), which closes
	// the check-then-spawn race with Park.
	parked bool
	// adopted records that the serving engine is an exact-pin orphan
	// (see EngineModeAdopted). Cleared when a later EnsureRunning
	// succeeds with its own spawn. Guarded by mu.
	adopted bool
	// liveVersion caches the serving engine's GET /api/version answer,
	// fetched best-effort after each successful readiness wait. ""
	// until first ready (or when the probe failed). Unlike the binary
	// `--version` the hardware profiler reports, this is the version
	// actually answering requests — the two differ in borrowed/adopted
	// modes. Guarded by mu.
	liveVersion string
	// logFile is the open <LogDir>/engine.log handle for the current
	// spawned child (nil when LogDir is unset or the engine is not
	// running). Re-opened (truncated) on each spawn and closed when the
	// process is stopped, so it tracks the child's lifetime. Guarded by mu.
	logFile *os.File
}

// NewOllamaAdapter constructs an adapter with sensible defaults.
func NewOllamaAdapter(cfg OllamaConfig) *OllamaAdapter {
	if cfg.HealthInterval <= 0 {
		cfg.HealthInterval = 5 * time.Second
	}
	if cfg.HealthSuccess <= 0 {
		cfg.HealthSuccess = 3
	}
	if cfg.HealthMaxFails <= 0 {
		cfg.HealthMaxFails = 3
	}
	if cfg.StartupReadyTimeout <= 0 {
		cfg.StartupReadyTimeout = 150 * time.Second
	}
	if cfg.StopTimeout <= 0 {
		cfg.StopTimeout = 5 * time.Second
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 3 * time.Second}
	}
	return &OllamaAdapter{
		cfg:        cfg,
		state:      Health{State: StateNotStarted},
		baseURL:    fmt.Sprintf("http://%s:%d", cfg.Host, cfg.Port),
		backendEnv: cfg.BackendEnv,
	}
}

// Name returns "ollama".
func (a *OllamaAdapter) Name() string { return "ollama" }

// resolveBinary returns the ollama binary path, lazily re-resolving via
// cfg.BinaryResolver when the configured path is empty and caching the
// result. This is what lets a "no engine" agent adopt a binary that was
// installed after boot without a restart (#188).
func (a *OllamaAdapter) resolveBinary() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cfg.Binary != "" {
		return a.cfg.Binary, nil
	}
	if a.cfg.BinaryResolver == nil {
		return "", errors.New("ollama: binary not configured")
	}
	bin, err := a.cfg.BinaryResolver()
	if err != nil {
		return "", fmt.Errorf("ollama: resolve binary: %w", err)
	}
	if bin == "" {
		return "", errors.New("ollama: binary resolver returned empty path")
	}
	a.cfg.Binary = bin
	return bin, nil
}

// BaseURL returns http://Host:Port.
func (a *OllamaAdapter) BaseURL() string { return a.baseURL }

// Health returns a snapshot of the engine's current state.
func (a *OllamaAdapter) Health(_ context.Context) Health {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state
}

// EnsureRunning starts the Ollama subprocess (if not already running)
// and blocks until either the engine is StateReady or the readiness
// probe gives up. The first call wins; subsequent calls return the
// memoised result without re-spawning.
func (a *OllamaAdapter) EnsureRunning(ctx context.Context) error {
	a.mu.Lock()
	// Parked check first: a hard-stopped engine must not be revived by
	// the per-request EnsureRunning the gateway calls (internal/gateway/
	// openai.go, anthropic.go) — otherwise the next inference request
	// would re-spawn ollama and undo the memory release.
	if a.parked {
		a.mu.Unlock()
		return ErrEngineParked
	}
	if a.state.State == StateReady {
		a.mu.Unlock()
		return nil
	}
	if a.state.State == StateStarting {
		a.mu.Unlock()
		return errors.New("ollama: EnsureRunning called while already starting")
	}
	a.state = Health{State: StateStarting}
	borrowed := a.cfg.Borrowed
	a.mu.Unlock()

	// Reuse mode: an ollama is expected to be running already. Probe it,
	// never spawn (a.proc stays nil so Stop is a no-op). No supervised
	// process here, so the snappy HealthMaxFails give-up is correct.
	if borrowed {
		if err := a.waitReady(ctx, false); err != nil {
			a.setState(Health{State: StateFailed, LastErr: err.Error()})
			return fmt.Errorf("ollama: borrowed engine not reachable at %s: %w", a.baseURL, err)
		}
		a.cacheVersion(ctx)
		a.setState(Health{State: StateReady, LastOK: time.Now()})
		return nil
	}

	binary, err := a.resolveBinary()
	if err != nil {
		a.setState(Health{State: StateFailed, LastErr: err.Error()})
		return err
	}

	if a.cfg.ModelsDir != "" {
		// Defensive: ollama creates it too, but a pre-created dir makes
		// permission failures surface here instead of inside the child.
		_ = os.MkdirAll(a.cfg.ModelsDir, 0o755)
	}
	args := []string{"serve"}
	a.refreshModelEnvFromProvider()
	env := a.processEnv()
	logW := a.openEngineLog()
	proc, err := a.cfg.Spawner.Spawn(ctx, binary, args, env, logW)
	if err != nil {
		a.closeEngineLog()
		a.setState(Health{State: StateFailed, LastErr: err.Error()})
		return fmt.Errorf("ollama: spawn: %w", err)
	}
	a.mu.Lock()
	a.proc = proc
	a.mu.Unlock()

	// Spawned engine: we own and supervise this child, so wait for it to
	// become ready up to StartupReadyTimeout (cold starts on a fresh host
	// — Windows Defender scanning the install, CPU-runner init — routinely
	// exceed HealthMaxFails*HealthInterval). A genuine crash is still
	// caught immediately by the process-exit channel inside waitReady; the
	// deadline only bounds the "alive but still warming up" case.
	startCtx := ctx
	if a.cfg.StartupReadyTimeout > 0 {
		var cancel context.CancelFunc
		startCtx, cancel = context.WithTimeout(ctx, a.cfg.StartupReadyTimeout)
		defer cancel()
	}
	if err := a.waitReady(startCtx, true); err != nil {
		// Our spawn didn't come up. Tear down our child so we don't leak it…
		_ = a.stopProcess(context.Background())
		a.mu.Lock()
		a.proc = nil
		a.mu.Unlock()
		// Make the deadline case legible: distinguish "still starting after
		// the budget" from a real crash (already worded by waitReady).
		if errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("ollama: not ready within %s (engine still starting; see %s)",
				a.cfg.StartupReadyTimeout, a.engineLogPath())
		}
		// …and identify whatever is still answering on OUR port. Since we
		// just killed our own child, an answering engine is one we do not
		// supervise. On the waired-owned port that is normally an orphan
		// of a previous agent run (the child outlived a crashed parent):
		// it reports exactly the pinned version, and serving from it is
		// safe — a.proc stays nil so Stop()/Park() never signal a process
		// we don't own. ANY other version is a foreign engine; adopting
		// it silently is how a 0.30.7-pinned node ended up served by a
		// system ollama 0.24.0 (wrong status version, server-side pull
		// failures with no indication why), so refuse with remediation
		// instead.
		if ver, verr := a.fetchVersion(ctx); verr == nil {
			if a.cfg.ExpectedVersion != "" && ver == a.cfg.ExpectedVersion {
				a.mu.Lock()
				a.adopted = true
				a.liveVersion = ver
				a.mu.Unlock()
				a.setState(Health{State: StateReady, LastOK: time.Now()})
				return nil
			}
			msg := fmt.Sprintf(
				"ollama: port %d is already in use by another ollama (version %s, expected %s); "+
					"refusing to adopt it. Stop that process or change inference.ollama_port in "+
					"agent.json; if you meant to use your own ollama, set ollama_source to "+
					"\"reuse\" (or re-run `sudo waired init`)",
				a.cfg.Port, ver, a.cfg.ExpectedVersion)
			a.setState(Health{State: StateFailed, LastErr: msg})
			return errors.New(msg)
		}
		a.setState(Health{State: StateFailed, LastErr: err.Error()})
		return err
	}
	a.mu.Lock()
	a.adopted = false
	a.mu.Unlock()
	a.cacheVersion(ctx)

	// Re-check parked: Park may have flipped the flag during the slow
	// waitReady probe window (after we passed the top-of-function check).
	// If so, tear down the process we just brought up so a concurrent
	// hard-stop wins instead of leaving a live engine with parked==true.
	a.mu.Lock()
	if a.parked {
		a.mu.Unlock()
		_ = a.stopProcess(context.Background())
		a.setState(Health{State: StateStopped})
		return ErrEngineParked
	}
	a.mu.Unlock()

	a.setState(Health{State: StateReady, LastOK: time.Now()})
	return nil
}

// processEnv returns the environment variables passed to `ollama
// serve`, derived from the parent process env plus the spec-mandated
// overrides and the GPU-backend selection (#290). Any inherited env var
// whose key we set ourselves is dropped from the base so our value wins
// regardless of getenv's first-vs-last duplicate resolution.
func (a *OllamaAdapter) processEnv() []string {
	a.mu.Lock()
	backend := a.backendEnv
	model := a.modelEnv
	a.mu.Unlock()

	// Keys we inject and that must override any inherited value.
	drop := map[string]bool{"OLLAMA_HOST": true}
	if a.cfg.ModelsDir != "" {
		drop["OLLAMA_MODELS"] = true
	}
	for _, kv := range backend {
		if k := envKey(kv); k != "" {
			drop[k] = true
		}
	}
	for _, kv := range model {
		if k := envKey(kv); k != "" {
			drop[k] = true
		}
	}

	base := os.Environ()
	out := make([]string, 0, len(base)+4+len(backend)+len(model)+len(a.cfg.ExtraEnv))
	for _, kv := range base {
		if drop[envKey(kv)] {
			continue
		}
		out = append(out, kv)
	}
	out = append(out,
		fmt.Sprintf("OLLAMA_HOST=%s:%d", a.cfg.Host, a.cfg.Port),
		"OLLAMA_NO_CLOUD=1",
		"OLLAMA_KEEP_ALIVE="+ollamaKeepAlive,
	)
	if a.cfg.ModelsDir != "" {
		out = append(out, "OLLAMA_MODELS="+a.cfg.ModelsDir)
	}
	// Backend and model-tuning overrides come before ExtraEnv so a test
	// ExtraEnv can still have the last word if it deliberately sets the
	// same key.
	out = append(out, backend...)
	out = append(out, model...)
	out = append(out, a.cfg.ExtraEnv...)
	return out
}

// SetBackendEnv replaces the GPU-backend env overrides applied to the
// NEXT `ollama serve` spawn. Used by the Strix Halo ROCm->Vulkan probe
// to switch backends across a Stop / re-EnsureRunning cycle (#290); it
// does not affect an already-running process until it is restarted.
func (a *OllamaAdapter) SetBackendEnv(env []string) {
	a.mu.Lock()
	a.backendEnv = append([]string(nil), env...)
	a.mu.Unlock()
}

// BackendEnv returns a copy of the current GPU-backend env overrides.
func (a *OllamaAdapter) BackendEnv() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.backendEnv...)
}

// SetModelEnvProvider registers the spawn-time tuning resolver — see
// the modelEnvProvider field for the boot-order gap it closes.
func (a *OllamaAdapter) SetModelEnvProvider(fn func() ([]string, ModelTuning, bool)) {
	a.mu.Lock()
	a.modelEnvProvider = fn
	a.mu.Unlock()
}

// refreshModelEnvFromProvider fills modelEnv (and the applied-tuning
// record) from the provider right before a spawn, but only when no
// explicit tuning env is present — SetModelEnv callers (the boot
// compute and the verify-degrade restart) stay authoritative.
func (a *OllamaAdapter) refreshModelEnvFromProvider() {
	a.mu.Lock()
	provider := a.modelEnvProvider
	empty := len(a.modelEnv) == 0
	a.mu.Unlock()
	if provider == nil || !empty {
		return
	}
	env, tuning, ok := provider()
	if !ok {
		return
	}
	a.mu.Lock()
	if len(a.modelEnv) == 0 { // re-check under the lock
		a.modelEnv = append([]string(nil), env...)
		a.appliedTuning = tuning
	}
	a.mu.Unlock()
}

// SetModelEnv replaces the per-model tuning env applied to the NEXT
// `ollama serve` spawn (#621). Like SetBackendEnv it does not affect an
// already-running process until it is restarted.
func (a *OllamaAdapter) SetModelEnv(env []string) {
	a.mu.Lock()
	a.modelEnv = append([]string(nil), env...)
	a.mu.Unlock()
}

// ModelEnv returns a copy of the current per-model tuning env.
func (a *OllamaAdapter) ModelEnv() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.modelEnv...)
}

// ModelTuning records the per-model serve tuning the agent exported to
// the engine (#621) and the post-load verification outcome. Surfaced by
// the inference status / doctor, and read by the Claude intercept to
// advertise the effective context window (#623).
type ModelTuning struct {
	ModelID   string
	VariantID string
	// ContextLength is the OLLAMA_CONTEXT_LENGTH value exported to the
	// engine; 0 means the var was not set (unknown sizing inputs) and the
	// engine runs at its own default.
	ContextLength int
	NumParallel   int
	// RecommendedMaxParallel is the VRAM-safe engine-parallelism ceiling the
	// sizing computed (floor(maxCtx/ctx) in the no-spill regime; 1 when spilling
	// or when the host is unsizable). It is ADVISORY telemetry surfaced to the
	// admin Device detail page (which warns before an operator sets NumParallel
	// above it via an informed override); it is NOT exported as an OLLAMA_* env.
	// 0 means "not computed" (older/untuned).
	RecommendedMaxParallel int
	// NumBatch is the generation ubatch the serve tuning selected (#642);
	// 0 means "left to Ollama's automatic batch sizing". Unlike the other
	// fields it is NOT delivered via an OLLAMA_* env (the pinned 0.31.1
	// exposes none) but through a locally derived model carrying
	// PARAMETER num_batch — see cmd/waired-agent/inference_ollama_derived.go.
	// Non-zero only on spilled discrete-GPU hosts, where Ollama's own
	// automaticGenerationBatch would otherwise fall back to 512.
	NumBatch int
	// KVCacheType is the OLLAMA_KV_CACHE_TYPE the sizing assumed —
	// normally "q8_0"; flips to "f16" when the post-load check detected
	// the engine fell back.
	KVCacheType string
	// Verified is true once the post-load /api/ps verification completed
	// (regardless of outcome).
	Verified bool
	// Warning is a user-visible note (context floored, f16 fallback,
	// spill detected, reused engine ignores tuning); "" when healthy.
	Warning string
}

// SetAppliedTuning records the tuning exported to the engine and (after
// verification) its outcome (#621).
func (a *OllamaAdapter) SetAppliedTuning(t ModelTuning) {
	a.mu.Lock()
	a.appliedTuning = t
	a.mu.Unlock()
}

// AppliedTuning returns the recorded per-model tuning, or the zero value
// before any tuning has been computed.
func (a *OllamaAdapter) AppliedTuning() ModelTuning {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.appliedTuning
}

// SetResolvedBackend records the GPU backend the engine settled on (#290),
// for the doctor / inference status to surface.
func (a *OllamaAdapter) SetResolvedBackend(b OllamaBackend) {
	a.mu.Lock()
	a.resolvedBackend = b
	a.mu.Unlock()
}

// ResolvedBackend returns the GPU backend the engine settled on, or ""
// before selection has run.
func (a *OllamaAdapter) ResolvedBackend() OllamaBackend {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.resolvedBackend
}

// envKey returns the variable name from a "KEY=VALUE" pair, or "" when
// the entry has no '='.
func envKey(kv string) string {
	k, _, ok := strings.Cut(kv, "=")
	if !ok {
		return ""
	}
	return k
}

// waitReady polls /api/tags every HealthInterval, declaring ready after
// HealthSuccess consecutive 2xx responses. The poll exits early if the
// process exits or the context is cancelled.
//
// supervised selects the failure policy:
//   - false (borrowed / steady-state, no owned child): give up after
//     HealthMaxFails consecutive failures — there is no process to watch,
//     so a quick failure is the only signal and the right one.
//   - true (spawned child): do NOT give up on consecutive failures while
//     the child is alive. A real crash fires the process-exit channel
//     (fail-fast); a child that simply hasn't bound its port yet is still
//     warming up, so we keep probing until the caller's ctx deadline
//     (StartupReadyTimeout). This is what lets a slow first cold start
//     finish instead of being killed after ~10s (HealthMaxFails*interval).
func (a *OllamaAdapter) waitReady(ctx context.Context, supervised bool) error {
	healthURL := a.baseURL + "/api/tags"
	consecOK, consecFail := 0, 0
	tick := time.NewTicker(a.cfg.HealthInterval)
	defer tick.Stop()
	// Run a probe immediately so fast tests don't have to wait one
	// HealthInterval for the first probe.
	for {
		ok := a.probeOnce(ctx, healthURL)
		if ok {
			consecOK++
			consecFail = 0
			if consecOK >= a.cfg.HealthSuccess {
				return nil
			}
		} else {
			consecFail++
			consecOK = 0
			if !supervised && consecFail >= a.cfg.HealthMaxFails {
				return fmt.Errorf("ollama: not ready after %d failed probes", consecFail)
			}
		}

		// In borrowed mode there is no child process; procDone stays nil
		// (a nil channel never fires) so we rely on the probe + ctx only.
		var procDone <-chan struct{}
		a.mu.Lock()
		proc := a.proc
		a.mu.Unlock()
		if proc != nil {
			procDone = proc.Done()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-procDone:
			return fmt.Errorf("ollama: process exited during startup: %v", proc.Err())
		case <-tick.C:
		}
	}
}

func (a *OllamaAdapter) probeOnce(ctx context.Context, url string) bool {
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

// fetchVersion asks the serving engine for its version (GET
// /api/version → {"version":"x.y.z"}). This is the engine actually
// answering on Host:Port — NOT the configured binary's `--version`.
func (a *OllamaAdapter) fetchVersion(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL+"/api/version", nil)
	if err != nil {
		return "", err
	}
	resp, err := a.cfg.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("ollama: /api/version: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("ollama: /api/version: %w", err)
	}
	v := strings.TrimSpace(body.Version)
	if v == "" {
		return "", errors.New("ollama: /api/version: empty version")
	}
	return v, nil
}

// cacheVersion refreshes liveVersion best-effort; a failed probe keeps
// the previous value (readiness is already established by the caller).
func (a *OllamaAdapter) cacheVersion(ctx context.Context) {
	v, err := a.fetchVersion(ctx)
	if err != nil {
		return
	}
	a.mu.Lock()
	a.liveVersion = v
	a.mu.Unlock()
}

// EngineVersion returns the serving engine's cached live version ("",
// before the first successful readiness + version probe).
func (a *OllamaAdapter) EngineVersion() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.liveVersion
}

// Mode reports who owns the serving engine process. Borrowed is a
// config-time fact; adopted is discovered at EnsureRunning time; the
// default (including before first start) is spawned.
func (a *OllamaAdapter) Mode() EngineMode {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case a.cfg.Borrowed:
		return EngineModeBorrowed
	case a.adopted:
		return EngineModeAdopted
	default:
		return EngineModeSpawned
	}
}

// Stop terminates the Ollama subprocess gracefully (SIGTERM, then
// SIGKILL after StopTimeout).
func (a *OllamaAdapter) Stop(ctx context.Context) error {
	a.mu.Lock()
	// Borrowed (reuse) engines are not ours to stop — leave the user's
	// ollama running. Just mark our view stopped.
	if a.cfg.Borrowed {
		a.state = Health{State: StateStopped}
		a.mu.Unlock()
		return nil
	}
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

func (a *OllamaAdapter) stopProcess(ctx context.Context) error {
	a.mu.Lock()
	proc := a.proc
	a.mu.Unlock()
	if proc == nil {
		return nil
	}
	// Best-effort SIGTERM; if the receiver has already exited the
	// signal call returns an error which we ignore.
	_ = proc.Signal(syscall.SIGTERM)
	select {
	case <-proc.Done():
		a.closeEngineLog()
		return nil
	case <-time.After(a.cfg.StopTimeout):
		// Escalate.
		if err := proc.Kill(); err != nil {
			return fmt.Errorf("ollama: kill: %w", err)
		}
		<-proc.Done()
		a.closeEngineLog()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// openEngineLog opens (truncating) <LogDir>/engine.log for the next
// spawn and returns a size-capped writer for the child's merged
// stdout+stderr. Returns nil when LogDir is unset or the file can't be
// opened — capture is best-effort and must never block bringing the
// engine up. The previous handle (if any) is closed first.
func (a *OllamaAdapter) openEngineLog() io.Writer {
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
	// Cap so a long-lived or crash-looping engine can't grow the file
	// without bound; the startup + early-request window is what matters
	// for diagnosis.
	return &cappedWriter{w: f, max: engineLogMaxBytes}
}

// closeEngineLog closes the current engine.log handle if open.
func (a *OllamaAdapter) closeEngineLog() {
	a.mu.Lock()
	f := a.logFile
	a.logFile = nil
	a.mu.Unlock()
	if f != nil {
		_ = f.Close()
	}
}

// engineLogPath is <LogDir>/engine.log (or "" when LogDir is unset).
func (a *OllamaAdapter) engineLogPath() string {
	if a.cfg.LogDir == "" {
		return ""
	}
	return filepath.Join(a.cfg.LogDir, "engine.log")
}

// engineLogMaxBytes caps the captured engine log. ollama serve's stdout
// is modest (startup + occasional request lines), so a few MB comfortably
// covers a cold start and the early requests that follow.
const engineLogMaxBytes = 8 << 20 // 8 MiB

// cappedWriter forwards writes to w until max bytes have been written,
// then drops the rest (after a one-time truncation marker). It exists so
// the engine log can't grow without bound; it is not a ring buffer —
// keeping the START of the log is the right trade for "why didn't the
// engine come up". The mutex makes it safe even if a caller wires it to
// distinct stdout/stderr streams (os/exec serialises a shared writer, but
// we don't rely on that here).
type cappedWriter struct {
	mu      sync.Mutex
	w       io.Writer
	max     int
	written int
	capped  bool
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.written >= c.max {
		return len(p), nil // silently drop; pretend success so the child isn't blocked
	}
	remaining := c.max - c.written
	if len(p) <= remaining {
		n, err := c.w.Write(p)
		c.written += n
		return n, err
	}
	// Partial write up to the cap, then a one-time marker.
	if _, err := c.w.Write(p[:remaining]); err != nil {
		c.written += remaining
		return len(p), err
	}
	c.written += remaining
	if !c.capped {
		c.capped = true
		_, _ = io.WriteString(c.w, "\n...[waired: engine log truncated at cap]...\n")
	}
	return len(p), nil
}

// Park hard-stops the engine and latches the parked flag so request
// traffic (the per-request EnsureRunning) cannot revive it until Unpark.
// This is the operator-driven "free my VRAM/RAM" action (#186). In reuse
// mode it returns ErrEngineBorrowed without touching the user's process —
// there is nothing waired may stop, and pretending otherwise would lie
// about the memory being freed.
func (a *OllamaAdapter) Park(ctx context.Context) error {
	a.mu.Lock()
	if a.cfg.Borrowed {
		a.mu.Unlock()
		return ErrEngineBorrowed
	}
	if a.adopted {
		// An adopted orphan has no process handle — we cannot free its
		// memory, and pretending otherwise would lie to the operator.
		a.mu.Unlock()
		return ErrEngineNotOwned
	}
	a.parked = true
	a.mu.Unlock()
	// Stop is a no-op when no process is running (e.g. parking before
	// first start), so this is safe in every state.
	return a.Stop(ctx)
}

// Unpark clears the parked latch so a subsequent EnsureRunning may spawn
// the engine again. It does NOT start the engine itself — the caller
// (engineController.StartEngine) kicks EnsureRunning afterwards.
func (a *OllamaAdapter) Unpark() {
	a.mu.Lock()
	a.parked = false
	a.mu.Unlock()
}

// IsParked reports whether the engine is currently hard-stopped (#186).
func (a *OllamaAdapter) IsParked() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.parked
}

// Borrowed reports reuse mode (#188): the engine is the user's own
// `ollama serve`, not managed by waired, so the power axis can't free it.
func (a *OllamaAdapter) Borrowed() bool { return a.cfg.Borrowed }

func (a *OllamaAdapter) setState(h Health) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state = h
}

// DefaultSpawner and the osProcess type that implements RunningProcess
// live in spawner_unix.go and spawner_windows.go because subprocess
// lifecycle on the two platforms is incompatible: Unix uses process
// groups + signal-to-pgid; Windows uses Job Objects + handle close.
// The shared Spawner / RunningProcess contracts are in adapter.go.
