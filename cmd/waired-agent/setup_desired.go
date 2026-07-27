package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"log/slog"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/controlclient"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// setupPushInterval paces the setup-progress reporter. The CP accepts
// 1 push / 2 s (burst 10, waired#835 §20.5); the reporter additionally
// dedupes identical snapshots, so steady state pushes nothing at all.
const setupPushInterval = 2 * time.Second

// setupKeepaliveInterval is how long an UNCHANGED snapshot may go
// unpushed while a setup is live (#130).
//
// Content dedup alone froze last_check for the length of a step with no
// moving field — a 15-minute ollama install, a 45-minute vLLM build, a
// running benchmark — and the wizard's staleness window (120 s) then
// called a perfectly healthy install offline and told the operator to
// intervene. Re-pushing the same content every 30 s advances last_check
// with four times the margin that window needs, and costs 2 pushes /
// minute on a host that is mid-setup — a state in which the fleet is not
// at rest anyway. A host with no onboarding activity still snapshots nil
// and pushes nothing at all.
const setupKeepaliveInterval = 30 * time.Second

// Onboarding step IDs (waired#835 §7, five ids as of waired#934). The CP
// treats them as opaque strings; NAVI's wizard keys its step rows off
// them. The three the elevated executor drives are named in
// internal/management, because the executor and the daemon have to agree
// on which row a report belongs to.
const (
	setupStepEngineDownload = management.SetupStepEngineDownload
	setupStepEngineInstall  = management.SetupStepEngineInstall
	setupStepModelPull      = "model_pull"
	setupStepBenchmark      = "benchmark"
	setupStepIntegration    = management.SetupStepIntegration
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
	// integrations is the coding-agent instruction (waired#935), flattened
	// so this struct stays comparable — change detection here is a plain
	// `!=`, and a slice field would not compile.
	//
	// Three states, and the difference between the last two is the whole
	// point: absent = never asked, integrationsNone = asked and every
	// toggle was off, a list = write these. Collapsing the middle case
	// into "no instruction" is how the wizard would report success for a
	// device it never configured — the waired#904 class.
	integrations string
}

// integrationsNone is the flattened form of "asked, and nothing was
// selected". Not a valid target id, so it cannot collide with one.
const integrationsNone = "\x00none"

// flattenIntegrations renders the desired-integrations instruction as a
// comparable string: "" when there is no instruction at all, the
// sentinel when the instruction is an empty set, and the sorted,
// de-duplicated, validated ids joined otherwise.
//
// Sorting matters: the wire order is the control plane's, and a reorder
// with the same contents is not a change the agent should react to.
func flattenIntegrations(d *signer.DesiredIntegrations) string {
	if d == nil {
		return ""
	}
	seen := map[string]bool{}
	var out []string
	for _, t := range d.Enabled {
		// Unknown targets are ignored rather than rejected: the set can
		// grow, and a newer control plane naming one this build has never
		// heard of must not cost the whole instruction.
		if !signer.IsValidIntegrationTarget(t) || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	if len(out) == 0 {
		return integrationsNone
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// integrationTargets is flattenIntegrations in reverse: the ids to
// write, or nil for either "no instruction" or "asked, none selected"
// (the caller separates those by comparing against integrationsNone).
func integrationTargets(flat string) []string {
	if flat == "" || flat == integrationsNone {
		return nil
	}
	return strings.Split(flat, ",")
}

// setupExecutorStep is what the elevated CLI last reported about one
// setup step: its phase, the failure detail if it failed, and the live
// transfer figures for a byte-denominated step.
//
// Byte fields are only overwritten by a report that actually carries
// them, so the terminating `done` post — which has nothing to say about
// bytes — does not blank the last figures the row was drawn from.
type setupExecutorStep struct {
	phase          string
	errText        string
	completedBytes int64
	totalBytes     int64
	rateBps        int64
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
	// setupPreferredModelID reports the model this device is currently
	// set to serve, or "" when nothing has been chosen. It is what makes
	// the model step converge on OBSERVABLE state rather than on an
	// in-memory "did I already do this" flag: a daemon restart clears the
	// flag, and re-applying an already-applied choice would bounce the
	// engine on every boot of a host that is already set up.
	setupPreferredModelID() string
	// setupApplyModel makes modelID the model this device serves — the
	// same operation the operator's own model switch performs (#347/#812),
	// not merely a download. downloading reports whether the weights still
	// had to be fetched; the activation then lands when the pull completes.
	//
	// It replaced a bare PullModel call, which downloaded the wizard's
	// choice and then served something else entirely (#230).
	setupApplyModel(ctx context.Context, modelID string) (downloading bool, err error)
	// PullModel is the fallback for a target the in-process switch cannot
	// apply (a cross-engine change). The weights are fetched now and the
	// activation happens on the next boot, from the preference
	// setupApplyModel already persisted.
	PullModel(ctx context.Context, modelOrAlias string) (management.PullJob, error)
}

// setupReconciler applies the CP-served desired state (waired#835 §6)
// and reports typed step progress back (§7). Apply is invoked on EVERY
// network-map frame — streaming has no dedup — so every action here
// must be idempotent: convergence is derived from observable state
// (catalog model states, the persisted preference, the persisted
// benchmark generation), never from "did I already do this" flags that
// could desync from reality. The one exception is model admission (one
// setupApplyModel call per desired model value) so a permanently failing
// download is not re-queued on every frame; an agent restart retries it
// once more, and the persisted preference stops that retry from bouncing
// the engine on a host that already converged.
type setupReconciler struct {
	provider   setupProvider
	push       *controlclient.Client // nil = report-nothing (no CP push)
	deviceID   string
	machineKey ed25519.PrivateKey
	logger     *slog.Logger
	now        func() time.Time // test seam
	interval   time.Duration    // push cadence; setupPushInterval outside tests

	mu           sync.Mutex
	desired      setupDesired
	active       bool            // a desired instruction has been seen this session
	modelApplied map[string]bool // one setupApplyModel call per desired model value
	// modelRejected records why applying the desired model was refused
	// (an unknown model, an engine that cannot serve it). It feeds the
	// model step's model_not_found rather than leaving it pending.
	modelRejected map[string]string
	kick          chan struct{} // wakes the push loop on Apply changes

	// Executor lease (§9/§11). The elevated CLI from `sudo waired init`
	// heartbeats here; a stale lease is what turns an install step into
	// executor_gone instead of a spinner. everSeen distinguishes "the
	// executor died" (recoverable — re-run the command) from "no executor
	// ever showed up" (permission_denied).
	executorAttached bool
	executorEverSeen bool
	executorElevated bool
	executorSeen     time.Time
	// executorSteps is the lease's phase and byte progress PER STEP
	// (#197). It used to be one phase for the whole lease, which was
	// enough while the executor drove exactly one row; now that the
	// engine download and the install proper are separate rows — and
	// waired#935 adds the integration — a single field would let the
	// install's phase erase the finished download's.
	executorSteps map[string]setupExecutorStep
	// executorDriver is the surface a live lease claims to be driving
	// (waired-agent#198) — in practice only ever "terminal". Bound to the
	// lease exactly like installClaimed below: a claim that outlived its
	// executor would have the wizard reporting a terminal that is not
	// running, with no way back.
	executorDriver string
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
		modelApplied:  map[string]bool{},
		modelRejected: map[string]string{},
		executorSteps: map[string]setupExecutorStep{},
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
		integrations: flattenIntegrations(st.DesiredIntegrations),
	}
	r.mu.Lock()
	if d == (setupDesired{}) && !r.active {
		r.mu.Unlock()
		return
	}
	changed := d != r.desired
	r.desired = d
	r.active = true
	applied := r.modelApplied[d.modelID]
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
	// the executor's job. Apply does two things with it.
	//
	// First, it gates the model step below: until the engine is actually
	// installed there is nothing to serve the model FROM, so applying the
	// choice can only fail. enginePresent carries that answer forward.
	//
	// Second, it watches for the engine APPEARING, because that
	// transition invalidates the one-shot admission. With the gate in
	// place the common engine-less case never burns the attempt, but an
	// engine that goes away and comes back (a reinstall, a profiler
	// cache that briefly reports it missing) still has to re-admit, or
	// the download stays red for the rest of the process's life. Keyed
	// on the transition, not on every frame, so a genuinely failing
	// download is still not re-queued in a loop.
	enginePresent := d.engine == ""
	if d.engine != "" {
		installed, _ := r.provider.setupEngineState(ctx, d.engine)
		enginePresent = installed
		r.mu.Lock()
		appeared := installed && r.engineObserved && !r.engineInstalled
		r.engineInstalled = installed
		r.engineObserved = true
		if appeared && d.modelID != "" {
			delete(r.modelApplied, d.modelID)
			delete(r.modelRejected, d.modelID)
			applied = false
			changed = true
		}
		r.mu.Unlock()
		if appeared && r.logger != nil {
			r.logger.Info("setup: engine became installed; re-admitting the desired model",
				"engine", d.engine, "model", d.modelID)
		}
	}

	// Model (§6: catalog IDs only — the provider resolves against the
	// catalog and refuses anything it doesn't know).
	//
	// This APPLIES the choice; it does not merely download it. Until
	// #230 the desired model was handed to PullModel and nowhere else,
	// so the wizard's choice was fetched — tens of gigabytes of it — and
	// then the daemon went on serving whatever it had auto-selected for
	// itself. Neither activation path could rescue it:
	// activateBundledIfUnset only fires for the install-time bundled
	// model, and activatePreferredIfNeeded needs a preference the setup
	// path never wrote. Worse, a choice whose weights were ALREADY on
	// disk skipped the pull condition entirely and therefore did nothing
	// at all. Applying it is the operator's own model switch (#347/#812):
	// persist the preference, then flip the active selection.
	//
	// Admission is once per desired model value, and never before the
	// engine that would serve it exists (see enginePresent above).
	// Holding it back is what lets the wizard write the engine and the
	// model in one gesture: the step sits at `pending` for the length of
	// the install instead of showing a failure that resolves itself
	// (waired#904).
	//
	// A device that has already CONVERGED — the desired model is the one
	// it is set to serve, and its weights are on disk — is left alone, so
	// a daemon restart re-reading the same instruction does not bounce a
	// healthy engine on every boot. Convergence needs both halves: a
	// preference alone is satisfied the instant the switch is published,
	// which would make the engine-reappears retry below a no-op and leave
	// a failed download red for the rest of the process's life.
	if d.modelID != "" && !applied && enginePresent {
		state, _, _, _ := r.provider.setupModelState(d.modelID)
		converged := state == catalog.ModelStateReady && r.provider.setupPreferredModelID() == d.modelID
		if !converged {
			r.mu.Lock()
			r.modelApplied[d.modelID] = true
			r.mu.Unlock()
			if _, err := r.provider.setupApplyModel(ctx, d.modelID); err != nil {
				r.mu.Lock()
				r.modelRejected[d.modelID] = err.Error()
				r.mu.Unlock()
				if r.logger != nil {
					r.logger.Warn("setup: desired model refused", "model", d.modelID, "err", err)
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
		r.executorDriver = ""
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
	// An unnamed step is the engine install: that is all a lease could
	// report before the split, and all it ever meant.
	stepID := req.Step
	if stepID == "" {
		stepID = setupStepEngineInstall
	}
	st := r.executorSteps[stepID]
	st.phase = phase
	st.errText = req.Error
	if req.CompletedBytes > 0 || req.TotalBytes > 0 {
		st.completedBytes = req.CompletedBytes
		st.totalBytes = req.TotalBytes
		st.rateBps = req.RateBps
	}
	r.executorSteps[stepID] = st
	if req.Attached {
		r.executorAttached = true
		r.executorEverSeen = true
		r.executorElevated = req.Elevated
		r.executorSeen = r.now()
		// Empty leaves the claim alone: a heartbeat does not have to
		// repeat it, and nothing else may quietly drop it.
		if req.Driver != "" {
			r.executorDriver = req.Driver
		}
		// Only an engine step moves the install claim. An integration
		// report (waired#935) rides the same lease and must not hand the
		// engine install to a second executor by reporting `done`.
		if management.IsEngineSetupStep(req.Step) {
			switch phase {
			case management.SetupExecutorPhaseInstalling:
				if req.Engine != "" {
					r.installClaimed = req.Engine
				}
			case management.SetupExecutorPhaseDone, management.SetupExecutorPhaseFailed:
				// The attempt is over either way; a fresh executor (or this
				// one, after the operator fixes whatever failed) may claim it
				// again.
				//
				// Except while the download is still the step being
				// reported: `engine_download: done` means the bytes are
				// here and the install proper is next, in the SAME lease.
				// Dropping the claim there would invite a second elevated
				// install of the engine this one is mid-way through.
				if stepID != setupStepEngineDownload || phase != management.SetupExecutorPhaseDone {
					r.installClaimed = ""
				}
			}
		}
	} else {
		// Explicit release — same effect as the lease expiring, minus the
		// TTL wait, so Ctrl-C surfaces as executor_gone promptly.
		r.executorAttached = false
		r.installClaimed = ""
		r.executorDriver = ""
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
	if d.integrations != "" {
		targets := integrationTargets(d.integrations)
		if targets == nil {
			targets = []string{} // asked, nothing selected
		}
		resp.Integrations = &targets
	}
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
	rejected := r.modelRejected[d.modelID]
	leaseLive := r.leaseLiveLocked()
	everSeen := r.executorEverSeen
	elevated := r.executorElevated
	download, downloadSeen := r.executorSteps[setupStepEngineDownload]
	install := r.executorSteps[setupStepEngineInstall]
	integ := r.executorSteps[setupStepIntegration]
	phase := install.phase
	execErr := install.errText
	// leaseLiveLocked above already dropped the driver if the lease died,
	// so reading it here needs no second liveness check.
	driver := r.executorDriver
	if driver == "" && active {
		// Nobody claimed it, and there is desired state: the browser
		// wrote it, so the browser is driving. Derived rather than
		// reported, because the wizard has no lease to report through
		// and the write it made is already the evidence.
		driver = signer.SetupDriverBrowser
	}
	r.mu.Unlock()
	// A terminal takeover produces no desired state and therefore no
	// steps — but the wizard is on screen waiting for this device, and
	// with nothing pushed it waits forever. A driver alone is worth a
	// push: zero steps keeps setup_complete false and the "setup
	// unfinished" banner away, and tells the wizard who has it
	// (waired-agent#198).
	if !active && driver == "" {
		return nil
	}

	p := &signer.SetupProgress{
		LastCheck: r.now().UTC().Format(time.RFC3339Nano),
		Driver:    driver,
	}
	if d.engine != "" {
		installed, ready := r.provider.setupEngineState(ctx, d.engine)
		// engine_download exists only when this host actually downloaded
		// something. A machine that already had the engine, or one whose
		// executor is older than the split, reports the single
		// engine_install row it always did — inventing a download row for
		// a transfer that never happened would be a step the wizard waits
		// on forever.
		if downloadSeen {
			p.Steps = append(p.Steps, engineDownloadStep(download, installed || ready))
		}
		step := signer.SetupStep{ID: setupStepEngineInstall}
		// Arm order is the contract here, not an accident (#187). It is
		// strongest-evidence-first: an engine that is serving beats a
		// stale failed phase from an earlier attempt; a failure the
		// executor reported beats mere on-disk presence, because a
		// half-configured install leaves a binary behind and still failed.
		switch {
		case ready:
			step.Status = signer.SetupStatusDone
		case phase == management.SetupExecutorPhaseFailed:
			// The executor tried and told us why. Its own text beats any
			// guess we could make from here. Ahead of `installed` so a
			// half-configured engine — a binary on disk that cannot
			// serve — reports the real failure instead of sitting at
			// "working on it" forever.
			step.Status = signer.SetupStatusFailed
			step.ErrorCode = classifySetupFailure(execErr)
			step.ErrorDetail = clampSetupDetail(execErr)
		case installed:
			// This step is "install the engine", and the engine is
			// installed. It used to complete only once the engine was
			// READY, which in this projection means the active MODEL is
			// ready — so the row said "working on it" for the whole model
			// download while the progress bar tracked the model step, and
			// the wizard showed two rows running at once.
			step.Status = signer.SetupStatusDone
		case downloadSeen && download.phase == management.SetupExecutorPhaseFailed:
			// The bytes never arrived, so there is nothing to install.
			// The failure is reported once, on the row it happened on;
			// repeating it here would show the operator two red steps for
			// one event and invite them to fix it twice.
			step.Status = signer.SetupStatusPending
		case downloadSeen && download.phase != management.SetupExecutorPhaseDone:
			// A download is still in flight. Keeping this row pending is
			// the same rule #187 settled for the model pull: exactly one
			// row is allowed to be the live one, or the wizard shows two
			// spinners and the progress bar belongs to neither.
			step.Status = signer.SetupStatusPending
		case phase == management.SetupExecutorPhaseDone:
			// The executor says it finished and we cannot see it yet.
			// Rare now that detection matches the daemon's own rule
			// (#179), but its explicit completion exists precisely to
			// advance the wizard, and discarding it is what left the step
			// spinning. The model step stays honest either way: its pull
			// is admitted from the engine probe, not from this.
			step.Status = signer.SetupStatusDone
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
			p.Benchmark = &signer.SetupBenchmark{
				Gen:           bs.Gen,
				MeasuredTokps: bs.MeasuredTokps,
				Trials:        bs.Trials,
				SpreadPct:     bs.SpreadPct,
				Method:        bs.Method,
			}
		case bs.Gen >= d.benchmarkGen && bs.State == management.BenchmarkStateFailed:
			step.Status = signer.SetupStatusFailed
			step.ErrorCode = signer.SetupErrorInternal
			step.ErrorDetail = clampSetupDetail(bs.Error)
			p.Benchmark = &signer.SetupBenchmark{Gen: bs.Gen}
		case bs.State == management.BenchmarkStateRunning:
			step.Status = signer.SetupStatusRunning
			// Per-measurement progress (waired-agent#199). MeasuredTokps
			// is deliberately absent while running: it is the FINAL
			// answer, and shipped wizards render it as "Speed: about N".
			// The converging figure goes in MedianTokps, and warm-up is
			// Trials set with Trial still 0.
			p.Benchmark = &signer.SetupBenchmark{
				Gen:         d.benchmarkGen,
				Trial:       bs.Trial,
				Trials:      bs.Trials,
				SampleTokps: bs.SampleTokps,
				MedianTokps: bs.MedianTokps,
				SpreadPct:   bs.SpreadPct,
				Method:      bs.Method,
			}
		default:
			step.Status = signer.SetupStatusPending
		}
		p.Steps = append(p.Steps, step)
	}
	if d.integrations != "" {
		p.Steps = append(p.Steps, integrationStep(d.integrations, integ))
	}
	return p
}

// integrationStep projects the coding-agent instruction onto its §7 row
// (waired#935).
//
// "Asked, and every toggle was off" reports `skipped` rather than no row
// at all. §7 defines skipped as "already true on this computer", which is
// exactly what an all-off answer means — and reporting it is what lets
// the control plane tell an integration that was declined from one that
// was never asked about. Until now nothing in the agent produced
// `skipped`; this is its first producer.
func integrationStep(flat string, st setupExecutorStep) signer.SetupStep {
	step := signer.SetupStep{ID: setupStepIntegration}
	if flat == integrationsNone {
		step.Status = signer.SetupStatusSkipped
		return step
	}
	switch st.phase {
	case management.SetupExecutorPhaseDone:
		step.Status = signer.SetupStatusDone
	case management.SetupExecutorPhaseFailed:
		step.Status = signer.SetupStatusFailed
		step.ErrorCode = classifyIntegrationFailure(st.errText)
		step.ErrorDetail = clampSetupDetail(st.errText)
	case management.SetupExecutorPhaseInstalling:
		step.Status = signer.SetupStatusRunning
	default:
		// No executor has spoken for this row yet. Pending rather than a
		// failure: the engine and the model come first, and the terminal
		// reaches the integration only after them.
		step.Status = signer.SetupStatusPending
	}
	return step
}

// classifyIntegrationFailure maps an integration failure to the §7 enum.
// Unlike an engine install these never fail for disk or network reasons
// — they write small files into a home directory — so the honest default
// is `internal`, and the detail carries the real text.
func classifyIntegrationFailure(errText string) string {
	if strings.Contains(strings.ToLower(errText), "permission denied") {
		return signer.SetupErrorPermissionDenied
	}
	return signer.SetupErrorInternal
}

// engineDownloadStep projects the executor's engine-download report onto
// its §7 row (#197). enginePresent short-circuits it: whatever the last
// lease said, an engine that is on this host was downloaded.
//
// The byte counters ride the row while it runs and are dropped once it is
// done — a finished download is a finished download, and a "1.4 GB /
// 1.4 GB" bar left on screen next to a green check is the kind of detail
// that reads as an unfinished step.
func engineDownloadStep(st setupExecutorStep, enginePresent bool) signer.SetupStep {
	step := signer.SetupStep{ID: setupStepEngineDownload}
	switch {
	case enginePresent || st.phase == management.SetupExecutorPhaseDone:
		step.Status = signer.SetupStatusDone
	case st.phase == management.SetupExecutorPhaseFailed:
		step.Status = signer.SetupStatusFailed
		step.ErrorCode = classifySetupFailure(st.errText)
		step.ErrorDetail = clampSetupDetail(st.errText)
	default:
		step.Status = signer.SetupStatusRunning
		step.CompletedBytes = st.completedBytes
		step.TotalBytes = st.totalBytes
		step.RateBps = st.rateBps
	}
	return step
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
// snapshots the step states and pushes to CP when the content changed
// since the last successful push, or when the last push has aged past
// setupKeepaliveInterval (#130 — an unchanging step must still prove it
// is alive). Hosts with no onboarding activity snapshot nil and never
// touch the network: the setup channel adds zero heartbeat to a fleet at
// rest, and the keepalive only runs for a host that is mid-setup.
func (r *setupReconciler) runPush(ctx context.Context) {
	if r == nil || r.push == nil || r.deviceID == "" || len(r.machineKey) != ed25519.PrivateKeySize {
		return
	}
	t := time.NewTicker(r.interval)
	defer t.Stop()
	var lastPushed string
	var lastPushAt time.Time
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
		if key == "" {
			continue
		}
		if key == lastPushed && r.now().Sub(lastPushAt) < setupKeepaliveInterval {
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
		lastPushAt = r.now()
	}
}

// --- agentInferenceProvider adapters (the real setupProvider) ---

// setupEngineState reports whether the desired engine kind is installed
// on this host and whether it is the one currently serving and ready.
// Installation state goes through engineInstalledOnHost — the daemon's
// own resolution rule — and readiness through the provider's usual
// EngineReady gate.
//
// It used to read the hardware profile's Engines map, which is a
// PATH-only probe: an engine installed under the state dir (the normal
// case, and the only one on Linux) read as absent, so /setup/state and
// /inference/status contradicted each other on the same host at the
// same instant. The wizard then never admitted the model pull, the step
// contents never changed, the progress push deduped everything away,
// last_check froze, and the run ended as executor_gone (#179).
//
// Resolving live also drops the profile's 30 s cache from the path, so
// an engine the executor just installed is visible on the next frame
// rather than up to half a minute later.
// The context parameter is kept for the setupProvider interface (a fake
// implements it too) but is no longer needed: nothing here profiles.
func (p *agentInferenceProvider) setupEngineState(_ context.Context, engine string) (installed, ready bool) {
	if !engineInstalledOnHost(runtime.GOOS, p.stateDir, p.cfg, engine) {
		return false, false
	}
	if p.servingEngine() != engine {
		return true, false
	}
	r, _ := p.EngineReady()
	return true, r
}

// setupStateDir is the agent's state root. The executor installs the
// bundled engine relative to this, so it matches bundledOllamaBinPath's
// join (engine_resolve.go) by construction rather than by coincidence.
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

// setupPreferredModelID is the model this device is currently set to
// serve. It reads the EFFECTIVE preference (the in-process #812
// override when one has been published, else the boot snapshot), not
// the file, so a choice applied moments ago is already visible and the
// reconciler does not re-apply it on the next frame.
func (p *agentInferenceProvider) setupPreferredModelID() string {
	return p.effectivePreferredModelID()
}

// setupApplyModel makes modelID the model this device serves. It is the
// setup path's half of the operator model switch, and mirrors
// management.handleInferencePreferredModel step for step:
//
//  1. Persist the preference, so the choice survives the restart an
//     engine install may cause. bootstrapPreferredModel re-pulls or
//     activates it on the next boot without any further instruction.
//  2. Apply it in process (#812): on-disk weights flip the active
//     selection and bounce the engine now; absent weights start a pull
//     whose completion runs activatePreferredIfNeeded.
//  3. Fall back to a bare pull when the in-process switch declines the
//     target (errSwapNeedsRestart — a cross-engine change). The weights
//     land now and step 1's preference activates them after the restart.
//
// Unlike the management handler this NEVER schedules a restart of its
// own: it runs while a browser wizard is watching the setup steps, and
// taking the daemon down mid-run is the class of silence #130 exists to
// prevent. The engine install the wizard is driving supplies whatever
// restart is genuinely needed.
//
// The switch runs on the daemon's long-lived context, never the frame's:
// Apply's context belongs to the network-map stream, and the pull plus
// engine bounce have to outlive it (same reason as modelSwapController).
func (p *agentInferenceProvider) setupApplyModel(ctx context.Context, modelID string) (bool, error) {
	if p.preferencePath != "" {
		if err := agentconfig.SavePreference(p.preferencePath, agentconfig.Preference{ModelID: modelID}); err != nil {
			// Not fatal: the in-process switch below still makes this the
			// served model for the life of this process. Only the
			// survives-a-restart guarantee is lost, and saying so beats
			// failing a setup the user can see working.
			p.logger.Warn("setup: persisting the chosen model failed", "model", modelID, "err", err)
		}
	}
	applyCtx := p.agentCtx
	if applyCtx == nil {
		applyCtx = ctx
	}
	downloading, err := p.SwapPreferredModel(applyCtx, modelID)
	if err == nil {
		return downloading, nil
	}
	if !errors.Is(err, errSwapNeedsRestart) {
		return false, err
	}
	p.logger.Info("setup: model switch needs a restart; downloading now and activating on the next boot",
		"model", modelID)
	if _, perr := p.PullModel(applyCtx, modelID); perr != nil {
		return false, perr
	}
	return true, nil
}
