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
	"path/filepath"
	"runtime"
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

	// Host is the loopback address the engine binds to (always
	// 127.0.0.1 in production; unit tests point at httptest).
	Host string
	// Port is the engine's listening port: the waired-owned port
	// (agentconfig.DefaultOllamaBundledPort), so it never contends with
	// a user's system ollama on 11434.
	Port int
	// ModelsDir, when non-empty, is exported as OLLAMA_MODELS to the
	// spawned engine so blobs live in a waired-owned directory
	// (<state-dir>/runtimes/ollama/models). Empty keeps the engine's own
	// default (tests).
	ModelsDir string
	// ExpectedVersion is the exact version (GET /api/version) an
	// EADDRINUSE survivor on our port must report to be adopted as an
	// orphan of a previous waired run. Production wires the pinned
	// version; empty disables adoption entirely. Any other survivor is
	// a foreign engine and EnsureRunning fails loudly instead of
	// silently serving from an unpinned engine.
	ExpectedVersion string
	// KeepAlive is how long the engine holds a model in (V)RAM after its
	// last request, exported as OLLAMA_KEEP_ALIVE. Zero or negative
	// means indefinitely. Empty (the zero Duration) is resolved by
	// ResolveKeepAlive to the product default; see its doc for why that
	// default is "hold".
	KeepAlive time.Duration
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
	// before declaring failed (default 3). It bounds steady-state
	// probing only — for a SPAWNED engine the adapter owns
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
	// spawn, size-capped). Empty discards the output (tests). Without it
	// a failed `ollama serve` leaves no trail and "not ready" is
	// undiagnosable in the field.
	LogDir string
	// StateHome is a writable, agent-owned directory used as $HOME for the
	// spawned `ollama serve` when the agent's own environment has none.
	// macOS system LaunchDaemons start with $HOME unset, and `ollama serve`
	// aborts at startup with "Error: $HOME is not defined" — it resolves
	// ~/.ollama for its key/config even when OLLAMA_MODELS redirects the
	// model blobs. Empty leaves any inherited HOME untouched (#22).
	StateHome string
	// StopTimeout is how long Stop waits after SIGTERM before
	// SIGKILL (default 5s).
	StopTimeout time.Duration
	// OnUnhealthy, when set, is called once per detected engine death with
	// the reason (including a tail of engine.log). The adapter has already
	// moved to StateFailed by then; the callback owns the recovery policy
	// — how many restarts, how spaced, and when to give up honestly —
	// because the adapter has no view of the model, the operator, or the
	// other engines. Invoked on its own goroutine, so it may call back in.
	OnUnhealthy func(detail string)
	// OnStartFailed, when set, is called once per start ATTEMPT that ended
	// without the engine serving.
	//
	// Distinct from OnUnhealthy on purpose. That one means "it WAS serving
	// and died", and its handler answers by scheduling a restart. This one
	// means "it never came up", and by the time it fires the caller has
	// already retried on its own budget — so its handler counts strikes
	// and gives up, it does not respawn.
	//
	// Without it an engine that is killed at exec — the macOS bundle whose
	// signature no longer verifies — recorded nothing at all: markUnhealthy
	// demotes only out of StateReady, which such an engine never reaches,
	// so no strike was counted, no latch was set, and every surface went on
	// reporting an engine that was merely "not ready yet" (#310).
	//
	// Invoked on its own goroutine, so it may call back in. Only the caller
	// that won the single-flight gate fires it: a burst of gateway requests
	// joining one failing start is one attempt, not one per request.
	OnStartFailed func(detail string)
}

// ErrEngineParked is returned by EnsureRunning when the engine has been
// administratively parked (hard-stopped via the engine power axis, #186).
// The gateway maps it to a 503 so request traffic does NOT resurrect an
// engine the operator explicitly stopped to free memory.
var ErrEngineParked = errors.New("ollama: engine parked (stopped by operator)")

// ErrEngineNotOwned is returned by Park when the engine was adopted as
// an orphan of a previous run: there is no process handle to signal, so
// waired cannot free its memory.
var ErrEngineNotOwned = errors.New("ollama: engine adopted from a previous run, not stoppable by waired")

// EngineMode describes who owns the serving engine process.
type EngineMode string

const (
	// EngineModeSpawned: the engine is waired's own supervised child
	// (the normal outcome).
	EngineModeSpawned EngineMode = "spawned"
	// EngineModeAdopted: an exact-pin orphan from a previous waired
	// run answered on our port; serving from it, but with no process
	// handle (Stop/Park cannot signal it).
	EngineModeAdopted EngineMode = "adopted"
)

// KeepAliveIndefinite is the OLLAMA_KEEP_ALIVE / keep_alive value that
// disables the idle unload. Ollama maps any negative duration to
// "forever" (a bare integer is read as seconds), so -1 is the portable
// spelling for both the environment variable and the per-request field.
const KeepAliveIndefinite = "-1"

// ResolveKeepAlive renders an idle timeout as the value the engine
// expects, for both OLLAMA_KEEP_ALIVE and the per-request keep_alive.
//
// Zero or negative is indefinite. That is the product default
// (agentconfig.Defaults sets Inference.IdleTimeout to 0), decided on
// measurement rather than on the previous 60m guess:
//
//   - Letting the model expire costs a weights reload AND a full
//     prefill on the next request — 16.9 / 43.4 / 55.8 s on the three
//     measured hosts, of which prefill is 57-77% (waired-agent#861).
//   - Only the weights half can be warmed back. The prompt cache is
//     keyed by the token sequence of a real request, so restoring it
//     after an expiry would mean replaying that request; replaying a
//     synthetic prefix instead recovered nothing on the models these
//     hosts serve.
//   - Holding is already arbitrated inside waired: under
//     MaxResidentModels one request for a different model evicts the
//     held one, indefinite keep-alive or not (owner ruling 2026-08-10,
//     waired-agent#644).
//
// See docs/decisions/20260820/0130-model-residency-is-a-setting.md.
func ResolveKeepAlive(idle time.Duration) string {
	if idle <= 0 {
		return KeepAliveIndefinite
	}
	return idle.String()
}

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
	// keepAlive is the live model-residency setting (#861). Seeded from
	// cfg.KeepAlive and changed at runtime by SetKeepAlive, because
	// OLLAMA_KEEP_ALIVE is only read at spawn (processEnv) and bouncing
	// the engine to apply a residency change would drop the very
	// residency being configured. Guarded by mu.
	keepAlive time.Duration
	// residency is the last observed answer to "are the weights in
	// (V)RAM right now" (#879), refreshed off the local inference probe
	// loop. Every readiness signal in the product bottoms out at
	// "process alive + model file on disk", so an engine that unloaded
	// an hour ago and one mid-token report the same thing — while the
	// first spends a weights reload and a full prefill before its first
	// token (#861). Zero value means "never observed", which is not the
	// same as "not resident". Guarded by mu.
	residency ModelResidency
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
	// actually answering requests — the two differ in adopted mode.
	// Guarded by mu.
	liveVersion string
	// logFile is the open <LogDir>/engine.log handle for the current
	// spawned child (nil when LogDir is unset or the engine is not
	// running). Re-opened (rotated) on each spawn and closed when the
	// process is stopped, so it tracks the child's lifetime. Guarded by mu.
	logFile *os.File
	// ensuring is non-nil while one EnsureRunning is in flight; later
	// callers JOIN it instead of erroring. Before waired-agent#29 a
	// concurrent entry during StateStarting returned a hard error, which
	// the gateway mapped to 503 runtime_unhealthy — and a crash-recovery
	// restart is precisely what makes entry concurrent, so recovery
	// without this would trade a permanent 500 for a wall of 503s.
	// Guarded by mu.
	ensuring chan struct{}
	// ensureErr is the in-flight EnsureRunning's result, published to
	// joiners when ensuring closes. Guarded by mu.
	ensureErr error
	// procGen counts spawned-process generations. superviseChild pins the
	// generation it watches so a DELIBERATE Stop / Park / reconcile bounce
	// — which also closes proc.Done() — is never reported as a crash.
	// Guarded by mu.
	procGen uint64
	// lastUnhealthy debounces markUnhealthy: the observed failure mode is
	// ~90 requests over 6 minutes all receiving the same engine 500, and
	// every one of them lands there. Guarded by mu.
	lastUnhealthy time.Time
	// giveUp latches "repeatedly crashed; stop respawning" so a
	// deterministically-crashing model cannot turn every request into a
	// fresh 150-second spawn attempt. Cleared by ClearFailure (which
	// engineController.StartEngine calls). Guarded by mu.
	giveUp bool
	// giveUpErr is the reason recorded when giveUp latched. Guarded by mu.
	giveUpErr string
}

// FailureReporter is an OPTIONAL interface a LOCAL, waired-supervised
// adapter implements when an engine's error reply can prove the ENGINE is
// broken rather than the request being bad. The gateway calls it for every
// non-2xx from the selected adapter; the adapter — which knows its own
// engine's error vocabulary — decides whether that means "dead".
//
// Peer adapters deliberately do NOT implement it, so a remote peer's 500
// can never demote THIS host's engine.
type FailureReporter interface {
	ReportUpstreamFailure(status int, body []byte)
}

// ErrEngineUnrecoverable is returned by EnsureRunning once automatic
// recovery has given up. The reason is in Health().LastErr, which the
// mgmt API, `waired status`, and the tray already surface.
var ErrEngineUnrecoverable = errors.New("ollama: engine repeatedly crashed; not retrying (see last_error)")

// engineDeadMarkers are substrings of an ollama error body that prove the
// MODEL RUNNER is gone rather than the request being bad. The parent
// `ollama serve` keeps answering /api/tags with 200 after its llama-server
// child dies, so this reply is the only signal separating "engine broken"
// from "bad request".
var engineDeadMarkers = []string{
	"process has terminated",
	"model runner has unexpectedly stopped",
}

// unhealthyDebounce collapses a burst of engine failures into one report.
const unhealthyDebounce = 2 * time.Second

// engineDeadBody reports whether an engine error body names a dead runner.
func engineDeadBody(body []byte) bool {
	s := string(body)
	for _, m := range engineDeadMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// ReportUpstreamFailure implements FailureReporter: it demotes the engine
// out of StateReady when its own error reply proves the model runner died.
//
// Requiring the marker (rather than treating any 5xx as fatal) is what
// keeps a one-off bad request from bouncing a healthy engine. An unmatched
// 5xx is logged instead, as a canary: if a future pinned ollama rewords
// its terminal error, detection silently reverts to the old behaviour and
// this line is the only way to notice.
func (a *OllamaAdapter) ReportUpstreamFailure(status int, body []byte) {
	if status < 500 {
		return // a 4xx is the request's fault, not the engine's
	}
	if !engineDeadBody(body) {
		slog.Warn("ollama returned 5xx with no dead-runner marker",
			"status", status, "body", firstLine(body))
		return
	}
	a.markUnhealthy(fmt.Sprintf("engine returned HTTP %d: %s", status, firstLine(body)))
}

// firstLine returns up to the first 300 bytes of b's first line.
func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// markUnhealthy demotes a serving engine out of StateReady and notifies the
// owner exactly once per death. The StateReady check under the mutex is
// what collapses a request burst to one notification; the debounce is
// belt-and-braces against a Ready→dead→Ready flap counting several strikes.
func (a *OllamaAdapter) markUnhealthy(detail string) {
	a.mu.Lock()
	if a.parked || a.state.State != StateReady || time.Since(a.lastUnhealthy) < unhealthyDebounce {
		a.mu.Unlock()
		return
	}
	a.lastUnhealthy = time.Now()
	// Fold the crash trace into LastErr NOW: an automatic restart is about
	// to reopen engine.log, and LastErr is what the mgmt API, `waired
	// status` and the tray surface without any change on their side.
	if tail := tailEngineLog(a.engineLogPath(), engineLogTailMaxBytes); tail != "" {
		detail += "\n--- ollama serve stderr (tail, full log: " + a.engineLogPath() + ") ---\n" + tail
	}
	a.state = Health{State: StateFailed, LastErr: detail}
	cb := a.cfg.OnUnhealthy
	a.mu.Unlock()
	if cb != nil {
		// Off-lock and on its own goroutine: the handler calls back into
		// IsParked()/Health(), which take a.mu.
		go cb(detail)
	}
}

// superviseChild watches a spawned child AFTER it reached Ready. Before
// waired-agent#29 nothing watched it once waitReady returned, so a crash
// left the adapter latched Ready for the rest of the process lifetime.
//
// gen pins the process generation, so a deliberate Stop/Park/reconcile
// bounce (which also closes proc.Done()) is not mistaken for a crash.
func (a *OllamaAdapter) superviseChild(proc RunningProcess, gen uint64) {
	<-proc.Done()
	a.mu.Lock()
	stale := a.procGen != gen
	a.mu.Unlock()
	if stale {
		return
	}
	a.markUnhealthy(startupExitError("ollama", a.engineLogPath(), proc.Err()).Error())
}

// LatchFailed marks the engine unrecoverable until ClearFailure. The
// per-request EnsureRunning then returns ErrEngineUnrecoverable instead of
// respawning. Symmetric with Park/Unpark/IsParked (#186), the existing
// vocabulary for "the engine is deliberately not coming back".
func (a *OllamaAdapter) LatchFailed(detail string) {
	a.mu.Lock()
	a.giveUp = true
	a.giveUpErr = detail
	a.state = Health{State: StateFailed, LastErr: detail}
	a.mu.Unlock()
}

// ClearFailure releases the LatchFailed latch and the crash bookkeeping so
// a manual start (or a model switch) can try again.
func (a *OllamaAdapter) ClearFailure() {
	a.mu.Lock()
	a.giveUp = false
	a.giveUpErr = ""
	a.lastUnhealthy = time.Time{}
	a.mu.Unlock()
}

// FailureLatched reports whether automatic recovery has given up.
func (a *OllamaAdapter) FailureLatched() bool {
	latched, _ := a.FailureLatchedReason()
	return latched
}

// FailureLatchedReason reports the latch AND the reason it latched with,
// under one lock so the pair cannot tear.
//
// Health().LastErr is the wrong source for that reason even though
// LatchFailed writes it there too. a.state has a different lifetime:
// Stop() overwrites it with no giveUp guard, so a model switch, a
// reconcile bounce or a park leaves a latched engine reporting
// giveUp=true with LastErr="" — a red setup row with nothing on it, and
// a runtime whose last_error vanished off the wire. giveUpErr is cleared
// only by ClearFailure, which is exactly the lifetime of the latch it
// explains (#310).
func (a *OllamaAdapter) FailureLatchedReason() (bool, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.giveUp, a.giveUpErr
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
		keepAlive:  cfg.KeepAlive,
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

// EnsureRunning starts the Ollama subprocess (if not already running) and
// blocks until either the engine is StateReady or the readiness probe gives
// up. Already-ready returns immediately; a call that arrives while another
// is starting JOINS it and returns its result.
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
	if a.giveUp {
		e := a.giveUpErr
		a.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrEngineUnrecoverable, e)
	}
	if a.state.State == StateReady {
		a.mu.Unlock()
		return nil
	}
	if ch := a.ensuring; ch != nil {
		// Join the in-flight start rather than erroring. This used to be a
		// hard error that the gateway mapped to 503 runtime_unhealthy for
		// every concurrent request (waired-agent#29).
		a.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err() // the joiner's own deadline still applies
		}
		a.mu.Lock()
		err := a.ensureErr
		a.mu.Unlock()
		return err
	}
	done := make(chan struct{})
	a.ensuring = done
	a.state = Health{State: StateStarting}
	needReap := a.proc != nil
	if needReap {
		// Bump the generation BEFORE the stop so superviseChild treats the
		// exit as ours, not as a fresh crash.
		a.procGen++
	}
	a.mu.Unlock()

	// Reap a dead child before respawning: after a crash a.proc still holds
	// the exited process and Spawner.Spawn would leak the old handle. On an
	// already-exited process stopProcess returns immediately and closes the
	// log handle — which is also what makes the rotation in openEngineLog safe.
	if needReap {
		_ = a.stopProcess(context.Background())
		a.mu.Lock()
		a.proc = nil
		a.mu.Unlock()
	}

	var err error
	defer func() {
		a.mu.Lock()
		a.ensureErr = err
		a.ensuring = nil
		// Read the park latch here, not in the handler: Park may have
		// landed during the slow readiness wait, and the teardown it
		// causes is not a start failure.
		parked := a.parked
		cb := a.cfg.OnStartFailed
		a.mu.Unlock()
		close(done)
		// The single funnel for a start that did not end with the engine
		// serving: every failure exit of ensureRunningLeader passes
		// through this deferred block, and only the leader reaches it
		// (the giveUp/parked/ready/joiner returns above all come before
		// `done` exists). Off-lock and on its own goroutine, like
		// markUnhealthy: the handler calls back into Health()/IsParked().
		if cb != nil && startFailureIsEvidence(err, parked) {
			go cb(err.Error())
		}
	}()
	err = a.ensureRunningLeader(ctx)
	return err
}

// startFailureIsEvidence reports whether a finished start attempt says
// anything about the engine's health.
//
// A pure decision rather than an inline condition, because the interesting
// case is one the integration tests can only reach by racing: Park landing
// DURING a slow readiness wait tears the child down, and the teardown error
// that comes back is the operator's own hard stop working — charging it as a
// strike would let `waired inference engine stop` help spend the recovery
// budget. Same reasoning as markUnhealthy's parked check, and now testable
// without a timing window.
//
// Both parked signals matter: the re-check inside the leader returns
// ErrEngineParked, while a Park that instead killed the child mid-wait
// surfaces as an ordinary startup error with only the flag to go on.
func startFailureIsEvidence(err error, parked bool) bool {
	return err != nil && !parked && !errors.Is(err, ErrEngineParked)
}

// ensureRunningLeader is EnsureRunning's body, run by whichever caller won
// the single-flight gate. Split out only so the gate's bookkeeping can live
// in one deferred block regardless of which of the many exits below is taken.
func (a *OllamaAdapter) ensureRunningLeader(ctx context.Context) error {
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
	a.procGen++
	procGen := a.procGen
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
			msg := fmt.Sprintf("ollama: not ready within %s (engine still starting; see %s)",
				a.cfg.StartupReadyTimeout, a.engineLogPath())
			if tail := tailEngineLog(a.engineLogPath(), engineLogTailMaxBytes); tail != "" {
				msg = fmt.Sprintf("%s\n--- ollama serve stderr (tail) ---\n%s", msg, tail)
			}
			err = errors.New(msg)
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
					"refusing to adopt it. Stop that process, or set inference.ollama_port in "+
					"agent.json to a free port",
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
	// Keep watching the child now that it is serving. Before
	// waired-agent#29 nothing did: waitReady's exit watcher ended with the
	// readiness wait, so a crash left the adapter latched Ready forever.
	go a.superviseChild(proc, procGen)
	return nil
}

// processEnv returns the environment variables passed to `ollama
// ollamaTuningKeys are the #621 serve-tuning variables. They are dropped from
// the inherited environment as a SET (not just the ones we emit) whenever a
// tuning was computed — see processEnv.
var ollamaTuningKeys = []string{
	"OLLAMA_CONTEXT_LENGTH",
	"OLLAMA_KV_CACHE_TYPE",
	"OLLAMA_NUM_PARALLEL",
	"OLLAMA_FLASH_ATTENTION",
}

// ollamaMaxLoadedModelsEnv caps how many models `ollama serve` keeps
// resident (MaxResidentModels). Deliberately NOT in ollamaTuningKeys: it is
// not part of the #621 per-model tuning, and adding it there would drop the
// operator's own value whenever any tuning was computed.
const ollamaMaxLoadedModelsEnv = "OLLAMA_MAX_LOADED_MODELS"

// serve`, derived from the parent process env plus the spec-mandated
// overrides and the GPU-backend selection (#290). Any inherited env var
// whose key we set ourselves is dropped from the base so our value wins
// regardless of getenv's first-vs-last duplicate resolution.
func (a *OllamaAdapter) processEnv() []string {
	a.mu.Lock()
	backend := a.backendEnv
	model := a.modelEnv
	keepAlive := a.keepAlive
	a.mu.Unlock()

	// Launch-environment guards come from the shared ChildBaseEnv so the
	// ollama/vLLM spawn paths stay in parity on the axis the #22
	// crash exposed:
	//   - HOME: macOS system LaunchDaemons start $HOME unset and `ollama
	//     serve` aborts with "$HOME is not defined" (it resolves ~/.ollama
	//     for its key/config even when OLLAMA_MODELS is redirected). Supply
	//     the runtime's StateHome only when the launcher gave none; never
	//     override an inherited HOME (#22).
	//   - PATH: launchd hands a system daemon a stripped PATH
	//     (/usr/bin:/bin:/usr/sbin:/sbin); prepend the resolved ollama
	//     binary's own dir so any sidecar it execs is still found (mirrors
	//     vLLM's venv-bin prepend).
	binDir := ""
	if a.cfg.Binary != "" {
		binDir = filepath.Dir(a.cfg.Binary)
	}
	base := ChildBaseEnv(runtime.GOOS, os.Environ(), a.cfg.StateHome, binDir, string(os.PathListSeparator))

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
	if len(model) > 0 {
		// When we have a computed serve tuning at all, drop EVERY tuning key
		// from the inherited environment — including ones the tuning
		// deliberately omits. An omission means "let the engine choose", and
		// an inherited value would silently override that: since the tuning
		// stopped always exporting OLLAMA_FLASH_ATTENTION (waired-agent#29), a
		// stray value in /etc/waired/agent.env or a developer shell would
		// otherwise re-arm exactly the combination this host opted out of.
		// With no computed tuning (no resolvable
		// target) the operator's own values are left alone.
		for _, k := range ollamaTuningKeys {
			drop[k] = true
		}
	}

	out := make([]string, 0, len(base)+3+len(backend)+len(model)+len(a.cfg.ExtraEnv))
	for _, kv := range base {
		if drop[envKey(kv)] {
			continue
		}
		out = append(out, kv)
	}
	out = append(out,
		fmt.Sprintf("OLLAMA_HOST=%s:%d", a.cfg.Host, a.cfg.Port),
		"OLLAMA_NO_CLOUD=1",
		"OLLAMA_KEEP_ALIVE="+ResolveKeepAlive(keepAlive),
	)
	// MaxResidentModels, delivered. Emitted HERE and not by ollamaTuning.Env()
	// even though it is a serve variable: a tuning only exists once a serve
	// target resolves, and the host-speed measurement this cap exists to
	// protect runs on hosts that have no target yet (a fresh install boots on
	// an untuned plan). An engine-wide invariant does not belong to the
	// per-model tuning that happens to be computed alongside it.
	//
	// The operator keeps the last word, read explicitly rather than left to
	// the child's first-vs-last duplicate resolution: this key is absent from
	// the drop set precisely so an /etc/waired/agent.env line survives in
	// base, and appending ours unconditionally would put two values in front
	// of getenv. Set-but-empty is the opt-out — it asks for the engine's own
	// default, so nothing is added back.
	if _, operatorSet := os.LookupEnv(ollamaMaxLoadedModelsEnv); !operatorSet {
		out = append(out, fmt.Sprintf("%s=%d", ollamaMaxLoadedModelsEnv, MaxResidentModels))
	}
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

// SetOnUnhealthy installs the engine-death handler after construction. The
// production handler is a provider method, and the provider is built from the
// adapter — so the callback cannot be part of the initial config. Same shape
// as SetModelEnvProvider above.
func (a *OllamaAdapter) SetOnUnhealthy(fn func(detail string)) {
	a.mu.Lock()
	a.cfg.OnUnhealthy = fn
	a.mu.Unlock()
}

// SetOnStartFailed installs the failed-start handler after construction, for
// the same reason as SetOnUnhealthy above (#310).
func (a *OllamaAdapter) SetOnStartFailed(fn func(detail string)) {
	a.mu.Lock()
	a.cfg.OnStartFailed = fn
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
	// ObservedNumParallel is the request parallelism the model runner is
	// ACTUALLY serving, read from its command line after load (#763). 0 =
	// not observed; when non-zero it overrides NumParallel (the intent) in
	// the inference status, because Ollama silently caps OLLAMA_NUM_PARALLEL
	// when the per-slot KV cache does not fit the configured window.
	ObservedNumParallel int
	// NumBatch is the generation ubatch the serve tuning selected (#642);
	// 0 means "left to Ollama's automatic batch sizing". Unlike the other
	// fields it is NOT delivered via an OLLAMA_* env (the pinned 0.31.1
	// exposes none) but through a locally derived model carrying
	// PARAMETER num_batch — see cmd/waired-agent/inference_ollama_derived.go.
	// Non-zero only on spilled discrete-GPU hosts, where Ollama's own
	// automaticGenerationBatch would otherwise fall back to 512.
	NumBatch int
	// KVCacheType is the OLLAMA_KV_CACHE_TYPE the sizing assumed — "q8_0"
	// where halving the KV cache buys context, "f16" where it does not (or
	// when the post-load check detected the engine fell back).
	KVCacheType string
	// FlashAttention is whether OLLAMA_FLASH_ATTENTION=1 is exported. It
	// tracks KVCacheType and is never set independently: Ollama silently
	// degrades a quantized KV cache to f16 without flash attention, so a
	// quantized cache REQUIRES it — and on an f16 cache it would force
	// llama.cpp's flash-attention path for no memory saving at all.
	FlashAttention bool
	// WindowFits is true when ContextLength is a window the sizing rules
	// proved this host holds (hostfit.OllamaRungPlan.Fits on the ollama
	// path; a real vLLM estimate on that path). False marks a window the
	// engine was given anyway — the lowest rung, forced because sub-rung
	// windows are not served (waired-ai/waired-agent#587).
	//
	// It no longer gates the mesh declaration. Such a window is served,
	// and a served window is declared: spill costs decode speed, not
	// window size, so withholding it made a host that answers real
	// requests invisible to the mesh at every session size
	// (waired-ai/waired-agent#657). Today the flag records WHY the host
	// is on that rung, for the local decision-reason wording only.
	WindowFits bool
	// Verified is true once the post-load /api/ps verification completed
	// (regardless of outcome).
	Verified bool
	// Warning is a user-visible note (context floored, f16 fallback,
	// spill detected, reused engine ignores tuning); "" when healthy.
	Warning string
}

// ServeInputsEqual reports whether t and o would produce the same engine
// process: same model, same window, same cache, same parallelism, same
// batch. It compares the INPUTS only — ModelID, VariantID,
// ContextLength, NumParallel, NumBatch, KVCacheType, FlashAttention.
//
// The fields it deliberately ignores are the ones this struct accretes
// AFTER the spawn, describing the outcome rather than the intent:
// Verified and Warning are written by the post-load verification,
// ObservedNumParallel by reading the runner's command line,
// RecommendedMaxParallel is advisory telemetry, and WindowFits is the
// sizing's own judgement of the window — a pure function of the inputs
// already compared, so comparing it too could never change the answer. A freshly computed
// tuning has none of them, so a whole-struct `==` against the applied
// value would differ on every single call — and a bounce predicate built
// on that would restart the engine each time anything asked for a
// reconcile.
//
// This exists because the predicate it replaces compared NumParallel
// alone, so a reconcile that resolved a DIFFERENT MODEL at the same
// parallelism returned without exporting anything (waired-agent#320).
func (t ModelTuning) ServeInputsEqual(o ModelTuning) bool {
	return t.ModelID == o.ModelID &&
		t.VariantID == o.VariantID &&
		t.ContextLength == o.ContextLength &&
		t.NumParallel == o.NumParallel &&
		t.NumBatch == o.NumBatch &&
		t.KVCacheType == o.KVCacheType &&
		t.FlashAttention == o.FlashAttention
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

// ModelResidency is one observation of whether the engine currently holds
// weights in (V)RAM, and until when (#879).
//
// Residency is the difference between a first token in ~0.5 s and one in
// 17-56 s (#861), and before this nothing on any surface carried it: the
// CLI, the tray and the peer health probe all bottom out at "process
// alive + model file on disk". `Observed` distinguishes "we looked and
// nothing is loaded" from "we have not looked", which are different
// answers a caller must not merge.
type ModelResidency struct {
	// Observed is false until a probe has actually read /api/ps. Callers
	// must not render the rest of this struct when it is false.
	Observed bool
	// Model is the engine-native tag that is resident, empty when none is.
	Model string
	// Until is when the engine intends to unload it. Zero when nothing is
	// resident, and also when the engine reports an expiry the agent could
	// not parse. Note an indefinite keep-alive renders as a date centuries
	// out rather than a sentinel, so callers must not treat a far-future
	// value as an error.
	Until time.Time
	// At is when the observation was taken, so a stale reading is
	// recognisable rather than silently presented as current.
	At time.Time
}

// Resident reports whether a model was loaded at the last observation.
func (r ModelResidency) Resident() bool { return r.Observed && r.Model != "" }

// KeepAlive renders this engine's configured residency for a per-request
// keep_alive field, so a caller that must not rely on the serve-level
// variable (an adopted engine's environment is a previous run's, not
// ours — waired-agent#320) sends the same value the spawn would export.
func (a *OllamaAdapter) KeepAlive() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return ResolveKeepAlive(a.keepAlive)
}

// KeepAliveDuration returns the live setting itself, for the surfaces
// that render or persist it rather than send it to the engine.
func (a *OllamaAdapter) KeepAliveDuration() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.keepAlive
}

// SetKeepAlive changes the residency setting for later spawns and for
// per-request keep_alive values. It does not itself touch a model that
// is already resident: the caller re-stamps that by loading it again
// with the new value, which the engine applies to the loaded copy
// without a reload (#861).
func (a *OllamaAdapter) SetKeepAlive(idle time.Duration) {
	a.mu.Lock()
	a.keepAlive = idle
	a.mu.Unlock()
}

// SetResidency records a /api/ps observation for the status surfaces.
func (a *OllamaAdapter) SetResidency(r ModelResidency) {
	a.mu.Lock()
	a.residency = r
	a.mu.Unlock()
}

// Residency returns the last observed model residency. The zero value
// (Observed false) means no probe has run yet.
func (a *OllamaAdapter) Residency() ModelResidency {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.residency
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
// supervised selects the failure policy. Since #489 every production
// start spawns a child, so the false arm is the policy the true one is
// defined against rather than a path production takes:
//   - false (no owned child): give up after HealthMaxFails consecutive
//     failures — there is no process to watch, so a quick failure is the
//     only signal and the right one.
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
				// A supervised child that has ALREADY exited must not be
				// reported ready, however healthy the port looks: something
				// else is answering there — normally an orphan of a previous
				// agent run, which is what the adopt path below EnsureRunning's
				// waitReady call exists to handle. Returning nil here instead
				// hands the caller a live-looking engine backed by a dead
				// process handle, so Stop()/Park() signal a corpse and report
				// success while the real engine keeps serving.
				//
				// The select at the bottom of this loop already returns on
				// procDone, but only when it is the only ready case. Once a
				// probe takes longer than one HealthInterval both it and
				// tick.C are ready and Go picks between them uniformly at
				// random, so the exit is observed or missed on a coin flip —
				// which is how TestEngineController_AdoptedNotManaged became
				// an intermittent failure, and why it turned up as soon as
				// the suite ran on a slower Windows runner (#216).
				if supervised {
					a.mu.Lock()
					proc := a.proc
					a.mu.Unlock()
					if proc != nil {
						select {
						case <-proc.Done():
							return startupExitError("ollama", a.engineLogPath(), proc.Err())
						default:
						}
					}
				}
				return nil
			}
		} else {
			consecFail++
			consecOK = 0
			if !supervised && consecFail >= a.cfg.HealthMaxFails {
				return fmt.Errorf("ollama: not ready after %d failed probes", consecFail)
			}
		}

		// With no child process procDone stays nil (a nil channel never
		// fires) so we rely on the probe + ctx only.
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
			return startupExitError("ollama", a.engineLogPath(), proc.Err())
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

// Mode reports who owns the serving engine process. Adopted is
// discovered at EnsureRunning time; the default (including before first
// start) is spawned.
func (a *OllamaAdapter) Mode() EngineMode {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case a.adopted:
		return EngineModeAdopted
	default:
		return EngineModeSpawned
	}
}

// ProcessGeneration returns the engine's current process generation. It
// moves whenever this adapter retires a child or spawns one — a Stop, a
// Park, a reconcile bounce, a backend fallback, a tuning-degrade restart,
// the reap of a dead child before a respawn — so a caller that samples it
// around a long operation can tell that the engine it was talking to is
// not the engine running now.
//
// It does NOT move on a crash. That is deliberate and is the same
// property superviseChild depends on: an exit with an unchanged
// generation is a crash, and one with a changed generation is ours.
//
// Counting our own stops is what lets `ollama pull` — a CLIENT of
// `ollama serve` — tell "waired restarted the engine under me" from a
// genuine download failure, without classifying the error text
// (waired-agent#359). Note the ordering an accurate reading needs: sample
// AFTER the caller's own EnsureRunning, since that reaps and respawns a
// dead child and would otherwise look like an interruption.
func (a *OllamaAdapter) ProcessGeneration() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.procGen
}

// Stop terminates the Ollama subprocess gracefully (SIGTERM, then
// SIGKILL after StopTimeout).
func (a *OllamaAdapter) Stop(ctx context.Context) error {
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

// stopProcess terminates the running child. Once it has started, it
// COMMITS TO THE KILL: no caller-side cancellation may return while the
// process is still alive (#316). The engine power axis latches "stopped"
// before the stop runs, so abandoning a live child mid-stop would make
// status claim the memory was freed while the engine still pinned VRAM —
// and the parked latch would then block EnsureRunning from reviving it.
func (a *OllamaAdapter) stopProcess(ctx context.Context) error {
	a.mu.Lock()
	proc := a.proc
	// Retire this generation before signalling: proc.Done() closes on a
	// deliberate stop too, and superviseChild must not report that as a crash.
	a.procGen++
	a.mu.Unlock()
	if proc == nil {
		return nil
	}
	// Best-effort SIGTERM. An ordinary error means the receiver already
	// exited and is ignored; ErrSignalUnsupported means the platform
	// cannot deliver signals at all (Windows), so waiting out StopTimeout
	// for a graceful exit would be waiting for something nobody asked for.
	if err := proc.Signal(syscall.SIGTERM); errors.Is(err, ErrSignalUnsupported) {
		return a.killAndReap(proc)
	}
	select {
	case <-proc.Done():
		a.closeEngineLog()
		return nil
	case <-time.After(a.cfg.StopTimeout):
		// Escalate.
		return a.killAndReap(proc)
	case <-ctx.Done():
		// The caller gave up waiting (a tray budget, a cancelled HTTP
		// request). The child does not get to survive that: escalate now
		// rather than leaving it alive behind a "stopped" power state.
		slog.Warn("ollama: stop caller cancelled; killing the engine rather than abandoning it",
			"pid", proc.PID(), "err", ctx.Err())
		return a.killAndReap(proc)
	}
}

// killAndReap force-kills proc and waits for it to be collected, closing
// the engine log on every exit path. The reap wait is bounded by the same
// StopTimeout budget the graceful phase uses: the pre-#316 code waited on
// Done() forever, so a child the OS refused to collect hung the
// management handler (and the tray behind it) for good.
func (a *OllamaAdapter) killAndReap(proc RunningProcess) error {
	if err := proc.Kill(); err != nil {
		a.closeEngineLog()
		return fmt.Errorf("ollama: kill: %w", err)
	}
	select {
	case <-proc.Done():
		a.closeEngineLog()
		return nil
	case <-time.After(a.cfg.StopTimeout):
		a.closeEngineLog()
		return fmt.Errorf("ollama: process %d did not exit within %s of being killed", proc.PID(), a.cfg.StopTimeout)
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
	// Rotate rather than truncate: automatic crash recovery respawns the
	// engine, and O_TRUNC would destroy the trace explaining WHY it crashed
	// — the only artifact that reaches CI, `waired doctor`, and a bug report
	// (waired-agent#29). Exactly one generation is kept, so the on-disk cost
	// stays bounded at 2 x engineLogMaxBytes. Best-effort like the rest of
	// this function: a rename failure falls through to the truncating open
	// rather than blocking the engine coming up.
	_ = os.Rename(a.engineLogPath(), a.engineLogPath()+".1")
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

// EngineLogTail returns up to maxBytes from the end of this adapter's
// engine.log, or "" when no log directory is configured or the file is
// missing. Callers outside this package use it to read what the engine
// said about a load, rather than inferring it: the runner's process
// table carries what it DID, and only the log carries why
// (waired-ai/waired-agent#877).
//
// It is a raw read. A caller putting any of it in front of a user owns
// bounding and sanitising what it extracts — the file is another
// program's stdout and carries filesystem paths.
func (a *OllamaAdapter) EngineLogTail(maxBytes int) string {
	return tailEngineLog(a.engineLogPath(), maxBytes)
}

// engineLogTailMaxBytes bounds how much of engine.log is folded into a
// startup-failure error (#22). The full capture can be several MB; the
// last few KB carries the actual crash reason into last_error / slog
// without bloating the mgmt-API status JSON.
const engineLogTailMaxBytes = 4 << 10 // 4 KiB

// tailEngineLog returns up to maxBytes from the END of the file at path,
// or "" when path is empty, the file is missing/empty, or unreadable. It
// is best-effort diagnostics for a startup failure, so any problem yields
// no tail rather than an error (the caller still surfaces the exit code).
// Shared by the ollama and vLLM adapters (identical engine.log capture).
func tailEngineLog(path string, maxBytes int) string {
	if path == "" || maxBytes <= 0 {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return ""
	}
	if fi.Size() > int64(maxBytes) {
		if _, err := f.Seek(-int64(maxBytes), io.SeekEnd); err != nil {
			return ""
		}
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// startupExitError wraps a spawned-engine startup exit (the crash caught
// by an adapter's process-exit channel) with the tail of engine.log — the
// child's own stdout/stderr. Without folding the capture in, the surfaced
// last_error is a bare "exit status 1" and the real reason (e.g. a Metal
// init failure on a host with no usable GPU) never leaves the box (#22).
// Shared by the ollama and vLLM adapters.
func startupExitError(engine, logPath string, procErr error) error {
	msg := fmt.Sprintf("%s: process exited during startup: %v", engine, procErr)
	if tail := tailEngineLog(logPath, engineLogTailMaxBytes); tail != "" {
		return fmt.Errorf("%s\n--- %s stderr (tail, full log: %s) ---\n%s",
			msg, engine, logPath, tail)
	}
	return errors.New(msg)
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
// This is the operator-driven "free my VRAM/RAM" action (#186). On an
// adopted orphan it returns ErrEngineNotOwned without touching the
// process — there is no handle waired may signal, and pretending
// otherwise would lie about the memory being freed.
func (a *OllamaAdapter) Park(ctx context.Context) error {
	a.mu.Lock()
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
	if err := a.Stop(ctx); err != nil {
		// The latch is what engine_power reports, and it was set before
		// the stop ran. If the stop could not free the memory, drop it:
		// claiming "stopped" for a process that may still be alive is the
		// worst of both worlds — status lies AND EnsureRunning refuses to
		// revive the engine for local and peer traffic alike (#316).
		a.Unpark()
		return err
	}
	return nil
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
