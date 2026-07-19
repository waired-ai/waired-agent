package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"log/slog"
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

	// Engine (§11): the agent cannot install an engine unprivileged —
	// installation is the executor's (elevated CLI) job. Nothing to
	// apply here; the snapshot below reports installed/ready truthfully
	// and permission_denied when the engine is missing.

	if changed {
		select {
		case r.kick <- struct{}{}:
		default:
		}
	}
}

// snapshot builds the current typed progress (§7), or nil when this
// host has no onboarding activity. Statuses derive from observable
// state only, so a restarted agent reports the same truth.
func (r *setupReconciler) snapshot(ctx context.Context) *signer.SetupProgress {
	r.mu.Lock()
	d := r.desired
	active := r.active
	rejected := r.pullRejected[d.modelID]
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
		default:
			// §11: unprivileged install is impossible; the executor
			// (elevated CLI) owns installation. NAVI turns this into
			// "continue on the device" guidance.
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
			step.ErrorCode = signer.SetupErrorNetworkError
			step.ErrorDetail = errText
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
			step.ErrorDetail = bs.Error
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
