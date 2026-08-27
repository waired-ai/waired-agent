package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/download"
	"github.com/waired-ai/waired-agent/internal/gateway"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/observability"
	"github.com/waired-ai/waired-agent/internal/platform/elevation"
	"github.com/waired-ai/waired-agent/internal/platform/proclist"
	"github.com/waired-ai/waired-agent/internal/router"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
	"github.com/waired-ai/waired-agent/proto/hostfit"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// inferenceSubsystem is the bag of components the agent wires up
// when the inference subsystem is enabled.
type inferenceSubsystem struct {
	cfg       agentconfig.InferenceConfig
	logger    *slog.Logger
	manifests []catalog.Manifest
	store     *catalog.Store
	profiler  *hardware.Profiler
	registry  *infruntime.Registry
	ollama    *infruntime.OllamaAdapter
	puller    *download.Puller
	gateway   *gateway.Server
	// overlayHandlerSet is the listener-agnostic gateway routes for
	// the Phase 4 peer-overlay listener. Built with a local-only
	// Selector (MeshSnapshotFn = nil, PeerAdapterFactory = nil) so a
	// peer-side request can never recurse through this agent to a
	// third peer.
	overlayHandlerSet *gateway.HandlerSet

	// claudeHandlerSet serves the Claude intercept (:9472) — a LOCAL
	// surface, so unlike overlayHandlerSet it is mesh-capable
	// (#601/#647): its claudeSelector applies the per-class node
	// policy and PeerAdapterFactory dispatches remote selections one
	// hop to a peer, whose own overlay stays local-only.
	claudeHandlerSet *gateway.HandlerSet

	// provider is the concrete provider so main.go can plumb its
	// Phase 8 EngineReady() into inference.Config.EngineReadyFn.
	// The management API uses the InferenceProvider interface shape;
	// this field gives main.go direct access to agent-internal
	// readiness helpers without expanding that interface.
	provider *agentInferenceProvider
}

// EngineReady is the closure shape inference.Config.EngineReadyFn
// expects: (engine ready, currently-active model ID). The Phase 8
// /healthz endpoint reads this so remote probes can distinguish "the
// peer is up but its engine is still loading" from "the peer is up
// and serving".
func (s *inferenceSubsystem) EngineReady() (bool, string) {
	if s == nil || s.provider == nil {
		return false, ""
	}
	return s.provider.EngineReady()
}

// EngineProvenance reports who owns the serving ollama process, the
// live version it answers with, and the agent-computed version warning
// (see RuntimeStatus.Mode / LiveVersion / VersionWarning). Read by the
// observability state so `waired doctor` can flag mismatches.
// Mode/version stay ollama-only (vLLM has no adopted mode),
// but when the serving engine is vllm the tuning warning comes from
// its adapter so a clamped context window reaches `waired doctor`
// (#675).
func (s *inferenceSubsystem) EngineProvenance() (mode, version, warning, tuningWarning string) {
	if s == nil {
		return "", "", "", ""
	}
	if s.provider != nil && s.provider.servingEngine() == catalog.RuntimeVLLM {
		if tuner, ok := s.provider.vllmAdapter().(interface{ AppliedTuning() infruntime.ModelTuning }); ok {
			tuningWarning = tuner.AppliedTuning().Warning
		}
	}
	if s.ollama == nil {
		return "", "", "", tuningWarning
	}
	version = s.ollama.EngineVersion()
	if tuningWarning == "" {
		tuningWarning = s.ollama.AppliedTuning().Warning
	}
	return string(s.ollama.Mode()), version,
		ollamaVersionWarning(version), tuningWarning
}

// inferenceSubsystemDeps bundles the per-agent hooks
// startInferenceSubsystem needs from main. Phase 4 grows this past
// the original (isPaused, isInferenceDisabled, inferenceState) tuple
// so it stays comprehensible.
type inferenceSubsystemDeps struct {
	IsPaused            func() bool
	IsInferenceDisabled func() bool
	InferenceState      func() (current, desired state.InferenceState)

	// PreferencePath is where the operator's chosen model is persisted
	// (preferred-model.json). It is threaded in rather than resolved here
	// because the loopback management API writes the SAME file from its
	// preferred-model handler: one expression in main.go feeds both, so
	// the browser wizard and the tray cannot end up pinning two different
	// files (#230). Empty disables the persistence half of
	// setupApplyModel, which is what unit tests that construct a provider
	// directly want.
	PreferencePath string
	// MeshSnapshotFn, when non-nil, enables Phase 4 peer-engine
	// routing on the LOOPBACK gateway: a Selection.Runtime of the
	// form "remote:<deviceID>" gets routed through PeerAdapterFactory.
	// nil disables it (= Phase 1+2+3 behaviour).
	MeshSnapshotFn func() inferencemesh.Snapshot
	// PeerAdapterFactory builds a runtime.Adapter for "remote:" runs.
	// Required when MeshSnapshotFn is non-nil; ignored otherwise.
	PeerAdapterFactory func(deviceID string) (infruntime.Adapter, error)

	// Phase 7 routing inputs. nil-safe: the loopback Selector inside
	// agentInferenceProvider checks for nil before consulting these
	// (the existing pre-Phase-7 mesh fallback tests rely on this).
	// The overlay-side Selector (localOnlySelector below) does NOT
	// receive these — loop prevention is maintained.
	Sticky         *router.StickyStore
	LocalInFlight  *router.InFlightTracker
	StickyInFlight *router.StickyInFlight
	LocalRTT       func() map[string]uint32
	LocalErrors    func() map[string]float32

	// Recorder is the Phase 9 composite telemetry sink threaded into
	// the loopback Selector (router.Inputs.Recorder), the loopback
	// gateway (gateway.Deps.Recorder), and the overlay inference
	// server (inference.Config.Recorder). nil disables all emission
	// uniformly; intermediate subsystems remain functionally unchanged.
	Recorder *observability.Recorder

	// --- Public Share consumer inputs (waired#827) --------------------
	//
	// Threaded into the LOOPBACK Selector only; localOnlySelector leaves
	// them unset so an overlay-arriving peer request never applies this
	// device's outbound public-routing posture (loop prevention, same
	// reasoning as Sticky / LocalInFlight above).

	// PublicPolicy returns the consumer's resolved Public Share posture.
	// nil admits no public candidates.
	PublicPolicy func() router.PublicPolicy
	// OnPublicGrantDemand wakes the background grant acquirer when a
	// request wanted a public candidate and no grant was held.
	OnPublicGrantDemand func()
	// OnPublicGrantUsed reports the Public Share grant behind each
	// committed public route so the acquirer renews grants in use and
	// lapses idle ones (waired#898). Loopback only. nil disables it.
	OnPublicGrantUsed func(grantID string)
	// OnPublicNudge receives the pre-consent hint; the receiver owns
	// once-ness.
	OnPublicNudge func(router.PublicNudge)
	// OnPublicUsage receives per-request token metering for the PEER
	// OVERLAY listener (:9474) only — the surface where this device
	// serves other people's requests (waired#829). nil disables
	// reporting; local telemetry is unaffected either way.
	OnPublicUsage func(context.Context, gateway.UsageSample)

	// LocalAdmission is threaded into the LOCAL gateway surfaces
	// (:9473 / :9472 / :9480) so the owner's own engine work shares one
	// admission counter with the peer overlay — the "local" half of the
	// owner-priority latch (public share spec §8.2, waired#899). The
	// overlay surface deliberately leaves it unset: its requests are
	// already counted by the inference server's capacityGate.
	//
	// nil disables the accounting (unit tests, pre-session boot).
	LocalAdmission func(context.Context) func()

	// ServingInflight / ServingAdmitted read the other end of the counter
	// LocalAdmission feeds — what this machine is serving now, and how
	// much it has served in total — so the install-time host-speed
	// measurement can tell a quiet engine from a working one
	// (waired-agent#703).
	//
	// Both come off the same localAdmissionRelay, which is why they are
	// deps rather than a field set after construction: the provider is
	// built here and the inference.Server exists further down main().
	//
	// nil means "nothing is serving", which is what a host with no
	// inference server is doing. Every existing caller and test keeps
	// today's behaviour.
	ServingInflight func() int
	ServingAdmitted func() uint64

	// OnPeerOutcome folds each request this device dispatched to a mesh
	// peer into main()'s router.ErrorWindow, whose Snapshot is already
	// threaded back the other way as LocalErrors above. It goes onto the
	// same LOCAL gateway surfaces as LocalAdmission — the ones that can
	// send work to a peer — and the overlay surface leaves it unset,
	// having no peer to observe.
	//
	// nil disables the accounting (unit tests, pre-session boot), which
	// leaves the Selector's error-rate tie-break reading zeros.
	OnPeerOutcome func(deviceID string, ok bool)

	// Routing returns the operator's currently-live RoutingPreference
	// (Tailscale-exit-node-style manual routing). The Selector calls
	// it once per SelectK to read mode + pinned peer atomically. nil
	// keeps the pre-feature behaviour (Mode=auto).
	Routing func() state.RoutingPreference

	// BrowserHardening turns on Host/Origin allow-listing for every loopback
	// gateway this file binds (waired-ai/waired#1195). It is the agent's
	// --browser-hardening flag; main.go feeds the same value to the
	// management API and the Claude gateway, so all four loopback listeners
	// carry the same allow-list.
	//
	// It used to skip the Local Gateway, on the grounds that its bearer
	// token already stood in a page's way and that the docs pointed hosted
	// browser chat UIs at it. Neither holds: the token is gone
	// (waired-ai/waired#1277), and a chat UI is now expected to run on this
	// machine or to reach it over the mesh.
	BrowserHardening bool
}

// startInferenceSubsystem brings up the runtime registry, gateway,
// background pre-pull goroutine, and InferenceProvider used by the
// management API. Returns the wired Provider so main.go can pass it
// to management.Server.WithInference.
//
// stateDir is the same `--state-dir` main resolves. No credential is read
// from it for the gateways: none of the loopback listeners carries one
// (waired-ai/waired#1277). What separates a legitimate local client from a
// page the user has open is the Host/Origin allow-list, not a secret.
func startInferenceSubsystem(ctx context.Context, wg *sync.WaitGroup, logger *slog.Logger, stateDir string, cfg agentconfig.InferenceConfig, deps inferenceSubsystemDeps) (*inferenceSubsystem, management.InferenceProvider, error) {
	isPaused := deps.IsPaused
	isInferenceDisabled := deps.IsInferenceDisabled
	inferenceState := deps.InferenceState
	// Including internal models: this list backs model-name RESOLUTION
	// for the router and gateway. An internal entry has to resolve —
	// the routing sentinel pins one as this daemon's bundled model —
	// and withholding it here would 404 the very request that proves
	// routing works. What must not offer it are the pickers, and those
	// take the filtered list.
	manifests, err := catalog.BundledManifestsIncludingInternal()
	if err != nil {
		return nil, nil, fmt.Errorf("inference: load bundled manifests: %w", err)
	}
	for _, m := range manifests {
		if err := m.Validate(); err != nil {
			return nil, nil, fmt.Errorf("inference: bundled manifest %s invalid: %w", m.ModelID, err)
		}
	}

	// Apply tray-driven preferred-model override (preferred-model.json)
	// before chooseEngine / model picking runs. This file is written by
	// POST /waired/v1/inference/preferred-model and survives restarts so
	// the tray's "click → SIGTERM → systemd restart" actually lands on
	// the operator's choice.
	prefPath := agentconfig.DefaultPreferencePath()
	// prefNone / prefUnanswered survive past this block into the provider
	// below: the "run without a local model" choice and the abandoned
	// model question (#586) are applied by the provider's pre-pull path,
	// not by rewriting cfg (see ApplyPreferenceOverride).
	prefNone, prefUnanswered := false, false
	if pref, ok, err := agentconfig.LoadPreference(prefPath); err != nil {
		logger.Warn("preferred-model.json unreadable; ignoring", "path", prefPath, "err", err)
	} else if ok && pref.None {
		prefNone = true
		logger.Info("preferred-model.json records no-model-selected; the bundled fallback pre-pull stands down",
			"set_at", pref.SetAt)
	} else if ok && pref.Unanswered {
		prefUnanswered = true
		logger.Info("preferred-model.json records an unanswered model question; the bundled fallback pre-pull stays down until someone chooses",
			"set_at", pref.SetAt)
	} else if ok {
		logger.Info("preferred-model override applied",
			"model_id", pref.ModelID, "set_at", pref.SetAt)
		agentconfig.ApplyPreferenceOverride(&cfg, pref)
	}

	statePath := catalog.DefaultStatePath()
	store := catalog.NewStore(statePath)

	cachePath := defaultCachePath()
	// The engine probe resolves the binary the way the daemon resolves
	// the one it spawns (state dir first), not from $PATH — waired's own
	// engine is deliberately off $PATH, so the stock PATH probe reported
	// no version on exactly the hosts waired provisioned (#238).
	//
	// Held as a value rather than passed inline: the provider takes the
	// SAME resolver (below), so it can measure the version when it needs
	// one instead of only through this profiler's 30 s snapshot — which
	// on a fresh install predates the engine install entirely (#361).
	engineVersionProbe := engineVersionOnHost(runtime.GOOS, stateDir, hardware.EngineVersionAt)

	// #826: bring an already-installed bundled engine onto this build's
	// pin. Background and once per process; the installer scripts cover
	// the path a person watches, this covers `apt upgrade` and anything
	// else that restarts the agent without passing through them.
	//
	// Not gated on the local-inference toggle. That toggle is a runtime
	// state which can flip without a restart (#465), and an engine that
	// does not match the pin cannot serve the moment it does; the
	// download only happens where an engine is already installed, which
	// is itself the record that this host opted into running models.
	startEngineConverge(logger, stateDir)

	profiler := hardware.NewProfiler(cachePath,
		hardware.WithEngineVersion(engineVersionProbe),
		// The persisted memory figure (#568): the catalog endpoint's
		// fit verdicts must match what the wire publishes.
		hardware.WithRAMAvailableAtInstall(hostMemoryMeasurement(stateDir, os.Getenv)))

	// Step 5 migration runs inside Load; warm it once now so the
	// bootstrap log records what happened.
	if migrated, err := store.Load(); err == nil && migrated.Version >= 2 && migrated.Active != nil && migrated.Active.DecidedBy == "migration" {
		logger.Info("state.json migrated v1 → v2 (active preserved)",
			"model_id", migrated.Active.ModelID,
			"runtime", migrated.Active.Runtime)
	}

	// Step 10: validate the persisted active runtime is viable on the
	// current host. AllowAutoFallback=false makes a strict-mode
	// deployment fail-fast here so an operator notices a degraded
	// host immediately instead of silently demoting to ollama.
	decision, derr := chooseEngine(ctx, store, profiler, cfg, stateDir)
	for _, r := range decision.Reasons {
		logger.Info("engine decision", "reason", r)
	}
	if derr != nil {
		return nil, nil, fmt.Errorf("inference: %w", derr)
	}
	if decision.Source == "fallback" {
		logger.Warn("engine fallback engaged — session-scoped, state.json unchanged",
			"persisted", activeRuntimeOrEmpty(decision.PersistedActive),
			"running", decision.Engine)
	}

	registry := infruntime.NewRegistry()

	bundledOllamaModels := filepath.Join(infruntime.BundledOllamaDir(stateDir), "models")
	// ollamaResolver resolves the engine binary lazily (re-run on each
	// EnsureRunning so a freshly installed ollama is picked up without
	// an agent restart). The rule itself lives in resolveOllamaBinary so
	// everything that asks "is the engine here" gets the SAME answer —
	// see engineInstalledOnHost.
	ollamaResolver := func() (string, error) {
		return resolveOllamaBinary(runtime.GOOS, stateDir)
	}

	binary, err := ollamaResolver()
	if err != nil {
		// Ollama isn't installed yet — log and proceed. Pull / ensure
		// will fail until the engine is installed
		// (`waired runtimes install ollama`).
		binary = ""
		logger.Warn("ollama binary not found; inference subsystem will be inert until installed",
			"err", err)
	}

	// #290: pick the GPU-backend env for `ollama serve` from the host
	// hardware profile. Strix Halo is keyed off the CPU model (not GPU
	// detection) because on Linux its iGPU is invisible to the profiler
	// unless rocm-smi is installed. backendPlan.Preferred() seeds the
	// adapter; when backendPlan.Probes() is true the bootstrap goroutine
	// verifies the GPU actually engaged and falls back to the next step.
	hwProfile := profiler.Profile(ctx)
	gpuVendor := ""
	gpuModel := ""
	if len(hwProfile.GPUs) > 0 {
		gpuVendor = strings.ToLower(hwProfile.GPUs[0].Vendor)
		gpuModel = hwProfile.GPUs[0].Model
	}
	backendPlan := infruntime.ResolveOllamaBackend(infruntime.BackendInputs{
		GOOS:             runtime.GOOS,
		PrimaryGPUVendor: gpuVendor,
		PrimaryGPUModel:  gpuModel,
		StrixHaloAPU:     hardware.IsStrixHaloAPU(hwProfile.CPU.Model),
		// Undetected-iGPU fallback: on Linux a non-Strix AMD mobile APU is
		// invisible without rocm-smi, so route it to Vulkan by CPU model (#68).
		AMDMobileAPU: hardware.IsAMDMobileAPU(hwProfile.CPU.Model),
	})
	logger.Info("ollama gpu backend selected",
		"backend", backendPlan.Preferred().Backend,
		"env", backendPlan.Preferred().Env,
		"probes_fallback", backendPlan.Probes(),
		"reason", backendPlan.Reason)

	ollamaCfg := infruntime.OllamaConfig{
		// waired-agent#861: residency is an operator setting, not a
		// constant. 0 (the default) holds the model indefinitely.
		KeepAlive:      cfg.IdleTimeout.Duration(),
		Binary:         binary,
		Host:           "127.0.0.1",
		Port:           cfg.ResolvedOllamaPort(),
		Spawner:        infruntime.DefaultSpawner{},
		BinaryResolver: ollamaResolver,
		// waired-agent#29: the adapter reports a dead engine here; the
		// provider owns the recovery policy. Assigned after the provider
		// exists (the closure needs it), just below.
		// #290: GPU-backend env (e.g. OLLAMA_VULKAN / HSA override).
		BackendEnv: backendPlan.Preferred().Env,
		// Blobs live in the waired-owned store, and only an exact-pin
		// orphan of a previous run may be adopted on a port conflict
		// (any other survivor fails loudly).
		ModelsDir:       bundledOllamaModels,
		ExpectedVersion: infruntime.OllamaPinnedVersion,
		// Capture `ollama serve`'s stdout/stderr so a cold-start failure
		// leaves a trail (the agent's slog only sees "not ready").
		LogDir: filepath.Join(infruntime.BundledOllamaDir(stateDir), "logs"),
		// #22: macOS system LaunchDaemons run with $HOME unset, so `ollama
		// serve` dies at startup ("$HOME is not defined") before it can even
		// bind the port. Give it a writable, agent-owned HOME under the
		// runtime dir (ollama creates ~/.ollama there for its key/config);
		// harmless where the launcher already sets HOME (Linux systemd).
		StateHome: infruntime.BundledOllamaDir(stateDir),
	}
	migrateLegacyOllamaModels(logger, bundledOllamaModels, "")
	ollama := infruntime.NewOllamaAdapter(ollamaCfg)
	registry.Register(ollama)
	// Record the chosen backend up front so the doctor / inference status
	// shows it even before (or without) the engagement probe runs. The
	// probe may revise it to a fallback or to "cpu" (#290).
	ollama.SetResolvedBackend(backendPlan.Preferred().Backend)
	// #621: size the serve tuning (context window / KV cache quantization
	// / parallelism) for the model this engine will serve and export it
	// to the spawn env — without it every model silently loads at the
	// engine-default 32k window regardless of the manifest. Computed here
	// (before the bootstrap goroutine's first EnsureRunning) so the very
	// first spawn carries it.
	var (
		ollamaTune         ollamaTuning
		ollamaTuned        bool
		ollamaTuneTag      string
		ollamaTuneManifest catalog.Manifest
		ollamaTuneVariant  catalog.Variant
	)
	if decision.Engine == catalog.RuntimeOllama {
		if tuneState, serr := store.Load(); serr != nil {
			logger.Warn("state.json unreadable; ollama serve keeps engine-default context", "err", serr)
		} else if tm, tv, ok := resolveTuningTarget(cfg, manifests, tuneState); ok {
			ollamaTuneManifest, ollamaTuneVariant = tm, tv
			ollamaTune = computeOllamaTuning(tm, tv, hwProfile, ollamaKVRequest(), ollamaObservedFromState(tuneState, tm, tv))
			ollamaTuned = true
			if ms, found := tuneState.Models[tm.ModelID]; found && ms.OllamaTag != "" {
				ollamaTuneTag = ms.OllamaTag
			} else if tv.Source.Type == catalog.SourceOllama {
				ollamaTuneTag = tv.Source.Tag
			}
			ollamaTune = applyModelDecisionReasons(cfg, tm, ollamaTune, logger)
			ollama.SetModelEnv(ollamaTune.Env())
			ollama.SetAppliedTuning(ollamaTune.ModelTuning)
			logger.Info("ollama serve tuning computed",
				"model", ollamaTune.ModelID, "variant", ollamaTune.VariantID,
				"ctx", ollamaTune.ContextLength, "kv", ollamaTune.KVCacheType,
				"parallel", ollamaTune.NumParallel, "warning", ollamaTune.Warning)
		} else {
			logger.Warn("no tuning target resolvable; ollama serve keeps engine-default context")
		}
	}
	// Spawn-time fallback resolver (#624): the block above runs once and
	// only when the boot-time engine decision already landed on ollama.
	// On a fresh install the binary can arrive mid-bootstrap ("no engine
	// viable: ollama needs binary"), after which the engine spawns
	// WITHOUT the env above and serves untuned at its 32k default. The
	// provider recomputes the tuning at each spawn that has no explicit
	// env yet, so late-viable engines come up tuned too. Explicit
	// SetModelEnv (above, and the verify-degrade restart) stays
	// authoritative.
	ollama.SetModelEnvProvider(func() ([]string, infruntime.ModelTuning, bool) {
		tuneState, serr := store.Load()
		if serr != nil {
			logger.Warn("spawn-time tuning: state.json unreadable; keeping engine-default context", "err", serr)
			return nil, infruntime.ModelTuning{}, false
		}
		tm, tv, ok := resolveTuningTarget(cfg, manifests, tuneState)
		if !ok {
			return nil, infruntime.ModelTuning{}, false
		}
		tune := applyModelDecisionReasons(cfg, tm,
			computeOllamaTuning(tm, tv, hwProfile, ollamaKVRequest(), ollamaObservedFromState(tuneState, tm, tv)), logger)
		logger.Info("ollama serve tuning computed at spawn",
			"model", tune.ModelID, "variant", tune.VariantID,
			"ctx", tune.ContextLength, "kv", tune.KVCacheType,
			"parallel", tune.NumParallel, "warning", tune.Warning)
		return tune.Env(), tune.ModelTuning, true
	})

	// `ollama pull` is a client of the serving engine — point it at the
	// resolved port or pulls land on whatever answers 11434. It resolves
	// the binary through ollamaResolver on every pull rather than freezing
	// the boot-time path: on a fresh install that path is empty, and the
	// puller's own fallback cannot see a state-dir install (#304).
	puller := download.NewResolvingPuller(ollamaResolver, download.DefaultRunner{},
		fmt.Sprintf("OLLAMA_HOST=127.0.0.1:%d", cfg.ResolvedOllamaPort()))

	provider := &agentInferenceProvider{
		cfg:            cfg,
		logger:         logger,
		agentCtx:       ctx,
		manifests:      manifests,
		store:          store,
		profiler:       profiler,
		registry:       registry,
		ollama:         ollama,
		puller:         puller,
		stateDir:       stateDir,
		preferencePath: deps.PreferencePath,
		dlProgress:     newDownloadProgress(),
		ollamaUsable:   func() bool { _, e := ollamaResolver(); return e == nil },
		// The one rule, not the cached profile (#225). engineViable and
		// setupEngineState already ask this way; this was the site that
		// did not.
		vllmUsable: func() bool {
			return engineInstalledOnHost(runtime.GOOS, stateDir, catalog.RuntimeVLLM)
		},
		// The same measurement the profiler is built with, reachable
		// without its cache (#361).
		engineVersionProbe: engineVersionProbe,
		bootPlan: engineBootstrapPlan{
			backend:      backendPlan,
			tuned:        ollamaTuned,
			tune:         ollamaTune,
			tuneTag:      ollamaTuneTag,
			tuneManifest: ollamaTuneManifest,
			tuneVariant:  ollamaTuneVariant,
		},
		isInferenceDisabled: isInferenceDisabled,
		inferenceState:      inferenceState,
		meshSnapshotFn:      deps.MeshSnapshotFn,
		sticky:              deps.Sticky,
		localInFlight:       deps.LocalInFlight,
		stickyInFlight:      deps.StickyInFlight,
		localRTT:            deps.LocalRTT,
		localErrors:         deps.LocalErrors,
		publicPolicy:        deps.PublicPolicy,
		onPublicGrantDemand: deps.OnPublicGrantDemand,
		onPublicGrantUsed:   deps.OnPublicGrantUsed,
		onPublicNudge:       deps.OnPublicNudge,
		recorder:            deps.Recorder,
		routing:             deps.Routing,
		servingInflight:     deps.ServingInflight,
		servingAdmitted:     deps.ServingAdmitted,
	}
	// engineChoice re-runs THIS boot's decision against the live host, so an
	// adopt trigger asks the same rule rather than a snapshot taken before
	// the engine existed (#339, the shape #304 gave the ollama binary). A
	// field rather than a direct chooseEngine call so the adopt path is
	// testable without a CUDA host and a real venv.
	//
	// Assigned here rather than in the literal above because it logs through
	// the provider's dedup slots, and a `provider := &T{...}` literal cannot
	// name the variable it is initialising.
	provider.engineChoice = func(c context.Context) (string, bool) {
		d, err := chooseEngine(c, store, profiler, cfg, stateDir)
		if err != nil {
			// Strict mode refusing to fall back. At boot that exits the
			// process; here the daemon is already running, so keep the
			// engine it has and say why. Deduped for the same reason the
			// answer below is: this runs per adopt trigger, not once.
			provider.logOnChange(&provider.lastReChoice, "live engine re-choice declined", err.Error())
			return "", false
		}
		// Info, deduped on the joined reason (#778). A level trigger would
		// write a line every two seconds on a wedged host; deduping keeps
		// the steady state quiet while the first answer and every CHANGE of
		// answer — which is the whole signal — still land at the default
		// level. Read `engine` alongside it: empty means no-engine.
		provider.logOnChange(&provider.lastReChoice, "live engine re-choice",
			strings.Join(d.Reasons, "; "), "engine", d.Engine, "source", d.Source)
		return d.Engine, true
	}
	// Not in the literal above: the field is an atomic.Pointer now (#339),
	// because the adopt trigger may take on a vLLM venv installed after
	// this process started.
	provider.setServingEngine(decision.Engine)
	// Not in the literal above either (atomic.Bool): the persisted "run
	// without a local model" choice and the abandoned-question record
	// (#586), read with the preference at the top of this function.
	provider.noModelSelected.Store(prefNone)
	provider.modelQuestionUnanswered.Store(prefUnanswered)

	// waired-agent#29: hand the adapter a way to report its engine's death.
	// Installed here rather than in OllamaConfig because the handler is a
	// provider method and the provider is built from the adapter.
	if ollama != nil {
		ollama.SetOnUnhealthy(provider.onEngineUnhealthy)
		ollama.SetOnStartFailed(provider.onEngineStartFailed)
		// waired-agent#1038: and a way to report that the accelerator ran
		// out of memory serving a request, which is a fact about the
		// configuration rather than about engine health.
		ollama.SetOnFitFailure(provider.onEngineFitFailure)
	}

	// Engine switch (#557): an explicit preferred_engine that differs from
	// a stale persisted Active means the operator changed engines. Clear
	// the old ActiveSelection so the bootstrap re-activates on the new
	// engine (and pulls that engine's variant) instead of trying to serve
	// the previous engine's model. The agent owns the state dir so this
	// write is ownership-safe (unlike a CLI-side write; cf. #484/#525). The
	// actual venv/HF-puller wiring + engine spawn happen in bootstrapVLLM
	// (Linux only), which vLLM binds to one on-disk model.
	if decision.Source == "preference" && decision.PersistedActive != nil &&
		decision.PersistedActive.Runtime != decision.Engine {
		if err := store.Update(func(s *catalog.State) { s.Active = nil }); err != nil {
			logger.Warn("engine switch: clearing stale active selection failed", "err", err)
		} else {
			logger.Info("engine switch: cleared stale active selection",
				"was", decision.PersistedActive.Runtime, "now", decision.Engine)
		}
	}

	// Core deps shared by all three gateway surfaces (local gateway :9473,
	// peer overlay :9474, Claude intercept :9472). Each surface sets its
	// policy-bearing fields (Allow*, gates, selection, class handling)
	// explicitly below so the intentional per-surface differences stay
	// visible at the construction site — only the fields that must never
	// diverge live here. The Recorder is wired on every surface: without
	// it, requests served there were invisible in the observability event
	// ring / metrics — the gap the #496 routing sentinel exposed.
	baseGatewayDeps := func() gateway.Deps {
		return gateway.Deps{
			Runtimes:      registry,
			ListManifests: func() []catalog.Manifest { return manifests },
			Recorder:      deps.Recorder,
			// #623 over-window guard, on EVERY surface that forwards a
			// prompt to an engine. It rode the intercept and the overlay
			// only, which left the OpenAI-speaking loopback surface
			// (:9473) handing an over-long prompt to ollama to truncate
			// at the head — the failure the guard exists to prevent,
			// reached by a different door. A surface-by-surface opt-in is
			// the wrong shape for it: the reason to guard is that a prompt
			// is about to reach an engine, which is true of all three.
			// 0 means "unknown" and fails open.
			ContextWindowFor: provider.ContextWindowFor,
			// What this device's engine holds right now (waired-agent#837).
			// On every surface for the same reason ContextWindowFor is: the
			// reason to observe is that a prompt is about to reach an engine.
			// It reads the cached /api/ps observation the local probe loop
			// already refreshes, so it costs no engine traffic, and it is
			// never consulted for a remote selection.
			LocalResidency: provider.LocalResidency,
		}
	}

	gwDeps := baseGatewayDeps()
	gwDeps.Selector = provider
	gwDeps.AllowOpenAI = cfg.AllowOpenAIAPI
	gwDeps.AllowAnthropic = cfg.AllowAnthropicAPI
	gwDeps.IsPaused = isPaused
	// No IsInferenceDisabled here: the toggle is one fact about the LOCAL
	// candidate, and the Selector reads it from Inputs.LocalServingOff
	// (baseRouterInputs). Gating the whole handler set on it kept an
	// engine-less node off the mesh entirely (waired-agent#829).
	gwDeps.PeerAdapterFactory = deps.PeerAdapterFactory
	// LOCAL surface: the owner's own engine work counts against the
	// machine's shared admission counter (§8.2, waired#899).
	gwDeps.LocalAdmission = deps.LocalAdmission
	// The read half of that same counter, so a request can record what it
	// arrived behind (waired-agent#837). Wired wherever LocalAdmission is,
	// and for the same reason: these are the listeners whose traffic lands on
	// this machine's engine.
	gwDeps.LocalInflight = deps.ServingInflight
	// LOCAL surface: it can dispatch to a peer, so it can observe how
	// that peer answered (waired-agent#281).
	gwDeps.OnPeerOutcome = deps.OnPeerOutcome
	gwAddr := fmt.Sprintf("127.0.0.1:%d", cfg.LocalGatewayPort)
	gw := gateway.NewServer(gateway.ServerConfig{
		Addr: gwAddr,
		// The Host/Origin allow-list that keeps a web page the user has
		// open from reaching this listener by DNS-rebinding: its connection
		// comes from 127.0.0.1 too, so the bind cannot see it
		// (waired-ai/waired#1195). It used to ride only the data plane
		// because this listener had a bearer token; it no longer does
		// (waired-ai/waired#1277), so the allow-list is what stands in a
		// page's way.
		BrowserHardening: deps.BrowserHardening,
	}, gwDeps)

	// Phase 4: build a SECOND HandlerSet for the overlay listener.
	// localOnlySelector wraps provider's selection logic but with a
	// nil MeshSnapshotFn so a peer-side request that fails the local
	// locality filter never recurses to a third peer (loop
	// prevention). PeerAdapterFactory is also nil here for the same
	// reason.
	overlaySelector := &localOnlySelector{p: provider}
	overlayDeps := baseGatewayDeps()
	overlayDeps.Selector = overlaySelector
	// Public Share usage reporting rides the overlay set alone: the
	// loopback / intercept / data-plane surfaces serve this device's own
	// operator, whose usage is nobody's to report (spec §12).
	overlayDeps.OnUsage = deps.OnPublicUsage
	overlayDeps.AllowOpenAI = cfg.AllowOpenAIAPI
	overlayDeps.AllowAnthropic = cfg.AllowAnthropicAPI
	// No ResolveUnknownModel here: the Claude intercept moved to
	// claudeHandlerSet below (#601), and peer traffic on :9474 is
	// OpenAI-shaped with an already-resolved EngineModel — exact
	// catalog semantics are correct for it, like :9473.
	// The base deps carry ContextWindowFor. It matters most here
	// (waired-agent#436): this is the SERVING side of a mesh leg, the one
	// HandlerSet whose traffic is not the owner's own, and the requesting
	// node's copy of the check is sized from an advertisement — a
	// snapshot that a re-tune between the push and the request leaves
	// stale. Only this side knows the window the engine is loaded with
	// right now.
	//
	// AuthToken intentionally empty: the inference.Server applies
	// peer auth via verifyPeerSignature; loopback bearer doesn't
	// apply to overlay traffic.
	// IsPaused / IsInferenceDisabled also empty: the inference.Server
	// wraps its own pausedGate / inferenceGate around this HandlerSet.
	// PeerAdapterFactory stays nil to enforce loop prevention.
	// LocalAdmission stays nil too: peer requests reach here THROUGH
	// the inference server's capacityGate, which already counted them
	// (§8.2). This is the one surface that is not the owner's own
	// traffic.
	// OnPeerOutcome stays nil for the same reason PeerAdapterFactory
	// does: this listener cannot dispatch to a peer, so it has no peer
	// outcome to report. Setting it here — or moving it into
	// baseGatewayDeps for symmetry — would hand the serving side a write
	// handle to the requesting side's error window.
	//
	// StreamKeepalive and LocalTTFBBudget stay 0, and this one must not be
	// "fixed" for symmetry either (waired-agent#837). Every selection on
	// this listener is local by construction, so the gateway's own
	// local-leg condition does NOT protect it — only the unwired dep does.
	// A serving peer that wrote a keepalive would hand its CALLER a first
	// byte its engine never produced, disarming that caller's #757 budget
	// and making every peer on the mesh look responsive.
	overlayHandlerSet := gateway.NewHandlerSet(overlayDeps)

	// Claude-intercept HandlerSet (#601/#647): the third HandlerSet,
	// serving only the managed-settings proxy (:9472). Unlike the
	// overlay set it is mesh-capable — the intercept is a LOCAL surface
	// (loopback from Claude Code on this device), so a remote dispatch
	// here is one hop and the receiving peer's overlay stays local-only.
	// The claudeSelector applies the operator's per-class node policy
	// (main / sub → local | pinned peer) per request; ClassifyModel
	// derives the class from the managed-settings subagent label; the
	// resolver maps unresolvable Anthropic ids to the class target
	// node's model (#600 extended per-class).
	claudeDeps := baseGatewayDeps()
	claudeDeps.Selector = &claudeSelector{p: provider}
	// AllowOpenAI stays false: the intercept surface speaks Anthropic
	// shapes only.
	claudeDeps.AllowAnthropic = cfg.AllowAnthropicAPI
	// The base deps' ContextWindowFor also feeds this surface's
	// /anthropic/v1/models advertisement of the served model's real
	// window (#623), alongside the over-window 400 that makes Claude Code
	// compact instead of overrunning the model.
	claudeDeps.ClassifyModel = classifyClaudeModel
	// #52 (opt-in): advertise the reserved route-directive ids in /v1/models
	// discovery so they appear in Claude Code's /model picker. The intercept
	// (buildClaudeListener) honours the same ids as per-request route
	// directives under the same flag.
	claudeDeps.ClaudeModelDirectives = cfg.ClaudeModelRouteDirectives
	// #757: bound the pre-first-byte window on a PEER leg per traffic class so a
	// stalled-but-reachable serving peer reroutes (auto mode only — see the
	// intercept's X-Waired-Fallback-Allowed gate) instead of hanging the turn.
	// Subagents get the tighter budget; 0 disables. The gateway arms this only
	// for remote:* selections, so a locally-served turn is never affected.
	claudeDeps.TTFBBudget = func(class string) time.Duration {
		ms := cfg.ClaudeTTFBBudgetMainMs
		if class == state.ClaudeClassSub {
			ms = cfg.ClaudeTTFBBudgetSubMs
		}
		if ms <= 0 {
			return 0
		}
		return time.Duration(ms) * time.Millisecond
	}
	// waired-agent#1040: past that budget, the peer itself decides whether
	// the wait goes on. Main only — 0 for the subagent class leaves it on
	// the flat deadline, which is where "a stalled subagent is cheap to
	// reroute" put it and where Claude Code's own 120 s helper deadline
	// keeps it (waired-agent#1041). The gateway ignores a ceiling that is
	// not longer than the class's budget, so a misconfiguration cannot
	// shorten a wait.
	claudeDeps.PeerWaitCeiling = func(class string) time.Duration {
		if class == state.ClaudeClassSub || cfg.ClaudePeerWaitCeilingMs <= 0 {
			return 0
		}
		return time.Duration(cfg.ClaudePeerWaitCeilingMs) * time.Millisecond
	}
	// waired-agent#837: the same bound, for a leg THIS computer's engine
	// serves. Its default is ten minutes rather than the peer budgets' 60/20
	// seconds — a cold load here is legitimate and rerouting one costs the
	// user the local serving they chose, so only a wait no client would still
	// be waiting on ends the turn. 0 disables it.
	claudeDeps.LocalTTFBBudget = func() time.Duration {
		if cfg.ClaudeLocalTTFBBudgetMs <= 0 {
			return 0
		}
		return time.Duration(cfg.ClaudeLocalTTFBBudgetMs) * time.Millisecond
	}
	// The other half, for the legs that may not be aborted: route=waired and
	// pinned turns have nowhere else to go, so they wait — but the wire stops
	// being empty while they do. The interval is the cadence on which this
	// agent re-observes its own engine, i.e. the cadence on which the fact
	// behind the wait can change.
	claudeDeps.StreamKeepalive = state.HeartbeatInterval
	// A bounded local leg leaves this computer's engine part-way through a
	// load nobody is waiting on any more. Finish it out of band so the next
	// turn is local again instead of paying for it a second time — the
	// warm-up is already single-flighted and checks /api/ps first, so a burst
	// of rerouted turns is one load.
	claudeDeps.OnLocalEngineAbandoned = provider.warmServingModel
	claudeDeps.ResolveUnknownModel = func(_, _ string) (string, bool) {
		// The Anthropic ids Claude Code sends name no catalog model and
		// were never meant to: the user picked a tier in /model, not a
		// model this fleet runs. So the honest translation is "the caller
		// named none" — the dynamic alias — and the routing mode picks a
		// NODE, whose own model answers (waired-agent#828).
		//
		// It used to resolve here instead, to the pinned peer's model or
		// else the device-active one. Both are answers to "which model
		// does the REQUESTER have in mind", which is the question that
		// made a pin work only when both ends ran the same thing.
		return router.DefaultModelAlias, true
	}
	// PeerAdapterFactory: unlike the overlay set, remote selections
	// are dispatched — that's the point of #601.
	claudeDeps.PeerAdapterFactory = deps.PeerAdapterFactory
	// LOCAL surface (§8.2, waired#899): the busiest one — this is where
	// the owner's coding agent lands. Only the legs this device's own
	// engine serves are counted; a remote leg loads the peer, not us.
	claudeDeps.LocalAdmission = deps.LocalAdmission
	claudeDeps.LocalInflight = deps.ServingInflight
	// The remote legs the line above does NOT count are exactly the ones
	// this observes: the busiest surface is also the one whose auto
	// fallback depends most on knowing which peers are answering.
	claudeDeps.OnPeerOutcome = deps.OnPeerOutcome
	// Claude Code presents its own subscription credentials; loopback plus
	// the Host/Origin allow-list is the trust boundary, the same posture
	// every gateway surface now has.
	claudeHandlerSet := gateway.NewHandlerSet(claudeDeps)

	// Spawn the gateway listener. This is the only local inference surface
	// there is, so a bind failure is fatal rather than a warning — and it
	// says which port it could not take, because "address already in use"
	// with no number is the least useful thing to hand someone whose editor
	// or exporter happens to sit on 9473.
	gwLn, err := net.Listen("tcp", gwAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("inference: the local gateway could not bind %s (set inference.local_gateway_port in agent.json to move it): %w", gwAddr, err)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := gw.Serve(ctx, gwLn); err != nil {
			logger.Error("gateway server stopped", "err", err)
		}
	}()

	// Engine startup + bundled-model pre-pull. Both run in the
	// background so the rest of the agent (overlay / management /
	// network map subscriber) doesn't block on a slow `ollama pull`.
	wg.Add(1)
	go func() {
		defer wg.Done()
		// ONE entry point for both engines since #339. It used to fork
		// here — a `vllmBootstrapOnce.Do(bootstrapVLLM)` arm that returned,
		// and the ollama arm below — which is what made vLLM the engine
		// with no adopt path: every later trigger reached only the ollama
		// half, so a venv installed after boot was never taken on.
		//
		// The whole start + bootstrap sequence lives on the provider so it
		// can run again when an engine is installed after boot (#304). It
		// re-checks for the engine itself — the boot-time `binary` snapshot
		// and, now, the boot-time engine decision are deliberately not
		// consulted. The two engines' orderings still differ (vLLM needs
		// the weights on disk before it can start, `ollama serve` starts
		// model-agnostic); that difference lives inside each arm.
		provider.runEngineBootstrap(ctx, "boot")
	}()

	// Step 12: pre-cache the better candidate (if any) in the
	// background. The next `waired runtimes refresh` then runs as a
	// near-instant swap because the weights are already on disk.
	// PreCacheUpdateCandidate=false (config) opts out.
	if cfg.PreCacheUpdateCandidate {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Wait briefly so the bundled-model pre-pull goroutine
			// dominates the bandwidth at startup; the pre-cache is
			// the lower-priority background task.
			select {
			case <-time.After(30 * time.Second):
			case <-ctx.Done():
				return
			}
			provider.maybePreCache(ctx)
		}()
	}

	// Stop the engine on shutdown so we don't leave a stray
	// `ollama serve` process behind.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := ollama.Stop(stopCtx); err != nil {
			logger.Warn("ollama stop returned error", "err", err)
		}
		// #557: stop the vLLM subprocess (if we spawned one) so a
		// restart doesn't leave an orphan holding the GPU / the port.
		if vllm := provider.vllmAdapter(); vllm != nil {
			if err := vllm.Stop(stopCtx); err != nil {
				logger.Warn("vllm stop returned error", "err", err)
			}
		}
	}()

	logger.Info("inference subsystem started",
		"gateway_addr", gwLn.Addr().String(),
		"ollama_port", cfg.ResolvedOllamaPort(),
		"bundled_model", cfg.BundledModelID,
		"pull_on_startup", cfg.PullOnStartup,
	)
	return &inferenceSubsystem{
		cfg: cfg, logger: logger, manifests: manifests, store: store,
		profiler: profiler, registry: registry, ollama: ollama,
		puller: puller, gateway: gw, overlayHandlerSet: overlayHandlerSet,
		claudeHandlerSet: claudeHandlerSet,
		provider:         provider,
	}, provider, nil
}

// defaultCodingModelID resolves what the dynamic coding alias
// (waired/default) serves on this host: the explicit
// preferred model, else the persisted active selection, else the
// bundled default — the same order resolveTuningTarget sizes the
// engine for (#632). Empty means "no dynamic default"; the router
// then falls back to static ModelAliases lookup.
func defaultCodingModelID(cfg agentconfig.InferenceConfig, st catalog.State) string {
	if cfg.PreferredModelID != "" {
		return cfg.PreferredModelID
	}
	if st.Active != nil && st.Active.ModelID != "" {
		return st.Active.ModelID
	}
	return cfg.BundledModelID
}

// localOnlySelector is the overlay-listener half of Phase 4 loop
// prevention. It reuses agentInferenceProvider's manifests / state /
// hardware / runtime registry but pins MeshSnapshotFn to nil so a
// peer-side request never enters the mesh-fallback branch of the
// router.
type localOnlySelector struct {
	p *agentInferenceProvider
}

func (l *localOnlySelector) buildSelector(ctx context.Context) *router.Selector {
	// Only the shared base Inputs, nothing layered on top:
	//   - MeshSnapshotFn stays nil — no recursion to mesh peers
	//     (Phase 4 loop prevention);
	//   - the Phase 7/8/9 routing signals and the manual routing
	//     override stay unset — an overlay-arriving peer request must
	//     not affect this agent's in-flight bookkeeping, sticky
	//     bindings, error window, reachability exclusions, or
	//     selection telemetry.
	return router.NewSelector(l.p.baseRouterInputs(ctx))
}

func (l *localOnlySelector) Select(ctx context.Context, req router.Request) (router.Selection, error) {
	return l.buildSelector(ctx).Select(ctx, req)
}

func (l *localOnlySelector) SelectK(ctx context.Context, req router.Request, k int) ([]router.Candidate, error) {
	return l.buildSelector(ctx).SelectK(ctx, req, k)
}

// agentInferenceProvider implements both management.InferenceProvider
// (the loopback API surface) and gateway.SelectorIface (so the
// gateway and management share one selector).
type agentInferenceProvider struct {
	cfg    agentconfig.InferenceConfig
	logger *slog.Logger
	// agentCtx is the daemon's long-lived context (the one that drives the
	// engine-startup + shutdown goroutines). The in-process reconcile (#812)
	// runs its Stop → EnsureRunning bounce against THIS ctx, never a request
	// or pull ctx, which are cancelled the moment their handler/job returns.
	agentCtx  context.Context
	manifests []catalog.Manifest
	store     *catalog.Store
	profiler  *hardware.Profiler
	registry  *infruntime.Registry
	ollama    *infruntime.OllamaAdapter
	puller    *download.Puller

	// #557 local vLLM serving. stateDir is the agent state root (needed
	// for the HF weight-download destination and the venv path). engine
	// is the chosen serving engine (decision.Engine); "" is treated as
	// ollama by servingEngine(). vllm is the running vLLM adapter, set by
	// bootstrapVLLM (Linux only) and held via the cross-platform Adapter
	// interface so this shared struct never names the Linux-only concrete
	// type; unset unless engine == vllm and the engine started. The HF
	// puller and venv paths are resolved on demand inside the Linux-only
	// vLLM code (inference_vllm_linux.go), not stored here.
	//
	// BOTH are atomic, and for the same reason (#337): they are written on
	// the engine-startup goroutine while the management handlers
	// (EngineProvenance, runtimeStatusFor, appliedContextWindow) and the
	// shutdown goroutine read them. Reach them through setVLLM /
	// vllmAdapter and setServingEngine / servingEngine, never the pointers
	// directly. The interface indirection is the same shape as proxy.go's
	// atomic.Pointer[http.Handler].
	//
	// engine stopped being write-once at construction with #339: the adopt
	// trigger re-runs the boot rule against the live host and may take on a
	// vLLM venv installed after this process started, which no restart-free
	// path could reach while the boot decision was frozen.
	stateDir string
	engine   atomic.Pointer[string]
	vllm     atomic.Pointer[infruntime.Adapter]
	// vllmParked is the operator's hard engine-power latch for the vLLM
	// engine (#881). Reach it through setVLLMParked / vllmIsParked; see
	// engine_power.go for why it lives here rather than on the adapter, the
	// way ollama's does.
	vllmParked atomic.Bool

	// lastReChoice / lastStartDecline dedup the two lines the engine
	// re-evaluation emits when it declines, so a repeating trigger does not
	// write one per firing while a CHANGE of answer still reaches the log.
	// Separate slots because the two sites fire in sequence on the same
	// trigger: one shared slot would alternate and dedup nothing.
	//
	// They exist because both sites were Debug (#778): on an info-level
	// host, a trigger that fired and declined and a trigger that never
	// fired at all produced the same journal — nothing — so the rc9
	// campaign could only record its reading of the code as an analysis.
	lastReChoice     atomic.Pointer[string]
	lastStartDecline atomic.Pointer[string]

	// preferencePath is preferred-model.json — the same file the loopback
	// management API's preferred-model handler writes. The setup
	// reconciler persists the wizard's choice here so it survives the
	// restart an engine install may cause (#230). "" means "do not
	// persist", which is the unit-test default.
	preferencePath string

	// dlProgress holds live byte progress for in-flight model pulls so
	// Status() (and thus `waired status`) can show a percentage + size.
	// In-memory only (transient; never persisted to state.json).
	dlProgress *downloadProgress

	// pullsWG tracks background pull goroutines spawned by PullModel so
	// tests can join them before their t.TempDir() is removed (#377).
	// Not awaited on production shutdown (a long pull must not block SIGTERM).
	pullsWG sync.WaitGroup

	// pullMu guards pullsInFlight. It is a leaf: never held across a
	// store.Update, an engine call, or requestEngineReconcile.
	pullMu sync.Mutex
	// pullsInFlight maps model_id to the single pull running for it, so a
	// second dispatcher joins instead of starting a duplicate (#305).
	// Keyed by model_id ALONE, deliberately: variant choice depends on the
	// engine version and fails closed, so the same model resolves to the
	// plain tag before the engine reports a version and to the mtp tag
	// after — keying on the tag would let both download at once, which is
	// the 16.3 + 18.0 GB defect this exists to prevent. Lazily built,
	// because several unit-test providers construct this struct literally.
	pullsInFlight map[string]*pullJob
	// swapBounceDeferred records that a completed pull wants the engine
	// bounced for a pending model swap. The bounce is held until no pull
	// is in flight: `ollama pull` is a client of `ollama serve`, so
	// stopping the engine fails a SIBLING model's download (#305d).
	swapBounceDeferred atomic.Bool
	// retuneDeferred records that a serve-tuning RE-RESOLVE is owed. Held
	// to the same point as the swap bounce, for the same #305d reason.
	//
	// Two writers. runPullJob sets it when a pull finishes, because the
	// tuning was sized before those weights existed. ApplyConcurrency sets
	// it INSTEAD of reconciling when a download is in flight, because the
	// bounce a capacity change fires would kill that download (#359) — see
	// deferRetuneWhilePulling.
	//
	// Set unconditionally on a successful pull, and it means only "look
	// again" — never "bounce". resolveTuningTarget can only read the
	// on-disk variant once the model is Ready, so before that it sizes
	// from a guess; nothing re-ran it afterwards, so a fresh install
	// served the whole session tuned for a model it had not downloaded
	// yet (waired-agent#320). Whether anything actually changed is
	// reconcileEngineServe's ServeInputsEqual test, which is why setting
	// this on an unrelated pull is free.
	retuneDeferred atomic.Bool
	// warmInFlight single-flights the warm-up load (#320). The load is
	// minutes on a cold multi-GB model and the engine serves one at a
	// time, so a second trigger arriving mid-load must drop, not queue.
	warmInFlight atomic.Bool

	// setupFrameMu guards the four fields below: what the last folded
	// control-plane frame said about this host. Written by the setup
	// reconciler (setupNoteDesired), read by the boot pre-pull's hold
	// (#379). A leaf lock like pullMu — nothing is called while it is held
	// except the channel swap.
	setupFrameMu sync.Mutex
	// setupFrameCh is closed and replaced on every note, so a waiter parks
	// on one read of it and wakes on the next frame without polling. Lazily
	// built: several unit-test providers construct this struct literally.
	setupFrameCh chan struct{}
	// setupNamedModel is the first desired model id setup ever named, and
	// it is STICKY. Once the control plane has said which model this host
	// is for, the boot pre-pull of the bundled fallback is wrong forever —
	// a later frame that carries no model (the CP replaying an empty
	// instruction, a wizard page closed mid-flight) must not re-arm it.
	setupNamedModel string
	// setupFrameSeen records that at least one frame was folded. "No frame
	// yet" and "a frame that named nothing" are different answers: the
	// first is a host that may still be enrolling, the second is proof the
	// control plane has spoken.
	setupFrameSeen bool
	// setupDriving is whether a wizard was driving this host as of the last
	// frame. Not sticky, deliberately: the hold ends when the wizard goes
	// away, and both of the predicates behind it self-expire.
	setupDriving bool
	// prePullFrameGrace bounds the wait for the FIRST frame, after which an
	// unenrolled or offline host pre-pulls exactly as it always did.
	// prePullHoldMax is the ceiling on holding for a wizard that is driving
	// but never names a model. Fields rather than constants so tests do not
	// wait out real time; zero means the package default.
	prePullFrameGrace time.Duration
	prePullHoldMax    time.Duration

	// bundledRetirementLogged keeps the "your pin was retired" line to one
	// per process (#200). bundledModelID() is called on every pull,
	// activation and status read, so an unguarded log there is one line
	// per request for the lifetime of a host that never edits its config.
	bundledRetirementLogged sync.Once

	// benchMu guards lastBench. The boot benchmark runs on the probe
	// goroutine (main.go) and calls SetLastBench; Status() and
	// RunBenchmark read it back to derive the #133 lighter-model
	// recommendation. nil lastBench = no benchmark has completed yet.
	benchMu   sync.Mutex
	lastBench *BenchResult

	// benchJobMu guards the explicit-benchmark job state (waired#835
	// §12). The measurement runs as a single-flight goroutine detached
	// from any request context — a CLI timeout or dropped connection
	// must not abort a 5-minute measurement — and its completion is
	// persisted to catalog.State.LastBenchmark so the declarative
	// generation counter survives restarts. benchJobDone is non-nil
	// exactly while a run is in flight; concurrent RunBenchmark calls
	// join it instead of starting a second engine-saturating run.
	benchJobMu      sync.Mutex
	benchJobDone    chan struct{}
	benchJobOutcome *management.BenchmarkOutcome // last completed outcome (in-memory)
	benchJobResult  *catalog.BenchmarkRecord     // last completed record (mirrors the persisted one)
	// benchJobJoined, when non-nil (tests only), is called under
	// benchJobMu each time startBenchmarkJob JOINS a run instead of
	// starting one. The join leaves no observable state of its own, so
	// without this a test synchronising on it can only sleep — and the
	// join-semantics test was flaky on loaded runners for exactly that
	// reason: the in-flight job could finish before the joining call
	// arrived, and the "join" started a legitimate second run.
	benchJobJoined func()
	// benchJobOutcomeKind is the finishing BenchResult.Outcome of the run
	// benchJobOutcome came from. RunBenchmark reads it to tell the one
	// not-ready ending apart from a run that reached the engine and
	// failed: the first is the 425 "poll status and retry" door, the
	// second the 503 one (#576). Written on every completion, so a later
	// successful run clears it. Empty on a result built before Outcome
	// existed (a cache entry, a test literal), which is not the not-ready
	// value and so reads as it did before.
	benchJobOutcomeKind string
	// benchJobProgress is the live per-measurement report of the run in
	// flight (waired-agent#199), nil between runs. Guarded by benchJobMu
	// like the rest of the job state: it is written from the detached
	// measuring goroutine and read by every /benchmark/status poll.
	benchJobProgress *BenchProgress
	// benchRun overrides the measurement itself for tests (the real
	// path shells out to the engine over HTTP with multi-minute
	// budgets). nil = RunBootBenchmark. Same injection style as
	// ollamaUsable / BenchDeps.Now.
	benchRun func(ctx context.Context) BenchResult
	// lastDepthBench is the most recent #624 long-context sweep (nil =
	// none yet). Shares benchMu with lastBench.
	lastDepthBench *DepthBenchResult

	// ollamaUsable reports whether the ollama engine is actually usable
	// on this host: the waired-managed binary is resolvable (under the
	// state dir, or on PATH where the install still lives outside it).
	// Drives the no_engine derivation so a bundled binary outside PATH
	// isn't mistaken for "no engine" (#188).
	// nil is treated as "not usable".
	ollamaUsable func() bool

	// vllmUsable is the same question for vllm, and exists because for a
	// long time only ollama had one. The vllm arm of hasUsableEngine read
	// hardware.Profile.Engines.VLLM.Installed instead, which made it the
	// last engine-presence site not routed through engineInstalledOnHost
	// (#225, the residual of the #179 class that PR #205 unified).
	//
	// The profile is no longer a PATH probe — #238 injects
	// engineVersionOnHost into the daemon's profiler, so it does resolve
	// the venv — but it is TTL-cached, and engine_resolve.go says in as
	// many words why that is not good enough here: cached for 30 s, so it
	// is still LATE for a fresh install, which is exactly what the wizard
	// could not tolerate. A host whose venv appeared during setup could
	// report no_engine for half a minute after it was usable.
	//
	// nil is treated as "not usable", and hasUsableEngine then falls back
	// to the profile — the same shape ollamaUsable has, so a unit fixture
	// that constructs the provider directly keeps working.
	vllmUsable func() bool

	// engineChoice answers "which engine would this host choose right now",
	// by re-running the boot rule (chooseEngine) against the live state dir
	// and hardware profile. ok=false means it could not answer — a strict
	// mode refusal, an unreadable state file — and the caller then keeps the
	// engine this process already has.
	//
	// Injected for the same reason ollamaUsable is: the adopt path has to be
	// testable, and the real answer needs a CUDA host with a verified vLLM
	// venv on disk. nil (every unit fixture that does not set it) also reads
	// as "cannot answer", which pins those fixtures to their boot engine.
	engineChoice func(ctx context.Context) (string, bool)

	// engineVersionProbe MEASURES an installed engine's version by
	// executing it — the same engineVersionOnHost closure the profiler
	// is built with (#238), held here so the answer can be taken when it
	// is needed rather than only through the profiler's 30 s snapshot.
	//
	// That snapshot is what #361 was: on a fresh install it is taken
	// BEFORE the engine is installed, so for up to 30 s afterwards the
	// version reads unknown — and an unknown version excludes every
	// variant carrying a MinEngineVersion floor, which is how a host
	// that should have pulled the mtp tag pulled the plain one instead.
	// nil (every unit fixture) degrades to the pre-#361 answer.
	engineVersionProbe func(ctx context.Context, engine string) (bool, string)
	// engineVerMu guards the memo below. Leaf: never held across a
	// store.Update or an engine call — only across the probe exec.
	engineVerMu  sync.Mutex
	engineVerAt  time.Time
	engineVerVal string

	// Phase 7 routing signals threaded into the loopback Selector.
	// All optional; nil keeps the pre-Phase-7 mesh-fallback
	// deterministic-pick behaviour.
	sticky         *router.StickyStore
	localInFlight  *router.InFlightTracker
	stickyInFlight *router.StickyInFlight
	localRTT       func() map[string]uint32
	localErrors    func() map[string]float32

	// Phase 9: telemetry composite. Threaded into the loopback
	// Selector and gateway so the agent emits Phase 9 events from
	// the loopback side. nil disables emission entirely.
	recorder *observability.Recorder

	// Public Share consumer inputs (waired#827); see
	// inferenceSubsystemDeps for the contract.
	publicPolicy        func() router.PublicPolicy
	onPublicGrantDemand func()
	onPublicGrantUsed   func(grantID string)
	onPublicNudge       func(router.PublicNudge)

	// isInferenceDisabled, when non-nil and returning true, makes
	// Status() report SubsystemState="disabled" regardless of engine
	// health. inferenceState reports (current, desired) for the
	// management API's DesiredState field. Both wired from main.go
	// when an inferenceController is attached; nil in unit tests that
	// only exercise this provider directly.
	isInferenceDisabled func() bool
	// enableInference turns local inference on, persisting the choice.
	// Set by run() from the inference controller; nil on a daemon
	// started with --disable-inference, where there is nothing to turn
	// on and the operator's kill switch is not a control-plane
	// instruction's to override (#465).
	enableInference func() error
	// disableInference turns local inference off, persisting the choice —
	// the same act as `waired inference off`, reached from the #496 host
	// cutoff rather than from a person. Wired alongside enableInference
	// and nil in the same places, and for the same reason: a daemon
	// started with --disable-inference has nothing to turn off.
	disableInference func() error
	inferenceState   func() (current, desired state.InferenceState)

	// hostSpeedMeasureMu serialises the measurement itself (#496). Every
	// path that takes it runs in its own goroutine and the probe is a
	// minutes-long engine-bound job, so two running at once would measure
	// each other's contention. Held across the whole measurement on
	// purpose: the second caller wants the first one's answer, not one of
	// its own.
	//
	// SEPARATE from hostSpeedMu, and that separation is load-bearing.
	// Status() reads hostSpeed on every poll, and while one mutex did both
	// jobs a running measurement blocked /waired/v1/inference/status for
	// as long as the engine took to answer — ten minutes on a small host,
	// with the tray, the CLI and the wizard all reading nothing
	// (waired#1099). A lock a reader waits on must never be held across an
	// engine request.
	hostSpeedMeasureMu sync.Mutex
	// hostSpeedWindow overrides hostSpeedMeasureDeadline — the #579 bound on
	// one ensureHostSpeedMeasured call — in tests. Zero means the constant.
	// A field rather than a package var (the prePullHoldMax idiom) so the
	// timing tests that shrink it can run in parallel.
	hostSpeedWindow time.Duration
	// remeasure overrides the timing of the post-activation re-measurement
	// loop (waired-agent#821) in tests. Zero fields mean the constants.
	// A field for the same reason hostSpeedWindow is one, and here it is
	// load-bearing rather than tidy: remeasureWhenQuiet outlives the call
	// that started it, so package vars would be written by one test's
	// Cleanup while another test's goroutine still reads them.
	remeasure remeasureTiming
	// hostCutoffClient is the client the host-cutoff measurement posts
	// with. Nil in production — postOllamaGenerate then uses
	// http.DefaultClient, which is what it has always done. A fixture sets
	// it so its fake engine can tell this provider's requests from other
	// traffic arriving on the same loopback port, which a test cannot do
	// from the request alone (waired-agent#932).
	hostCutoffClient *http.Client
	// hostSpeedMu guards the fields below and is a LEAF: taken briefly,
	// never held across an engine request or a disk write of unbounded
	// size, and never while hostSpeedMeasureMu is being acquired.
	hostSpeedMu sync.Mutex
	// hostSpeed is this process's copy of the published measurement, nil
	// until one has been taken or loaded back from the state dir;
	// hostSpeedLoaded records that the load has been attempted, so a host
	// that has never measured does not re-read the absent file every
	// probe tick.
	hostSpeed       *signer.HostSpeed
	hostSpeedLoaded bool
	// hostSpeedAgentVersion is the agent build that took hostSpeed, from
	// the stored record. It is the half of "does this figure still apply"
	// that the wire form cannot answer — EngineKind/EngineVersion cover
	// the engine, and this covers the install (waired#1099).
	hostSpeedAgentVersion string
	// hostSpeedTakenHere records that THIS process ran the measurement,
	// rather than reading one off disk. It is what lets an install-flow
	// re-run ask for a fresh figure without measuring a fresh install's
	// host twice: on a fresh install the engine bootstrap measured seconds
	// before `waired init` asks (waired-agent#599, and the "no second
	// measurement in one install" half of
	// docs/decisions/20260807/1700-host-speed-is-an-install-time-step.md).
	hostSpeedTakenHere atomic.Bool
	// hostSpeedForce makes the next ensureHostSpeedMeasured ignore a stored
	// figure that would otherwise still apply. Set by Remeasure and consumed
	// once, so a request cannot latch the host into measuring every boot.
	hostSpeedForce atomic.Bool
	// hostSpeedStage / hostSpeedStageDetail are how far the measurement has
	// got, for the setup-progress reporter's two rows (waired#1143). Report
	// only — nothing reads them to decide anything.
	//
	// Guarded by hostSpeedMu alongside the figure rather than kept in an
	// atomic of their own, because the stage and the figure are read
	// TOGETHER: a process that has not measured but has one stored reports
	// "measured", which is what makes a daemon restart on an already-set-up
	// host report done rows straight away instead of pending ones for the
	// up-to-hostSpeedSettleWait it spends waiting for a quiet engine.
	hostSpeedStage       hostSpeedStage
	hostSpeedStageDetail string

	// engineExclusive is held by whichever measurement is monopolising
	// the engine right now. There are two on this host — the install-time
	// host-speed probe and the boot/setup benchmark — and each used to
	// ask "is the engine quiet" through a predicate that could not see
	// the other (waired-agent#703). ONE piece of state, so neither can go
	// blind to the other on its own.
	//
	// Claimed with a CAS and never waited on: the two callers already
	// have the right way to yield and they are not the same way. The
	// benchmark leaves through the 425 door it already answers on a busy
	// engine, and the measurement is inside a bounded poll loop
	// (awaitQuietEngine) that will come back round.
	//
	// Same idiom as warmInFlight, and for the same reason: an atomic.Bool
	// can be READ cheaply by the quiet predicates, which a sync.Mutex
	// cannot.
	engineExclusive atomic.Bool

	// servingInflight / servingAdmitted are inferenceSubsystemDeps'
	// fields of the same name. nil ⇒ nothing is serving.
	servingInflight func() int
	servingAdmitted func() uint64

	// meshSnapshotFn, when non-nil, threads the inferencemesh
	// aggregator into Select so a request whose model isn't local-
	// ready can fall through to a peer's engine (Phase 4 peer-engine
	// routing). nil keeps the Selector in Phase 1+2+3 mode (local-only).
	meshSnapshotFn func() inferencemesh.Snapshot

	// routing returns the operator's currently-live RoutingPreference
	// (Tailscale-exit-node-style manual peer selection). The closure
	// shape lets the Selector see a fresh snapshot per SelectK without
	// the provider holding a reference to the controller — keeps the
	// dependency direction one-way (main → provider, provider does not
	// import workerController). nil keeps Mode=auto.
	routing func() state.RoutingPreference

	// desiredParallel is the operator's max-concurrent-requests target
	// (#per-node-claude-serving), delivered by the CP via
	// nm.Self.InferenceState.Capacity and applied to the ollama engine's
	// OLLAMA_NUM_PARALLEL. 0 = automatic (VRAM-sized). Read lock-free by the
	// spawn-time tuning provider and written by ApplyConcurrency;
	// engineReconcileInFlight coalesces the guarded restart goroutine so
	// overlapping map updates don't stack engine restarts.
	desiredParallel atomic.Int64

	// engineReconcileInFlight coalesces the single background goroutine
	// (reconcileEngineServe) that owns every ollama serve-env change —
	// operator concurrency retunes (ApplyConcurrency) and in-process model
	// switches (#812) — so overlapping requests never stack two
	// Stop/EnsureRunning cycles on the one subprocess.
	engineReconcileInFlight atomic.Bool
	// engineOpMu serialises the two owners of an engine stop/start cycle:
	// reconcileEngineServe (serve-env changes) and startEngineAndBootstrap
	// (#304), whose backend probe and tuning verify both bounce the engine.
	// Before #304 the latter ran only in the first seconds of process life,
	// so the two could not realistically overlap; now a late engine install
	// can start it at any moment, including mid-swap. Not folded into
	// engineReconcileInFlight: that flag is a coalescer (drop if busy), and
	// dropping an adopt would silently lose the fix.
	engineOpMu sync.Mutex
	// engineStartInFlight coalesces the goroutine that starts the engine and
	// runs the boot bootstrap (#304), the way engineReconcileInFlight
	// coalesces the serve-env reconcile.
	engineStartInFlight atomic.Bool
	// engineBootstrapOnce latches once the post-start bootstrap (bundled /
	// preferred model, backend probe, tuning verify) has run. The engine
	// START stays re-entrant — that is what adopts a late install — but the
	// tail must not repeat: its backend probe and tuning verify both stop
	// and restart the engine, which would fail any download in flight
	// against it. Once per process is exactly the pre-#304 behaviour.
	//
	// There is no vLLM equivalent, and deliberately so. `vllmBootstrapOnce`
	// held the boot goroutine to one bootstrapVLLM attempt because the
	// function was not idempotent — each call registered a fresh adapter
	// over the previous one and spawned a second process on the same port.
	// #337/#510 moved that decision into the function itself
	// (decideVLLMBootstrap), and #339 needs the retry the latch forbade: a
	// venv that appears after boot, and a bootstrap whose weight download
	// or EnsureRunning failed, both have to be reachable again. Re-entry is
	// owned by engineStartInFlight (coalescing) and decideVLLMBootstrap
	// (idempotency) instead.
	engineBootstrapOnce atomic.Bool
	// bootPlan holds the boot-computed engine-start inputs the provider
	// cannot re-derive. See engineBootstrapPlan.
	bootPlan engineBootstrapPlan
	// swapPending signals reconcileEngineServe that an in-process model
	// switch is waiting: it forces a bounce (Option 2 — always stop-then-
	// start on a switch) and resets KV to the q8_0 default for the new
	// model (the old model's verify-degraded f16 does not carry over).
	swapPending atomic.Bool
	// engineRecoverPending marks the next reconcile as a crash-recovery
	// bounce (waired-agent#29). Reusing reconcileEngineServe rather than
	// adding a parallel restart path buys mutual exclusion with model swaps
	// and concurrency retunes for free — that is what engineReconcileInFlight
	// already exists for.
	engineRecoverPending atomic.Bool
	// engineRespawnPending marks the next reconcile as a bounce for a
	// spawn-env input that ServeInputsEqual does not compare, because it
	// is not part of the serve TUNING: model residency
	// (OLLAMA_KEEP_ALIVE, waired-agent#908). Without it a residency
	// change reaches the running process by no route at all — the engine
	// reads that variable once at spawn, and the serving path cannot
	// carry a per-request keep_alive because waired serves over ollama's
	// OpenAI-compatible endpoint, which discards the field.
	engineRespawnPending atomic.Bool
	// askReconcileFn is the seam the respawn path asks through. nil means
	// requestEngineReconcile. It exists because the behaviour under test
	// is "the request is not lost when the coalescer drops it", and
	// letting the real reconcile run to observe that needs a whole
	// provider — store, manifests, profiler — behind a fact about two
	// atomics. Same reason residencyController takes applierFn.
	askReconcileFn func(swap bool)
	// crashMu guards the crash bookkeeping below.
	crashMu sync.Mutex
	// crashStrikes counts engine deaths inside engineRecoveryStableFor. A
	// start that never came up counts too (#310): both mean "the engine is
	// not staying up", and one budget is what lets a host that alternates
	// between the two still reach a verdict.
	crashStrikes int
	// lastEngineCrash is when the last death was observed; a run that stays
	// up for engineRecoveryStableFor resets the strike count, so one crash a
	// day never accumulates into a give-up.
	lastEngineCrash time.Time
	// now is time.Now, injectable so the stability window is testable.
	now func() time.Time
	// pendingSwapModel holds the model_id of an operator switch whose weights
	// are still downloading; runPullJob's completion kicks the reconcile once
	// that model reaches Ready. It distinguishes an operator switch from a
	// boot-time pull so boot never triggers a spurious engine bounce.
	pendingSwapModel atomic.Pointer[string]
	// preferredOverride is the in-process source of truth for the operator's
	// preferred model after a #812 switch. cfg.PreferredModelID is a frozen
	// boot snapshot (preferred-model.json is only re-read on a restart), so
	// every in-process reader of the preference routes through
	// effectivePreferredModelID() / effectiveCfg() instead.
	preferredOverride atomic.Pointer[string]
	// restartOnWedge, when non-nil, is the supervised-restart fallback the
	// reconcile invokes if the engine fails to come back after a switch
	// bounce (a wedged engine). Wired from main.go to the same scheduler the
	// management RestartScheduler uses. nil in unit tests.
	restartOnWedge func()

	// noModelSelected is the operator's standing "run without a local
	// model" choice (waired-agent#586): true when preferred-model.json
	// records None, or the management API just applied it in process. The
	// bundled fallback pre-pull stands down while it holds; any model
	// choice clears it.
	noModelSelected atomic.Bool
	// modelQuestionUnanswered is the persisted "asked, and nobody
	// answered" record (Preference.Unanswered, #586): the model question
	// expired on some boot, so the fallback download stays down — this
	// boot and every later one — until an actual answer clears it. Set
	// from the preference at boot and by the expiry arms of the two
	// holds; cleared wherever an answer lands.
	modelQuestionUnanswered atomic.Bool
	// modelChoiceMu guards the terminal install flow's claim that a model
	// question is about to be asked (waired-agent#586). While
	// modelChoicePendingUntil is in the future, the bundled fallback
	// download waits — the terminal twin of the browser wizard's
	// setupDriving hold, bounded server-side so a killed `waired init`
	// cannot park it past the deadline. modelChoiceCh is close-and-replace
	// (the setupFrameCh pattern) so a parked waiter wakes on every change.
	modelChoiceMu           sync.Mutex
	modelChoicePendingUntil time.Time
	modelChoiceCh           chan struct{}
	// modelChoiceWaitMax overrides the claim's server-side ceiling in
	// tests; zero means the package default (modelChoiceWaitMax).
	modelChoiceWait time.Duration
}

// setVLLM records the running vLLM adapter. Called from bootstrapVLLM on the
// engine-startup goroutine; every reader is on another one (#337).
func (p *agentInferenceProvider) setVLLM(a infruntime.Adapter) {
	p.vllm.Store(&a)
}

// vllmAdapter returns the running vLLM adapter, or nil when none has been
// started. Callers type-assert it for the optional interfaces (AppliedTuning);
// an assertion on a nil interface yields ok == false, which is the same answer
// the plain field gave before #337.
func (p *agentInferenceProvider) vllmAdapter() infruntime.Adapter {
	if a := p.vllm.Load(); a != nil {
		return *a
	}
	return nil
}

// effectivePreferredModelID returns the operator's currently-effective
// preferred model_id: the in-process #812 switch override when one has been
// set, else the boot-time cfg snapshot. The snapshot alone is stale after an
// in-process switch (cfg is a frozen value copy; preferred-model.json is only
// re-read on a restart), so every in-process reader of the preference must
// route through here (or effectiveCfg).
func (p *agentInferenceProvider) effectivePreferredModelID() string {
	if v := p.preferredOverride.Load(); v != nil {
		return *v
	}
	return p.cfg.PreferredModelID
}

// effectiveCfg returns a copy of the boot inference config with
// PreferredModelID overwritten by the in-process #812 switch override, so the
// free helpers that size/route off cfg (resolveTuningTarget,
// modelDecisionReasons, defaultCodingModelID, computeAvailableUpdate) observe
// the operator's current choice after an in-process switch rather than the
// frozen boot value.
func (p *agentInferenceProvider) effectiveCfg() agentconfig.InferenceConfig {
	c := p.cfg
	c.PreferredModelID = p.effectivePreferredModelID()
	return c
}

// ApplyConcurrency applies the operator's max-concurrent-requests target
// (delivered by the CP via nm.Self.InferenceState.Capacity) to the ollama
// engine's OLLAMA_NUM_PARALLEL. It records the target and, when the resolved
// engine parallelism actually changes and this agent owns a restartable bundled
// ollama, re-tunes and restarts the engine ONCE in a coalesced background
// goroutine (never blocking the network-map loop) so the change takes effect
// promptly. target <= 0 clears the override (automatic VRAM sizing).
//
// No-op when the target is unchanged or when serving vLLM. If a target arrives before
// the engine is up the restart is skipped; it applies on the next CP change or
// agent restart. The admission cap (Server.SetCapacity) is applied separately by
// the caller and is non-disruptive; this method drives only engine parallelism.
// A target that arrives while a model is downloading is recorded and applied
// when the download finishes, rather than bounced into (#359).
func (p *agentInferenceProvider) ApplyConcurrency(ctx context.Context, target int) {
	if p == nil {
		return
	}
	if target < 0 {
		target = 0
	}
	if int(p.desiredParallel.Swap(int64(target))) == target {
		return // unchanged since the last frame
	}
	if p.ollama == nil || p.servingEngine() != catalog.RuntimeOllama {
		return // recorded; nothing we own to restart (vLLM / no engine)
	}
	// A capacity frame arriving while a model downloads is routine — the
	// control plane sends one within seconds of the agent joining the map,
	// which is exactly when a fresh install is fetching its first multi-GB
	// model — and the bounce this would fire kills that download, because
	// `ollama pull` is a client of `ollama serve` (#359).
	//
	// Nothing is lost by waiting. desiredParallel already holds the target
	// and the deferred reconcile re-reads it, so what eventually applies is
	// the LATEST value, not this frame's. The admission cap the same frame
	// carries is applied separately by the caller (Server.SetCapacity) and
	// is non-disruptive, so the capacity decision itself is not deferred —
	// only the engine's OLLAMA_NUM_PARALLEL retune is.
	if p.deferRetuneWhilePulling() {
		return
	}
	p.requestEngineReconcile(false)
}

// deferRetuneWhilePulling records the re-resolve instead of firing a
// reconcile when a download is in flight, and reports whether it did.
// endPull fires what it recorded once the last pull leaves.
//
// The emptiness test and the flag write share pullMu with endPull's
// removal, which is what makes the intent neither lost nor doubled — the
// same critical-section argument endPull documents for its own side. A
// check outside the lock would race the last endPull: it could observe a
// non-empty registry, be overtaken by the removal that consumes both
// flags, and then set a flag nothing is left to fire.
//
// Only the capacity retune calls it, deliberately, and there is no swap
// equivalent. The two other bounces this function looks like it could
// serve must NOT be held: crash recovery, because the engine is already
// dead and the download is dying either way, so deferring the restart is
// strictly worse; and an operator's model switch, because a person is
// waiting on it and the download they are waiting behind is one they did
// not ask about. Both survive the bounce through the engine-generation
// grace in runPullJob instead.
func (p *agentInferenceProvider) deferRetuneWhilePulling() bool {
	p.pullMu.Lock()
	defer p.pullMu.Unlock()
	if len(p.pullsInFlight) == 0 {
		return false
	}
	p.retuneDeferred.Store(true)
	return true
}

// requestEngineReconcile kicks the single background goroutine that owns every
// ollama serve-env change. swap=true marks an in-process model switch (#812) so
// the reconcile flips Active and force-bounces (Option 2 — always stop-then-
// start on a switch). Coalesced via engineReconcileInFlight: if a reconcile is
// already running it re-reads swapPending/desiredParallel on its next
// iteration, so overlapping concurrency changes and switches never stack two
// Stop/EnsureRunning cycles on the one subprocess. The bounce always runs on
// the daemon's long-lived agentCtx, never the caller's request/pull ctx.
// respawnChaseAttempts / respawnChaseInterval bound the re-trigger below.
// Vars so a test can drive it without sleeping for real.
var (
	respawnChaseAttempts = 20
	respawnChaseInterval = 250 * time.Millisecond
)

// requestEngineRespawn asks for a bounce that the tuning comparison
// would otherwise skip, for a spawn-env input the tuning does not carry
// (waired-agent#908). Routed through the same reconcile as everything
// else that touches the serve env so it inherits engineOpMu, the parked
// / not-ready staging, the post-spawn finalize and the warm-up — a
// hand-rolled Stop+EnsureRunning here would have none of them.
//
// The re-trigger exists because requestEngineReconcile DROPS a request
// when a reconcile is already running, on the premise that the running
// one re-reads the intent on its next iteration. reconcileEngineServe
// has more than a dozen return paths, so it may instead finish this
// iteration and exit — leaving engineRespawnPending set and nobody left
// to act on it. The flag then sits there until some unrelated pull or
// retune happens to run, and in the meantime the setting silently does
// not govern the next load while the operator has been told the engine
// restarted (waired-agent#916 follow-up). Coalescing is fine for
// swapPending, whose caller keeps producing work; a residency change can
// be the only thing happening on the host.
func (p *agentInferenceProvider) requestEngineRespawn() {
	if p == nil {
		return
	}
	p.engineRespawnPending.Store(true)
	p.askReconcile(false)
	if !p.engineRespawnPending.Load() {
		return // already consumed by a reconcile
	}
	// Read the bounds HERE, not in the goroutine: they are vars so a test
	// can shorten them, and a goroutine reading them races that test's
	// own write.
	go p.chaseEngineRespawn(respawnChaseAttempts, respawnChaseInterval)
}

func (p *agentInferenceProvider) askReconcile(swap bool) {
	if p.askReconcileFn != nil {
		p.askReconcileFn(swap)
		return
	}
	p.requestEngineReconcile(swap)
}

// chaseEngineRespawn re-asks until the pending flag is consumed. Bounded
// rather than indefinite: if the engine cannot be reconciled at all
// (parked, not ready, no ollama), reconcileEngineServe returns without
// consuming the flag every time, and an unbounded chase would spin for
// the life of the daemon. Giving up leaves the value staged for the next
// spawn, which is what the parked and not-ready branches report anyway.
func (p *agentInferenceProvider) chaseEngineRespawn(attempts int, interval time.Duration) {
	for i := 0; i < attempts; i++ {
		time.Sleep(interval)
		if !p.engineRespawnPending.Load() {
			return
		}
		p.askReconcile(false)
	}
	if p.logger != nil {
		p.logger.Warn("residency respawn was never picked up by a reconcile; the value applies at the next engine start",
			"attempts", attempts)
	}
}

func (p *agentInferenceProvider) requestEngineReconcile(swap bool) {
	if swap {
		p.swapPending.Store(true)
	}
	if !p.engineReconcileInFlight.CompareAndSwap(false, true) {
		return // a reconcile is already running; it will observe the new intent
	}
	ctx := p.agentCtx
	if ctx == nil {
		ctx = context.Background()
	}
	go p.reconcileEngineServe(ctx)
}

// engineRecoveryMaxAttempts / engineRecoveryStableFor bound automatic crash
// recovery. Three attempts at 0s / 15s / 60s cover a transient runner fault
// (the observed segfault class), while a deterministically-crashing model — a
// corrupt GGUF, a sizing that always OOMs — exhausts the budget in ~75s and
// then reports the truth instead of respawning forever.
//
// The first attempt has NO delay on purpose: the common case is a one-off
// fault with a human sitting at a Claude Code prompt. (The boot retry at
// startLocalInference uses 10s/20s because nobody is waiting there.)
const (
	engineRecoveryMaxAttempts = 3
	engineRecoveryStableFor   = 5 * time.Minute
)

// engineRecoveryBackoff is the delay before recovery attempt n (1-based).
func engineRecoveryBackoff(attempt int) time.Duration {
	switch attempt {
	case 1:
		return 0
	case 2:
		return 15 * time.Second
	default:
		return 60 * time.Second
	}
}

// onEngineUnhealthy is the OllamaConfig.OnUnhealthy handler: the adapter has
// just found its engine dead and moved to StateFailed, and this decides what
// to do about it.
//
// Before waired-agent#29 nothing did. ollama's parent process keeps answering
// /api/tags with 200 after its llama-server child segfaults, so the adapter
// stayed latched Ready, every request 500'd forever, and `waired status` kept
// saying the engine was fine.
func (p *agentInferenceProvider) onEngineUnhealthy(detail string) {
	if !p.engineIsWairedsToGiveUpOn() {
		return
	}

	n := p.recordEngineStrike()

	if n > engineRecoveryMaxAttempts {
		p.logger.Error("ollama engine crashed repeatedly; automatic restart disabled",
			"crashes", n, "window", engineRecoveryStableFor)
		p.ollama.LatchFailed(fmt.Sprintf(
			"engine crashed %d times within %s; automatic restart disabled — see the engine log, "+
				"then `waired inference engine start` (or switch model) to retry\n%s",
			n, engineRecoveryStableFor, detail))
		return
	}

	delay := engineRecoveryBackoff(n)
	p.logger.Warn("ollama engine died; scheduling restart",
		"crash", n, "max", engineRecoveryMaxAttempts, "in", delay)
	ctx := p.agentCtx
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		if delay > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
		}
		p.engineRecoverPending.Store(true)
		p.requestEngineReconcile(false)
	}()
}

// onEngineFitFailure is the OllamaConfig.OnFitFailure handler: the engine
// has just told a request the accelerator was out of memory.
//
// Before waired-agent#1038 nothing did. The body carried no dead-runner
// marker, so it reached only the canary log — while the runner died
// anyway, the model was evicted, and the next request paid a ~8 s cold
// reload and failed the same way, forever. The post-load verify pass
// could not help either: it runs once, right after the load, before any
// prompt has allocated a working compute buffer.
//
// It steps the same ladder the verify pass steps, and it never demotes
// the engine: the engine is serving; what does not fit is the
// configuration it was given.
func (p *agentInferenceProvider) onEngineFitFailure(detail string) {
	applied := p.ollama.AppliedTuning()
	if applied.ModelID == "" {
		return
	}
	if applied.NumBatch < ollamaLargeBatch {
		// Already at the bottom of the ladder this handler can reach: the
		// batch is the engine's own. Record the fact and stop — bouncing
		// the engine into the same configuration is what this fix exists
		// to prevent.
		applied.Degraded = true
		applied.WindowFits = false
		applied.Warning = joinTuningWarn(applied.Warning,
			"this computer's GPU ran out of memory serving a request at this model and window ("+detail+")")
		p.ollama.SetAppliedTuning(applied)
		return
	}

	m, ok := catalog.LookupByAlias(applied.ModelID, p.manifests)
	if !ok {
		return
	}
	base, err := p.dropForcedOllamaBatch(m)
	if err != nil {
		p.logger.Warn("dropping the forced prefill batch after an out-of-memory failed", "err", err)
		return
	}
	p.logger.Warn("ollama: dropping the forced prefill batch after an out-of-memory; the next request loads the engine's own batch sizing",
		"model", applied.ModelID, "tag", base, "detail", detail)
	applied.NumBatch = 0
	applied.Warning = joinTuningWarn(applied.Warning,
		"this computer's GPU could not hold the larger prefill batch; using the engine's own batch sizing instead")
	p.ollama.SetAppliedTuning(applied)
}

// engineIsWairedsToGiveUpOn reports whether this host's engine is one waired
// owns — and may therefore restart, and eventually stop restarting.
//
// Nothing waired owns to restart. The StateFailed the adapter recorded IS the
// whole answer: an adopted orphan has no handle to signal. A parked one was
// stopped on purpose.
func (p *agentInferenceProvider) engineIsWairedsToGiveUpOn() bool {
	return p.ollama != nil &&
		p.ollama.Mode() != infruntime.EngineModeAdopted && !p.ollama.IsParked()
}

// recordEngineStrike charges one failure to the recovery budget and returns
// the running count.
//
// ONE budget for both failure shapes (#310). A host whose engine dies during
// startup, gets restarted, dies again mid-serve, and so on would otherwise
// keep two counters that each stay under the limit forever — while the engine
// is plainly not staying up.
func (p *agentInferenceProvider) recordEngineStrike() int {
	nowFn := p.now
	if nowFn == nil {
		nowFn = time.Now
	}
	p.crashMu.Lock()
	defer p.crashMu.Unlock()
	if !p.lastEngineCrash.IsZero() && nowFn().Sub(p.lastEngineCrash) > engineRecoveryStableFor {
		p.crashStrikes = 0
	}
	p.crashStrikes++
	p.lastEngineCrash = nowFn()
	return p.crashStrikes
}

// onEngineStartFailed is the OllamaConfig.OnStartFailed handler: a start
// attempt ended without the engine serving.
//
// This is the hole #310 fell through. The engine that will not launch — a
// macOS bundle whose signature no longer verifies, so every exec of it is
// killed — never reaches StateReady, so markUnhealthy returns early, no
// strike is charged, FailureLatched() stays false, and the wizard's engine
// row keeps reporting Done over a device with no local AI. Meanwhile every
// gateway request tries to spawn it again.
//
// Deliberately does NOT schedule a restart, unlike its sibling above. The
// caller has already retried on its own budget (startEngineAndBootstrap makes
// engineEnsureAttempts tries with backoff, and the gateway re-enters on the
// next request), so throwing another restart at it from here is how the macOS
// respawn storm gets built — the thing engine_bootstrap.go refuses to do by
// clearing the latch. All this owes the system is a verdict.
func (p *agentInferenceProvider) onEngineStartFailed(detail string) {
	if !p.engineIsWairedsToGiveUpOn() {
		return
	}

	n := p.recordEngineStrike()
	if n <= engineRecoveryMaxAttempts {
		if p.logger != nil {
			p.logger.Warn("ollama engine did not start; leaving the retry to the caller",
				"attempt", n, "max", engineRecoveryMaxAttempts)
		}
		return
	}

	if p.logger != nil {
		p.logger.Error("ollama engine repeatedly failed to start; automatic restart disabled",
			"attempts", n, "window", engineRecoveryStableFor)
	}
	p.ollama.LatchFailed(fmt.Sprintf(
		"engine failed to start %d times within %s; automatic restart disabled — see the engine log, "+
			"then `waired inference engine start` (or switch model) to retry\n%s",
		n, engineRecoveryStableFor, detail))
}

// reconcileEngineServe recomputes the ollama serve env for the currently
// effective preferred/Active model and desiredParallel, and bounces the engine
// (Stop → EnsureRunning) to apply it — the agent process, gateway, mesh, and
// management API all stay up; only `ollama serve` restarts. It loops so a
// target that moved mid-bounce is not dropped.
//
// A concurrency-only change bounces iff the resolved parallelism (or sized
// model) moved and preserves the applied KV type (a prior #621 verify f16
// degrade is kept). An in-process model switch (swapPending, #812) always
// bounces, commits the new model as Active once it is Ready, and resets KV to
// the q8_0 default (the old model's degrade does not carry over). On a
// parked/down engine the serve env is staged so the eventual (re)start serves
// the new tuning without forcing a spawn. If the engine fails to come back after a
// switch bounce (wedged), it self-heals via the supervised restart fallback —
// the only restart #812 keeps.
// Its stop/start span shares engineOpMu with startEngineAndBootstrap (#304),
// the other owner of an engine restart.
func (p *agentInferenceProvider) reconcileEngineServe(ctx context.Context) {
	defer p.engineReconcileInFlight.Store(false)
	// Warm on every exit, not just the bouncing one (#320). A reconcile
	// that decides nothing moved is the common steady-state case, and the
	// engine may still hold no weights at all — that is exactly the state
	// this exists to fix. warmServingModel decides for itself whether the
	// moment is right (parked, mid-pull, already resident), so
	// asking on every path costs an /api/ps probe.
	defer p.warmServingModel()
	if p.ollama == nil {
		return
	}
	for {
		want := int(p.desiredParallel.Load())
		swap := p.swapPending.Swap(false)
		// recover: a crash-recovery bounce. Unlike a concurrency retune it must
		// run even when the resolved tuning is unchanged, and even though the
		// engine is NOT StateReady — that is the whole point (waired-agent#29).
		recover := p.engineRecoverPending.Swap(false)
		// respawn: a spawn-env input outside the tuning moved (residency,
		// #908). Like recover it must bounce even when the tuning compares
		// equal, but unlike recover it is not a fault: it does not clear
		// the failure latch and it only ever runs with nothing resident,
		// so it costs no reload.
		respawn := p.engineRespawnPending.Swap(false)
		st, err := p.store.Load()
		if err != nil {
			return
		}
		// On an operator switch, commit the new preferred model as Active
		// (once its weights are Ready) before sizing/bouncing, so routing and
		// /inference/status reflect the target immediately.
		if swap {
			if tm, ok := p.preferredManifest(); ok {
				vid := ""
				if ms, found := st.Models[tm.ModelID]; found {
					vid = ms.VariantID
				}
				p.activatePreferredIfNeeded(tm.ModelID, vid)
				st, _ = p.store.Load() // re-read Active after the flip
			}
		}
		cur := p.ollama.AppliedTuning()
		tm, tv, ok := resolveTuningTarget(p.effectiveCfg(), p.manifests, st)
		if !ok {
			return
		}
		// KV cache type: an operator switch re-decides from scratch (the old
		// model's post-verify f16 degrade does not carry over, and the new
		// model may not want the same cache); a concurrency-only re-tune
		// preserves the applied KV so a prior degrade is kept (#621). An
		// explicit type is a pin, so preserving it still works now that the
		// default is decided per host (waired-agent#29).
		kvType := ollamaKVRequest()
		if !swap && cur.KVCacheType != "" {
			kvType = cur.KVCacheType
		}
		// The engine's last answer about request parallelism feeds back in:
		// cur carries the runner's own -np (#763), and a slot it declined
		// for this model at this window must not be requested again
		// (waired-ai/waired-agent#846). grantedFor drops it when the target
		// or the window moved, so an operator switch starts from the
		// arithmetic again.
		tune := computeOllamaTuningOpts(tm, tv, p.profiler.Profile(ctx), kvType, 0, want,
			ollamaObservedServe{
				ModelID:       cur.ModelID,
				VariantID:     cur.VariantID,
				ContextLength: cur.ContextLength,
				NumParallel:   cur.ObservedNumParallel,
			})
		// The third caller of the decision reasons, and until now the one
		// that had none: a model switched in process (#812) served with no
		// decision warning at all — including the below-context-floor one —
		// until the next restart recomputed it. effectiveCfg rather than
		// p.cfg because the boot snapshot is stale after a switch, which is
		// exactly the case this path handles.
		tune = applyModelDecisionReasons(p.effectiveCfg(), tm, tune, p.logger)
		// Bounce predicate: an operator switch always bounces (Option 2) so the
		// new model's per-model spawn env applies; otherwise bounce iff any
		// input to the engine's spawn env actually moved.
		//
		// This used to compare NumParallel alone, inherited from the pre-#812
		// retuneParallelLoop, whose only caller was a concurrency change. Once
		// a finished pull could also land here that was too narrow: the first
		// pull of a fresh install resolves a DIFFERENT MODEL at the same
		// parallelism, so the predicate returned before SetModelEnv and the
		// engine kept serving the pre-download guess for the life of the
		// process (waired-agent#320). ServeInputsEqual compares the spawn
		// inputs and ignores the verification outcome — see its doc for why
		// a whole-struct comparison would instead bounce on every call.
		if !swap && !recover && !respawn && tune.ServeInputsEqual(cur) {
			return // the running engine already serves this exact tuning
		}
		// Only a live, un-parked ollama process can bounce. Parked or
		// not-yet-ready: stage the serve env so the eventual (re)start serves
		// the new tuning, but don't force a spawn here.
		if p.servingEngine() != catalog.RuntimeOllama {
			return
		}
		p.ollama.SetModelEnv(tune.Env())
		p.ollama.SetAppliedTuning(tune.ModelTuning)
		if p.ollama.IsParked() {
			return // staged; Unpark / StartEngine brings it up tuned
		}
		if !recover && !respawn && p.ollama.Health(ctx).State != infruntime.StateReady {
			return // staged; StartEngine / normal boot brings it up tuned
		}
		p.logger.Info("reconciling ollama serve env",
			"model", tune.ModelID, "variant", tune.VariantID, "switch", swap,
			"ctx", tune.ContextLength, "kv", tune.KVCacheType,
			"num_parallel", tune.NumParallel, "warning", tune.Warning)
		// The bounce runs under engineOpMu so it cannot interleave with
		// startEngineAndBootstrap's own restarts (#304). Taken inside the
		// loop, not around it, so a long engine adopt does not pin this
		// loop's re-read of desiredParallel / swapPending below.
		if stop := func() bool {
			p.engineOpMu.Lock()
			defer p.engineOpMu.Unlock()
			if err := p.ollama.Stop(ctx); err != nil && !recover {
				p.logger.Warn("stop for engine reconcile failed; keeping current engine", "err", err)
				return true
			} else if err != nil {
				// On the recovery path a failed Stop is expected — the child is
				// usually already gone. Keep going: EnsureRunning reaps it.
				p.logger.Debug("stop during engine recovery reported an error (child likely already exited)", "err", err)
			}
			if err := p.ollama.EnsureRunning(ctx); err != nil {
				p.logger.Warn("restart for engine reconcile failed; engine down until retry", "err", err)
				if swap && p.restartOnWedge != nil {
					// Wedged after an in-process model switch: fall back to the
					// supervised restart (preferred-model.json is saved, so the
					// reboot serves the new model). The only restart #812 keeps.
					p.logger.Warn("engine wedged after in-process model switch; falling back to supervised restart")
					p.restartOnWedge()
				}
				return true
			}
			// Re-establish the post-spawn GPU-fit safety net for the (possibly new)
			// model: create the #642 derived batch model if needed and verify the
			// exported tuning, degrading KV once on spill evidence — the same
			// finalize step a boot spawn runs.
			tag := ""
			if ms, found := st.Models[tm.ModelID]; found {
				tag = ms.OllamaTag
			}
			if tag == "" && tv.Source.Type == catalog.SourceOllama {
				tag = tv.Source.Tag
			}
			p.finalizeOllamaServeTuning(ctx, tune, tm, tv, tag)
			return false
		}(); stop {
			return
		}
		if int(p.desiredParallel.Load()) == want && !p.swapPending.Load() {
			return // no newer target/switch arrived during the bounce
		}
	}
}

// finalizeOllamaServeTuning runs the post-spawn tuning steps that need a live
// engine, shared by the boot startup goroutine and the in-process engine
// reconcile (#812) so a model switched without a restart gets the same GPU-fit
// safety net a restart gives: create the #642 derived batch model when the
// tuning forces a large generation ubatch, then verify the exported tuning
// against the running engine and degrade KV once on spill/f16 evidence. tag is
// the model's serving tag (state OllamaTag, else the variant's source tag).
func (p *agentInferenceProvider) finalizeOllamaServeTuning(ctx context.Context, tune ollamaTuning, m catalog.Manifest, v catalog.Variant, tag string) {
	verifyTag := tag
	derivedInUse := false
	if tune.NumBatch >= ollamaLargeBatch && v.Source.Type == catalog.SourceOllama {
		baseTag := v.Source.Tag
		derived, derr := ensureOllamaDerivedModel(ctx, &http.Client{}, p.ollama.BaseURL(), baseTag, tune.NumBatch)
		switch {
		case derr != nil:
			p.logger.Warn("ollama derived batch model unavailable; serving base tag with automatic batch",
				"base", baseTag, "num_batch", tune.NumBatch, "err", derr)
		default:
			// The gateway routes on ModelState.OllamaTag, so a derived
			// model that could not be recorded is a derived model
			// nothing will ever load. Both the "Update failed" and the
			// "no row to update" cases leave the base tag serving, so
			// both fall through to the same report.
			persisted := false
			uerr := p.store.Update(func(s *catalog.State) {
				ms, ok := s.Models[m.ModelID]
				if !ok {
					return
				}
				ms.OllamaTag = derived
				ms.BaseOllamaTag = baseTag
				s.Models[m.ModelID] = ms
				persisted = true
			})
			if uerr != nil || !persisted {
				p.logger.Warn("persist derived ollama tag failed; serving base tag with automatic batch",
					"base", baseTag, "derived", derived, "num_batch", tune.NumBatch, "err", uerr)
				break
			}
			verifyTag = derived
			derivedInUse = true
			p.logger.Info("ollama derived batch model ready",
				"tag", derived, "from", baseTag, "num_batch", tune.NumBatch)
		}
	}
	tune = servedOllamaTuning(tune, derivedInUse)
	applyOllamaTuningVerification(ctx, p.ollama, tune,
		m, v, p.profiler.Profile(ctx), verifyTag, p.ollama.BaseURL(),
		&http.Client{}, p.ollamaVerifyDeps(m), p.logger)
}

// servedOllamaTuning returns the tuning describing what the engine will
// ACTUALLY serve, given what the sizing asked for and whether the #642
// derived batch model is in use (waired-agent#1064).
//
// The forced generation ubatch is the one field of the tuning that does
// not reach the engine through an OLLAMA_* env: it rides a locally
// derived model (inference_ollama_derived.go), so when that model could
// not be created the engine serves the base tag with its own batch
// sizing while the recorded tuning went on carrying the value nobody
// applied. Everything downstream reads the recorded one — the inference
// status, `waired status`, `models ls --detail`, and #1038's post-load
// ladder, whose first rung would otherwise spend an allocation probe
// dropping a batch that was never there and persist a refusal for it.
//
// Deliberately not the observed value read back off the runner's
// command line, which is the shape #763 used for parallelism: this is
// the fact the caller already holds, at the moment it holds it.
func servedOllamaTuning(t ollamaTuning, derivedInUse bool) ollamaTuning {
	if t.NumBatch >= ollamaLargeBatch && !derivedInUse {
		t.NumBatch = 0
	}
	return t
}

// ollamaVerifyDeps wires the post-load evidence and repair seams
// (waired-agent#1038). Separated from finalizeOllamaServeTuning so the
// verification can be driven with fakes.
func (p *agentInferenceProvider) ollamaVerifyDeps(m catalog.Manifest) ollamaVerifyDeps {
	return ollamaVerifyDeps{
		FreeVRAMMB: hardware.TightestGPUFreeMB,
		Allocate: func(ctx context.Context, tag string, promptTokens int) error {
			return probeOllamaAllocation(ctx, &http.Client{}, p.ollama.BaseURL(), tag, promptTokens)
		},
		ApplyStep: func(context.Context, ollamaTuning) (string, error) {
			return p.dropForcedOllamaBatch(m)
		},
		ListProcs: proclist.List,
	}
}

// dropForcedOllamaBatch reverts the serving tag to the pulled base tag
// and records that this host refused the #642 forced generation ubatch
// for this variant (waired-agent#1038).
//
// The refusal is PERSISTED rather than held in memory because that is
// what stops the loop: an out-of-memory kills the runner and evicts the
// model, so without it every following request — and every later boot —
// pays a cold reload into the configuration that just failed.
//
// No engine restart: num_batch is not an OLLAMA_* env, it rides the
// derived model (inference_ollama_derived.go), so the next load of the
// base tag is already the stepped-down configuration.
func (p *agentInferenceProvider) dropForcedOllamaBatch(m catalog.Manifest) (string, error) {
	base := ""
	if err := p.store.Update(func(s *catalog.State) {
		ms, ok := s.Models[m.ModelID]
		if !ok {
			return
		}
		base = ms.BaseOllamaTag
		if base != "" {
			ms.OllamaTag = base
			ms.BaseOllamaTag = ""
		} else {
			base = ms.OllamaTag
		}
		ms.ForcedBatchRefusedAt = time.Now().UTC()
		s.Models[m.ModelID] = ms
	}); err != nil {
		return "", err
	}
	if base == "" {
		return "", fmt.Errorf("no serving tag recorded for %s", m.ModelID)
	}
	return base, nil
}

// modelsSnapshot projects the stored model lifecycle onto the management
// snapshot the CLI and tray read. Pure, and separated from Status so it
// can be tested without an engine, a profiler or a registry: everything
// interesting here is a mapping decision, and everything around it in
// Status is plumbing that would have to be faked to reach it.
//
// progress is the in-flight byte aggregator, taken as a function for the
// same reason.
//
// manifests is the catalog this daemon serves from, and it is here
// because `models` cannot answer for a model nothing has started on:
// it is the state CACHE, so an untouched model has no entry to sort at
// all (waired-agent#403). Enumerating the catalog is what ListModels
// already does to answer the same question on GET /waired/v1/models.
func modelsSnapshot(models map[string]catalog.ModelState, manifests []catalog.Manifest,
	progress func(string) (completed, total, rateBps int64, ok bool)) management.ModelsSnapshot {
	snap := management.ModelsSnapshot{}
	for id, m := range models {
		switch m.State {
		case catalog.ModelStateReady:
			snap.Ready = append(snap.Ready, id)
		case catalog.ModelStateDownloading, catalog.ModelStateQueued, catalog.ModelStateVerifying:
			snap.Downloading = append(snap.Downloading, id)
			if completed, total, _, ok := progress(id); ok {
				snap.Downloads = append(snap.Downloads, management.ModelDownload{
					Model: id, CompletedBytes: completed, TotalBytes: total,
				})
			}
		case catalog.ModelStateFailed:
			snap.Failed = append(snap.Failed, id)
			if m.Error != "" {
				// runPullJob has stored the real cause all along; this
				// snapshot was the wall it never crossed, which is why
				// `waired models pull` could only say "failed"
				// (waired-agent#328). Omitted rather than faked when
				// nothing was recorded — an older failure, or one written
				// before the field existed.
				snap.Failures = append(snap.Failures, management.ModelFailure{
					Model: id, Error: m.Error,
				})
			}
		}
	}
	// The models the switch above claimed nothing for: no state row, or a
	// row in a state none of the three lanes takes (not_present, evicted,
	// and anything a future state adds). Walking the manifests rather than
	// the state map is the whole point — the interesting case is the model
	// with no row at all.
	//
	// Manifest order, so the list a caller diffs between two polls does not
	// reshuffle. The three lanes above keep their map order; changing that
	// is not this projection's job.
	for _, m := range manifests {
		switch models[m.ModelID].State {
		case catalog.ModelStateReady, catalog.ModelStateDownloading, catalog.ModelStateQueued,
			catalog.ModelStateVerifying, catalog.ModelStateFailed:
		default:
			snap.NotPresent = append(snap.NotPresent, m.ModelID)
		}
	}
	return snap
}

// endpointState reconciles what the catalog RECORDED about an endpoint with
// what its engine is doing now.
//
// catalog.EndpointState.State is written exactly once, as the literal
// "ready", when a model's weights finish downloading — and then persisted to
// state.json. Nothing ever downgrades it: not an engine failure, not a park,
// not a stop, not a restart. So on a host whose engine could not bind its
// port, /inference/status reported subsystem_state oscillating
// starting → engine_failed while active_endpoints kept saying the vLLM
// endpoint was "ready", for as long as the weights stayed on disk
// (waired-agent#1026).
//
// An endpoint cannot be readier than the engine that serves it, so the
// engine's state wins whenever it is not ready. The recorded value is kept
// otherwise: "the weights are here" is still the fact it was written to
// carry, and this function does not invent a better one.
//
// An engine with no runtime entry at all (an old daemon, a runtime the
// registry does not know) leaves the record alone rather than guessing.
func endpointState(recorded string, rt management.RuntimeStatus) string {
	if rt.State == "" || rt.State == infruntime.StateReady {
		return recorded
	}
	return rt.State
}

func (p *agentInferenceProvider) Status(ctx context.Context) management.InferenceStatus {
	state, _ := p.store.Load()
	hwProfile := p.profiler.Profile(ctx)
	rs := map[string]management.RuntimeStatus{}
	for _, name := range p.registry.Names() {
		rs[name] = p.runtimeStatusFor(ctx, name, hwProfile)
	}
	models := modelsSnapshot(state.Models, p.manifests, p.dlProgress.aggregate)
	endpoints := []management.ActiveEndpoint{}
	for id, e := range state.Endpoints {
		endpoints = append(endpoints, management.ActiveEndpoint{
			EndpointID: id, Runtime: e.Runtime, ModelID: e.ModelID,
			State: endpointState(e.State, rs[e.Runtime]),
		})
	}
	subState := subsystemState(p.subsystemFacts(ctx, hwProfile, state))
	desiredStateStr := ""
	if p.inferenceState != nil {
		_, desired := p.inferenceState()
		desiredStateStr = string(desired)
	}
	// Whether anyone has actually written the toggle, read from the file the
	// daemon's own cutoff reads (hostCutoffIsStillOurs). DesiredState above
	// cannot answer it: an unset file reports the LIVE state, so a host that
	// is on by default and a host somebody turned on both say "enabled".
	// waired#1142 is what that costs — install-flow step 6 could not tell a
	// choice from a default, so it treated only "off" as an answer.
	desiredStateSet := p.desiredInferenceStateSet()
	p.benchMu.Lock()
	depth := p.lastDepthBench
	p.benchMu.Unlock()
	// waired-agent#837: how many requests this machine's engine is serving
	// right now. Read through the nil check rather than servingInFlight(),
	// which folds "nothing wired" into 0 — here those must stay apart, and
	// the wire says "not reported" by omitting the field.
	var inflight *int
	if p.servingInflight != nil {
		n := p.servingInflight()
		inflight = &n
	}
	return management.InferenceStatus{
		Inflight:        inflight,
		SubsystemState:  subState,
		Runtimes:        rs,
		Models:          models,
		ActiveEndpoints: endpoints,
		Active:          activeFromCatalog(state.Active),
		AvailableUpdate: computeAvailableUpdate(ctx, p.store, p.profiler, p.manifests, p.effectiveCfg(), p.servingEngineVersion(ctx)),
		LongContext:     longContextBenchFor(depth),
		DesiredState:    desiredStateStr,
		DesiredStateSet: desiredStateSet,
		NoModelSelected: p.noModelSelected.Load(),
		HostSpeed:       p.hostSpeedStatus(),
		// Read through setupHostSpeedProgress, the same accessor the setup
		// rows use, so a daemon restart on an already-measured host reports
		// "measured" here too rather than looking like an unstarted
		// measurement for the length of its settle window (waired#1143).
		HostSpeedStage: p.setupHostSpeedProgress().Stage.String(),
	}
}

// hostSpeedStatus maps the host measurement onto the management wire
// shape (#496). nil when this host has never measured.
func (p *agentInferenceProvider) hostSpeedStatus() *management.HostSpeedStatus {
	s := p.hostSpeedNow()
	if s == nil {
		return nil
	}
	return &management.HostSpeedStatus{
		TurnSeconds:      s.TurnSeconds,
		BudgetSeconds:    hostfit.HostCutoffTurnBudgetSeconds,
		TurnFloorSeconds: s.TurnFloorSeconds,
		Method:           s.Method,
		DepthTokens:      s.DepthTokens,
		PromptTokens:     s.PromptTokens,

		PrefillTokps:       s.PrefillTokps,
		DecodeTokps:        s.DecodeTokps,
		Samples:            s.Samples,
		SpreadPct:          s.SpreadPct,
		ProbeModelID:       s.ProbeModelID,
		MeasuredAt:         s.MeasuredAt,
		TurnedInferenceOff: p.hostSpeedTurnedInferenceOff(),
	}
}

// longContextBenchFor maps the agent-side depth sweep onto the
// management wire shape. nil in, nil out.
func longContextBenchFor(d *DepthBenchResult) *management.LongContextBench {
	if d == nil || len(d.Stages) == 0 {
		return nil
	}
	out := &management.LongContextBench{
		ContextLength: d.ContextLength,
		KVCacheType:   d.KVCacheType,
		Completed:     d.Completed,
		MeasuredAt:    d.MeasuredAt,
	}
	for _, st := range d.Stages {
		out.Stages = append(out.Stages, management.LongContextStage{
			TargetTokens: st.TargetTokens,
			PromptTokens: st.PromptTokens,
			PrefillTokps: st.PrefillTokps,
			DecodeTokps:  st.DecodeTokps,
			Failed:       st.Failed,
		})
	}
	return out
}

// hasUsableEngine reports whether at least one registered runtime can
// actually serve. Only the waired-managed engines count (ollama / vllm),
// and only when the binary is installed — the adapter is registered
// unconditionally at boot (see the OllamaAdapter wiring in setupInference)
// even when the binary is missing, so "registered" alone does not imply
// "usable". When nothing is usable the caller reports SubsystemState
// "no_engine", which the tray / CLI surface as an "Install Ollama" prompt.
// inferenceSubsystemFacts is everything the subsystem_state answer
// depends on, read once so the decision itself is a pure function of
// them. Split out for waired#1064: the same answer now has two readers —
// the local management API, and the mesh push that tells peers why this
// node is or is not serving — and two derivations would drift into two
// different stories about one machine.
//
// The engine axis collapses to EngineState / FailureLatched: both are
// only asked of an ollama adapter, so a host without one (a vLLM host,
// or a bare provider in a test) leaves EngineState empty, which reaches
// neither engine arm — the behaviour the `p.ollama != nil` guards used
// to spell out at each case.
type inferenceSubsystemFacts struct {
	// Disabled is the operator's soft pause; Parked is the hard engine
	// stop that frees memory (#186). Distinct: parked means a usable
	// engine exists and is intentionally down.
	Disabled bool
	Parked   bool
	// UsableEngine is hasUsableEngine — is there an engine at all.
	UsableEngine bool
	// EngineState is an infruntime.State* reading, empty when there is
	// no ollama adapter to ask.
	EngineState    string
	FailureLatched bool
	// HasActive is "a model has been chosen"; ModelKnown is "and the
	// catalog has a row for it". ModelState is that row's lifecycle
	// state, meaningless unless ModelKnown.
	HasActive  bool
	ModelKnown bool
	ModelState string
}

// subsystemState answers WHAT is wrong, never whether it will fix
// itself: a crash loop alternates between starting and engine_failed for
// as long as its recovery budget lasts. Order is load-bearing — the
// first matching arm wins, and the arms are ordered most-decisive first.
func subsystemState(f inferenceSubsystemFacts) string {
	switch {
	case f.Disabled:
		// The operator's pause overrides engine health: reporting a
		// crashed engine on a machine that was told not to serve would
		// send someone looking for a fault that is a setting.
		return signer.SubsystemStateDisabled
	case f.Parked:
		return signer.SubsystemStateStopped
	case !f.UsableEngine:
		return signer.SubsystemStateNoEngine
	case f.EngineState == infruntime.StateStarting:
		// Restart in flight (e.g. just after a start request); not
		// serving yet even if the active model is on disk.
		return signer.SubsystemStateStarting
	case f.EngineState == infruntime.StateFailed:
		// A crashed model runner, an exhausted recovery budget, or a boot
		// that never came up. This arm used to be absent, so any of those
		// fell through to ready whenever the active model happened to be
		// on disk (waired-agent#29).
		return signer.SubsystemStateEngineFailed
	case f.FailureLatched:
		// Latched, but the live reading no longer says so: LatchFailed
		// writes StateFailed and a later Stop() overwrites it with no
		// giveUp guard — a model switch, a reconcile bounce — so an
		// engine that has permanently stopped restarting could fall
		// through to ready (#310). The latch outlives the state, so it
		// decides here too. After the StateFailed arm rather than folded
		// into it: that one is the live, more specific reading, and both
		// carry the same answer whenever both apply.
		return signer.SubsystemStateEngineFailed
	case f.EngineState == infruntime.StateStopped,
		f.EngineState == infruntime.StateNotStarted:
		// Not up, and not on its way up. Both fell through to ready
		// whenever the active model happened to be on disk, the same hole
		// the StateFailed arm above closed for its own state
		// (waired-agent#1026). It is reachable in an ordinary bootstrap:
		// bootstrapVLLM stops the recorded adapter and then registers a
		// freshly built one, whose initial state is NotStarted — so a
		// crash-looping host flickered through "ready" between attempts.
		//
		// AFTER the latch arm, for the reason the latch arm is after
		// StateFailed: Stop() overwrites StateFailed with StateStopped,
		// so a latched engine that was then bounced arrives here, and
		// answering "starting" would undo #310 (TestStatus_LatchedEngine
		// StaysEngineFailedThroughAStop pins exactly that sequence).
		//
		// The parked case never reaches here — f.Parked is answered at the
		// top — so what is left is a start that is expected to follow.
		return signer.SubsystemStateStarting
	case !f.HasActive, !f.ModelKnown:
		return signer.SubsystemStateAwaitingModel
	case f.ModelState == catalog.ModelStateFailed:
		return signer.SubsystemStatePullFailed
	case f.ModelState != catalog.ModelStateReady:
		return signer.SubsystemStateLoading
	}
	return signer.SubsystemStateReady
}

// subsystemFacts reads the facts above. hw is passed in rather than
// sampled here so a caller that already has a profile does not resample;
// the profiler is TTL-cached (hardwareResampleInterval) either way.
func (p *agentInferenceProvider) subsystemFacts(ctx context.Context, hw hardware.Profile, st catalog.State) inferenceSubsystemFacts {
	f := inferenceSubsystemFacts{
		Disabled:     p.isInferenceDisabled != nil && p.isInferenceDisabled(),
		UsableEngine: hasUsableEngine(p.registry, hw, p.ollamaUsable, p.vllmUsable),
	}
	// The SERVING engine's facts, not the ollama adapter's (#944). p.ollama
	// is non-nil on every host, whatever engine serves, so reading it
	// unconditionally meant a vLLM host reported an idle adapter's health as
	// its own — and an ollama park flipped subsystem_state to "stopped",
	// which is pushed to the mesh, so peers stopped routing to a host that
	// was still answering.
	f.Parked = p.engineIsParked()
	if a := p.servingAdapter(); a != nil {
		f.EngineState = a.Health(ctx).State
		if fl, ok := a.(interface{ FailureLatched() bool }); ok {
			f.FailureLatched = fl.FailureLatched()
		}
	}
	if st.Active != nil {
		f.HasActive = true
		ms, ok := st.Models[st.Active.ModelID]
		f.ModelKnown = ok
		f.ModelState = ms.State
	}
	return f
}

// SubsystemState is the mesh-facing reader of the same answer
// (waired#1064). Cheap enough for the 5 s probe tick: one catalog load,
// one TTL-cached hardware profile, and in-memory adapter reads.
func (p *agentInferenceProvider) SubsystemState(ctx context.Context) string {
	st, _ := p.store.Load()
	return subsystemState(p.subsystemFacts(ctx, p.profiler.Profile(ctx), st))
}

func hasUsableEngine(reg *infruntime.Registry, hw hardware.Profile, ollamaUsable, vllmUsable func() bool) bool {
	for _, name := range reg.Names() {
		if engineUsableOnHost(name, hw, ollamaUsable, vllmUsable) {
			return true
		}
	}
	return false
}

// engineUsableOnHost is that question for ONE engine, and is the single
// rule both hasUsableEngine and runtimeStatusFor's Installed field ask,
// so subsystem_state and the INSTALLED column cannot disagree about the
// same host (#852).
//
// Resolution order per engine:
//
//  1. The agent's live resolver, which knows the bundled binary under
//     the state dir that a PATH-based probe cannot see (#188), and for
//     vllm the verified venv (#225 — that arm read the profile directly
//     and was the last engine-presence site not routed through
//     engineInstalledOnHost).
//  2. Only when no resolver was wired — a unit fixture constructing the
//     provider directly — the hardware profile. It resolves the same way
//     since #238, but it is TTL-cached for 30 s, and engine_resolve.go
//     says in as many words why that is not good enough: it is LATE for
//     a fresh install, so a host whose engine appeared during setup
//     would report absent for half a minute after it was usable.
//
// A resolver that answers "no" is the answer; it does not fall through
// to the profile for a second opinion.
func engineUsableOnHost(name string, hw hardware.Profile, ollamaUsable, vllmUsable func() bool) bool {
	switch name {
	case "ollama":
		if ollamaUsable != nil {
			return ollamaUsable()
		}
		return hw.Engines.Ollama.Installed
	case "vllm":
		if vllmUsable != nil {
			return vllmUsable()
		}
		return hw.Engines.VLLM.Installed
	default:
		// An engine kind the registry knows and this rule does not.
		// Unknown means not installed, the same way
		// engineInstalledOnHost answers it.
		return false
	}
}

func (p *agentInferenceProvider) Hardware(ctx context.Context) hardware.Profile {
	return p.profiler.Profile(ctx)
}

// EngineReady is the Phase 8 /healthz answer: is the local inference
// engine up and serving the active model? Cheaper than full Status()
// because it inspects only the catalog state record + the disabled
// flag — runtime adapter Health() probes (HTTP calls to Ollama /
// vLLM) happen separately on the inference probe loop's 5 s cadence
// and write through the state snapshot the engine reaches here.
//
// The remote /healthz coordinator combines this with the gate flags
// (paused / share_off / capacity_used vs capacity_total) the handler
// reads directly from the Server struct — so a single 200 body
// captures four orthogonal admission signals.
func (p *agentInferenceProvider) EngineReady() (bool, string) {
	if p.isInferenceDisabled != nil && p.isInferenceDisabled() {
		return false, ""
	}
	// Hard-stopped (#186): no engine serving, so the remote /healthz
	// coordinator must not advertise capacity that would 503. The SERVING
	// engine's latch — reading p.ollama's meant an ollama park made a vLLM
	// host advertise itself as not-ready while vLLM served on (#944).
	if p.engineIsParked() {
		return false, ""
	}
	// The engine's OWN health, not just the catalog record (waired-agent#29):
	// an ollama whose model runner died keeps answering /api/tags with 200
	// while every inference request 500s, and this function is what the peer
	// /healthz, the observability gauges, `waired doctor`, the setup engine
	// gate and the benchmark ready gate all read. A whitelist (!= Ready)
	// rather than a blacklist, so an unforeseen state reads as not-ready.
	// Gated on "there is an adapter" rather than on "the engine is ollama"
	// (#944): the old spelling skipped the check entirely on a vLLM host, so
	// a dead vLLM kept advertising capacity as long as a model was recorded
	// Active. A whitelist (!= Ready) rather than a blacklist, so an
	// unforeseen state reads as not-ready — and no adapter at all is
	// not-ready too, which on a vLLM host means the bootstrap has not
	// reached the spawn.
	a := p.servingAdapter()
	if a == nil || a.Health(context.Background()).State != infruntime.StateReady {
		return false, ""
	}
	st, _ := p.store.Load()
	if st.Active == nil {
		return false, ""
	}
	modelID := st.Active.ModelID
	ms, ok := st.Models[modelID]
	if !ok || ms.State != catalog.ModelStateReady {
		return false, modelID
	}
	return true, modelID
}

// ActiveModelID returns the catalog model_id of the device's committed
// ActiveSelection. It backs the Claude-intercept model mapping (#600):
// unlike EngineReady it does NOT gate on ready/parked state — a mid-pull
// or loading model must still resolve so the router can answer with the
// precise ErrModelNotReady (503 + Retry-After, which auto mode falls back
// on) rather than a blanket "no local model". Only a missing selection
// reports false.
//
// The setup report reads it too, through setupActiveModelID: on a host
// nobody ever asked which model to run, what this device is SERVING is
// the only evidence there is that it finished setting up (#753/#756).
func (p *agentInferenceProvider) ActiveModelID() (string, bool) {
	st, _ := p.store.Load()
	if st.Active == nil || st.Active.ModelID == "" {
		return "", false
	}
	return st.Active.ModelID, true
}

// LocalModelChoiceAt reports when a person at THIS machine last answered
// the model question, formatted for signer.InferenceState's field of the
// same name. It reads the preference file live rather than a cached copy:
// the answer can arrive at any time through the loopback management API,
// and the control plane's use for it is an ordering against its own
// instruction, so a stale reading is worse than none.
//
// "" whenever the file says anything else — no file, an abandoned
// question, an instruction the setup reconciler applied, or a record
// written before provenance existed. Every one of those is "no claim",
// and the consumer's own doc comment says what it must do with that.
func (p *agentInferenceProvider) LocalModelChoiceAt() string {
	if p.preferencePath == "" {
		return ""
	}
	pref, ok, err := agentconfig.LoadPreference(p.preferencePath)
	if err != nil || !ok || !pref.ChosenHere() || pref.SetAt.IsZero() {
		return ""
	}
	return pref.SetAt.UTC().Format(time.RFC3339Nano)
}

// ContextWindowFor reports the effective input-token window the given model
// id can serve on this host — min(manifest native window, host-sustainable
// applied window) — for the #623 Claude context-window advertisement and
// overflow guard (gateway.Deps.ContextWindowFor). The applied window is the
// serve tuning the agent actually exported (OLLAMA_CONTEXT_LENGTH /
// vLLM max-model-len), already native-capped by the tuner (#621/#624).
//
// The id may be a catalog model id / alias, a dynamic coding alias
// (waired/default), or an unknown claude-* id Claude Code sends; the latter
// two aren't catalog entries, so they resolve to the device-active model —
// the same target ResolveUnknownModel maps them to (#600). Returns 0 when
// the window can't be determined (no manifest, unknown sizing), so callers
// fail open (no advertisement / no 400) rather than guessing.
func (p *agentInferenceProvider) ContextWindowFor(modelID string) int {
	m, ok := catalog.LookupByAlias(modelID, p.manifests)
	if !ok {
		active, has := p.ActiveModelID()
		if !has {
			return 0
		}
		if m, ok = catalog.LookupByAlias(active, p.manifests); !ok {
			return 0
		}
	}

	// Host-sustainable applied window, from whichever engine is serving
	// this model. AppliedTuning is per-adapter (1-agent-1-model), so match
	// on ModelID to avoid reading a stale tuning for a different model.
	host := p.appliedContextWindow(m)

	native := m.ContextLength
	switch {
	case native > 0 && host > 0:
		if native < host {
			return native
		}
		return host
	case host > 0:
		return host
	case native > 0:
		// Untuned (cold engine, or unknown sizing): fall back to the
		// serve-time floor the tuner aims for, capped at native (#624).
		return router.EffectiveContextFloor(m)
	default:
		return 0
	}
}

// appliedTuningFor is the tuning the serving engine ACTUALLY loaded for
// m, ok=false when nothing has tuned yet. AppliedTuning is per-adapter
// (1 agent = 1 model), so the ModelID match is what keeps a stale tuning
// for a different model out of the answer. vLLM's answer wins when both
// adapters somehow carry one, preserving the precedence of the two-step
// scan this replaces.
func (p *agentInferenceProvider) appliedTuningFor(m catalog.Manifest) (infruntime.ModelTuning, bool) {
	if tuner, ok := p.vllmAdapter().(interface {
		AppliedTuning() infruntime.ModelTuning
	}); ok {
		if t := tuner.AppliedTuning(); t.ContextLength > 0 && t.ModelID == m.ModelID {
			return t, true
		}
	}
	if p.ollama != nil {
		if t := p.ollama.AppliedTuning(); t.ContextLength > 0 && t.ModelID == m.ModelID {
			return t, true
		}
	}
	return infruntime.ModelTuning{}, false
}

// appliedContextWindow is appliedTuningFor's window alone, 0 when
// nothing has tuned yet — the shape the guard sizing wants: the engine
// really is serving this window (fit-proven or forced rung alike), so
// overflow guards must size to it either way.
func (p *agentInferenceProvider) appliedContextWindow(m catalog.Manifest) int {
	t, ok := p.appliedTuningFor(m)
	if !ok {
		return 0
	}
	return t.ContextLength
}

// DeclaredContextWindow reports the window this device is willing to
// STAND BEHIND for its active model — signer.InferenceState.ContextWindow
// (waired#1031). 0 means "declares nothing", and every consumer reads that
// as unknown and falls open.
//
// It is deliberately NOT ContextWindowFor. That one exists to size a guard
// and therefore answers optimistically for an untuned engine: no applied
// tuning yet falls back to EffectiveContextFloor, the window the tuner AIMS
// for. Aiming for a window is exactly what a declaration may not be built
// on — a cold engine that later trims would have advertised a window it
// never loaded, which is the class of lie this field exists to remove. So
// the applied tuning is the only input, and its absence declares nothing.
//
// Below the smallest declarable window the answer is 0 rather than the real
// (smaller) number. "I serve less than I said" and "I say nothing" are
// different claims: a routing consumer can act safely on the second and
// cannot act on the first without also having to decide what a 98k peer
// means for a 200k session, which is the decision the two-window contract
// exists to avoid. The engine keeps serving that window for this device's
// own keyboard through the local /model directive — it just stops being a
// mesh answer.
//
// SPILL IS NOT A REASON TO WITHHOLD. A host whose weights partly sit in
// system RAM is serving the window it names — spill costs decode speed,
// not window size — so it declares that window like any other host. This
// reverses an earlier reading in which a rung the sizing could not prove
// the host holds (WindowFits false, the forced lowest rung of
// waired-agent#587) declared nothing: waired-ai/waired-agent#657 found a
// Windows host serving 200,704 tokens at a measured 12 s per coding turn
// while telling the mesh nothing, and the admin page rendered that silence
// as "takes no Claude Code sessions". Owner ruling (2026-08-11, recorded
// on the waired-ai/waired window-contract decision of 2026-08-02): Waired
// does not force state on a device — a machine the operator chose to run
// a model on is published on their own inference network, spilling or not.
//
// The honesty guard that remains is the window SIZE check below: a host
// tuned under the smallest declarable window still declares nothing,
// because that is the case where a 200k session would actually be
// truncated (the waired-ai/waired-agent#623 failure). Speed-based
// exclusion belongs to a consumer that can see speed — HostSpeed reaches
// the control plane and the management API, and is stripped from the
// served NetworkMap by design — not to the agent withholding a true fact.
func (p *agentInferenceProvider) DeclaredContextWindow() int {
	active, ok := p.ActiveModelID()
	if !ok {
		return 0
	}
	m, ok := catalog.LookupByAlias(active, p.manifests)
	if !ok {
		return 0
	}
	t, ok := p.appliedTuningFor(m)
	if !ok || t.ContextLength <= 0 {
		return 0
	}
	win := t.ContextLength
	// Never claim past the model's own window, whatever the engine was
	// told: a tuning above native is a misconfiguration, not a capability.
	if m.ContextLength > 0 && win > m.ContextLength {
		win = m.ContextLength
	}
	if win < hostfit.ServingWindow200k {
		return 0
	}
	return win
}

func (p *agentInferenceProvider) Runtimes(ctx context.Context) []management.RuntimeStatus {
	hwProfile := p.profiler.Profile(ctx)
	out := []management.RuntimeStatus{}
	for _, name := range p.registry.Names() {
		out = append(out, p.runtimeStatusFor(ctx, name, hwProfile))
	}
	return out
}

// runtimeStatusFor builds the per-engine wire entry shared by Status()
// and Runtimes(). Version stays the binary-`--version` the hardware
// profiler detected (old-client semantics); the provenance fields
// (mode / live_version / pinned_version / version_warning /
// last_error) describe the engine actually serving.
//
// Installed used to be the literal true, for any adapter the registry
// knew about — the one field named "installed" was the one that never
// asked (#852). A Windows host whose daemon logged "bundled ollama not
// installed (expected at ...)" on the same boot printed INSTALLED yes.
// engine_resolve.go states the rule that covers this: every place that
// answers "is this engine installed here" must go through the same
// resolution, and four separate sites had already got it wrong by
// reaching for a convenient probe. This was a fifth, arriving at the
// wrong answer by a different route — not a bad probe, no probe.
//
// Three surfaces read this field and were all wrong on such a host:
// the INSTALLED column of `waired runtimes ls`, the `if !r.Installed`
// skip in `waired status` (a branch that had never once fired), and
// install.sh's waired_engine_installed, whose done banner therefore
// always claimed the engine was installed.
func (p *agentInferenceProvider) runtimeStatusFor(ctx context.Context, name string, hwProfile hardware.Profile) management.RuntimeStatus {
	ad, _ := p.registry.Lookup(name)
	h := ad.Health(ctx)
	entry := management.RuntimeStatus{
		Name:      name,
		Installed: engineUsableOnHost(name, hwProfile, p.ollamaUsable, p.vllmUsable),
		State:     h.State,
	}
	if h.State == infruntime.StateFailed {
		entry.LastError = h.LastErr
	}
	switch name {
	case "ollama":
		if hwProfile.Engines.Ollama.Installed {
			entry.Version = hwProfile.Engines.Ollama.Version
		}
		if p.ollama != nil {
			// #310: say whether waired has STOPPED restarting this engine.
			// Without it a client watching subsystem_state cannot tell
			// "down, recovering" from "down, and nothing will change until
			// you act" — `waired init` had to guess from how long the
			// state had held. The reason comes with it, because Stop()
			// clobbers the copy in Health() while the latch stands.
			latched, reason := p.ollama.FailureLatchedReason()
			entry.FailureLatched = latched
			if latched && entry.LastError == "" {
				entry.LastError = reason
			}
			// #290: surface the resolved GPU backend so a silent CPU
			// fallback (GPU present but not engaged) is visible.
			entry.Backend = string(p.ollama.ResolvedBackend())
			entry.Mode = string(p.ollama.Mode())
			// #879: whether the weights are actually in (V)RAM. Left off
			// the wire entirely until a probe has looked, so a client can
			// tell "nothing loaded" from "no claim".
			if res := p.ollama.Residency(); res.Observed {
				resident := res.Resident()
				entry.ModelResident = &resident
				entry.ModelResidentModel = res.Model
				if resident && res.Indefinite {
					entry.ModelResidentIndefinitely = true
				} else if resident && !res.Until.IsZero() {
					entry.ModelResidentUntil = res.Until.UTC().Format(time.RFC3339)
				}
				// waired-agent#837: when the reading was taken, and
				// whether what is loaded is what this computer serves.
				// Both are things only this side can answer, and both
				// stay absent rather than guess.
				if !res.At.IsZero() {
					entry.ModelResidentAt = res.At.UTC().Format(time.RFC3339)
				}
				if tags := p.activeServingTags(); len(tags) > 0 {
					isActive := slices.Contains(tags, res.Model)
					entry.ModelResidentIsActive = &isActive
				}
			}
			entry.LiveVersion = p.ollama.EngineVersion()
			entry.PinnedVersion = infruntime.OllamaPinnedVersion
			entry.VersionWarning = ollamaVersionWarning(entry.LiveVersion)
			// #621: surface the exported serve tuning + its verification
			// outcome so a floored window / f16 fallback / spill is
			// never silent. Zero value (tuning never computed) leaves
			// the fields empty for old-agent parity.
			if tune := p.ollama.AppliedTuning(); tune != (infruntime.ModelTuning{}) {
				entry.ContextLength = tune.ContextLength
				entry.KVCacheType = tune.KVCacheType
				// #763: report the runner's real request parallelism when it
				// was observed (Ollama caps OLLAMA_NUM_PARALLEL silently);
				// fall back to the exported intent otherwise.
				entry.NumParallel = tune.NumParallel
				if tune.ObservedNumParallel > 0 {
					entry.NumParallel = tune.ObservedNumParallel
				}
				entry.NumBatch = tune.NumBatch
				entry.TuningWarning = tune.Warning
				entry.TuningDegraded = tune.Degraded
				entry.PostLoadFreeVRAMMB = tune.PostLoadFreeVRAMMB
			}
		}
	case "vllm":
		if hwProfile.Engines.VLLM.Installed {
			entry.Version = hwProfile.Engines.VLLM.Version
		}
		// #843: ollama parity for the pin. Until the converge shipped
		// there was nothing to report — a venv could sit several
		// releases behind the pin and no surface said so, on a host
		// where the parser table and serve flags this build emits were
		// read out of the pinned release. The converge is what fixes
		// it; this is how a host says the converge has not happened
		// yet.
		entry.PinnedVersion = infruntime.VLLMPinnedVersion
		entry.VersionWarning = vllmVersionWarning(entry.Version)
		// ollama parity for #310's give-up latch (waired-agent#1026). The
		// vLLM arm published no latch at all, so a client watching this
		// host could not tell "down, retrying" from "down, and nothing
		// will change until you act" — and until #1026 wired
		// OnStartFailed there was no latch to publish either. The reason
		// rides with it for the reason the ollama arm gives: Stop()
		// clobbers the copy in Health() while the latch stands.
		if l, ok := p.vllmAdapter().(interface {
			FailureLatchedReason() (bool, string)
		}); ok {
			latched, reason := l.FailureLatchedReason()
			entry.FailureLatched = latched
			if latched && entry.LastError == "" {
				entry.LastError = reason
			}
		}
		// #675: surface the exported max-model-len sizing and its
		// warning, ollama parity. The adapter is the linux-only
		// VLLMAdapter behind the Adapter interface, so reach the
		// tuning through an assertion this untagged file can compile
		// on every platform.
		if tuner, ok := p.vllmAdapter().(interface{ AppliedTuning() infruntime.ModelTuning }); ok {
			if tune := tuner.AppliedTuning(); tune != (infruntime.ModelTuning{}) {
				entry.ContextLength = tune.ContextLength
				entry.TuningWarning = tune.Warning
			}
		}
	}
	return entry
}

// ollamaEngineVersion is the serving-engine version used against
// per-variant MinEngineVersion floors, from the cheapest authoritative
// source available:
//
//  1. the adapter's live /api/version, once the engine has been ready
//     once — this is the process that will actually load the weights;
//  2. the profiler's snapshot, which holds the same measurement the
//     probe below makes, already paid for;
//  3. a fresh measurement of the installed binary.
//
// Step 3 exists because step 2 is a 30 s cache and a fresh install
// takes its first snapshot BEFORE the engine is installed: for up to
// 30 s after the binary lands the version read unknown, and unknown
// excludes every floored variant, so the pull silently dropped to the
// lower one and #305's dedup then pinned it there (#361).
//
// "" still means genuinely unknown — no live engine, no snapshot, and
// a binary that could not be executed or did not parse. Floored
// variants keep failing closed on it, which is the behaviour the
// qwen3.6 mtp incident asked for.
func (p *agentInferenceProvider) ollamaEngineVersion(ctx context.Context) string {
	if p.ollama != nil {
		if v := p.ollama.EngineVersion(); v != "" {
			return v
		}
	}
	if p.profiler != nil {
		if v := p.profiler.Profile(ctx).Engines.Ollama.Version; v != "" {
			return v
		}
	}
	return p.probedOllamaVersion(ctx)
}

// engineVersionMemoTTL bounds how long a MEASURED engine version is
// reused. Same 30 s the profiler caches its whole snapshot for: the
// value changes only when the binary is replaced, and re-measuring is
// an exec with a 5 s timeout.
const engineVersionMemoTTL = 30 * time.Second

// probedOllamaVersion measures the installed ollama binary, memoized.
//
// The lock is held ACROSS the exec on purpose: the callers that reach
// here (Status' AvailableUpdate on every poll, the recommendation
// surfaces, PullModel) can arrive together, and one serialized probe is
// cheaper than a herd of concurrent ones. A negative result is memoized
// too — a host that cannot report a version must not pay an exec per
// caller to learn that again.
func (p *agentInferenceProvider) probedOllamaVersion(ctx context.Context) string {
	if p.engineVersionProbe == nil {
		return "" // no resolver wired (unit fixtures): the pre-#361 answer
	}
	p.engineVerMu.Lock()
	defer p.engineVerMu.Unlock()
	if !p.engineVerAt.IsZero() && time.Since(p.engineVerAt) < engineVersionMemoTTL {
		return p.engineVerVal
	}
	installed, v := p.engineVersionProbe(ctx, catalog.RuntimeOllama)
	if !installed {
		v = ""
	}
	p.engineVerVal, p.engineVerAt = v, time.Now()
	return v
}

// ollamaVersionWarning derives the agent-side version warning. The
// serving engine must be exactly the pin — anything else means waired
// is not in control of what answers requests. An unknown live version
// ("") yields no warning — absence of data, not evidence of mismatch.
func ollamaVersionWarning(live string) string {
	if live == "" {
		return ""
	}
	if live != infruntime.OllamaPinnedVersion {
		return fmt.Sprintf("engine version %s does not match the bundled pin %s — restart waired-agent or %s",
			live, infruntime.OllamaPinnedVersion, elevation.Hint("waired runtimes install ollama"))
	}
	return ""
}

// vllmVersionWarning is ollamaVersionWarning for the venv (#843). Same
// rule — the pin is exact, so anything else means this build is not
// serving with what it was tested against — and the same treatment of
// an unknown version: absence of data, not evidence of mismatch.
//
// The advice differs. Ollama's points at `runtimes install`, which is
// how that engine was repaired before #826; this one points at the
// converge verb, because rebuilding a venv by hand through `runtimes
// install vllm` prompts for a ~6 GB confirmation the converge has
// already earned.
func vllmVersionWarning(installed string) string {
	if installed == "" {
		return ""
	}
	if installed != infruntime.VLLMPinnedVersion {
		return fmt.Sprintf("vLLM venv %s does not match the pin %s — %s",
			installed, infruntime.VLLMPinnedVersion, elevation.Hint("waired runtimes upgrade vllm"))
	}
	return ""
}

func (p *agentInferenceProvider) ListModels(_ context.Context) []management.ModelEntry {
	state, _ := p.store.Load()
	out := []management.ModelEntry{}
	for _, m := range p.manifests {
		st := state.Models[m.ModelID]
		entry := management.ModelEntry{
			ModelID:   m.ModelID,
			Aliases:   m.ModelAliases,
			State:     stateOrDefault(st.State, catalog.ModelStateNotPresent),
			SizeBytes: st.SizeBytes,
			VariantID: st.VariantID,
		}
		if len(m.Variants) > 0 {
			entry.Source = m.Variants[0].Source.Type + ":" + m.Variants[0].Source.Tag
		}
		out = append(out, entry)
	}
	return out
}

// modelSizesTimeout bounds the engine round trip ModelSizes makes. Short,
// unlike probeHTTPTimeout: this one sits under an interactive
// `waired models ls`, where a wedged engine should cost a moment and an
// empty column rather than ten seconds of nothing.
const modelSizesTimeout = 2 * time.Second

// ModelSizes reports each downloaded model's on-disk bytes, keyed by model
// id, by asking the engine.
//
// The engine is the source because nothing else has the figure:
// catalog.ModelState.SizeBytes is declared and read but never written, so
// `waired models ls` printed "-" in the SIZE column for every model,
// including the several gigabytes actually on disk (#661). /api/tags
// reports the on-disk blob size per tag — the same figure the tuning
// verification already trusts over the manifest's estimate — and one
// request covers every model.
//
// Reading it live rather than recording it at pull time is deliberate: a
// figure written at pull time would be missing for every model pulled
// before this shipped, which is exactly the population the report was
// about.
//
// Nil on every uncertainty — a non-ollama engine, an engine that never
// started, a request that failed or timed out. The caller then shows what
// the state file holds, which is what this column did before. A stopped
// engine means the size is unknown, not zero.
func (p *agentInferenceProvider) ModelSizes(ctx context.Context) map[string]int64 {
	if p.servingEngine() != catalog.RuntimeOllama || p.ollama == nil {
		return nil
	}
	sizes, err := ollamaTagSizes(ctx, http.DefaultClient, p.ollama.BaseURL(), modelSizesTimeout)
	if err != nil || len(sizes) == 0 {
		return nil
	}
	state, _ := p.store.Load()
	out := make(map[string]int64, len(state.Models))
	for modelID, st := range state.Models {
		// OllamaTag, not BaseOllamaTag: on a host running a derived
		// batch tag (#642) the derived model is what occupies the disk,
		// and it is the tag the engine reports.
		if b := sizes[st.OllamaTag]; st.OllamaTag != "" && b > 0 {
			out[modelID] = b
		}
	}
	return out
}

// servingEngine is the engine the agent actually serves from. The empty
// string — the unset pointer in unit tests and pre-#557 code paths —
// means ollama, preserving the historical default so existing behaviour
// is unchanged for hosts that never opt into vLLM.
func (p *agentInferenceProvider) servingEngine() string {
	e := p.engine.Load()
	if e == nil || *e == "" {
		return catalog.RuntimeOllama
	}
	return *e
}

// setServingEngine records which engine this process serves from. Written
// once at construction, and again by the adopt trigger when the boot rule
// re-run against the live host names a different engine (#339) — see
// adoptEngine, which is the only caller that changes it after boot.
func (p *agentInferenceProvider) setServingEngine(engine string) {
	p.engine.Store(&engine)
}

// engineVersionFor returns the installed version of the given serving
// engine, used to gate variant selection by MinEngineVersion. "" means
// unknown (the gate then fails closed only for floored variants). vLLM's
// version comes from the venv the installer activated; ollama's from the
// live engine probe.
// servingEngineVersion is engineVersionFor asked of the engine this host
// actually serves with. Every caller that measures a candidate against a
// per-variant MinEngineVersion floor wants this one: the update hint, the
// benchmark recommendations, and the version the mesh publishes all used
// ollamaEngineVersion unconditionally, so a vLLM host judged its shelf
// against an engine it was not running — or against "" on a host with no
// ollama binary at all, which fails every floored variant closed
// (waired-agent#1028, waired-agent#948).
func (p *agentInferenceProvider) servingEngineVersion(ctx context.Context) string {
	if p == nil {
		return ""
	}
	return p.engineVersionFor(ctx, p.servingEngine())
}

func (p *agentInferenceProvider) engineVersionFor(ctx context.Context, engine string) string {
	if engine == catalog.RuntimeVLLM {
		v, _ := vllmActiveVersion(p.stateDir)
		return v
	}
	return p.ollamaEngineVersion(ctx)
}

// Pull-refusal sentinels. PullModel's callers used to have only its
// prose to go on, so the setup reconciler reported every refusal as
// model_not_found — "pick another model" — including the two cases where
// no other model helps (waired-agent#134). These carry the distinction
// as a value; each is wrapped into the message that already explains it,
// so what the operator reads is unchanged apart from the parenthetical.
//
// Deliberately NOT a catch-all "pull failed" sentinel: the default for
// an unmarked refusal is still model_not_found, which is the truth for
// an unknown alias or a manifest with no servable variant.
var (
	// errPullsDisabled: this host does not download models at all.
	errPullsDisabled = errors.New("pulls are turned off on this device")
	// errEngineTooOld: the model is real and the engine cannot load it.
	errEngineTooOld = errors.New("the engine on this device is too old for this model")
	// errUnsupportedSource: the variant's source cannot be fetched by
	// this engine on this OS (an HF/vLLM variant off Linux, a source type
	// no runtime claims).
	errUnsupportedSource = errors.New("this device cannot fetch this model's files")
)

func (p *agentInferenceProvider) PullModel(ctx context.Context, modelOrAlias string) (management.PullJob, error) {
	if !p.cfg.AllowPull {
		return management.PullJob{}, fmt.Errorf("pulls are disabled by config (allow_pull=false): %w", errPullsDisabled)
	}
	// A retired name pulls its successor (#200). `waired models pull
	// <retired>` is a person typing a name they last saw in our own docs,
	// and "unknown model" would be a wrong answer: we shipped it.
	manifest, retired, ok := catalog.ResolveModel(modelOrAlias, p.manifests)
	if !ok {
		return management.PullJob{}, fmt.Errorf("unknown model %q", modelOrAlias)
	}
	if retired.SuccessorModelID != "" {
		p.logger.Info("pull target was retired; pulling its successor",
			"requested", modelOrAlias, "model", manifest.ModelID)
	}
	if len(manifest.Variants) == 0 {
		return management.PullJob{}, fmt.Errorf("manifest %s has no variants", manifest.ModelID)
	}
	// First variant the serving engine supports AND is new enough to
	// load (generalizes the historical Variants[0] rule): a too-old
	// engine pulls the plain variant instead of an mtp tag its registry
	// would refuse server-side with no useful error.
	engine := p.servingEngine()
	// #307: `ollama pull` is a CLIENT of a server that cannot exist
	// without a binary, so a pull dispatched onto an engine-less host is
	// doomed before it starts. Refusing HERE rather than letting it fail
	// is what keeps it from writing a `failed` row into the catalog:
	// snapshot() projects that row onto the wizard's "Download the AI
	// model" step, and the text it fails with ("download: ollama binary
	// not found", or a bare "exit status 1") carries nothing a classifier
	// can read, so it rendered as "check its internet connection" for the
	// whole multi-minute engine install (waired#986 F11).
	//
	// The setup reconciler already gates its own dispatch on the same
	// answer (setup_desired.go, enginePresent). This covers the paths that
	// do not: SwapPreferredModel from the tray, `waired models pull`, and
	// applyDaemonInitInference, which runs BEFORE the engine install in
	// `waired init`.
	//
	// The list used to name `waired models use` as a fourth. That command
	// has never existed — see cmd/waired/init_daemon_inference.go, where
	// #465 removed one of two remediation lines naming it.
	//
	// Live, not a boot-time snapshot: ollamaUsable is the state-dir-aware
	// resolver, so a host whose engine appears mid-run pulls on the next
	// attempt with no restart (#188). nil means "no resolver wired" (the
	// unit fixtures) — fail open there, exactly as startEngineAndBootstrap
	// does.
	if engine == catalog.RuntimeOllama && p.ollamaUsable != nil && !p.ollamaUsable() {
		return management.PullJob{}, fmt.Errorf(
			"cannot download %s yet: %w", manifest.ModelID, errEngineNotInstalled)
	}
	engineVersion := p.engineVersionFor(ctx, engine)
	variant, pullable := router.FirstPullableVariant(manifest, engine, engineVersion)
	if !pullable {
		floor := manifest.Variants[0].MinEngineVersion
		have := engineVersion
		if have == "" {
			have = "unknown"
		}
		return management.PullJob{}, fmt.Errorf(
			"model %s requires %s >= %s (engine reports %s); upgrade the engine or choose another model: %w",
			manifest.ModelID, engine, floor, have, errEngineTooOld)
	}
	if variant.VariantID != manifest.Variants[0].VariantID {
		p.logger.Info("pull skipped a variant the engine cannot load",
			"model", manifest.ModelID,
			"skipped", manifest.Variants[0].VariantID,
			"chosen", variant.VariantID,
			"engine", engine,
			"engine_version", engineVersion)
	}

	jobID := newJobID()
	// ctx bounds the synchronous admission above (alias lookup, the engine
	// version probe): a client that hangs up should abort that. The
	// download itself outlives the caller and runs on the daemon's ctx
	// (#305a).
	jobCtx := p.backgroundCtx()
	// dlCtx bounds the download and nothing else, so CancelPull can stop
	// it without touching work that must outlive the job — above all the
	// engine ollama.EnsureRunning may spawn on jobCtx (#305/R0).
	//
	// Created HERE, before beginPull publishes the job, so there is no
	// window in which a cancellable job is reachable with no CancelFunc
	// behind it. A cancel arriving in that window was one of the five
	// objections in
	// docs/decisions/20260805/1202-hold-the-bundled-prepull-instead-of-cancelling.md;
	// ordering removes it rather than guarding it.
	dlCtx, dlCancel := context.WithCancel(jobCtx)

	// Claim the model's single in-flight slot BEFORE the state write
	// below — that write is precisely what a second dispatcher must not
	// perform, since it resets a downloading model to queued and swaps the
	// tag out from under the job already fetching it (#305b).
	job := &pullJob{
		jobID: jobID, modelID: manifest.ModelID,
		variantID: variant.VariantID, tag: variant.Source.Tag,
		// Whether the choice above was made without knowing what the
		// engine can load. runPullJob revisits it after the engine is
		// serving, where the answer is always known (#361).
		resolvedBlind: engineVersion == "",
		stop:          newPullStop(dlCancel),
	}
	if running, joined := p.beginPull(job); joined {
		// This job never runs, so nothing will ever cancel its context.
		dlCancel()
		if running.tag != variant.Source.Tag {
			// The one line that would have made the rc7 double download
			// obvious: two dispatchers, one model, two tags.
			p.logger.Info("pull joined an in-flight job for a different variant of the same model",
				"model", manifest.ModelID, "in_flight_tag", running.tag,
				"requested_tag", variant.Source.Tag, "job", running.jobID)
		}
		return management.PullJob{JobID: running.jobID, ModelID: running.modelID, Status: "queued"}, nil
	}
	// From here every exit must release the slot: the ones that spawn do it
	// via spawnPull's defer, the ones that return early do it inline —
	// together with dlCancel, since a job that never starts has nobody
	// left to cancel it.

	switch variant.Source.Type {
	case catalog.SourceOllama:
		// A re-pull of a model that is already ready on disk must not
		// downgrade it to queued: serving continues from the on-disk blobs
		// while the pull runs, and a failed re-pull keeps it ready (#614).
		// The job's own writes re-check the same condition inside the
		// store's lock (recordPullState), so this is a fast path, not the
		// guard — a flag captured here would go stale the moment another
		// job moved the model (#305c).
		if err := p.store.Update(func(s *catalog.State) {
			if s.Models[manifest.ModelID].State == catalog.ModelStateReady {
				return
			}
			s.Models[manifest.ModelID] = catalog.ModelState{
				VariantID: variant.VariantID,
				OllamaTag: variant.Source.Tag,
				State:     catalog.ModelStateQueued,
			}
		}); err != nil {
			dlCancel()
			p.endPull(manifest.ModelID)
			return management.PullJob{}, err
		}
		p.spawnPull(job, func() {
			p.runPullJob(jobCtx, dlCtx, *job, manifest)
		})
	case catalog.SourceHuggingFace:
		// #557: vLLM safetensors. dispatchHFPull is defined per-OS — the
		// Linux build downloads the weights under <stateDir>/models/hf/
		// <repo> and records LocalPath so the next boot's bootstrapVLLM
		// spawns the engine against them; non-Linux returns an error
		// (vLLM serving is Linux-only). It writes the queued state itself.
		// It takes dlCtx: the HF path spawns no engine, so the download is
		// all there is to bound.
		if err := p.dispatchHFPull(dlCtx, job, manifest, variant); err != nil {
			dlCancel()
			p.endPull(manifest.ModelID)
			return management.PullJob{}, err
		}
	default:
		dlCancel()
		p.endPull(manifest.ModelID)
		return management.PullJob{}, fmt.Errorf("unsupported variant source type %q for engine %q: %w", variant.Source.Type, engine, errUnsupportedSource)
	}
	return management.PullJob{JobID: jobID, ModelID: manifest.ModelID, Status: "queued"}, nil
}

// pullJob is one in-flight model pull. Immutable once published under
// pullMu, so a joining caller reads it without holding the lock.
type pullJob struct {
	jobID     string
	modelID   string
	variantID string
	tag       string
	// resolvedBlind records that variantID/tag were chosen while the
	// engine version was unknown, so every floored variant was excluded
	// on no evidence. runPullJob re-resolves once the engine is serving
	// and replaces the registry entry with a new job value; the struct
	// itself stays immutable (#361).
	resolvedBlind bool
	// stop is the job's cancellation half. A POINTER because pullJob is
	// copied — runPullJob takes a value, and upgradeBlindVariant
	// republishes a replacement — so the flag every copy reads has to
	// live behind one shared address. It is never nil for a job
	// PullModel published; the unit fixtures that build a pullJob by
	// hand leave it nil, which reads as "cannot be cancelled".
	stop *pullStop
}

// pullStop carries the intent to abort one download.
//
// The intent is RECORDED rather than inferred from what the child
// process did. download.DefaultRunner.Run returns cmd.Wait()'s error, so
// a killed `ollama pull` surfaces as *exec.ExitError "signal: killed" —
// indistinguishable from an OOM kill, and errors.Is(err,
// context.Canceled) is never true. That is one of the five objections
// docs/decisions/20260805/1202-hold-the-bundled-prepull-instead-of-cancelling.md
// raised against cancelling a pull at all; a flag the canceller sets
// answers it, because provenance stops being a guess.
//
// cancel bounds ONLY the download. It must never reach
// ollama.EnsureRunning: that call can win the single-flight leader race
// and spawn `ollama serve` through exec.CommandContext, so a pull that
// cancelled the engine's own context would kill the engine it just
// started (#305/R0). runPullJob keeps the two contexts apart for that
// reason.
type pullStop struct {
	cancel    context.CancelFunc
	requested atomic.Bool
	// done closes when the job goroutine has fully unwound — including
	// the cancelled-row cleanup. DeleteModel waits on it so it acts on a
	// settled catalog rather than racing the job it just stopped.
	done chan struct{}
}

// newPullStop builds the cancellation half of a job. done is buffered by
// being a plain channel closed exactly once, in spawnPull's outermost
// defer.
func newPullStop(cancel context.CancelFunc) *pullStop {
	return &pullStop{cancel: cancel, done: make(chan struct{})}
}

// requestedStop reports whether this job was asked to stop. Safe on a
// job whose stop half is nil (the hand-built fixtures).
func (j pullJob) requestedStop() bool {
	return j.stop != nil && j.stop.requested.Load()
}

// beginPull claims the in-flight slot for j.modelID. It returns the job
// that is already running and joined=true when one is, in which case the
// caller must NOT touch the model's state row or start anything: the
// running job already stamped the variant it is downloading.
func (p *agentInferenceProvider) beginPull(j *pullJob) (running *pullJob, joined bool) {
	p.pullMu.Lock()
	defer p.pullMu.Unlock()
	if cur, ok := p.pullsInFlight[j.modelID]; ok && cur != nil {
		return cur, true
	}
	if p.pullsInFlight == nil {
		p.pullsInFlight = make(map[string]*pullJob)
	}
	p.pullsInFlight[j.modelID] = j
	return j, false
}

// endPull releases the slot and, when it was the last one, fires the
// reconcile a finished pull asked for. Removal, the emptiness test and
// the fire decision share one critical section, so the request can be
// neither lost (a job that arrives in the window makes `last` false and
// fires it on its own way out) nor doubled (the CAS consumes the
// intent). The reconcile itself runs off-lock.
//
// Two independent intents, consumed separately and collapsed into one
// reconcile. swap keeps its exact meaning — an operator switch, which
// always bounces and re-decides the KV cache type from scratch. A plain
// retune passes swap=false and leaves the decision to the reconcile's
// own ServeInputsEqual test, so an unrelated pull costs a resolve and
// nothing else. Both are consumed even when only one fires the call:
// a swap subsumes the retune.
func (p *agentInferenceProvider) endPull(modelID string) {
	p.pullMu.Lock()
	delete(p.pullsInFlight, modelID)
	last := len(p.pullsInFlight) == 0
	p.pullMu.Unlock()
	if !last {
		return
	}
	swap := p.swapBounceDeferred.CompareAndSwap(true, false)
	retune := p.retuneDeferred.CompareAndSwap(true, false)
	if swap || retune {
		p.requestEngineReconcile(swap)
	}
}

// spawnPull runs body as the background job for job.modelID and releases
// the slot afterwards. Defers are LIFO, so the cancelled-job cleanup runs
// before endPull, which runs before pullsWG.Done — and waitForPulls()
// returning therefore implies an empty registry. Covers panics as well as
// every return path inside body.
//
// The cleanup ordering is load-bearing: dropping a cancelled job's row
// after the slot is free would let a pull dispatched in that window have
// its own queued row deleted by the job that just left.
func (p *agentInferenceProvider) spawnPull(job *pullJob, body func()) {
	p.pullsWG.Add(1)
	go func() {
		// Registered first, so it runs LAST: a waiter woken by done must
		// find the slot released and the cancelled row already gone.
		if job.stop != nil {
			defer close(job.stop.done)
		}
		defer p.pullsWG.Done()
		defer p.endPull(job.modelID)
		defer p.settleCancelledPull(job)
		body()
	}()
}

// CancelPull stops the download in flight for modelID. It reports
// whether one was running: a model with no pull in flight is not an
// error, because "stop this" and "there is nothing to stop" leave the
// host in the same state and the operator asked for that state.
//
// Keyed on the model, not the job ID: the in-flight registry is keyed by
// model_id (#305 — keying by tag let two variants of one model download
// at once) and no index from job ID to job exists.
func (p *agentInferenceProvider) CancelPull(ctx context.Context, modelID string) (management.PullCancel, error) {
	p.pullMu.Lock()
	job := p.pullsInFlight[modelID]
	p.pullMu.Unlock()
	if job == nil || job.stop == nil {
		return management.PullCancel{ModelID: modelID, Status: pullCancelNotDownloading}, nil
	}
	// Record the intent BEFORE cancelling, so the job goroutine can never
	// observe a cancelled context without also seeing why it was
	// cancelled. The reverse order leaves a window in which the download
	// dies and runPullJob reads it as a download failure.
	job.stop.requested.Store(true)
	job.stop.cancel()
	p.logger.Info("stopping model download", "model", modelID, "tag", job.tag, "job", job.jobID)

	// Return only once the job has unwound, so the very next `models ls`
	// tells the truth. Killing the `ollama pull` child is immediate and
	// the retry backoff selects on the cancelled context, so this is
	// sub-second in practice; the cap is there so a wedged job cannot
	// hold the request open past the CLI's own 10s timeout.
	select {
	case <-job.stop.done:
	case <-ctx.Done():
		p.logger.Warn("the caller went away before the download stopped; it is still unwinding",
			"model", modelID, "job", job.jobID)
	case <-time.After(pullCancelSettle):
		p.logger.Warn("the download did not stop within the settle window; it is still unwinding",
			"model", modelID, "job", job.jobID, "waited", pullCancelSettle)
	}
	return management.PullCancel{ModelID: modelID, JobID: job.jobID, Status: pullCancelCancelled}, nil
}

// Statuses CancelPull reports. They are part of the management API's wire
// shape, and `waired models cancel` renders each one differently.
const (
	pullCancelCancelled      = "cancelled"
	pullCancelNotDownloading = "not_downloading"
)

// pullCancelSettle bounds the wait for a stopped job to unwind. Under the
// CLI's 10s DELETE timeout, so a wedged job surfaces as a warning here
// rather than as a client-side timeout with no explanation.
var pullCancelSettle = 5 * time.Second

// settleCancelledPull removes the half-finished row a cancelled job
// leaves behind, so the host lands where it was before the pull started.
//
// A model that reached Ready anyway is left alone. The cancel raced the
// last bytes and lost; the weights are on disk, and dropping the record
// would leave them with no name — the exact defect waired-agent#641
// reported and waired-agent#671 fixed on the delete path.
//
// The partly-downloaded blobs of a job that did NOT finish are not
// reclaimed: ollama keeps them as `<blob>-partial` under its own model
// dir, and `ollama rm` cannot name a tag whose manifest was never
// written. A later pull of the same model resumes from them. See the PR
// body for the follow-up.
func (p *agentInferenceProvider) settleCancelledPull(job *pullJob) {
	if !job.requestedStop() {
		return
	}
	landed := false
	if err := p.store.Update(func(s *catalog.State) {
		m, ok := s.Models[job.modelID]
		if !ok {
			return
		}
		if m.State == catalog.ModelStateReady {
			landed = true
			return
		}
		delete(s.Models, job.modelID)
	}); err != nil {
		p.logger.Warn("clearing the cancelled download's record failed",
			"model", job.modelID, "job", job.jobID, "err", err)
		return
	}
	if landed {
		p.logger.Info("model download finished before the cancel landed; keeping it",
			"model", job.modelID, "tag", job.tag, "job", job.jobID)
		return
	}
	p.logger.Info("cancelled download's record removed; the part already fetched stays on disk",
		"model", job.modelID, "tag", job.tag, "job", job.jobID)
}

// waitForPulls blocks until all background pull goroutines started by
// PullModel have returned — ollama and HuggingFace alike. Tests use it to
// join the writer goroutine before t.TempDir() cleanup (#377). When it
// returns, the in-flight registry is empty.
func (p *agentInferenceProvider) waitForPulls() { p.pullsWG.Wait() }

// backgroundCtx is the context for work that must outlive whoever asked
// for it. It is the daemon's own context — cancelled on SIGTERM and on
// session teardown, and nothing else — so a multi-GB download is neither
// orphaned at shutdown nor killed when an HTTP handler returns (#305a).
// Falls back to context.Background() for the unit-test providers that
// construct an agentInferenceProvider without one.
func (p *agentInferenceProvider) backgroundCtx() context.Context {
	if p.agentCtx != nil {
		return p.agentCtx
	}
	return context.Background()
}

// recordPullState moves modelID to next unless the model is currently
// ready, and records errMsg when non-empty. Every state write a pull job
// makes goes through it.
//
// It replaces a `refresh` flag computed at DISPATCH time (#614), which a
// job then carried for its whole life: a pull dispatched while the model
// was merely downloading held refresh=false and would overwrite a
// sibling's completed `ready` with `failed` (#305c). The state read inside
// the store's own lock is the only trustworthy input — a ready model is
// serving from on-disk blobs, and nothing a later pull does may take that
// down. The error text is still recorded either way, since it is the only
// observability a failed refresh leaves behind.
func (p *agentInferenceProvider) recordPullState(modelID, next, errMsg string) {
	_ = p.store.Update(func(s *catalog.State) {
		m := s.Models[modelID]
		if m.State != catalog.ModelStateReady {
			m.State = next
		}
		if errMsg != "" {
			m.Error = errMsg
		}
		s.Models[modelID] = m
	})
}

// modelPullAttempts / modelPullBackoff pace the download retry, matching
// the engine-start retry's shape (engineEnsureAttempts). The retry lives
// in the job rather than in any dispatcher because the job already owns
// the model's in-flight slot, so a retry can never become a second
// concurrent download — and because it is the one place every driver
// (bundled, preferred, setup, `waired models pull`, pre-cache) passes
// through. The reconciler could not host it: runPush's ticker returns
// immediately without a control-plane client, and Apply only runs when a
// network-map frame arrives. Backoff is a var so tests don't sleep.
const modelPullAttempts = 3

// enginePullBounceGrace bounds the extra attempts a job may take because
// the ENGINE was restarted under it rather than because the download
// failed (#359). It is a separate budget from modelPullAttempts on
// purpose: that one exists for the registry, the network and the disk,
// and spending it on our own restarts is how a perfectly healthy download
// reached `failed` — the wizard's model row then went red for a reason
// that had nothing to do with the model.
//
// Two, because two is the worst case the daemon can inflict in one go: a
// backend fallback restart and a tuning-degrade restart, one each. A
// bound rather than an unbounded free pass, so an engine that restarts
// forever still reaches an honest verdict instead of retrying forever.
const enginePullBounceGrace = 2

var modelPullBackoff = 15 * time.Second

// engineProcessGen reads the ollama adapter's process generation, nil
// safely. The unit fixtures that construct a provider without an adapter
// get a constant 0, so the generation never appears to move and their
// pulls are charged exactly as they were before #359.
func (p *agentInferenceProvider) engineProcessGen() uint64 {
	if p.ollama == nil {
		return 0
	}
	return p.ollama.ProcessGeneration()
}

// pullDiagnosticMax bounds one captured line. The scanner behind
// `ollama pull` admits a 1 MiB token, and a pull can run for hours, so
// the capture is clamped where it happens rather than where it is read.
// Same budget as clampSetupDetail, because that is where this text ends
// up on the wire.
const pullDiagnosticMax = setupDetailMax

// pullDiagnostic captures the line `ollama pull` printed to explain
// itself.
//
// The engine reports its real diagnosis on stderr and then exits with a
// bare status, so Pull returns cmd.Wait()'s *exec.ExitError — literally
// "exit status 1" — and everything downstream was classifying that.
// The explanation was already reaching this process (parseProgressLine
// hands it over as a StateUnknown Progress with the raw line in
// Message); dlProgress.observe drops it, correctly, because it carries
// no layer digest. This keeps a copy (#307).
//
// Only lines that ANNOUNCE an error are kept, not every line the parser
// left unrecognised. Ollama renders its progress bar by overwriting one
// line, and a redraw that arrives with its cursor-control prefix intact
// no longer starts with "pulling" — so "the last unrecognised line"
// would routinely be a progress fragment masking the real cause. Every
// byte kept here is also a byte published to the control plane.
//
// The prefix is the whole test, with no State check beside it: a line
// beginning "error" matches none of parseProgressLine's positive arms,
// so it is always StateUnknown, and a second condition that cannot vary
// is a branch no test can hold.
//
// The mutex is not decoration. DefaultRunner scans stdout and stderr on
// two goroutines and calls onLine from both.
type pullDiagnostic struct {
	mu   sync.Mutex
	last string
}

func (d *pullDiagnostic) observe(pr download.Progress) {
	line := strings.TrimSpace(pr.Message)
	if !strings.HasPrefix(strings.ToLower(line), "error") {
		return
	}
	d.mu.Lock()
	d.last = clampUTF8(line, pullDiagnosticMax)
	d.mu.Unlock()
}

func (d *pullDiagnostic) text() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.last
}

// engineNotRunningMarker leads the failure text when the engine could
// not be brought up for the attempt that failed.
//
// An agent-owned phrase, not a scrape of somebody else's prose: it is
// what classifyModelPullFailure keys on to answer engine_not_ready, so
// the two sides must move together or the wizard silently falls back to
// "check its internet connection". The wizard also shows this string, so
// it names the engine the way every other user-facing surface does.
const engineNotRunningMarker = "the inference engine on this device was not running"

// pullFailureText builds the failure a model row records.
//
// The pull's own error is always kept — it is the only thing that is
// certainly about this attempt — and the two explanations are added when
// there are any: the engine's inability to start, then whatever the CLI
// printed on its way out.
//
// engineErr leads because it is the earliest cause in the chain, and it
// is only ever set when EnsureRunning failed on THIS attempt. The
// conjunction is deliberate: EnsureRunning failing does not by itself
// mean the pull is doomed — an Ollama busy loading a 40 GB model fails
// the readiness probe while still serving downloads perfectly well,
// and a foreign engine holding the port answers pulls too. Only
// when both halves of the same attempt failed is the engine named, and
// even then the other two clauses stay so nothing is lost if the
// attribution is wrong.
func pullFailureText(err error, diag, engineErr string) string {
	if err == nil {
		return ""
	}
	parts := make([]string, 0, 3)
	if engineErr != "" {
		parts = append(parts, engineNotRunningMarker+": "+engineErr)
	}
	parts = append(parts, err.Error())
	if diag != "" {
		parts = append(parts, diag)
	}
	return strings.Join(parts, "; ")
}

// upgradeBlindVariant re-runs variant selection for a job whose variant
// was chosen while the engine version was unknown, and reports the
// better variant when there is one.
//
// It CANNOT fail the job, and that is why the refusal path above stays
// synchronous. An unfloored variant satisfies the gate at any version,
// so the set of variants a KNOWN version admits is always a superset of
// the set an unknown one admits: re-resolving can only move the choice
// earlier in manifest order (the author's preference), never to nothing.
// Every "leave it alone" case below is therefore a return, not an error
// — including the one that should be unreachable.
//
// Known limitation: an engine whose listening server is a different
// build from the binary on disk is not covered, because a version
// measured from that binary makes the choice non-blind and this is
// skipped. Reaching it needs the server to be unreachable at dispatch
// AND the on-disk binary to be a different version; the live
// /api/version wins whenever it exists.
func (p *agentInferenceProvider) upgradeBlindVariant(
	ctx context.Context, manifest catalog.Manifest, jobID, current string,
) (catalog.Variant, bool) {
	engine := p.servingEngine()
	engineVersion := p.engineVersionFor(ctx, engine)
	if engineVersion == "" {
		return catalog.Variant{}, false // still unknown: keep the blind choice
	}
	variant, pullable := router.FirstPullableVariant(manifest, engine, engineVersion)
	if !pullable || variant.VariantID == current {
		return catalog.Variant{}, false
	}
	p.logger.Info("pull upgraded the variant once the engine reported its version",
		"model", manifest.ModelID, "was", current, "now", variant.VariantID,
		"engine", engine, "engine_version", engineVersion, "job", jobID)

	// Correct the row PullModel stamped with the blind choice, so nothing
	// reads a tag this job is no longer fetching. Same #614 rule as that
	// pre-flight write: a model that is Ready is serving from its on-disk
	// blobs and must not be moved by a pull that has not finished. The
	// variant test also drops out of a race with a concurrent mover.
	if err := p.store.Update(func(s *catalog.State) {
		m := s.Models[manifest.ModelID]
		if m.State == catalog.ModelStateReady || m.VariantID != current {
			return
		}
		m.VariantID = variant.VariantID
		m.OllamaTag = variant.Source.Tag
		m.BaseOllamaTag = "" // a derived model built from the old variant is void
		s.Models[manifest.ModelID] = m
	}); err != nil {
		p.logger.Warn("recording the upgraded variant failed; the download proceeds on the new tag",
			"model", manifest.ModelID, "err", err)
	}

	// Republish the in-flight entry so a joining dispatcher compares
	// against the tag actually being fetched. A REPLACEMENT rather than a
	// write-through: pullJob is immutable once published precisely so
	// joiners can read it without holding pullMu.
	p.pullMu.Lock()
	if cur, ok := p.pullsInFlight[manifest.ModelID]; ok && cur != nil && cur.jobID == jobID {
		p.pullsInFlight[manifest.ModelID] = &pullJob{
			jobID: cur.jobID, modelID: cur.modelID,
			variantID: variant.VariantID, tag: variant.Source.Tag,
			// The SAME stop half, not a fresh one: a cancel that reaches
			// the replacement has to arrive at the context the download
			// is actually running on.
			stop: cur.stop,
		}
	}
	p.pullMu.Unlock()
	return variant, true
}

// runPullJob downloads one model.
//
// TWO contexts, and the split is load-bearing:
//
//   - ctx MUST be the daemon's long-lived context — PullModel dispatches
//     on backgroundCtx(), never on a request ctx (#305a: net/http cancels
//     the handler's context the microsecond the 202 is written, which
//     killed every `waired models pull`). EnsureRunning below can win the
//     single-flight leader race and spawn `ollama serve` through
//     exec.CommandContext, so it gets THIS one: a pull that cancelled the
//     engine's context would kill the engine it just started (#305/R0 — a
//     regression from #304's EnsureRunning join).
//   - dlCtx is a cancellable child of it that bounds the DOWNLOAD alone.
//     CancelPull cancels it. Nothing that must outlive the job may be
//     started on it.
//
// Neither is re-wrapped here in a WithCancel this function cancels on
// return — that is the same hazard, one layer down.
//
// job is a VALUE: the pointer PullModel published under pullMu stays
// immutable for joining callers, and this function's own upgrade of a
// blindly-resolved variant (#361) republishes a replacement rather than
// writing through. Its stop half is a pointer, so the copy reads the same
// cancellation flag the canceller wrote. manifest is carried so that
// upgrade can re-run the selection without a second alias lookup.
func (p *agentInferenceProvider) runPullJob(ctx, dlCtx context.Context, job pullJob, manifest catalog.Manifest) {
	modelID, jobID := job.modelID, job.jobID
	variantID, tag := job.variantID, job.tag
	p.recordPullState(modelID, catalog.ModelStateDownloading, "")

	// Forget live progress once the pull terminates (success or failure)
	// so a finished/failed model never lingers as a stale "downloading".
	defer p.dlProgress.forget(modelID)
	var err error
	// failure is what the model row records, which is NOT err.Error():
	// see pullFailureText. Assigned every attempt so the text describes
	// the attempt that produced it.
	var failure string
	// charged counts the attempts this DOWNLOAD is answerable for. An
	// attempt the engine was restarted out from under is not one of them,
	// and gets a free retry off bounceGrace instead (#359).
	charged := 0
	bounceGrace := enginePullBounceGrace
	for {
		// #304: `ollama pull` is a CLIENT of the serving engine. Setup
		// admission keys off a stat of the binary, which flips true seconds
		// before `ollama serve` is listening; the pull then dies on
		// connection-refused. EnsureRunning is single-flight and returns
		// immediately when already ready, so this JOINS whatever start is in
		// flight rather than adding one. A parked or given-up engine returns
		// its sentinel WITHOUT spawning: log it and let the pull report the
		// real error, exactly as before. Inside the loop because retrying a
		// download against an engine that has since died is pointless.
		//
		// engineErr is remembered rather than only logged (#307): when
		// this attempt then fails, it is the most specific thing anyone
		// knows about why, and the pull's own "exit status 1" cannot say
		// it. Scoped to the attempt — a cold start that timed out once
		// must not be blamed for a network failure two attempts later.
		var engineErr string
		if p.ollama != nil && p.servingEngine() == catalog.RuntimeOllama {
			if ensureErr := p.ollama.EnsureRunning(ctx); ensureErr != nil {
				p.logger.Warn("engine not ready before pull", "model", modelID, "tag", tag, "err", ensureErr)
				engineErr = ensureErr.Error()
			}
		}
		// #361: the join above is the first moment the engine's version is
		// knowable from the engine itself, and PullModel had to choose a
		// variant before it. If that choice was made blind, take it again
		// now — before a single byte is fetched, so the correction is free.
		// Once per job: a later attempt's engine is the same engine.
		if job.resolvedBlind && engineErr == "" {
			job.resolvedBlind = false
			// dlCtx: a version probe belongs to this download and must
			// not outlive a cancel.
			if v, ok := p.upgradeBlindVariant(dlCtx, manifest, jobID, variantID); ok {
				variantID, tag = v.VariantID, v.Source.Tag
			}
		}
		// The one gate before a byte is fetched. A cancel that landed
		// while EnsureRunning was joining an engine start must not be
		// followed by a multi-GB download.
		if job.requestedStop() {
			break
		}
		// Sampled AFTER EnsureRunning on purpose: that call reaps a dead
		// child and respawns it, which moves the generation for a reason
		// this download did not cause and has already waited out.
		gen := p.engineProcessGen()
		// Fresh per attempt: attempt 1's transient must never be reported
		// as attempt 3's cause.
		var diag pullDiagnostic
		err = p.puller.Pull(dlCtx, tag, func(pr download.Progress) {
			p.dlProgress.observe(modelID, pr)
			if pr.State == download.StateVerifying {
				p.recordPullState(modelID, catalog.ModelStateVerifying, "")
			}
			diag.observe(pr)
		})
		// WE stopped the engine this pull was talking to (#359). Four
		// paths do it — a CP capacity retune, crash recovery, an operator's
		// model switch whose weights were already on disk, and the boot
		// tail's backend probe / tuning verify — and a download that dies
		// to one of them owes nothing: it is not charged an attempt, its
		// error text is not recorded, and no backoff is served, because the
		// wait that belongs here is the next iteration's EnsureRunning
		// joining the restart.
		//
		// Counting our own stops rather than classifying the error is what
		// makes this hold for a bounce added later: every one of them goes
		// through the adapter. `ollama pull` surfaces a killed engine as a
		// bare non-zero exit, so there is no error text to key on anyway.
		// An operator cancel ends the job here, ahead of the engine-bounce
		// grace: a cancelled download is not one an engine restart owes a
		// free retry to, and the flag is read rather than the error
		// because a killed `ollama pull` is indistinguishable from an OOM
		// kill (see pullStop).
		if job.requestedStop() {
			break
		}
		if err != nil && bounceGrace > 0 && p.engineProcessGen() != gen {
			bounceGrace--
			p.logger.Info("model download interrupted by an engine restart; retrying without charging the attempt",
				"model", modelID, "tag", tag, "grace_left", bounceGrace)
			// Shutdown still ends the job — see the same gate below.
			if ctx.Err() != nil {
				failure = pullFailureText(err, diag.text(), engineErr)
				break
			}
			continue
		}
		charged++
		failure = pullFailureText(err, diag.text(), engineErr)
		// A full disk cannot clear itself; three more multi-GB attempts only
		// delay the honest error the wizard needs to show.
		//
		// Read from `failure`, not from err.Error(): the marker is on the
		// engine's stderr and never in the exit status, so keying this on
		// the returned error alone made it dead code for every real pull.
		if err == nil || charged >= modelPullAttempts || isDiskFullText(failure) {
			break
		}
		p.logger.Warn("ollama pull failed; retrying",
			"model", modelID, "tag", tag, "attempt", charged, "max", modelPullAttempts, "err", failure)
		select {
		case <-time.After(time.Duration(charged) * modelPullBackoff):
		case <-dlCtx.Done():
		}
		// The single gate on starting another attempt. Shutdown ends the
		// job rather than restarting it: agentCtx is cancelled on SIGTERM
		// and session teardown, nothing is waiting for the result, and the
		// failure the cancelled Pull returned is the one to record.
		if ctx.Err() != nil {
			break
		}
	}
	// A cancelled job records nothing. "failed" would be a wrong answer —
	// nothing failed, someone asked it to stop — and settleCancelledPull
	// is about to drop the row anyway, so writing one here only makes the
	// log read like a fault.
	if job.requestedStop() {
		return
	}
	if err != nil {
		p.logger.Warn("ollama pull failed", "model", modelID, "tag", tag, "err", failure)
		p.recordPullState(modelID, catalog.ModelStateFailed, failure)
		return
	}
	_ = p.store.Update(func(s *catalog.State) {
		m := s.Models[modelID]
		// Record the variant this job actually fetched. The pre-flight
		// write skips a model that was already ready, so without this a
		// refresh pull that resolved a NEW variant downloaded the new
		// blobs and left state pointing at the old tag — which is the tag
		// the gateway puts on the wire and the mesh advertises (#305).
		// Guarded on a change so a derived batch tag written mid-pull
		// (#642) is not stomped by the base tag we were asked for.
		if m.VariantID != variantID {
			m.VariantID = variantID
			m.OllamaTag = tag
			// A derived model built from the OLD variant is void.
			m.BaseOllamaTag = ""
		}
		m.State = catalog.ModelStateReady
		m.Error = ""
		m.PulledAt = time.Now().UTC()
		s.Models[modelID] = m

		epID := "ep_local_ollama_" + sanitiseModelID(modelID)
		s.Endpoints[epID] = catalog.EndpointState{
			Runtime:   catalog.RuntimeOllama,
			ModelID:   modelID,
			VariantID: variantID,
			State:     "ready",
			Since:     time.Now().UTC(),
		}
	})
	p.logger.Info("ollama pull completed", "model", modelID, "tag", tag, "job", jobID)
	// First-run activation: if the model that just became ready is the
	// bundled one and nothing is active yet, commit it (a fresh install has
	// no ActiveSelection). Guarded to the bundled model so an unrelated
	// `waired models pull` can't hijack the active slot. See
	// activateBundledIfUnset.
	servedBefore := p.activeModelID()
	if p.isBundledModel(modelID) {
		p.activateBundledIfUnset(modelID, variantID)
	}
	// Preferred-model switch: when the model that just became ready is
	// the operator's chosen one, commit it as Active. Before this the
	// switch never landed — nothing wrote Active after the restart, so
	// the agent came back up serving the old model (issue #347).
	p.activatePreferredIfNeeded(modelID, variantID)
	// A model that BECAME what this host serves, right here, is one no
	// benchmark has seen. That is the takeover path's ending: init handed
	// the download over and exited, so the only result on file belongs to
	// whatever was serving before, and every asking surface reads it as
	// this model's (waired-ai/waired-agent#783).
	//
	// A TRANSITION, not a state. "This pull's model is the active one" was
	// not enough: pre-caching a better variant of the model already served
	// (#361) satisfies it while changing nothing about what answers
	// requests. Reading the selection either side of the two activation
	// arms is what tells the two apart. Scoped to the model — activation
	// never swaps a variant under an unchanged model id, so there is no
	// same-model-new-variant case to catch here.
	//
	// Started and NOT waited for, and that part is load-bearing rather
	// than convenience: endPull is one of this function's deferred calls,
	// so this pull is still in pullsInFlight right here — and
	// engineIsQuiet answers false while any pull is. Blocking on the run
	// would have it wait for a quiet engine that cannot go quiet until
	// this call returns. The job's own gates handle the ordering instead:
	// by the time it has settled, the defers have run.
	if servedBefore != modelID && p.activeModelID() == modelID {
		_ = p.remeasureForActiveModel(modelID)
	}
	// #320: the serve tuning was sized before this model existed on disk.
	// resolveTuningTarget only reads the real variant once the model is
	// Ready, so until this point the engine has been running on a guess —
	// on a fresh install, one made against a model that had not been
	// downloaded yet. Ask for a re-resolve now that Ready is written.
	//
	// Unconditional, and deliberately not conditioned on "is this the
	// serving model": that question is resolveTuningTarget's, and asking
	// it here would duplicate its precedence rules (preferred, then
	// Active, then bundled) at a second site that could drift from it.
	// The reconcile's ServeInputsEqual test makes a wrong guess here free.
	p.retuneDeferred.Store(true)
	// #812: if this pull completed an operator's in-process model switch
	// (SwapPreferredModel recorded pendingSwapModel while the weights
	// downloaded), bounce the engine now so the new model's per-model serve
	// env applies — the same in-process swap the on-disk path takes, just
	// deferred until the download finished. Boot-time / unrelated pulls never
	// set pendingSwapModel, so they don't trigger a spurious bounce.
	if psm := p.pendingSwapModel.Load(); psm != nil && *psm == modelID {
		p.pendingSwapModel.CompareAndSwap(psm, nil)
		// Record the intent; do not bounce here. `ollama pull` is a CLIENT
		// of `ollama serve`, so stopping the engine makes a SIBLING model's
		// download exit non-zero and records THAT model failed (#305d).
		// endPull fires it once no pull is left in flight.
		p.swapBounceDeferred.Store(true)
	}
}

// activateBundledIfReady commits the bundled model as Active when its
// weights are already on disk AND the engine is really serving that tag,
// reporting whether it did. A fresh install pre-pulls the bundled model
// during `waired init` (setup.Deploy), so the agent can reach here with
// the model Ready but no ActiveSelection — committing it is what lets the
// subsystem leave "awaiting_model". See activateBundledIfUnset.
//
// Split off from the startup pre-pull because the two halves have
// different owners now (#306): the pre-pull is skipped whenever the
// operator's own model took responsibility, but this half must still run
// — it is the only caller of activateBundledIfUnset on the boot path, and
// skipping it would leave Active nil for the hours the chosen model
// downloads, on a host with a perfectly good model already on disk.
func (p *agentInferenceProvider) activateBundledIfReady(ctx context.Context) bool {
	modelID := p.bundledModelID()
	if modelID == "" {
		return false
	}
	cur := p.bundledModelState(modelID)
	if cur.State != catalog.ModelStateReady || !p.engineServesTag(ctx, cur.OllamaTag) {
		return false
	}
	p.activateBundledIfUnset(modelID, cur.VariantID)
	return true
}

// bundledModelID is the CANONICAL catalog id cfg.BundledModelID names.
//
// The configured value accepts any catalog alias — `qwen2.5-coder-14b`,
// `Qwen/Qwen2.5-Coder-14B-Instruct` — while every id the pull path writes
// (state.Models keys, models.ready, the PullModel argument) is the
// canonical manifest.ModelID. Resolving once, here, is what keeps the two
// ends of that comparison the same kind of string (#380).
//
// A RETIRED name resolves to its successor (#200): the value came out of
// a config file written before the entry went away, and the alternative
// is a host that pre-pulls nothing and reports a model this build no
// longer ships. Logged once, because this is the pin the operator chose
// and they should be able to find out it moved.
//
// An unresolvable value is returned unchanged, which degrades to exactly
// the comparison the caller would have made anyway; "" means no bundled
// model is configured, which is a real state now that there is no
// compiled-in default.
func (p *agentInferenceProvider) bundledModelID() string {
	if p.cfg.BundledModelID == "" {
		return ""
	}
	m, retired, ok := catalog.ResolveModel(p.cfg.BundledModelID, p.manifests)
	if !ok || m.ModelID == "" {
		return p.cfg.BundledModelID
	}
	if retired.SuccessorModelID != "" {
		p.logBundledRetirementOnce(retired)
	}
	return m.ModelID
}

// logBundledRetirementOnce reports the bundled pin's migration the first
// time it is resolved. bundledModelID is called on every pull, activation
// and status read, so an unguarded line here would be one per request.
func (p *agentInferenceProvider) logBundledRetirementOnce(r catalog.Retirement) {
	p.bundledRetirementLogged.Do(func() {
		p.logger.Info("configured bundled model was retired; using its successor",
			"configured", p.cfg.BundledModelID,
			"model", r.SuccessorModelID,
			"reason", r.Reason)
	})
}

// isBundledModel reports whether modelID — a canonical id, as written by
// the pull path — is the model cfg.BundledModelID names. False when
// nothing is configured, so an unset default cannot claim a pull.
func (p *agentInferenceProvider) isBundledModel(modelID string) bool {
	bundled := p.bundledModelID()
	return bundled != "" && modelID == bundled
}

// bundledModelState is the stored catalog row for modelID, or the zero
// value when the store is unreadable.
func (p *agentInferenceProvider) bundledModelState(modelID string) catalog.ModelState {
	state, _ := p.store.Load()
	return state.Models[modelID]
}

// The agent-startup pre-pull of spec waired_inference_spec.md §11.1 (a
// background download, so inference requests succeed without the user
// invoking `waired models pull`) is the next two functions. It used to be
// one — bootstrapBundledModel — until #379 put the setup hold between the
// decision and the download; bootstrapAfterEngineStart has driven the two
// halves directly ever since, and the old driver was removed once nothing
// but a test was left calling it (#542).

// bundledPrePullTarget answers which model the startup pre-pull would
// download, and false when there is nothing to download. Everything it
// does is local and cheap, and the boot path runs it SYNCHRONOUSLY even
// when the dispatch itself is held back (#379): its already-ready arm
// commits the Active selection, and deferring that would leave a host
// whose weights are right there on disk with Active nil — EngineReady()
// false, benchmarks 425ing, Status() reporting awaiting_model — for as
// long as the hold lasts.
func (p *agentInferenceProvider) bundledPrePullTarget(ctx context.Context) (string, bool) {
	if p.cfg.BundledModelID == "" {
		// Not an error: there is no compiled-in default, so "unset" is
		// what a host whose selection has not run — or one that had
		// nothing to select — legitimately looks like. Info, not Warn:
		// nothing is broken and nothing needs a model invented for it.
		p.logger.Info("no bundled model configured; skipping pre-pull")
		return "", false
	}
	if p.noModelSelected.Load() {
		// The operator chose to run without a local model (#586). A
		// choice, not a fault, and Info for the same reason as above: the
		// engine stays up, and picking a model later re-enters through
		// /preferred-model, which clears this.
		p.logger.Info("the operator chose to run without a local model; skipping the bundled pre-pull")
		return "", false
	}
	if p.modelQuestionUnanswered.Load() {
		// The model question was asked on some boot and nobody answered
		// (#586; owner ruling 2026-08-09). An abandoned question is not
		// consent, so the fallback stays down until someone chooses —
		// through the browser dashboard, `waired models pull`, or a
		// re-run `waired init`.
		p.logger.Info("the install flow asked which model to download and nobody answered; the bundled pre-pull stays down until someone chooses")
		return "", false
	}
	// Through bundledModelID, not a second LookupByAlias: the pre-pull and
	// everything that later asks "is this the bundled model" have to
	// resolve the configured value identically, and only one of the two
	// used to know about retirements.
	modelID := p.bundledModelID()
	if _, ok := catalog.LookupByAlias(modelID, p.manifests); !ok {
		p.logger.Warn("bundled model not found in manifests; skipping pre-pull", "model", p.cfg.BundledModelID)
		return "", false
	}
	if p.activateBundledIfReady(ctx) {
		p.logger.Info("bundled model already ready; skipping pre-pull", "model", modelID)
		return "", false
	}
	// #338 moved the allow_pull refusal off the engine start and onto the
	// dispatchers. BELOW the activation above, on purpose: a host that
	// downloads nothing still has to commit weights that are already
	// there, which is the whole point of the move — and the comment on
	// this function says what deferring that costs.
	//
	// PullModel refuses again on its own, so this is not what makes the
	// download not happen. Answering false here is what keeps the boot
	// from registering a hold on pullsWG and parking it for prePullHoldMax
	// only to be refused, and from logging a dispatch failure every boot
	// for a setting that is doing exactly what it was asked to. Re-taken
	// at hold release through prePullStillWanted, so a config that changed
	// meanwhile is honoured there too.
	if !p.cfg.AllowPull {
		p.logger.Info("pulls are disabled (allow_pull=false); skipping the bundled pre-pull",
			"model", modelID)
		return "", false
	}
	// The second reason not to download, in the same place as the first
	// and for the same reason (#526). It used to sit at the boot caller,
	// wrapped around the whole call — so it suppressed the activation
	// above as well as the download, on exactly the hosts that had
	// weights to activate: applyBundledSelection turns pull_on_startup
	// off on the install-time selector's disk-short verdict.
	//
	// Every caller of this function is the startup pre-pull the setting
	// is named for — the boot fallback arm, and prePullStillWanted
	// re-taking the decision when the #379 hold releases — so there is no
	// caller for which reading it here is wrong.
	if !p.cfg.PullOnStartup {
		p.logger.Info("startup pull is disabled (pull_on_startup=false); skipping the bundled pre-pull",
			"model", modelID)
		return "", false
	}
	return modelID, true
}

// dispatchBundledPrePull starts the fallback download. Separate from the
// decision above so the hold in inference_prepull_hold.go can re-take the
// decision when it releases, minutes after the boot that made it.
func (p *agentInferenceProvider) dispatchBundledPrePull(ctx context.Context, modelID string) {
	if _, err := p.PullModel(ctx, modelID); err != nil {
		p.logger.Warn("bundled model pre-pull dispatch failed", "model", modelID, "err", err)
	}
}

// engineServesTag reports whether the serving engine's own store holds
// the tag (GET /api/tags). state.json's ModelStateReady alone is not
// proof: after the 9475 port/store cutover (or any OLLAMA_MODELS
// change) the record describes the OLD store, while the engine now
// reads an empty one. Errors and unknown tags return false so the
// caller falls through to PullModel — a pull over existing blobs is a
// fast no-op, while skipping a needed pull leaves the model 404ing.
// Empty tags return true (nothing meaningful to verify).
func (p *agentInferenceProvider) engineServesTag(ctx context.Context, tag string) bool {
	if tag == "" || p.ollama == nil {
		return true
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, p.ollama.BaseURL()+"/api/tags", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false
	}
	var body struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false
	}
	for _, m := range body.Models {
		if m.Name == tag {
			return true
		}
	}
	return false
}

// activateBundledIfUnset commits the bundled model as the ActiveSelection
// when none is set yet. A fresh install (state.json born at the current
// version) never gets an ActiveSelection: MigrateInPlace only synthesises
// one on a v1→v2 carry-over, and nothing else commits one at boot. Without
// this the agent stays in subsystem_state "awaiting_model" forever —
// EngineReady() is false, so engineModelForActive() is empty (the boot
// benchmark POSTs no model and 400s, /inference/benchmark 425s, Capacity
// falls back to 1) even though the engine serves requests on demand. An
// explicit preferred-model or an update-swap still overrides this (we only
// fill the gap when Active is nil). Idempotent.
func (p *agentInferenceProvider) activateBundledIfUnset(modelID, variantID string) {
	decidedBy, reason := bundledActivationRecord(p.operatorChosenModelID(), modelID)
	committed := false
	if err := p.store.Update(func(s *catalog.State) {
		if s.Active != nil {
			return
		}
		ms, ok := s.Models[modelID]
		if !ok || ms.State != catalog.ModelStateReady {
			return
		}
		if variantID == "" {
			variantID = ms.VariantID
		}
		s.Active = &catalog.ActiveSelection{
			Runtime:        p.servingEngine(),
			ModelID:        modelID,
			VariantID:      variantID,
			DecidedAt:      time.Now().UTC(),
			DecidedBy:      decidedBy,
			DecisionReason: []string{reason},
		}
		committed = true
	}); err != nil {
		p.logger.Warn("auto-activate bundled model failed", "model", modelID, "err", err)
		return
	}
	if committed {
		p.logger.Info("auto-activated bundled model", "model", modelID, "variant", variantID,
			"decided_by", decidedBy)
	}
}

// remeasureForActiveModel measures the model that just became the active
// selection, unless one already on file measured it.
//
// The floor check is not a daemon-side decision — nothing here may step a
// host down, because what is missing for that is consent and the daemon
// cannot ask (the same reasoning setupReconciler's model step states). What
// the daemon CAN do is make sure the number the asking surfaces read
// describes the model actually serving. Until now the only measurement was
// the one taken at boot: activate a model afterwards and every consumer —
// `waired runtimes status`, the tray, the next `waired init` — compared the
// new model against the old model's rate, or against nothing at all.
//
// Detached and single-flight (startBenchmarkJob), so a run already going —
// the boot benchmark on a fresh install, or one `waired init` asked for —
// is joined rather than duplicated, and its own gates (EngineReady,
// EngineQuiet, EngineClaim) still decide whether it may proceed.
// activeModelID is the committed active selection read from THIS
// provider's store. modelIDForActive answers the same question from the
// default state path, which is what the benchmark deps are built from;
// this one is for callers that already hold the store.
func (p *agentInferenceProvider) activeModelID() string {
	if p.store == nil {
		return ""
	}
	st, err := p.store.Load()
	if err != nil || st.Active == nil {
		return ""
	}
	return st.Active.ModelID
}

// # It waits for a quiet engine, on a goroutine of its own
//
// The trigger fires from runPullJob's tail, which is the ONE moment the
// engine cannot be measured: endPull is one of that function's DEFERRED
// calls, so the pull is still in pullsInFlight and engineIsQuiet answers
// false for it. Starting the job there and walking away spent the single
// attempt on a gate that was always going to decline, and the model this
// host had just activated stayed unmeasured — the state this whole function
// exists to end (waired-agent#821, seen on the browser-takeover path).
//
// Retrying at the endPull boundary instead would not have been enough
// either: runPullJob stores retuneDeferred unconditionally, so endPull
// always fires a serve reconcile, and engineIsQuiet counts a PENDING
// reconcile as busy for the same reason it counts a running one. The window
// closes some time after that boundary, not at it.
//
// So the wait is real, and it lives on a NEW goroutine. What is
// load-bearing is that runPullJob does not block: blocking there would make
// the wait depend on the defers of the very call it is blocking, which is
// the deadlock TestRunPullJob_ReMeasuresTheModelItJustMadeActive exists to
// catch. Past that boundary there is nothing left to deadlock against, so
// this may wait the way every other measurement on this host already does
// (startHostSpeedMeasurement, awaitScreenQuiet).
//
// It returns a channel closed when the whole attempt has finished, or nil
// when it started nothing. Callers in the daemon ignore it — the point is
// that the attempt is detached — but a test that does not wait leaves a
// goroutine writing into its temp directory after it has returned.
func (p *agentInferenceProvider) remeasureForActiveModel(modelID string) <-chan struct{} {
	// No profiler means nothing here can run a benchmark: runBenchmarkJob
	// reads the hardware profile before it reaches its own gates. A
	// provider assembled without one is not a host that should be measured
	// (--disable-inference, and the narrow providers in tests).
	if modelID == "" || p.profiler == nil {
		return nil
	}
	if !p.activeModelNeedsMeasurement(modelID) {
		return nil
	}
	p.logger.Info("benchmarking the newly active model", "model", modelID)
	// The daemon's own context, so a shutdown ends the wait. Nil in the
	// narrow test providers — the same fallback requestEngineReconcile
	// makes for the same reason.
	ctx := p.agentCtx
	if ctx == nil {
		ctx = context.Background()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.remeasureWhenQuiet(ctx, modelID)
	}()
	return done
}

// activeModelNeedsMeasurement reports whether this host still lacks a
// measurement of its own of modelID.
//
// Only a real measurement OF THIS MODEL answers false. Nothing on file, a
// skipped run (Capacity 0), a failed one — which is what a run declined at
// the engine gates leaves behind — an unlabelled one from a build predating
// BenchResult.ModelID, or one of another model all leave this host's actual
// model unmeasured. Note this is stricter than benchDescribes, which answers
// a different question: whether an existing result may still be USED, where
// an unlabelled one is kept rather than discarded.
//
// It is both the entry test and the success test of the retry loop below,
// so "did the run work" is answered by the same predicate that decided to
// start one, rather than by a second reading that could drift from it.
func (p *agentInferenceProvider) activeModelNeedsMeasurement(modelID string) bool {
	p.benchMu.Lock()
	last := p.lastBench
	p.benchMu.Unlock()
	return last == nil || last.Failed || last.Capacity <= 0 || last.ModelID != modelID
}

// stillWantsRemeasure is the retry loop's "is this attempt still worth
// making" test: modelID is what this host serves, and nothing has measured
// it yet.
//
// Both halves can go false while the loop waits, and they mean different
// things when they do. A changed selection means the work belongs to
// whatever activation replaced it, which fires its own trigger; a
// measurement appearing means someone else — the boot benchmark, or a run
// `waired init` asked for — got there first and this attempt would only
// re-measure what is already on file.
func (p *agentInferenceProvider) stillWantsRemeasure(modelID string) bool {
	return p.activeModelID() == modelID && p.activeModelNeedsMeasurement(modelID)
}

// remeasureWhenQuiet is the retry loop behind remeasureForActiveModel: wait
// for an engine nothing else is using, start the single-flight benchmark,
// and try again if the run it started (or joined) was declined anyway.
//
// The retry is not redundant with the wait. The two ask the same question at
// two different moments — benchQuietNow can answer yes about an engine that
// a request, a sibling pull or a reconcile takes away before
// RunBootBenchmark re-asks — and it is that second reading, not the first,
// that decides whether anything gets measured.
//
// Bounded by remeasureTiming.window from the first attempt, so a host that
// never goes quiet gives up and says so once instead of spinning. Three
// ways to stop before that, all silent: the model is no longer what this
// host serves (whatever replaced it brings its own trigger), a measurement
// of it appeared (the boot benchmark, or one `waired init` asked for), or
// the daemon is shutting down.
func (p *agentInferenceProvider) remeasureWhenQuiet(ctx context.Context, modelID string) {
	t := p.remeasureTimers()
	deadline := time.Now().Add(t.window)
	declined := 0
	for {
		// Asked on EVERY pass, not only at the top and after a wait. The
		// loop can run for minutes, both halves can go false while it
		// does, and this is also what ends the goroutine promptly when the
		// provider it belongs to is torn down — a wait loop that only
		// re-asked on either side would go on polling a dead engine for
		// the rest of the window.
		if !p.stillWantsRemeasure(modelID) {
			return
		}
		if time.Now().After(deadline) {
			p.logger.Warn("the newly active model stays unmeasured",
				"model", modelID, "waited", t.window, "declined_runs", declined)
			return
		}
		if !p.benchQuietNow(ctx) {
			select {
			case <-time.After(min(t.poll, time.Until(deadline))):
			case <-ctx.Done():
				return
			}
			continue
		}
		select {
		case <-p.startBenchmarkJob(0):
		case <-ctx.Done():
			return
		}
		if !p.activeModelNeedsMeasurement(modelID) {
			return
		}
		// Declined after this loop had just read the engine as quiet. The
		// two readings are taken at different moments, and the job also
		// gates on things this loop does not test — EngineReady above all
		// — so the pause is what keeps that disagreement from spinning
		// through the whole window.
		declined++
		p.logger.Info("the benchmark of the newly active model was declined; retrying once the engine is quiet",
			"model", modelID, "attempt", declined)
		select {
		case <-time.After(min(t.retry, time.Until(deadline))):
		case <-ctx.Done():
			return
		}
	}
}

// bundledActivationRecord is how the gap-filling activation above records
// itself, given the model a person at this machine has chosen ("" when
// nobody has).
//
// It exists because that activation used to claim "no prior selection"
// unconditionally, having read nothing that could tell. On the browser
// takeover path the operator's pick IS the bundled model — the picker
// preselects the recommended one, which is what BundledModelID names — so
// the common case wrote decided_by "auto" over a preference recorded
// seconds earlier, and activatePreferredIfNeeded then found the ids equal
// and stood down (waired-ai/waired-agent#783).
//
// The same-model arm deliberately reuses activatePreferredIfNeeded's own
// wording: whichever of the two commits it, the operator's choice is
// recorded the same way. The third arm is the case the boot path exists
// for — serving something while the chosen model downloads
// (activateBundledIfReady) — which is a real auto decision, just not one
// made in the absence of a selection.
func bundledActivationRecord(chosenModelID, modelID string) (decidedBy, reason string) {
	switch chosenModelID {
	case "":
		return "auto", "bundled model auto-activated on first run (no prior selection)"
	case modelID:
		return "user", "preferred-model switch applied (model ready)"
	default:
		return "auto", "bundled model activated while the chosen model is not ready"
	}
}

// operatorChosenModelID is the model a person at THIS machine has chosen,
// or "" when nobody has.
//
// It reads preferred-model.json live rather than the boot cfg snapshot,
// for the reason LocalModelChoiceAt states: the answer can arrive at any
// time through the loopback management API, and on the path this exists
// for it arrives ninety seconds before the activation it has to inform.
//
// A record with no provenance answers "" — the file predates Source, so
// it carries no claim either way, and inventing one here would be the
// mirror of the bug (waired-ai/waired-agent#647).
func (p *agentInferenceProvider) operatorChosenModelID() string {
	if p.preferencePath == "" {
		return ""
	}
	pref, ok, err := agentconfig.LoadPreference(p.preferencePath)
	if err != nil || !ok || !pref.ChosenHere() {
		return ""
	}
	return pref.ModelID
}

// preferredManifest resolves cfg.PreferredModelID (a model_id from
// preferred-model.json, or any catalog alias when set via flag/env)
// against the bundled manifests. ok=false when no preference is set
// or it names nothing in the catalog.
//
// A retired name resolves to its successor (#200). preferred-model.json
// is written once and read on every boot thereafter, so the file long
// outlives the catalog it was written against; leaving the miss here
// would strand the operator's own choice with no path back.
func (p *agentInferenceProvider) preferredManifest() (catalog.Manifest, bool) {
	pref := p.effectivePreferredModelID()
	if pref == "" {
		return catalog.Manifest{}, false
	}
	m, _, ok := catalog.ResolveModel(pref, p.manifests)
	return m, ok
}

// activatePreferredIfNeeded commits the operator's preferred model as
// the ActiveSelection once it is Ready. Unlike activateBundledIfUnset
// this DOES replace an existing Active — that is the point of the
// preferred-model switch. No-op when modelID is not the preferred
// model, the model is not Ready, or Active already points at it.
func (p *agentInferenceProvider) activatePreferredIfNeeded(modelID, variantID string) {
	manifest, ok := p.preferredManifest()
	if !ok || manifest.ModelID != modelID {
		return
	}
	committed := false
	if err := p.store.Update(func(s *catalog.State) {
		if s.Active != nil && s.Active.ModelID == modelID {
			return
		}
		ms, ok := s.Models[modelID]
		if !ok || ms.State != catalog.ModelStateReady {
			return
		}
		if variantID == "" {
			variantID = ms.VariantID
		}
		s.Active = &catalog.ActiveSelection{
			Runtime:        p.servingEngine(),
			ModelID:        modelID,
			VariantID:      variantID,
			DecidedAt:      time.Now().UTC(),
			DecidedBy:      "user",
			DecisionReason: []string{"preferred-model switch applied (model ready)"},
		}
		committed = true
	}); err != nil {
		p.logger.Warn("activate preferred model failed", "model", modelID, "err", err)
		return
	}
	if committed {
		p.logger.Info("activated preferred model", "model", modelID, "variant", variantID)
	}
}

// bootstrapPreferredModel self-heals the preferred-model switch across
// the restart it schedules: the POST kicks off a background pull, the
// SIGTERM cancels it ("download: start ollama: context canceled" in
// issue #347), and before this nothing re-pulled the chosen model on
// the next boot. It re-pulls the preferred model when it is missing, or
// commits it as Active when it is already on disk.
//
// It runs FIRST in the engine-startup goroutine (it used to run after
// the bundled pre-pull) and reports whether it took the model on:
// activated it, or dispatched its download. Only then is the bundled
// pre-pull redundant — see bootstrapAfterEngineStart (#306).
//
// The answer is deliberately "did something happen", not "is a preference
// set": a preference that merely RESOLVES in the catalog is no guarantee
// the engine can serve it, and reporting true for one would leave the
// host downloading nothing at all.
func (p *agentInferenceProvider) bootstrapPreferredModel(ctx context.Context) bool {
	manifest, ok := p.preferredManifest()
	if !ok {
		return false
	}
	state, _ := p.store.Load()
	if cur := state.Models[manifest.ModelID]; cur.State == catalog.ModelStateReady &&
		p.engineServesTag(ctx, cur.OllamaTag) {
		p.activatePreferredIfNeeded(manifest.ModelID, cur.VariantID)
		return true
	}
	// #338, the same refusal as bundledPrePullTarget's at the other
	// dispatcher. false is the honest answer and the load-bearing one: it
	// did NOT take the model on, so bootstrapAfterEngineStart falls to the
	// bundled arm, which is where activateBundledIfReady commits weights
	// that are already there. PullModel would refuse this anyway; what
	// changes is that a host configured to download nothing stops
	// reporting a failure for doing so.
	if !p.cfg.AllowPull {
		p.logger.Info("preferred model is not on disk and pulls are disabled (allow_pull=false)",
			"model", manifest.ModelID)
		return false
	}
	if _, err := p.PullModel(ctx, manifest.ModelID); err != nil {
		p.logger.Warn("preferred model re-pull dispatch failed", "model", manifest.ModelID, "err", err)
		return false
	}
	return true
}

// errSwapNeedsRestart signals that an in-process model switch is not possible
// for this target — a cross-engine change (ollama↔vLLM) or a target with no
// variant servable by the running engine — so the caller must fall back to the
// supervised restart path (WillRestart:true). It is the sentinel the
// preferred-model handler treats (like any non-nil error) as "restart to
// apply". Cross-engine in-process swap is a deferred #812 follow-up.
var errSwapNeedsRestart = errors.New("waired-agent: model switch needs restart (cross-engine)")

// SwapPreferredModel applies an operator's preferred-model switch in process
// (#812) instead of restarting the whole agent: it publishes the new preference
// as the effective source of truth, ensures the weights are on disk (dispatching
// a pull when they are not), and kicks the engine reconcile to flip Active and
// bounce `ollama serve` onto the new model — the management API, gateway, and
// mesh stay up throughout. The old model keeps serving until the new one is
// Ready. downloading reports whether a background pull was started (the switch
// then completes from runPullJob once the weights land). It returns
// errSwapNeedsRestart for a cross-engine target so the caller restart-falls-back,
// and management.ErrModelSwitchUnavailable when the weights cannot be fetched at
// all, which no restart would fix.
//
// A retired name switches to its successor (#200). This is the
// convergence keystone: the id published here is what
// setupPreferredModelID() reports, and the setup reconciler compares that
// against the control plane's desired_model_id — so both ends have to
// canonicalise the same way or the wizard waits for a string that never
// appears (see setupCanonicalModelID).
func (p *agentInferenceProvider) SwapPreferredModel(ctx context.Context, modelOrAlias string) (downloading bool, err error) {
	manifest, retired, ok := catalog.ResolveModel(modelOrAlias, p.manifests)
	if !ok {
		return false, fmt.Errorf("swap preferred model: unknown model %q", modelOrAlias)
	}
	if retired.SuccessorModelID != "" {
		p.logger.Info("model switch target was retired; switching to its successor",
			"requested", modelOrAlias, "model", manifest.ModelID)
	}
	// Same-engine only (v1): the in-process bounce restarts `ollama serve`; a
	// cross-engine switch (ollama↔vLLM) needs adapter re-registration + a
	// decision.Engine change and stays on the restart path.
	if p.servingEngine() != catalog.RuntimeOllama {
		return false, errSwapNeedsRestart
	}
	if _, pullable := router.FirstPullableVariant(manifest, catalog.RuntimeOllama, p.ollamaEngineVersion(ctx)); !pullable {
		return false, errSwapNeedsRestart // target has no ollama-servable variant
	}

	// Publish the effective preference so every in-process reader (tuning
	// target, Active-flip guard, coding-alias default, available-update pick)
	// sees the new model rather than the frozen boot snapshot.
	id := manifest.ModelID
	p.preferredOverride.Store(&id)
	// A model choice ends every #586 "no model yet" state: the standing
	// no-model-selected record, the abandoned-question record (the file
	// was just overwritten by the caller / self-heals on the next boot),
	// and the install flow's pending-question claim, so a held fallback
	// dispatch re-reads the world instead of waiting out its deadline.
	p.noModelSelected.Store(false)
	p.modelQuestionUnanswered.Store(false)
	p.noteModelChoiceAnswered()

	st, _ := p.store.Load()
	if ms, found := st.Models[manifest.ModelID]; found && ms.State == catalog.ModelStateReady {
		// On disk: flip Active + bounce the engine now.
		p.requestEngineReconcile(true)
		return false, nil
	}
	// Not on disk: record the pending switch and start the pull. The bounce
	// fires from runPullJob's completion once the weights reach Ready; the old
	// model keeps serving until then. The preference is already published and
	// self-heals on the next boot, so it is left in place either way.
	//
	// A dispatch that fails is REPORTED, not swallowed (waired-agent#257).
	// Returning (false, nil) here made this indistinguishable from "the
	// weights were already on disk and the switch is done": the setup
	// reconciler recorded no refusal, its model_pull row sat at pending
	// forever — admission is once per desired value, so nothing retried it —
	// and the operator's own switch reported success while nothing was
	// downloading.
	//
	// Two %w on purpose: the cause sentinel (errPullsDisabled and friends)
	// has to stay in the chain, because classifyModelRejection reads it to
	// pick the §7 code the wizard renders.
	p.pendingSwapModel.Store(&id)
	if _, perr := p.PullModel(ctx, manifest.ModelID); perr != nil {
		p.pendingSwapModel.CompareAndSwap(&id, nil)
		p.logger.Warn("swap preferred model: pull dispatch failed", "model", manifest.ModelID, "err", perr)
		return false, fmt.Errorf("start the download for %s: %w: %w",
			manifest.ModelID, perr, management.ErrModelSwitchUnavailable)
	}
	return true, nil
}

func (p *agentInferenceProvider) DeleteModel(ctx context.Context, modelID string) error {
	// Stop a download of this model before touching the catalog
	// (waired-agent#641). Deleting the row out from under a running job
	// did not stop the job — it only removed the one thing that showed it
	// was running, because models.downloads is derived from state.Models
	// alone. The job then finished and wrote the model back as Ready,
	// which is the "deleted model returns to ready" the issue reported.
	//
	// CancelPull waits for the job to unwind, so everything below reads a
	// settled catalog rather than racing it.
	stopped := false
	if res, cerr := p.CancelPull(ctx, modelID); cerr == nil && res.Status == pullCancelCancelled {
		stopped = true
	}
	state, err := p.store.Load()
	if err != nil {
		return err
	}
	m, ok := state.Models[modelID]
	if !ok {
		// The cancelled job's own cleanup already removed the row. The
		// operator asked for the model to be gone and it is gone, so this
		// is the success case, not "no such model".
		if stopped {
			p.logger.Info("model record removed by the cancelled download it was still fetching",
				"model", modelID)
			p.forgetDeletedModel(modelID)
			return nil
		}
		return fmt.Errorf("model %q not present", modelID)
	}
	// Delete the weights before the record, not after. The record is how
	// this host finds the tag again; dropping it first and then failing
	// to remove the bytes leaves weights nothing can name, which is the
	// state waired-agent#641 found on a 16 GB machine — 12 GB of models
	// against 6.4 GB of intent, and a later rescan re-adopting the
	// "deleted" entry as ready.
	//
	// The shared-tag question the original Phase A comment deferred is
	// answered here: several manifests can resolve to one engine tag, so
	// the tag is only removed once no OTHER model record still names it.
	// Sharing is rarer than deleting, and the wrong answer to it takes a
	// second model down with the one that was asked for.
	if tag := m.OllamaTag; tag != "" && p.puller != nil {
		if shared := modelIDsForTag(state.Models, tag, modelID); len(shared) > 0 {
			p.logger.Info("model record removed; weights kept, another model shares the tag",
				"model", modelID, "tag", tag, "shared_with", shared)
		} else if err := p.puller.Remove(ctx, tag); err != nil {
			// Do not report success. Answering "deleted" while the bytes
			// stay is the defect: the operator reads a freed disk that is
			// still full, and #641's rescan brings the entry back.
			p.logger.Warn("deleting the weights failed; keeping the model record",
				"model", modelID, "tag", tag, "err", err)
			return fmt.Errorf("delete the weights for %s: %w", modelID, err)
		} else {
			p.logger.Info("model weights deleted", "model", modelID, "tag", tag)
		}
	}
	delete(state.Models, modelID)
	for k, e := range state.Endpoints {
		if e.ModelID == modelID {
			delete(state.Endpoints, k)
		}
	}
	// An ActiveSelection that survives the model it names is not merely
	// stale, it is load-bearing in the wrong direction (waired-agent#641):
	// activeEngineTag resolves Active through state.Models, so a dangling
	// Active makes it answer "", narrowPublishedModels reads that as
	// "nothing to enforce" and passes the probe result through unmodified,
	// and within one 5s tick the host advertises every tag /api/tags
	// reports — the host-speed probe model included. That is the failure
	// waired-agent#656 reported, reached through a second door that #670
	// did not close.
	if state.Active != nil && state.Active.ModelID == modelID {
		state.Active = nil
	}
	if err := p.store.Save(state); err != nil {
		return err
	}
	p.logger.Info("model record removed", "model", modelID, "tag", m.OllamaTag)
	p.forgetDeletedModel(modelID)
	return nil
}

// forgetDeletedModel drops the standing instructions that would download
// the model again, so a removal survives a restart (waired-agent#641: the
// model "came back" after several daemon restarts).
//
// Two records name a model outside state.json. The in-process override is
// what every live reader consults; preferred-model.json is what the next
// boot reads, where bootstrapPreferredModel re-pulls a preferred model
// that is missing — which is exactly a model someone just deleted.
//
// Only a record that names THIS model is touched. A "run without a local
// model" (None) or an abandoned-question (Unanswered) record is a
// different statement about a different question (#586) and is left
// alone.
//
// Clearing the preference returns the host to the standing default for a
// host that has not chosen: bootstrapAfterEngineStart falls to the
// bundled arm, which may pre-pull the bundled model on the next boot.
// That is a different model from the one just deleted, and it is the
// behaviour every never-asked host already has — but it is a download, so
// it is logged rather than left for the operator to discover.
func (p *agentInferenceProvider) forgetDeletedModel(modelID string) {
	if cur := p.preferredOverride.Load(); cur != nil && *cur == modelID {
		empty := ""
		p.preferredOverride.Store(&empty)
	}
	if p.preferencePath == "" {
		return
	}
	pref, ok, err := agentconfig.LoadPreference(p.preferencePath)
	if err != nil {
		p.logger.Warn("preferred-model.json unreadable; the deleted model may be re-downloaded on the next restart",
			"model", modelID, "path", p.preferencePath, "err", err)
		return
	}
	if !ok || pref.ModelID != modelID {
		return
	}
	pref.ModelID = ""
	if err := agentconfig.SavePreference(p.preferencePath, pref); err != nil {
		p.logger.Warn("clearing the deleted model from preferred-model.json failed; it may be re-downloaded on the next restart",
			"model", modelID, "path", p.preferencePath, "err", err)
		return
	}
	p.logger.Info("the deleted model was this host's preferred model; the preference is cleared, so the next restart falls back to the bundled pick",
		"model", modelID)
}

// modelIDsForTag lists the models OTHER than except whose weights are the
// same engine tag. Non-empty means removing the tag would take those
// models' weights with it.
func modelIDsForTag(models map[string]catalog.ModelState, tag, except string) []string {
	var shared []string
	for id, e := range models {
		if id != except && e.OllamaTag == tag {
			shared = append(shared, id)
		}
	}
	slices.Sort(shared)
	return shared
}

func (p *agentInferenceProvider) buildSelector(ctx context.Context) *router.Selector {
	// Routing preference snapshot — read once per SelectK so a
	// concurrent operator transition cannot tear the (mode, peer) pair.
	var pref state.RoutingPreference
	if p.routing != nil {
		pref = p.routing()
	}
	// Public-only is a per-request Claude-surface choice; general inference
	// has no /model to pick it from.
	return p.buildSelectorWith(ctx, pref, false)
}

// baseRouterInputs assembles the router Inputs shared by every
// selection surface (catalog manifests, local state, hardware profile,
// runtime registry, default model). Each surface layers its posture on
// top: buildSelectorWith adds the full mesh/signals posture below,
// localOnlySelector.buildSelector deliberately adds nothing (overlay
// loop-prevention posture). A new Inputs field belongs here only if
// BOTH postures must carry it.
func (p *agentInferenceProvider) baseRouterInputs(ctx context.Context) router.Inputs {
	st, _ := p.store.Load()
	hw := p.profiler.Profile(ctx)
	return router.Inputs{
		Manifests:      p.manifests,
		LocalState:     st,
		Hardware:       hw,
		Runtimes:       p.registry,
		DefaultModelID: defaultCodingModelID(p.effectiveCfg(), st),
		// Both postures carry it: with local inference off this device
		// executes nothing itself, which is as true of a peer-arriving
		// request on the overlay Selector as it is of the owner's own
		// (waired-agent#829). The overlay listener's own gate answers
		// first there, so this is the defensive half of one fact.
		LocalServingOff: p.isInferenceDisabled != nil && p.isInferenceDisabled(),
	}
}

// buildSelectorWith builds the loopback Selector with an explicit
// routing preference instead of the operator's live worker preference.
// The Claude surface's claudeSelector uses it to apply a per-class
// preference (#647) without duplicating the provider's Inputs wiring.
func (p *agentInferenceProvider) buildSelectorWith(ctx context.Context, pref state.RoutingPreference, publicOnly bool) *router.Selector {
	in := p.baseRouterInputs(ctx)
	// waired-agent#901: the "Waired public share" /model entry narrows this
	// one selection to other people's machines. It never widens: PublicPolicyFn
	// still decides what is admissible at all.
	in.PublicOnly = publicOnly
	in.MeshSnapshotFn = p.meshSnapshotFn
	// waired#1031: the local half of the /model tier filter. Loopback
	// only — localOnlySelector leaves it unset, because a request that
	// arrived FROM a peer was already filtered by that peer's router and
	// re-applying the rule here would let a serving node veto work it had
	// just been asked to do.
	in.LocalContextWindow = p.DeclaredContextWindow
	// Phase 7 routing signals — all five are nil-safe inside
	// the Selector. localOnlySelector deliberately leaves them
	// unset so an overlay-arriving peer request never affects
	// in-flight bookkeeping or sticky bindings for the local
	// agent's outbound traffic.
	in.Sticky = p.sticky
	in.LocalInFlight = p.localInFlight
	in.StickyInFlight = p.stickyInFlight
	in.LocalRTT = p.localRTT
	in.LocalErrors = p.localErrors
	// Phase 9 telemetry: emit RecordSelection on every SelectK
	// return. nil disables emission. The composite Recorder is
	// supplied via inferenceSubsystemDeps from main.go.
	in.Recorder = p.recorder
	// Tailscale-exit-node-style manual routing override (Phase
	// "worker-pin"). Empty mode == RoutingModeAuto == current
	// pre-feature behaviour.
	in.RoutingMode = pref.Mode
	in.PinnedPeerDeviceID = pref.PinnedPeerDeviceID
	// Public Share consumer posture (waired#827). Loopback only —
	// localOnlySelector never sets these, so a peer-arriving request can
	// never be re-routed onward to a public node.
	in.PublicPolicyFn = p.publicPolicy
	in.OnPublicGrantDemand = p.onPublicGrantDemand
	in.OnPublicGrantUsed = p.onPublicGrantUsed
	in.OnPublicNudge = p.onPublicNudge
	return router.NewSelector(in)
}

func (p *agentInferenceProvider) Select(ctx context.Context, req router.Request) (router.Selection, error) {
	return p.buildSelector(ctx).Select(ctx, req)
}

func (p *agentInferenceProvider) SelectK(ctx context.Context, req router.Request, k int) ([]router.Candidate, error) {
	return p.buildSelector(ctx).SelectK(ctx, req, k)
}

// --- helpers ---

func newJobID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("job_%d", time.Now().UnixNano())
	}
	return "job_" + hex.EncodeToString(b)
}

func stateOrDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// advertisedEngineTag resolves state.Active to the engine-side name
// this agent should publish to MESH PEERS, which is not always the
// name its own engine loads.
//
// When ollama tuning forces a large generation ubatch the agent builds
// a derived `<base>-wb<batch>` model (#642) and records it as
// OllamaTag, keeping the pulled base tag in BaseOllamaTag. The derived
// name is a local tuning artifact: consumers build their want sets
// from `Variant.Source.Tag` / `Source.RepoID` only
// (internal/router.variantWantSets), so a peer advertising
// `...-wb2048` matches nothing and is permanently unroutable
// (waired-agent#324).
//
// Advertising the BASE tag is safe end to end: the serving side
// resolves an engine-native name back to its manifest
// (router.lookupByEngineModel) and then names its own local tag via
// router.engineModelFor, which returns ModelState.OllamaTag — the
// derived one. The batch tuning survives the peer hop; only the wire
// name changes.
//
// Returns ("", false) under the same conditions as activeEngineTag.
func advertisedEngineTag(s catalog.State) (string, bool) {
	tag, ok := activeEngineTag(s)
	if !ok {
		return "", false
	}
	if s.Active.Runtime != catalog.RuntimeOllama {
		return tag, true
	}
	if ms, mok := s.Models[s.Active.ModelID]; mok && ms.BaseOllamaTag != "" {
		return ms.BaseOllamaTag, true
	}
	return tag, true
}

// activeEngineTag resolves state.Active to the engine-side tag this
// agent's own engine serves (Ollama /api/tags name, or vLLM
// /v1/models id). Returns ("", false) when no Active is set, the
// active model is not present in state.Models, or the runtime has no
// usable tag recorded.
//
// Backs the "1 agent = 1 model" invariant: agent publishes only the
// active variant's tag in InferenceState.Models even when extra
// models happen to be pulled locally. For what goes on the WIRE, see
// advertisedEngineTag — the two differ whenever a derived batch model
// is in use.
func activeEngineTag(s catalog.State) (string, bool) {
	if s.Active == nil {
		return "", false
	}
	ms, ok := s.Models[s.Active.ModelID]
	if !ok {
		return "", false
	}
	if ms.VariantID != "" && ms.VariantID != s.Active.VariantID {
		return "", false
	}
	switch s.Active.Runtime {
	case catalog.RuntimeOllama:
		if ms.OllamaTag != "" {
			return ms.OllamaTag, true
		}
	case catalog.RuntimeVLLM:
		if ms.HFRepo != "" {
			return ms.HFRepo, true
		}
	}
	return "", false
}

func sanitiseModelID(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_':
			out = append(out, c)
		case c >= 'A' && c <= 'Z':
			out = append(out, c+('a'-'A'))
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}

// defaultCachePath returns the model cache root used by the hardware
// profiler's free-space probe. Mirrors catalog.DefaultStatePath but
// for the cache-home subtree. os.UserHomeDir (not $HOME) so the probe
// also resolves on Windows, where %USERPROFILE% is the home variable.
func defaultCachePath() string {
	if x := os.Getenv("XDG_CACHE_HOME"); x != "" {
		return filepath.Join(x, "waired", "inference")
	}
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return filepath.Join(h, ".cache", "waired", "inference")
	}
	return os.TempDir()
}

// --- Step 2: state-driven engine selection + AllowAutoFallback ---

// engineDecision captures the bootstrap-time choice of which engine
// the agent will serve from. Source describes how it was reached
// ("persisted" = read verbatim from state.Active and verified;
// "fallback" = fell down the chain because the persisted choice
// wasn't viable; "fresh" = no state.Active and the auto-picker chose).
type engineDecision struct {
	Engine          string
	Source          string
	Reasons         []string
	PersistedActive *catalog.ActiveSelection
	Fallbacks       []string
	NoEngine        bool // true when the chain ran out
}

// chooseEngine decides which engine the agent should bring up. Reads
// state.json (Step 5 migration runs as a side-effect of Load), then:
//
//  1. If state.Active is set and the engine is viable on this host,
//     keep using it ("persisted").
//  2. Else, if AllowAutoFallback, walk vllm → ollama → no-engine and
//     pick the first viable hop. Returns engineDecision with Source
//     = "fallback".
//  3. Else (strict mode), return an error so main.go can exit non-zero.
func chooseEngine(ctx context.Context, store *catalog.Store, profiler *hardware.Profiler, cfg agentconfig.InferenceConfig, stateDir string) (engineDecision, error) {
	state, err := store.Load()
	if err != nil {
		return engineDecision{}, fmt.Errorf("chooseEngine: load state: %w", err)
	}
	hw := profiler.Profile(ctx)

	// Explicit operator opt-in — agent.json `inference.preferred_engine`,
	// the --inference-preferred-engine flag, or the matching env var. A
	// viable preference forces an engine regardless of the auto-picker: it
	// wins over both a stale persisted Active and the auto-pick chain, so
	// preferred_engine="vllm" (or "ollama") takes effect on the next boot
	// without hand-editing state.json (the bootstrap clears the mismatched
	// ActiveSelection — see startInferenceSubsystem). Since #557 landed the
	// auto-picker can itself choose vLLM on a qualifying host
	// (router.VLLMAutoSelectable=true), so a preference is now an override,
	// not the only route onto vLLM.
	if pref := cfg.PreferredEngine; pref != "" {
		if engineViable(pref, hw, stateDir) {
			return engineDecision{
				Engine:          pref,
				Source:          "preference",
				Reasons:         []string{fmt.Sprintf("preferred_engine=%q honoured (viable on this host)", pref)},
				PersistedActive: state.Active,
			}, nil
		}
		if !cfg.AllowAutoFallback {
			return engineDecision{
				Engine:          pref,
				Source:          "strict-fail",
				PersistedActive: state.Active,
				Reasons: []string{
					fmt.Sprintf("preferred_engine %q not viable on this host", pref),
					"AllowAutoFallback=false: refusing to fall back",
				},
			}, fmt.Errorf("strict mode: preferred_engine %q not viable; refusing to start", pref)
		}
		// Preference not viable but fallback allowed: fall through so a
		// viable engine still serves this session.
	}

	if state.Active != nil {
		if engineViable(state.Active.Runtime, hw, stateDir) {
			return engineDecision{
				Engine:          state.Active.Runtime,
				Source:          "persisted",
				Reasons:         []string{fmt.Sprintf("state.json active runtime=%q is viable on this host", state.Active.Runtime)},
				PersistedActive: state.Active,
			}, nil
		}
		// Persisted choice not viable. AllowAutoFallback gates whether
		// we fall back or fail-fast.
		if !cfg.AllowAutoFallback {
			return engineDecision{
				Engine:          state.Active.Runtime,
				Source:          "strict-fail",
				PersistedActive: state.Active,
				Reasons: []string{
					fmt.Sprintf("active runtime %q not viable on this host", state.Active.Runtime),
					"AllowAutoFallback=false: refusing to fall back",
					"to permanently re-evaluate, run: waired runtimes refresh",
				},
			}, fmt.Errorf("strict mode: active runtime %q not viable; refusing to start", state.Active.Runtime)
		}
		// Fall through to chain walk.
	}

	// Auto-pick chain. Since #557 landed vLLM serving is wired, so the
	// hardware auto-picker may include it (router.VLLMAutoSelectable=true):
	// on a qualifying host (NVIDIA GPU, VRAM >= MinVLLMVRAMMB) vLLM leads the
	// chain and ollama backs it up. engineViable still gates each entry, so a
	// host without an installed venv falls straight through to ollama — the
	// picker advertises vLLM, it never forces an uninstalled engine. A
	// persisted Active (checked above) still wins, so an existing ollama host
	// is not silently switched. Gate the picker off to pin ollama-only.
	chain := []string{catalog.RuntimeOllama}
	if router.VLLMAutoSelectable {
		chain = []string{catalog.RuntimeVLLM, catalog.RuntimeOllama}
	}
	walked := []string{}
	// declined collects why each hop said no, in the order they were
	// asked. It is what the no-engine reason below is built from: the
	// sentence used to be a literal naming a GPU and a binary whatever the
	// chain had actually rejected, so on the #778 host — an idle RTX PRO
	// 4000 whose venv simply had not been installed yet — the log named
	// the one term that was fine.
	declined := []string{}
	for _, e := range chain {
		walked = append(walked, e)
		viable, why := engineViability(e, hw, stateDir)
		if !viable {
			declined = append(declined, why)
		}
		if viable {
			d := engineDecision{
				Engine:          e,
				Source:          sourceForChainHop(state.Active != nil, e),
				Reasons:         []string{fmt.Sprintf("auto-picked %q (host viable)", e)},
				PersistedActive: state.Active,
				Fallbacks:       walked,
			}
			if state.Active != nil && state.Active.Runtime != e {
				d.Reasons = append(d.Reasons,
					fmt.Sprintf("WARN: persisted active runtime %q not viable; running %q this session only", state.Active.Runtime, e))
			}
			return d, nil
		}
	}

	return engineDecision{
		Engine:          "",
		Source:          "no-engine",
		NoEngine:        true,
		Fallbacks:       walked,
		PersistedActive: state.Active,
		Reasons: []string{
			"no engine viable: " + strings.Join(declined, "; "),
			"inference API will return 503; install with `waired runtimes install --auto`",
		},
	}, nil
}

// engineViable returns true iff name's binary / venv is present and
// (for vllm) a CUDA-capable accelerator was detected. The CUDA check
// keys on hw.Accelerators.CUDA so the agent and router
// (internal/router/endpoint_router.go) share one predicate for
// "vllm can run on this host". A future Linux+AMD+vLLM (ROCm) path
// would land as an additional `|| hw.Accelerators.ROCm && ...`
// clause; Windows + vLLM stays disabled per the W-1 decision.
//
// Presence goes through engineInstalledOnHost, i.e. the SAME rule the
// bootstrap's resolver uses. It used to read hw.Engines.Ollama.Installed,
// a PATH probe that cannot see a state-dir install — so on a host whose
// engine waired installed itself, boot picked "no-engine" and then
// resolved a binary anyway (#179).
//
// Doesn't actually start the engine; the bootstrap's EnsureRunning
// still has to succeed.
func engineViable(name string, hw hardware.Profile, stateDir string) bool {
	ok, _ := engineViability(name, hw, stateDir)
	return ok
}

// engineViability is engineViable plus the reason a declined engine
// declined, so the no-engine decision can log the term that actually
// failed rather than a sentence covering every term at once (#778). The
// rule lives here and engineViable is the boolean façade, so the verdict
// and its explanation cannot drift apart.
//
// The reasons name the engine because the caller joins several hops into
// one line, and they say what is missing rather than what to do about it:
// the decision's second line already carries the install command, and a
// per-hop instruction would repeat it once per engine.
func engineViability(name string, hw hardware.Profile, stateDir string) (bool, string) {
	switch name {
	case catalog.RuntimeVLLM:
		// CUDA first: it is the term that cannot be fixed by installing
		// anything, so on a host without one "install the venv" would be
		// advice that leads nowhere.
		if !hw.Accelerators.CUDA {
			return false, "vllm: no CUDA-capable GPU detected on this host"
		}
		if !engineInstalledOnHost(runtime.GOOS, stateDir, name) {
			return false, "vllm: no installed venv under the state dir"
		}
		return true, ""
	case catalog.RuntimeOllama:
		if !engineInstalledOnHost(runtime.GOOS, stateDir, name) {
			return false, "ollama: no bundled binary installed"
		}
		return true, ""
	default:
		return false, fmt.Sprintf("%s: not an engine this build can serve", name)
	}
}

func sourceForChainHop(hadPersisted bool, hop string) string {
	if hadPersisted {
		return "fallback"
	}
	return "fresh"
}

// computeAvailableUpdate runs the engine + model auto-picker against
// the live hardware and reports whether refreshing would land on a
// strictly better candidate than state.Active. nil means "no upgrade
// to suggest" (either Active is already optimal or the picker can't
// fit anything new). Used by Status to surface AvailableUpdate.
// engineVersion must be the SERVING engine's, not ollama's: it is measured
// against per-variant MinEngineVersion floors, so a vLLM host judged against
// an ollama version (or against "" on a host with no ollama binary, which
// fails every floored variant closed) picks from the wrong shelf.
func computeAvailableUpdate(ctx context.Context, store *catalog.Store, profiler *hardware.Profiler, manifests []catalog.Manifest, cfg agentconfig.InferenceConfig, engineVersion string) *management.AvailableUpdate {
	state, err := store.Load()
	if err != nil {
		return nil
	}
	hw := profiler.Profile(ctx)

	// The engine this host is actually configured to run, not one re-picked
	// from a preference nothing writes (waired-agent#1028).
	//
	// cfg.PreferredEngine is set by an operator hand-editing agent.json and
	// by nothing else — the wizard's choice lives in the control plane's
	// desired_engine and reaches the host as an installed venv, never as
	// this field. So on a wizard-installed vLLM host the preference was
	// "", PickEngine ran the whole hardware ladder, and any of its terms
	// failing returned ollama: the hint then told a healthy vLLM host it
	// should swap to an ollama variant, and PreCacheUpdateCandidate — which
	// ACTS on the hint — warmed weights for the wrong engine.
	//
	// This is recommendationFromBench's rule (inference_recommendation.go),
	// which had it right in the sibling function all along: Active.Runtime
	// first, the picker only when there is no active selection to read.
	engine := ""
	if state.Active != nil {
		engine = state.Active.Runtime
	}
	if engine == "" {
		enginePick, err := router.PickEngine(router.EnginePickInput{
			Hardware:   hw,
			Preference: cfg.PreferredEngine,
			Catalog:    manifests,
		})
		if err != nil {
			return nil
		}
		engine = enginePick.Engine
	}
	modelPick, err := router.PickModel(router.PickInput{
		Catalog:          manifests,
		Hardware:         hw,
		Engine:           engine,
		EngineVersion:    engineVersion,
		PreferredModelID: cfg.PreferredModelID,
		// "Refreshing would land somewhere better" must not name a model
		// this host has already run and measured below its floor
		// (waired-agent#784). Inert when PreferredModelID is set, which
		// bypasses every rung by design.
		Measured:   measuredRatesFrom(state),
		FloorTokps: resolveInteractiveFloor(cfg.InteractiveFloorTokps),
	})
	if err != nil {
		return nil
	}

	// No active yet → the picker output is itself the "update".
	if state.Active == nil {
		return availableUpdateFromPick(engine, modelPick, state)
	}
	// Same engine + same model → nothing to suggest.
	if state.Active.Runtime == engine && state.Active.ModelID == modelPick.Manifest.ModelID && state.Active.VariantID == modelPick.Variant.VariantID {
		return nil
	}
	return availableUpdateFromPick(engine, modelPick, state)
}

func availableUpdateFromPick(engine string, mp router.Pick, state catalog.State) *management.AvailableUpdate {
	// PreCached is about the VARIANT, not the model: computeAvailableUpdate
	// reports an update when Active differs from the pick in EITHER, and a
	// variant-only difference is a whole set of weights that is not on this
	// disk. Reading only the model id made every such update claim to be
	// already cached — so maybePreCache returned early and never fetched
	// it, and ExpectedSwapSeconds promised 5 seconds for a multi-GB
	// download. That is why a host which resolved its variant blind stayed
	// on the lower one with nothing to move it (#361).
	//
	// A Ready row with no VariantID recorded is pre-#305 state (or a
	// carried-over migration): treated as not cached, which costs a pull
	// that `ollama pull` completes as a fast no-op over the existing blobs
	// and which fills the missing field in on the way through.
	precached := false
	if ms, ok := state.Models[mp.Manifest.ModelID]; ok &&
		ms.State == catalog.ModelStateReady && ms.VariantID == mp.Variant.VariantID {
		precached = true
	}
	swap := 60
	if precached {
		swap = 5
	}
	reasons := append([]string{}, mp.Reasons...)
	reasons = append(reasons, fmt.Sprintf("would swap to %s/%s on %s", mp.Manifest.ModelID, mp.Variant.VariantID, engine))
	return &management.AvailableUpdate{
		Runtime:             engine,
		ModelID:             mp.Manifest.ModelID,
		VariantID:           mp.Variant.VariantID,
		Reasons:             reasons,
		PreCached:           precached,
		ExpectedSwapSeconds: swap,
	}
}

// maybePreCache runs the auto-picker against the live host and, if
// the result differs from state.Active, pulls the candidate's weights
// in the background. Idempotent: a candidate already on disk skips
// straight through. Step 12 — keeps the next refresh fast.
func (p *agentInferenceProvider) maybePreCache(ctx context.Context) {
	// The third dispatcher #338 moved the refusal onto. Reachable on a
	// pulls-off host only BECAUSE of #338: the Active check below was
	// always nil while the engine never started, so this returned before
	// it could dispatch. Now that Active can be committed, without this
	// the fix would introduce a pre-cache dispatch failure per boot on a
	// host doing exactly what it was configured to do.
	if !p.cfg.AllowPull {
		return
	}
	// Pre-caching an UPDATE presupposes something to update.
	// computeAvailableUpdate reports the picker's own output as "the
	// update" when nothing is active — right for the /inference/status
	// field it also feeds, wrong as a trigger to download: on a fresh
	// install state.Active is nil, so this dispatched a third multi-GB
	// pull alongside the operator's model and the bundled fallback (#306).
	// The suppression is here rather than in computeAvailableUpdate so the
	// status field keeps answering "what would this host run".
	if st, err := p.store.Load(); err != nil || st.Active == nil {
		return
	}
	upd := computeAvailableUpdate(ctx, p.store, p.profiler, p.manifests, p.effectiveCfg(), p.servingEngineVersion(ctx))
	if upd == nil {
		return
	}
	if upd.PreCached {
		return
	}
	// Only pre-cache ollama-source variants in this milestone — vLLM
	// pre-cache requires HF download wiring through the HFPuller +
	// venv path resolution which is a follow-up.
	manifest, ok := catalog.LookupByAlias(upd.ModelID, p.manifests)
	if !ok || len(manifest.Variants) == 0 {
		return
	}
	for _, v := range manifest.Variants {
		if v.VariantID == upd.VariantID && v.Source.Type == catalog.SourceOllama {
			p.logger.Info("pre-caching update candidate", "model", upd.ModelID, "variant", upd.VariantID)
			if _, err := p.PullModel(ctx, manifest.ModelID); err != nil {
				p.logger.Warn("pre-cache pull dispatch failed", "err", err)
			}
			return
		}
	}
	p.logger.Info("pre-cache skipped: vLLM variant pre-fetch deferred to a later milestone",
		"model", upd.ModelID, "variant", upd.VariantID)
}

// activeRuntimeOrEmpty is a one-liner for log lines that want the
// persisted runtime name without nil-checking inline.
func activeRuntimeOrEmpty(a *catalog.ActiveSelection) string {
	if a == nil {
		return ""
	}
	return a.Runtime
}

// activeFromCatalog adapts catalog.ActiveSelection to the management
// wire shape. Returns nil for nil input.
func activeFromCatalog(a *catalog.ActiveSelection) *management.ActiveSelection {
	if a == nil {
		return nil
	}
	return &management.ActiveSelection{
		Runtime:        a.Runtime,
		RuntimeVersion: a.RuntimeVersion,
		ModelID:        a.ModelID,
		VariantID:      a.VariantID,
		DecidedBy:      a.DecidedBy,
		DecisionReason: a.DecisionReason,
	}
}

// probeTargetForActive consults the persisted catalog state to find
// which engine chooseEngine picked at bootstrap and returns the
// (kind, port) pair the local probe loop should target.
//
// Falls back to (ollama, cfg.ResolvedOllamaPort()) when state.Active
// is unset — pre-Phase-5 installs have no Active row yet, and the
// existing boot path still spawns ollama in that case. Runtime values
// outside {ollama, vllm} short-circuit to ("none", 0) so the probe
// loop declines to fire instead of pushing a misleading ollama
// heartbeat.
// probeTargetLive is probeTargetForActive asked of the engine this process
// is serving with RIGHT NOW, for the mesh probe (waired-agent#948).
//
// Two reasons it does not just call probeTargetForActive per tick:
//
//   - that one reads state.Active.Runtime, and adoptEngine NILS Active
//     when it changes engines (engine_bootstrap.go) — so through the whole
//     adoption window it would answer ollama on a host that had just
//     adopted vLLM, which is the bug with a shorter duration;
//   - servingEngine() is the accessor the rest of the subsystem already
//     uses, and the point of the fix is that this loop stops being the one
//     place with its own answer.
//
// The engine-less case stays with the caller: servingEngine() returns
// RuntimeOllama for an unset pointer and can never express "none", so
// deciding it here would quietly turn every engine-less device into an
// ollama probe. The boot target answers that question, once, where it is
// a configuration fact rather than a live one.
func (p *agentInferenceProvider) probeTargetLive(cfg agentconfig.InferenceConfig) (kind string, port int) {
	if p == nil {
		return signer.InferenceTypeNone, 0
	}
	if p.servingEngine() == catalog.RuntimeVLLM {
		return signer.InferenceTypeVLLM, cfg.ResolvedVLLMPort()
	}
	return signer.InferenceTypeOllama, cfg.ResolvedOllamaPort()
}

func probeTargetForActive(cfg agentconfig.InferenceConfig) (kind string, port int) {
	st, _ := catalog.NewStore(catalog.DefaultStatePath()).Load()
	if st.Active == nil {
		return signer.InferenceTypeOllama, cfg.ResolvedOllamaPort()
	}
	switch st.Active.Runtime {
	case catalog.RuntimeVLLM:
		return signer.InferenceTypeVLLM, cfg.ResolvedVLLMPort()
	case catalog.RuntimeOllama:
		return signer.InferenceTypeOllama, cfg.ResolvedOllamaPort()
	default:
		return signer.InferenceTypeNone, 0
	}
}

// engineModelForActive returns the engine-native model identifier
// the boot benchmark sends in its /v1/chat/completions request.
// Ollama wants the tag (e.g. "qwen3:8b-q4_K_M"); vLLM wants the
// HF repo id served via --served-model-name. Falls back to an
// empty string when state.Active is missing so the benchmark
// short-circuits cleanly.
func engineModelForActive(cfg agentconfig.InferenceConfig) string {
	st, _ := catalog.NewStore(catalog.DefaultStatePath()).Load()
	if st.Active == nil {
		return ""
	}
	// Active records ModelID + VariantID; the engine-native name
	// (Ollama tag / HF repo id) lives on the per-model ModelState
	// entry the puller wrote at install time.
	modelState, ok := st.Models[st.Active.ModelID]
	if !ok {
		return st.Active.ModelID
	}
	switch st.Active.Runtime {
	case catalog.RuntimeOllama:
		if modelState.OllamaTag != "" {
			return modelState.OllamaTag
		}
		return st.Active.ModelID
	case catalog.RuntimeVLLM:
		if modelState.HFRepo != "" {
			return modelState.HFRepo
		}
		return st.Active.ModelID
	}
	return ""
}

// variantIDForActive returns the catalog variant ID of the engine's
// currently-active model. Recorded on the benchmark result for
// traceability (the value never feeds back into the benchmark
// decision). Empty when state.Active is missing.
func variantIDForActive() string {
	st, _ := catalog.NewStore(catalog.DefaultStatePath()).Load()
	if st.Active == nil {
		return ""
	}
	return st.Active.VariantID
}

// modelIDForActive is the catalog model id of the active selection, the
// companion to variantIDForActive. Recorded on a benchmark result so a
// later reader can tell whether the rate still describes what this host
// serves (waired-ai/waired-agent#783).
func modelIDForActive() string {
	st, _ := catalog.NewStore(catalog.DefaultStatePath()).Load()
	if st.Active == nil {
		return ""
	}
	return st.Active.ModelID
}

// variantSHAForActive returns catalog.VariantSHA of the active
// variant, looking the variant up in the bundled manifests by
// (ModelID, VariantID). Empty when state.Active is nil, the model is
// unknown, or the variant id is missing — all of which disable the
// boot benchmark cache for this run (the alternative would be a
// global digest that conflates "no variant installed yet" with the
// real one).
func variantSHAForActive() string {
	st, _ := catalog.NewStore(catalog.DefaultStatePath()).Load()
	if st.Active == nil {
		return ""
	}
	// Including internal models: the active model may BE one (CI pins
	// it), and a device serving a model it cannot name reads as broken.
	manifests, err := catalog.BundledManifestsIncludingInternal()
	if err != nil {
		return ""
	}
	return activeVariantSHA(manifests, st.Active.ModelID, st.Active.VariantID)
}

// activeEngineTagsForActive is the main-side wrapper around
// advertisedEngineTag / activeEngineTag — loads the catalog state from
// the default path and resolves both engine-side names for the agent's
// Active selection. Both are "" when no Active is set or the runtime
// has no usable tag recorded.
//
// advertise is what goes into InferenceState.Models (what peers may
// ask this node for); serving is what this node's own engine loaded.
// They differ only when a #642 derived batch model is in use. main.go
// feeds this to the probe loop as inferenceProbeDeps.EngineTags, which
// needs advertise to enforce the "1 agent = 1 model" invariant on the
// wire and serving to recognise the engine's own report of that tag.
//
// Called once per probe tick (#656), not once at boot: the Active
// selection lands asynchronously and a boot-time pair went stale for the
// life of the process. One call resolves both names from a single Load so
// a tick cannot pair an advertise name from before a model switch with a
// serving name from after it.
//
// A failed Load yields the zero State and therefore two empty names,
// which the probe treats as "no Active selection" and passes the engine's
// report through unmodified. That is the same fail-open the absent-state
// case takes, so there is nothing for a caller to branch on; the error is
// dropped rather than logged because this runs every
// state.HeartbeatInterval and a genuinely unreadable state file would
// repeat forever.
func activeEngineTagsForActive() (advertise, serving string) {
	st, _ := catalog.NewStore(catalog.DefaultStatePath()).Load()
	advertise, _ = advertisedEngineTag(st)
	serving, _ = activeEngineTag(st)
	return advertise, serving
}
