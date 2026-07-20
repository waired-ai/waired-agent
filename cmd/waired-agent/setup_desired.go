package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/controlclient"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// setupPushInterval paces the setup-progress reporter. The CP accepts
// 1 push / 2 s (burst 10, waired#835 §20.5); the reporter additionally
// dedupes identical snapshots, so steady state pushes nothing at all.
const setupPushInterval = 2 * time.Second

// Onboarding step IDs (waired#835 §7). The CP treats them as opaque
// strings; NAVI's wizard keys its step rows off them.
const (
	setupStepEngineInstall = "engine_install"
	setupStepModelPull     = "model_pull"
	setupStepBenchmark     = "benchmark"
)

// Executor lease timings (waired#835 §9/§11). Both sides of the range
// hurt, which is why these are named rather than inline:
//   - too SHORT and a legitimate 15-minute elevated engine install
//     (installOllama's ctx budget on all three OSes) trips a spurious
//     executor_gone while it is still working — but note the executor
//     heartbeats throughout, so only a stall that long matters;
//   - too LONG and the wizard keeps claiming the install is in progress
//     after the operator has already pressed Ctrl-C, which is exactly
//     the never-resolving spinner §9-4 exists to forbid.
//
// 45 s tolerates four missed 10 s heartbeats before declaring the
// executor gone.
const (
	setupExecutorTTL       = 45 * time.Second
	setupExecutorHeartbeat = 10 * time.Second
)

// setupDesired is the (engine, model, benchmark-gen) triple the CP
// serves on the device's own Self map entry (waired#835 §6). The zero
// value means "no instruction" — the common case for every host that
// never ran a NAVI setup.
type setupDesired struct {
	engine       string
	modelID      string
	benchmarkGen int
}

// setupProvider is the narrow view of agentInferenceProvider the
// desired-state applier needs; a test fake implements it without an
// engine.
type setupProvider interface {
	// setupEngineState reports (installed, ready) for one engine kind.
	setupEngineState(ctx context.Context, engine string) (installed, ready bool)
	// setupStateDir is the agent's state root, published to the executor
	// so a bundled engine lands where this daemon will look for it.
	setupStateDir() string
	// setupModelState reports one catalog model's lifecycle state plus
	// live pull bytes and any stored failure detail.
	setupModelState(modelID string) (state string, completed, total int64, errText string)
	BenchmarkStatus() management.BenchmarkStatusResponse
	// startSetupBenchmark kicks the single-flight benchmark job at the
	// given generation (waired#835 §12; join semantics from #99 make
	// repeated calls safe).
	startSetupBenchmark(gen int)
	PullModel(ctx context.Context, modelOrAlias string) (management.PullJob, error)
}

// setupReconciler applies the CP-served desired state (waired#835 §6)
// and reports typed step progress back (§7). Apply is invoked on EVERY
// network-map frame — streaming has no dedup — so every action here
// must be idempotent: convergence is derived from observable state
// (catalog model states, the persisted benchmark generation), never
// from "did I already do this" flags that could desync from reality.
// The one exception is pull admission (one PullModel call per desired
// model value) so a permanently failing download is not re-queued on
// every frame; an agent restart retries it once more.
type setupReconciler struct {
	provider   setupProvider
	push       *controlclient.Client // nil = report-nothing (no CP push)
	deviceID   string
	machineKey ed25519.PrivateKey
	logger     *slog.Logger
	now        func() time.Time // test seam
	interval   time.Duration    // push cadence; setupPushInterval outside tests

	mu            sync.Mutex
	desired       setupDesired
	active        bool // a desired instruction has been seen this session
	pullAttempted map[string]bool
	pullRejected  map[string]string // PullModel refused (unknown model, ...)
	kick          chan struct{}     // wakes the push loop on Apply changes

	// Executor lease (§9/§11). The elevated CLI from `sudo waired init`
	// heartbeats here; a stale lease is what turns an install step into
	// executor_gone instead of a spinner. everSeen distinguishes "the
	// executor died" (recoverable — re-run the command) from "no executor
	// ever showed up" (permission_denied).
	executorAttached bool
	executorEverSeen bool
	executorElevated bool
	executorSeen     time.Time
	executorPhase    string
	executorErr      string
	// installClaimed names the engine whose install a live lease claimed.
	// Bound to the LEASE, never to desired_engine: a claim that outlived
	// its executor would make the "re-run sudo waired init" recovery a
	// no-op and would let one local POST block installation forever.
	installClaimed string

	// Last observed engine-installed state, used to detect the
	// false->true transition that re-admits a model pull which failed
	// only because there was no engine to pull with.
	engineInstalled bool
	engineObserved  bool
}

func newSetupReconciler(provider setupProvider, push *controlclient.Client, deviceID string, machineKey ed25519.PrivateKey, logger *slog.Logger) *setupReconciler {
	return &setupReconciler{
		provider:      provider,
		push:          push,
		deviceID:      deviceID,
		machineKey:    machineKey,
		logger:        logger,
		now:           time.Now,
		interval:      setupPushInterval,
		pullAttempted: map[string]bool{},
		pullRejected:  map[string]string{},
		kick:          make(chan struct{}, 1),
	}
}

// Apply reconciles toward the desired state on the device's Self map
// entry. Called from streaming on every frame; hosts that never ran a
// NAVI setup take the zero-value fast path and do no work at all.
func (r *setupReconciler) Apply(ctx context.Context, st *signer.InferenceState) {
	if r == nil || st == nil {
		return
	}
	d := setupDesired{
		engine:       st.DesiredEngine,
		modelID:      st.DesiredModelID,
		benchmarkGen: st.DesiredBenchmarkGen,
	}
	r.mu.Lock()
	if d == (setupDesired{}) && !r.active {
		r.mu.Unlock()
		return
	}
	changed := d != r.desired
	r.desired = d
	r.active = true
	attempted := r.pullAttempted[d.modelID]
	r.mu.Unlock()

	// Benchmark (§12): the served generation counter is the request;
	// the persisted last-completed generation is the answer. A run that
	// FAILED at the requested gen is still an answer (the error rides
	// setup-progress; NAVI re-bumps to retry), so only a genuinely
	// behind, not-running job starts one.
	if d.benchmarkGen > 0 {
		bs := r.provider.BenchmarkStatus()
		if bs.State != management.BenchmarkStateRunning && bs.Gen < d.benchmarkGen {
			r.provider.startSetupBenchmark(d.benchmarkGen)
		}
	}

	// Engine (§11): the agent cannot install one unprivileged — that is
	// the executor's job. What Apply does here is watch for the engine
	// APPEARING, because that transition is what unblocks a model pull
	// that failed for the only reason it could have: on an engine-less
	// host the inference subsystem starts inert, so PullModel fails
	// immediately and the one-shot admission below would otherwise keep
	// the download red for the rest of the process's life — even though
	// the executor installed the engine seconds later. Keyed on the
	// transition, not on every frame, so a genuinely failing download is
	// still not re-queued in a loop.
	if d.engine != "" {
		installed, _ := r.provider.setupEngineState(ctx, d.engine)
		r.mu.Lock()
		appeared := installed && r.engineObserved && !r.engineInstalled
		r.engineInstalled = installed
		r.engineObserved = true
		if appeared && d.modelID != "" {
			delete(r.pullAttempted, d.modelID)
			delete(r.pullRejected, d.modelID)
			attempted = false
			changed = true
		}
		r.mu.Unlock()
		if appeared && r.logger != nil {
			r.logger.Info("setup: engine became installed; re-admitting the desired model pull",
				"engine", d.engine, "model", d.modelID)
		}
	}

	// Model (§6: catalog IDs only — PullModel resolves against the
	// catalog and refuses anything it doesn't know). Present/pulling
	// models are left alone; only a genuinely absent model is queued,
	// and only once per desired value.
	if d.modelID != "" && !attempted {
		state, _, _, _ := r.provider.setupModelState(d.modelID)
		if state == "" || state == catalog.ModelStateNotPresent || state == catalog.ModelStateEvicted {
			r.mu.Lock()
			r.pullAttempted[d.modelID] = true
			r.mu.Unlock()
			if _, err := r.provider.PullModel(ctx, d.modelID); err != nil {
				r.mu.Lock()
				r.pullRejected[d.modelID] = err.Error()
				r.mu.Unlock()
				if r.logger != nil {
					r.logger.Warn("setup: desired model pull refused", "model", d.modelID, "err", err)
				}
			}
		}
	}

	if changed {
		r.kickPush()
	}
}

// kickPush wakes the reporter loop so a state change reaches NAVI on the
// next push rather than on the next tick boundary.
func (r *setupReconciler) kickPush() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// leaseLiveLocked reports whether the executor lease is still fresh and,
// when it is not, drops the lease-bound install claim. Callers hold mu.
func (r *setupReconciler) leaseLiveLocked() bool {
	if !r.executorAttached {
		return false
	}
	if r.now().Sub(r.executorSeen) > setupExecutorTTL {
		r.executorAttached = false
		r.installClaimed = ""
		return false
	}
	return true
}

// NoteExecutor records one lease heartbeat or release from the elevated
// CLI (§9/§11) and returns the resulting state, so the executor learns
// the install claim in the same round trip.
func (r *setupReconciler) NoteExecutor(ctx context.Context, req management.SetupExecutorRequest) management.SetupStateResponse {
	if r == nil {
		return management.SetupStateResponse{}
	}
	r.mu.Lock()
	phase := req.Phase
	if phase == "" {
		phase = management.SetupExecutorPhaseIdle
	}
	r.executorPhase = phase
	r.executorErr = req.Error
	if req.Attached {
		r.executorAttached = true
		r.executorEverSeen = true
		r.executorElevated = req.Elevated
		r.executorSeen = r.now()
		switch phase {
		case management.SetupExecutorPhaseInstalling:
			if req.Engine != "" {
				r.installClaimed = req.Engine
			}
		case management.SetupExecutorPhaseDone, management.SetupExecutorPhaseFailed:
			// The attempt is over either way; a fresh executor (or this
			// one, after the operator fixes whatever failed) may claim it
			// again.
			r.installClaimed = ""
		}
	} else {
		// Explicit release — same effect as the lease expiring, minus the
		// TTL wait, so Ctrl-C surfaces as executor_gone promptly.
		r.executorAttached = false
		r.installClaimed = ""
	}
	r.mu.Unlock()
	r.kickPush()
	return r.SetupState(ctx)
}

// SetupState projects what a setup executor needs in order to decide
// whether to act. Everything here is derived from observable state.
func (r *setupReconciler) SetupState(ctx context.Context) management.SetupStateResponse {
	if r == nil {
		return management.SetupStateResponse{}
	}
	r.mu.Lock()
	d := r.desired
	resp := management.SetupStateResponse{
		Active:              r.active,
		DesiredEngine:       d.engine,
		DesiredModelID:      d.modelID,
		DesiredBenchmarkGen: d.benchmarkGen,
	}
	if r.leaseLiveLocked() {
		resp.ExecutorAttached = true
		resp.ExecutorElevated = r.executorElevated
	}
	resp.InstallClaimed = r.installClaimed
	r.mu.Unlock()

	if d.engine != "" {
		resp.EngineInstalled, resp.EngineReady = r.provider.setupEngineState(ctx, d.engine)
	}
	// Published unconditionally. #115 served this only alongside a desired
	// engine, reasoning that there is nothing to install otherwise — that
	// turned out to be false. `waired init` on the daemon path installs
	// the engine whenever the host wants inference, wizard or not, and it
	// needs the destination in exactly that case (waired#835 §11).
	resp.StateDir = r.provider.setupStateDir()
	return resp
}

// snapshot builds the current typed progress (§7), or nil when this
// host has no onboarding activity. Statuses derive from observable
// state only, so a restarted agent reports the same truth.
func (r *setupReconciler) snapshot(ctx context.Context) *signer.SetupProgress {
	r.mu.Lock()
	d := r.desired
	active := r.active
	rejected := r.pullRejected[d.modelID]
	leaseLive := r.leaseLiveLocked()
	everSeen := r.executorEverSeen
	elevated := r.executorElevated
	phase := r.executorPhase
	execErr := r.executorErr
	r.mu.Unlock()
	if !active {
		return nil
	}

	p := &signer.SetupProgress{
		LastCheck: r.now().UTC().Format(time.RFC3339Nano),
	}
	if d.engine != "" {
		step := signer.SetupStep{ID: setupStepEngineInstall}
		installed, ready := r.provider.setupEngineState(ctx, d.engine)
		switch {
		case ready:
			step.Status = signer.SetupStatusDone
		case installed:
			step.Status = signer.SetupStatusRunning
		case phase == management.SetupExecutorPhaseFailed:
			// The executor tried and told us why. Its own text beats any
			// guess we could make from here.
			step.Status = signer.SetupStatusFailed
			step.ErrorCode = classifySetupFailure(execErr)
			step.ErrorDetail = clampSetupDetail(execErr)
		case leaseLive && !elevated:
			// An executor is present but cannot install — reporting
			// executor_gone here would send the operator to re-run a
			// command that would fail the same way.
			step.Status = signer.SetupStatusFailed
			step.ErrorCode = signer.SetupErrorPermissionDenied
			step.ErrorDetail = "the setup command on this device is not running with administrator privileges"
		case leaseLive:
			// Elevated executor attached: installing, or about to.
			step.Status = signer.SetupStatusRunning
		case everSeen:
			// §9-4: it was here and it is gone. This is the recoverable
			// case — NAVI offers the command to re-run.
			step.Status = signer.SetupStatusFailed
			step.ErrorCode = signer.SetupErrorExecutorGone
			step.ErrorDetail = "the setup command on this device exited before the engine was installed"
		default:
			// §11: never attached at all. Unprivileged install is
			// impossible, so this is a permissions problem, not a
			// liveness one.
			step.Status = signer.SetupStatusFailed
			step.ErrorCode = signer.SetupErrorPermissionDenied
			step.ErrorDetail = "engine is not installed and the agent cannot install it unprivileged"
		}
		p.Steps = append(p.Steps, step)
	}
	if d.modelID != "" {
		step := signer.SetupStep{ID: setupStepModelPull}
		state, completed, total, errText := r.provider.setupModelState(d.modelID)
		switch {
		case rejected != "":
			step.Status = signer.SetupStatusFailed
			step.ErrorCode = signer.SetupErrorModelNotFound
			step.ErrorDetail = rejected
		case state == catalog.ModelStateReady:
			step.Status = signer.SetupStatusDone
		case state == catalog.ModelStateQueued || state == catalog.ModelStateDownloading || state == catalog.ModelStateVerifying:
			step.Status = signer.SetupStatusRunning
			step.CompletedBytes = completed
			step.TotalBytes = total
		case state == catalog.ModelStateFailed:
			step.Status = signer.SetupStatusFailed
			step.ErrorCode = classifySetupFailure(errText)
			step.ErrorDetail = clampSetupDetail(errText)
		default: // not_present / evicted / unknown — pull not admitted yet
			step.Status = signer.SetupStatusPending
		}
		p.Steps = append(p.Steps, step)
	}
	if d.benchmarkGen > 0 {
		step := signer.SetupStep{ID: setupStepBenchmark}
		bs := r.provider.BenchmarkStatus()
		switch {
		case bs.Gen >= d.benchmarkGen && bs.State == management.BenchmarkStateDone:
			step.Status = signer.SetupStatusDone
			p.Benchmark = &signer.SetupBenchmark{Gen: bs.Gen, MeasuredTokps: bs.MeasuredTokps}
		case bs.Gen >= d.benchmarkGen && bs.State == management.BenchmarkStateFailed:
			step.Status = signer.SetupStatusFailed
			step.ErrorCode = signer.SetupErrorInternal
			step.ErrorDetail = clampSetupDetail(bs.Error)
			p.Benchmark = &signer.SetupBenchmark{Gen: bs.Gen}
		case bs.State == management.BenchmarkStateRunning:
			step.Status = signer.SetupStatusRunning
		default:
			step.Status = signer.SetupStatusPending
		}
		p.Steps = append(p.Steps, step)
	}
	return p
}

// setupDetailMax mirrors the control plane's error_detail clamp
// (waired#835 §20.5). Clamping here too keeps a long installer log from
// costing a whole push.
const setupDetailMax = 512

func clampSetupDetail(s string) string {
	if len(s) <= setupDetailMax {
		return s
	}
	return s[:setupDetailMax]
}

// diskFullMarkers are the substrings that mean "out of disk" across the
// three OSes and both engines' downloaders. Matching is best-effort by
// nature — the failure arrives as text — but the cost of guessing wrong
// is asymmetric: telling someone to check their internet connection when
// the real problem is a full disk sends them nowhere, while the reverse
// at least points at the machine.
var diskFullMarkers = []string{
	"no space left on device",
	"not enough space",
	"insufficient disk space",
	"insufficient space",
	"disk full",
	"enospc",
	"there is not enough space on the disk",
}

// classifySetupFailure maps a free-form failure string to the §7 error
// code enum. Anything unrecognised stays network_error, which is what
// this code path reported unconditionally before.
func classifySetupFailure(errText string) string {
	l := strings.ToLower(errText)
	for _, m := range diskFullMarkers {
		if strings.Contains(l, m) {
			return signer.SetupErrorDiskFull
		}
	}
	return signer.SetupErrorNetworkError
}

// progressKey canonicalizes a snapshot for change detection, ignoring
// the always-moving LastCheck timestamp.
func progressKey(p *signer.SetupProgress) string {
	c := *p
	c.LastCheck = ""
	b, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return string(b)
}

// runPush is the reporter loop (§5.2/§7): every setupPushInterval it
// snapshots the step states and pushes to CP only when the content
// changed since the last successful push. Hosts with no onboarding
// activity snapshot nil and never touch the network — the setup
// channel adds zero heartbeat to a fleet at rest.
func (r *setupReconciler) runPush(ctx context.Context) {
	if r == nil || r.push == nil || r.deviceID == "" || len(r.machineKey) != ed25519.PrivateKeySize {
		return
	}
	t := time.NewTicker(r.interval)
	defer t.Stop()
	var lastPushed string
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-r.kick:
		}
		snap := r.snapshot(ctx)
		if snap == nil {
			continue
		}
		key := progressKey(snap)
		if key == "" || key == lastPushed {
			continue
		}
		pushCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := r.push.PushSetupProgress(pushCtx, r.deviceID, *snap, r.machineKey)
		cancel()
		if err != nil {
			if r.logger != nil && !errors.Is(err, context.Canceled) {
				r.logger.Warn("setup progress push failed", "err", err)
			}
			continue // retry with fresh content next tick
		}
		lastPushed = key
	}
}

// --- agentInferenceProvider adapters (the real setupProvider) ---

// setupEngineState reports whether the desired engine kind is installed
// on this host and whether it is the one currently serving and ready.
// Installation state comes from the cached hardware profile (cheap);
// readiness from the provider's usual EngineReady gate.
func (p *agentInferenceProvider) setupEngineState(ctx context.Context, engine string) (installed, ready bool) {
	prof := p.profiler.Profile(ctx)
	switch engine {
	case "ollama":
		installed = prof.Engines.Ollama.Installed
	case "vllm":
		installed = prof.Engines.VLLM.Installed
	}
	if !installed {
		return false, false
	}
	if p.servingEngine() != engine {
		return true, false
	}
	r, _ := p.EngineReady()
	return true, r
}

// setupStateDir is the agent's state root. The executor installs the
// bundled engine relative to this, so it matches bundledOllamaBin's
// join (inference.go) by construction rather than by coincidence.
func (p *agentInferenceProvider) setupStateDir() string { return p.stateDir }

// setupModelState reports one catalog model's lifecycle state, live
// pull bytes (while downloading) and the stored failure detail.
func (p *agentInferenceProvider) setupModelState(modelID string) (string, int64, int64, string) {
	st, err := p.store.Load()
	if err != nil {
		return "", 0, 0, ""
	}
	ms, ok := st.Models[modelID]
	if !ok {
		return catalog.ModelStateNotPresent, 0, 0, ""
	}
	completed, total, _ := p.dlProgress.aggregate(modelID)
	return ms.State, completed, total, ms.Error
}

// startSetupBenchmark kicks the single-flight benchmark job (#99) at
// the served generation without waiting for it.
func (p *agentInferenceProvider) startSetupBenchmark(gen int) {
	p.startBenchmarkJob(gen)
}
