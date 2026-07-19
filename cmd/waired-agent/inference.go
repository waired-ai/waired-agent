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
	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/observability"
	"github.com/waired-ai/waired-agent/internal/platform/elevation"
	"github.com/waired-ai/waired-agent/internal/platform/proclist"
	"github.com/waired-ai/waired-agent/internal/router"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/internal/runtime/openaicompat"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
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
// Mode/version stay ollama-only (vLLM has no borrowed/adopted modes),
// but when the serving engine is vllm the tuning warning comes from
// its adapter so a clamped context window reaches `waired doctor`
// (#675).
func (s *inferenceSubsystem) EngineProvenance() (mode, version, warning, tuningWarning string) {
	if s == nil {
		return "", "", "", ""
	}
	if s.provider != nil && s.provider.servingEngine() == catalog.RuntimeVLLM {
		if tuner, ok := s.provider.vllm.(interface{ AppliedTuning() infruntime.ModelTuning }); ok {
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
		ollamaVersionWarning(s.ollama.Borrowed(), version), tuningWarning
}

// inferenceSubsystemDeps bundles the per-agent hooks
// startInferenceSubsystem needs from main. Phase 4 grows this past
// the original (isPaused, isInferenceDisabled, inferenceState) tuple
// so it stays comprehensible.
type inferenceSubsystemDeps struct {
	IsPaused            func() bool
	IsInferenceDisabled func() bool
	InferenceState      func() (current, desired state.InferenceState)
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
	Sticky        *router.StickyStore
	LocalInFlight *router.InFlightTracker
	LocalRTT      func() map[string]uint32
	LocalErrors   func() map[string]float32

	// LocalReachable returns the disco prober's per-peer reachability
	// snapshot (Phase 8 hard-exclusion signal). Wired from main.go's
	// disco service when enabled; nil disables the hard-exclusion
	// (= Phase 7 behaviour: every reachable+non-stale snapshot peer
	// is eligible).
	LocalReachable func() map[string]bool

	// Recorder is the Phase 9 composite telemetry sink threaded into
	// the loopback Selector (router.Inputs.Recorder), the loopback
	// gateway (gateway.Deps.Recorder), and the overlay inference
	// server (inference.Config.Recorder). nil disables all emission
	// uniformly; intermediate subsystems remain functionally unchanged.
	Recorder *observability.Recorder

	// Routing returns the operator's currently-live RoutingPreference
	// (Tailscale-exit-node-style manual routing). The Selector calls
	// it once per SelectK to read mode + pinned peer atomically. nil
	// keeps the pre-feature behaviour (Mode=auto).
	Routing func() state.RoutingPreference

	// OnClaudeNodeFallback records a Claude request whose worker pin could
	// not serve it, so the non-destructive local retry is never silent
	// (#648). nil disables recording.
	OnClaudeNodeFallback func(class, peerDeviceID, reason string)
}

// startInferenceSubsystem brings up the runtime registry, gateway,
// background pre-pull goroutine, and InferenceProvider used by the
// management API. Returns the wired Provider so main.go can pass it
// to management.Server.WithInference.
//
// stateDir is the same `--state-dir` main resolves; we use it to load
// (or create) the per-install gateway token at <state>/secrets/gateway-token,
// which the loopback gateway (LocalGatewayPort) enforces on every
// Authorization: Bearer header. The integration exports the same token via
// the user's env.sh for env-driven clients. OpenCode instead points at the
// separate no-token data-plane listener (OpenCodeGatewayPort) and the Claude
// proxy uses the no-token overlay handler, so neither presents this token.
func startInferenceSubsystem(ctx context.Context, wg *sync.WaitGroup, logger *slog.Logger, stateDir string, cfg agentconfig.InferenceConfig, deps inferenceSubsystemDeps) (*inferenceSubsystem, management.InferenceProvider, error) {
	isPaused := deps.IsPaused
	isInferenceDisabled := deps.IsInferenceDisabled
	inferenceState := deps.InferenceState
	manifests, err := catalog.BundledManifests()
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
	if pref, ok, err := agentconfig.LoadPreference(prefPath); err != nil {
		logger.Warn("preferred-model.json unreadable; ignoring", "path", prefPath, "err", err)
	} else if ok {
		logger.Info("preferred-model override applied",
			"model_id", pref.ModelID, "set_at", pref.SetAt)
		agentconfig.ApplyPreferenceOverride(&cfg, pref)
	}

	statePath := catalog.DefaultStatePath()
	store := catalog.NewStore(statePath)

	cachePath := defaultCachePath()
	profiler := hardware.NewProfiler(cachePath)

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

	// Ollama source (#188): "reuse" borrows a user-run ollama (no spawn);
	// anything else (incl. "" for pre-#188 configs) is the bundled model
	// where waired downloads + supervises its own binary under state-dir.
	borrowedOllama := cfg.OllamaSource == agentconfig.OllamaSourceReuse
	bundledOllamaBin := filepath.Join(stateDir, "runtimes", "ollama", "bin", "ollama")
	bundledOllamaModels := filepath.Join(stateDir, "runtimes", "ollama", "models")
	// ollamaResolver resolves the engine binary lazily (re-run on each
	// EnsureRunning so a freshly installed ollama is picked up without
	// an agent restart). Bundled mode on Linux is STRICT: only the
	// waired-managed binary qualifies — falling back to PATH used to
	// spawn whatever system ollama was installed (unpinned version) on
	// our port. Windows/macOS keep the PATH fallback because their
	// "bundled" installs live outside state-dir (follow-up issue).
	// Reuse mode keeps the fallback too: the binary is only used as a
	// pull client there, never spawned.
	ollamaResolver := func() (string, error) {
		if fi, statErr := os.Stat(bundledOllamaBin); statErr == nil && fi.Mode().IsRegular() {
			return bundledOllamaBin, nil
		}
		if !borrowedOllama && runtime.GOOS == "linux" {
			return "", fmt.Errorf(
				"bundled ollama not installed (expected at %s): run `sudo waired runtimes install ollama`, "+
					"or switch ollama_source to \"reuse\" in agent.json / re-run `sudo waired init`",
				bundledOllamaBin)
		}
		return download.ResolveBinary("")
	}

	binary, err := ollamaResolver()
	if err != nil {
		// Ollama isn't installed yet — log and proceed. Pull / ensure
		// will fail until the engine is installed (bundled via
		// `waired runtimes install ollama`, or user-provided for reuse).
		binary = ""
		logger.Warn("ollama binary not found; inference subsystem will be inert until installed",
			"err", err, "source", cfg.OllamaSource)
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
		Binary:         binary,
		Host:           "127.0.0.1",
		Port:           cfg.ResolvedOllamaPort(),
		Spawner:        infruntime.DefaultSpawner{},
		BinaryResolver: ollamaResolver,
		// #290: GPU-backend env (e.g. OLLAMA_VULKAN / HSA override).
		BackendEnv: backendPlan.Preferred().Env,
		// reuse: probe the user's running ollama, never spawn/stop it.
		Borrowed: borrowedOllama,
	}
	if !borrowedOllama {
		// Bundled: blobs live in the waired-owned store, and only an
		// exact-pin orphan of a previous run may be adopted on a port
		// conflict (any other survivor fails loudly).
		ollamaCfg.ModelsDir = bundledOllamaModels
		ollamaCfg.ExpectedVersion = infruntime.OllamaPinnedVersion
		// Capture `ollama serve`'s stdout/stderr so a cold-start failure
		// leaves a trail (the agent's slog only sees "not ready").
		ollamaCfg.LogDir = filepath.Join(stateDir, "runtimes", "ollama", "logs")
		// #22: macOS system LaunchDaemons run with $HOME unset, so `ollama
		// serve` dies at startup ("$HOME is not defined") before it can even
		// bind the port. Give it a writable, agent-owned HOME under the
		// runtime dir (ollama creates ~/.ollama there for its key/config);
		// harmless where the launcher already sets HOME (Linux systemd).
		ollamaCfg.StateHome = filepath.Join(stateDir, "runtimes", "ollama")
		migrateLegacyOllamaModels(logger, bundledOllamaModels, "")
	}
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
	// first spawn carries it. Reuse mode owns no process to configure;
	// the post-load verification below still runs read-only and warns.
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
			ollamaTune = computeOllamaTuning(tm, tv, hwProfile, "q8_0")
			ollamaTuned = true
			if ms, found := tuneState.Models[tm.ModelID]; found && ms.OllamaTag != "" {
				ollamaTuneTag = ms.OllamaTag
			} else if tv.Source.Type == catalog.SourceOllama {
				ollamaTuneTag = tv.Source.Tag
			}
			if !borrowedOllama {
				reasons, extraWarn := modelDecisionReasons(cfg, tm, ollamaTune)
				if extraWarn != "" {
					ollamaTune.Warning = joinTuningWarn(ollamaTune.Warning, extraWarn)
				}
				ollama.SetModelEnv(ollamaTune.Env())
				ollama.SetAppliedTuning(ollamaTune.ModelTuning)
				for _, r := range reasons {
					logger.Info("model decision", "reason", r)
				}
				logger.Info("ollama serve tuning computed",
					"model", ollamaTune.ModelID, "variant", ollamaTune.VariantID,
					"ctx", ollamaTune.ContextLength, "kv", ollamaTune.KVCacheType,
					"parallel", ollamaTune.NumParallel, "warning", ollamaTune.Warning)
			}
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
	// authoritative. Reuse mode owns no process to configure.
	if !borrowedOllama {
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
			tune := computeOllamaTuning(tm, tv, hwProfile, "q8_0")
			reasons, extraWarn := modelDecisionReasons(cfg, tm, tune)
			if extraWarn != "" {
				tune.Warning = joinTuningWarn(tune.Warning, extraWarn)
			}
			for _, r := range reasons {
				logger.Info("model decision", "reason", r)
			}
			logger.Info("ollama serve tuning computed at spawn",
				"model", tune.ModelID, "variant", tune.VariantID,
				"ctx", tune.ContextLength, "kv", tune.KVCacheType,
				"parallel", tune.NumParallel, "warning", tune.Warning)
			return tune.Env(), tune.ModelTuning, true
		})
	}

	// `ollama pull` is a client of the serving engine — point it at the
	// resolved port or pulls land on whatever answers 11434.
	puller := download.NewPuller(binary, download.DefaultRunner{},
		fmt.Sprintf("OLLAMA_HOST=127.0.0.1:%d", cfg.ResolvedOllamaPort()))

	// Phase 5: register operator-configured external OpenAI-compat
	// adapters. Each one becomes a separately-named entry in the
	// registry so the router's tryExternalFallback can pick whichever
	// matches the requested model. The probe loops are kicked off in
	// the background — EnsureRunning blocks until first Ready, but a
	// transiently-down endpoint must not gate agent boot, so the
	// goroutine just logs and lets later Selector queries observe
	// the Failed state.
	externalAdapters := buildExternalAdapters(cfg.ExternalEndpoints, logger)
	for _, ext := range externalAdapters {
		registry.Register(ext)
		// The probe loop is started lazily by EnsureRunning. Spawn
		// one goroutine per adapter so the agent main loop doesn't
		// block on a slow / unreachable LAN endpoint.
		wg.Add(1)
		go func(a *openaicompat.Adapter) {
			defer wg.Done()
			if err := a.EnsureRunning(ctx); err != nil {
				logger.Warn("external openai-compat adapter not ready",
					"name", a.Name(), "url", a.BaseURL(), "err", err)
			} else {
				logger.Info("external openai-compat adapter ready",
					"name", a.Name(), "url", a.BaseURL())
			}
		}(ext)
	}

	provider := &agentInferenceProvider{
		cfg:                 cfg,
		logger:              logger,
		agentCtx:            ctx,
		manifests:           manifests,
		store:               store,
		profiler:            profiler,
		registry:            registry,
		ollama:              ollama,
		puller:              puller,
		stateDir:            stateDir,
		engine:              decision.Engine,
		dlProgress:          newDownloadProgress(),
		ollamaUsable:        func() bool { _, e := ollamaResolver(); return e == nil },
		isInferenceDisabled: isInferenceDisabled,
		inferenceState:      inferenceState,
		meshSnapshotFn:      deps.MeshSnapshotFn,
		sticky:              deps.Sticky,
		localInFlight:       deps.LocalInFlight,
		localRTT:            deps.LocalRTT,
		localErrors:         deps.LocalErrors,
		localReachable:      deps.LocalReachable,
		recorder:            deps.Recorder,
		routing:             deps.Routing,
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

	authToken, err := loadGatewayAuthToken(stateDir, logger)
	if err != nil {
		return nil, nil, fmt.Errorf("inference: gateway auth token: %w", err)
	}

	// Core deps shared by all four gateway surfaces (loopback :9473,
	// peer overlay :9474, Claude intercept :9472, OpenCode :9480). Each
	// surface sets its policy-bearing fields (Allow*, auth, gates,
	// selection, class handling) explicitly below so the intentional
	// per-surface differences stay visible at the construction site —
	// only the fields that must never diverge live here. The Recorder
	// is wired on every surface, including the no-token ones: without
	// it, requests served there were invisible in the observability
	// event ring / metrics — the gap the #496 routing sentinel exposed.
	baseGatewayDeps := func() gateway.Deps {
		return gateway.Deps{
			Runtimes:      registry,
			ListManifests: func() []catalog.Manifest { return manifests },
			Recorder:      deps.Recorder,
		}
	}

	gwDeps := baseGatewayDeps()
	gwDeps.Selector = provider
	gwDeps.AllowOpenAI = cfg.AllowOpenAIAPI
	gwDeps.AllowAnthropic = cfg.AllowAnthropicAPI
	gwDeps.AuthToken = authToken
	gwDeps.IsPaused = isPaused
	gwDeps.IsInferenceDisabled = isInferenceDisabled
	gwDeps.PeerAdapterFactory = deps.PeerAdapterFactory
	gw := gateway.NewServer(gateway.ServerConfig{
		Addr: fmt.Sprintf("127.0.0.1:%d", cfg.LocalGatewayPort),
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
	overlayDeps.AllowOpenAI = cfg.AllowOpenAIAPI
	overlayDeps.AllowAnthropic = cfg.AllowAnthropicAPI
	// No ResolveUnknownModel here: the Claude intercept moved to
	// claudeHandlerSet below (#601), and peer traffic on :9474 is
	// OpenAI-shaped with an already-resolved EngineModel — exact
	// catalog semantics are correct for it, like :9473 and :9479.
	// AuthToken intentionally empty: the inference.Server applies
	// peer auth via verifyPeerSignature; loopback bearer doesn't
	// apply to overlay traffic.
	// IsPaused / IsInferenceDisabled also empty: the inference.Server
	// wraps its own pausedGate / inferenceGate around this HandlerSet.
	// PeerAdapterFactory stays nil to enforce loop prevention.
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
	claudeDeps.Selector = &claudeSelector{
		p:              provider,
		onNodeFallback: deps.OnClaudeNodeFallback,
	}
	// AllowOpenAI stays false: the intercept surface speaks Anthropic
	// shapes only.
	claudeDeps.AllowAnthropic = cfg.AllowAnthropicAPI
	// #623: advertise the served model's real context window (via the
	// intercept's /anthropic/v1/models) and hard-reject over-window
	// prompts with an Anthropic-shaped 400 so Claude Code compacts
	// instead of overrunning the model. Rides with the intercept
	// surface — it moved here from the overlay set along with the
	// unknown-model resolver (#601).
	claudeDeps.ContextWindowFor = provider.ContextWindowFor
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
	claudeDeps.ResolveUnknownModel = func(_, _ string) (string, bool) {
		// When the worker preference pins a mesh peer and it can serve,
		// resolve an unresolvable Anthropic id to that peer's model so the
		// pinned selection reports the precise pin state; otherwise (and on
		// an unusable pin — down / stale / nothing servable) fall to the
		// device-active model so the claudeSelector's local retry works.
		if deps.Routing != nil && deps.MeshSnapshotFn != nil {
			if pref := deps.Routing(); pref.Mode == state.RoutingModePinned && pref.PinnedPeerDeviceID != "" {
				if m, ok := router.ResolveModelForPeer(manifests, deps.MeshSnapshotFn(), pref.PinnedPeerDeviceID); ok {
					return m.ModelID, true
				}
			}
		}
		return provider.ActiveModelID()
	}
	// PeerAdapterFactory: unlike the overlay set, remote selections
	// are dispatched — that's the point of #601.
	claudeDeps.PeerAdapterFactory = deps.PeerAdapterFactory
	// AuthToken empty: Claude Code presents its own subscription
	// credentials, not waired's gateway token; loopback is the
	// trust boundary (same posture as :9479).
	claudeHandlerSet := gateway.NewHandlerSet(claudeDeps)

	// Spawn the gateway listener.
	gwLn, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.LocalGatewayPort))
	if err != nil {
		return nil, nil, fmt.Errorf("inference: listen gateway: %w", err)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := gw.Serve(ctx, gwLn); err != nil {
			logger.Error("gateway server stopped", "err", err)
		}
	}()

	// OpenCode data plane: a no-token loopback gateway on a separate port.
	// The waired-authored OpenCode plugin points its provider baseURL here.
	// The bearer-token gate is dropped on purpose: the system-service
	// deployment runs the agent as User=waired and the desktop user's
	// OpenCode cannot read the 0600 gateway token, so loopback is the trust
	// boundary (same posture as the Claude proxy's no-token overlay
	// handler). loopbackOnly + pause + inference gates still apply. A zero
	// port, or AllowOpenAIAPI being off, disables the listener; a bind
	// failure is non-fatal (only the OpenCode integration is affected).
	if cfg.AllowOpenAIAPI && cfg.OpenCodeGatewayPort > 0 {
		ocGwLn, lerr := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.OpenCodeGatewayPort))
		if lerr != nil {
			logger.Warn("opencode gateway listener disabled (bind failed)",
				"port", cfg.OpenCodeGatewayPort, "err", lerr)
		} else {
			ocDeps := baseGatewayDeps()
			ocDeps.Selector = provider
			ocDeps.AllowOpenAI = true
			// AllowAnthropic false, AuthToken empty (no token — see
			// comment above).
			ocDeps.IsPaused = isPaused
			ocDeps.IsInferenceDisabled = isInferenceDisabled
			ocDeps.PeerAdapterFactory = deps.PeerAdapterFactory
			ocGw := gateway.NewServer(gateway.ServerConfig{
				Addr: fmt.Sprintf("127.0.0.1:%d", cfg.OpenCodeGatewayPort),
			}, ocDeps)
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := ocGw.Serve(ctx, ocGwLn); err != nil {
					logger.Error("opencode gateway server stopped", "err", err)
				}
			}()
			logger.Info("opencode gateway listener started", "addr", ocGwLn.Addr().String())
		}
	}

	// Engine startup + bundled-model pre-pull. Both run in the
	// background so the rest of the agent (overlay / management /
	// network map subscriber) doesn't block on a slow `ollama pull`.
	wg.Add(1)
	go func() {
		defer wg.Done()
		// #557 vLLM serving path: download the safetensors, spawn the
		// vLLM subprocess bound to that on-disk model, register the
		// adapter, and activate. Kept separate from the ollama path
		// below because vLLM needs the weights on disk before it can
		// start, whereas `ollama serve` starts model-agnostic.
		if provider.servingEngine() == catalog.RuntimeVLLM {
			provider.bootstrapVLLM(ctx)
			return
		}
		if binary == "" || !cfg.AllowPull {
			return
		}
		// EnsureRunning already waits a full cold-start budget
		// (StartupReadyTimeout) per attempt; the retry is insurance
		// against a transient spawn failure or a first start that
		// exceeds even that budget, so local inference recovers on its
		// own without an agent restart.
		const maxAttempts = 3
		var ensureErr error
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			if ensureErr = ollama.EnsureRunning(ctx); ensureErr == nil {
				break
			}
			logger.Warn("ollama EnsureRunning failed",
				"attempt", attempt, "max", maxAttempts, "err", ensureErr)
			if attempt == maxAttempts {
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(attempt) * 10 * time.Second):
			}
		}
		if ensureErr != nil {
			logger.Error("ollama did not become ready after retries; local inference unavailable until restart",
				"attempts", maxAttempts, "err", ensureErr)
			return
		}
		if ollama.Mode() == infruntime.EngineModeAdopted {
			logger.Info("adopted orphan bundled ollama (exact pin match)",
				"version", ollama.EngineVersion())
		}
		if cfg.PullOnStartup {
			provider.bootstrapBundledModel(ctx)
		}
		// Finish a preferred-model switch interrupted by its own
		// restart (issue #347): re-pull or activate the chosen model.
		provider.bootstrapPreferredModel(ctx)
		// #290: for hosts with a fallback backend (Strix Halo Linux:
		// ROCm then Vulkan), verify the GPU actually engaged and switch
		// to the next backend if it didn't, so the host never runs on
		// CPU silently while a working GPU path exists. Conservative:
		// an inconclusive probe keeps the preferred backend.
		if backendPlan.Probes() {
			resolved := resolveBackendWithProbe(ctx, ollama, backendPlan, ollama.BaseURL(), &http.Client{}, logger)
			ollama.SetResolvedBackend(resolved)
		}
		// #621: verify the exported serve tuning against the running
		// engine and degrade once on positive evidence (silent f16
		// fallback / spill). Ordered after the backend probe so the two
		// never interleave restarts. Reuse mode is read-only — waired
		// cannot restart a process it doesn't own — so a mismatch only
		// records a user-visible warning.
		if ollamaTuned {
			if borrowedOllama {
				verdict, detail := verifyOllamaTuning(ctx, &http.Client{}, ollama.BaseURL(), ollamaTune, ollamaTuneTag, hwProfile)
				mt := infruntime.ModelTuning{
					ModelID:   ollamaTune.ModelID,
					VariantID: ollamaTune.VariantID,
					Verified:  verdict != tuningInconclusive,
				}
				// #763: surface the reused engine's real request parallelism
				// when its runner can be attributed to this model.
				if verdict != tuningInconclusive {
					if np, ok := observeRunnerParallel(ollamaTune, proclist.List); ok {
						mt.ObservedNumParallel = np
					}
				}
				if verdict != tuningInconclusive && (detail != "" || verdict != tuningOK) {
					mt.Warning = "reused ollama is not tuned by waired (" + detail +
						"); consider setting OLLAMA_CONTEXT_LENGTH / OLLAMA_KV_CACHE_TYPE on your ollama service"
					logger.Warn("reused ollama tuning check", "detail", detail)
				}
				ollama.SetAppliedTuning(mt)
			} else {
				// #642 derived-batch-model creation + #621 post-spawn tuning
				// verification, shared with the in-process reconcile (#812).
				provider.finalizeOllamaServeTuning(ctx, ollamaTune,
					ollamaTuneManifest, ollamaTuneVariant, ollamaTuneTag)
			}
		}
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
	// `ollama serve` process, and cancel any external adapter probe
	// loops in parallel so their goroutines exit before the WG drain.
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
		if provider.vllm != nil {
			if err := provider.vllm.Stop(stopCtx); err != nil {
				logger.Warn("vllm stop returned error", "err", err)
			}
		}
		for _, ext := range externalAdapters {
			if err := ext.Stop(stopCtx); err != nil {
				logger.Warn("external adapter stop returned error",
					"name", ext.Name(), "err", err)
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

// defaultCodingModelID resolves what the dynamic coding aliases
// (waired/default, waired/coding) serve on this host: the explicit
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
	//   - AllowExternal stays false — even though the registry may
	//     contain openai-compat adapters for the loopback gateway's
	//     use, the overlay-side Selector must NOT proxy WG-arriving
	//     peer requests through them; that would leak the operator's
	//     LAN endpoint and Bearer token to the mesh (Phase 5);
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
	// type; nil unless engine == vllm and the engine started. The HF
	// puller and venv paths are resolved on demand inside the Linux-only
	// vLLM code (inference_vllm_linux.go), not stored here.
	stateDir string
	engine   string
	vllm     infruntime.Adapter

	// dlProgress holds live byte progress for in-flight model pulls so
	// Status() (and thus `waired status`) can show a percentage + size.
	// In-memory only (transient; never persisted to state.json).
	dlProgress *downloadProgress

	// pullsWG tracks background pull goroutines spawned by PullModel so
	// tests can join them before their t.TempDir() is removed (#377).
	// Not awaited on production shutdown (a long pull must not block SIGTERM).
	pullsWG sync.WaitGroup

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
	// benchRun overrides the measurement itself for tests (the real
	// path shells out to the engine over HTTP with multi-minute
	// budgets). nil = RunBootBenchmark. Same injection style as
	// ollamaUsable / BenchDeps.Now.
	benchRun func(ctx context.Context) BenchResult
	// lastDepthBench is the most recent #624 long-context sweep (nil =
	// none yet). Shares benchMu with lastBench.
	lastDepthBench *DepthBenchResult

	// ollamaUsable reports whether the ollama engine is actually usable
	// on this host: a binary is resolvable (bundled under state-dir or on
	// PATH) for the bundled source, or — for the reuse source — the
	// user's ollama binary is present. Drives the no_engine derivation so
	// a bundled binary outside PATH isn't mistaken for "no engine" (#188).
	// nil is treated as "not usable".
	ollamaUsable func() bool

	// Phase 7 routing signals threaded into the loopback Selector.
	// All optional; nil keeps the pre-Phase-7 mesh-fallback
	// deterministic-pick behaviour.
	sticky        *router.StickyStore
	localInFlight *router.InFlightTracker
	localRTT      func() map[string]uint32
	localErrors   func() map[string]float32

	// Phase 8: disco-prober reachability snapshot. nil keeps the
	// pre-Phase-8 behaviour (no hard exclusions); main.go wires it
	// to disco.Service.ReachableSnapshot once the disco subsystem
	// is up.
	localReachable func() map[string]bool

	// Phase 9: telemetry composite. Threaded into the loopback
	// Selector and gateway so the agent emits Phase 9 events from
	// the loopback side. nil disables emission entirely.
	recorder *observability.Recorder

	// isInferenceDisabled, when non-nil and returning true, makes
	// Status() report SubsystemState="disabled" regardless of engine
	// health. inferenceState reports (current, desired) for the
	// management API's DesiredState field. Both wired from main.go
	// when an inferenceController is attached; nil in unit tests that
	// only exercise this provider directly.
	isInferenceDisabled func() bool
	inferenceState      func() (current, desired state.InferenceState)

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
	// swapPending signals reconcileEngineServe that an in-process model
	// switch is waiting: it forces a bounce (Option 2 — always stop-then-
	// start on a switch) and resets KV to the q8_0 default for the new
	// model (the old model's verify-degraded f16 does not carry over).
	swapPending atomic.Bool
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
// No-op when the target is unchanged, when serving vLLM, or when the ollama
// process is borrowed/reused (not ours to restart). If a target arrives before
// the engine is up the restart is skipped; it applies on the next CP change or
// agent restart. The admission cap (Server.SetCapacity) is applied separately by
// the caller and is non-disruptive; this method drives only engine parallelism.
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
	if p.ollama == nil || p.servingEngine() != catalog.RuntimeOllama || p.ollama.Borrowed() {
		return // recorded; nothing we own to restart (vLLM / reuse / no engine)
	}
	p.requestEngineReconcile(false)
}

// requestEngineReconcile kicks the single background goroutine that owns every
// ollama serve-env change. swap=true marks an in-process model switch (#812) so
// the reconcile flips Active and force-bounces (Option 2 — always stop-then-
// start on a switch). Coalesced via engineReconcileInFlight: if a reconcile is
// already running it re-reads swapPending/desiredParallel on its next
// iteration, so overlapping concurrency changes and switches never stack two
// Stop/EnsureRunning cycles on the one subprocess. The bounce always runs on
// the daemon's long-lived agentCtx, never the caller's request/pull ctx.
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
// borrowed/parked/down engine the serve env is staged (or, for reuse mode,
// only the Active flip applies) so the eventual (re)start serves the new
// tuning without forcing a spawn. If the engine fails to come back after a
// switch bounce (wedged), it self-heals via the supervised restart fallback —
// the only restart #812 keeps.
func (p *agentInferenceProvider) reconcileEngineServe(ctx context.Context) {
	defer p.engineReconcileInFlight.Store(false)
	if p.ollama == nil {
		return
	}
	for {
		want := int(p.desiredParallel.Load())
		swap := p.swapPending.Swap(false)
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
		// KV cache type: an operator switch resets to the q8_0 default (the
		// old model's post-verify f16 degrade does not carry over); a
		// concurrency-only re-tune preserves the applied KV so a prior degrade
		// is kept (#621).
		kvType := "q8_0"
		if !swap && cur.KVCacheType != "" {
			kvType = cur.KVCacheType
		}
		tune := computeOllamaTuningOpts(tm, tv, p.profiler.Profile(ctx), kvType, true, want)
		// Bounce predicate: an operator switch always bounces (Option 2) so the
		// new model's per-model spawn env applies; a concurrency-only change
		// bounces iff the resolved engine parallelism actually moved (the exact
		// pre-#812 retuneParallelLoop behaviour).
		if !swap && tune.NumParallel == cur.NumParallel {
			return // already at the resolved parallelism
		}
		// Only a live, owned, un-parked ollama process can bounce. Borrowed
		// (reuse mode): we own no process to restart or configure — the Active
		// flip above is the whole switch. Owned but parked/not-yet-ready:
		// stage the serve env so the eventual (re)start serves the new tuning,
		// but don't force a spawn here.
		if p.servingEngine() != catalog.RuntimeOllama || p.ollama.Borrowed() {
			return
		}
		p.ollama.SetModelEnv(tune.Env())
		p.ollama.SetAppliedTuning(tune.ModelTuning)
		if p.ollama.IsParked() || p.ollama.Health(ctx).State != infruntime.StateReady {
			return // staged; StartEngine / normal boot brings it up tuned
		}
		p.logger.Info("reconciling ollama serve env",
			"model", tune.ModelID, "variant", tune.VariantID, "switch", swap,
			"ctx", tune.ContextLength, "kv", tune.KVCacheType,
			"num_parallel", tune.NumParallel, "warning", tune.Warning)
		if err := p.ollama.Stop(ctx); err != nil {
			p.logger.Warn("stop for engine reconcile failed; keeping current engine", "err", err)
			return
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
			return
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
// Owned (non-borrowed) ollama only — the caller guards borrowed/reuse mode.
func (p *agentInferenceProvider) finalizeOllamaServeTuning(ctx context.Context, tune ollamaTuning, m catalog.Manifest, v catalog.Variant, tag string) {
	verifyTag := tag
	if tune.NumBatch >= ollamaLargeBatch && v.Source.Type == catalog.SourceOllama {
		baseTag := v.Source.Tag
		if derived, derr := ensureOllamaDerivedModel(ctx, &http.Client{}, p.ollama.BaseURL(), baseTag, tune.NumBatch); derr != nil {
			p.logger.Warn("ollama derived batch model unavailable; serving base tag with automatic batch",
				"base", baseTag, "num_batch", tune.NumBatch, "err", derr)
		} else {
			verifyTag = derived
			if uerr := p.store.Update(func(s *catalog.State) {
				if ms, ok := s.Models[m.ModelID]; ok {
					ms.OllamaTag = derived
					ms.BaseOllamaTag = baseTag
					s.Models[m.ModelID] = ms
				}
			}); uerr != nil {
				p.logger.Warn("persist derived ollama tag failed", "err", uerr)
			}
			p.logger.Info("ollama derived batch model ready",
				"tag", derived, "from", baseTag, "num_batch", tune.NumBatch)
		}
	}
	applyOllamaTuningVerification(ctx, p.ollama, tune,
		m, v, p.profiler.Profile(ctx), verifyTag, p.ollama.BaseURL(),
		&http.Client{}, proclist.List, p.logger)
}

func (p *agentInferenceProvider) Status(ctx context.Context) management.InferenceStatus {
	state, _ := p.store.Load()
	hwProfile := p.profiler.Profile(ctx)
	rs := map[string]management.RuntimeStatus{}
	for _, name := range p.registry.Names() {
		rs[name] = p.runtimeStatusFor(ctx, name, hwProfile)
	}
	models := management.ModelsSnapshot{}
	for id, m := range state.Models {
		switch m.State {
		case catalog.ModelStateReady:
			models.Ready = append(models.Ready, id)
		case catalog.ModelStateDownloading, catalog.ModelStateQueued, catalog.ModelStateVerifying:
			models.Downloading = append(models.Downloading, id)
			if completed, total, ok := p.dlProgress.aggregate(id); ok {
				models.Downloads = append(models.Downloads, management.ModelDownload{
					Model: id, CompletedBytes: completed, TotalBytes: total,
				})
			}
		case catalog.ModelStateFailed:
			models.Failed = append(models.Failed, id)
		}
	}
	endpoints := []management.ActiveEndpoint{}
	for id, e := range state.Endpoints {
		endpoints = append(endpoints, management.ActiveEndpoint{
			EndpointID: id, Runtime: e.Runtime, ModelID: e.ModelID, State: e.State,
		})
	}
	// Step 2 subsystem_state derivation:
	//   operator-disabled (soft pause) → disabled (overrides engine health)
	//   operator-stopped (hard, #186)  → stopped
	//   no usable engine               → no_engine
	//   engine restart in flight       → starting
	//   active.model_id missing        → awaiting_model
	//   active model failed download   → pull_failed
	//   active model not yet ready     → loading
	//   bootstrap fell back            → degraded
	//   else                           → ready
	subState := "ready"
	switch {
	case p.isInferenceDisabled != nil && p.isInferenceDisabled():
		subState = "disabled"
	case p.ollama != nil && p.ollama.IsParked():
		// Hard engine power axis (#186): operator stopped the engine to
		// free memory. Distinct from no_engine (binary missing) — here a
		// usable engine exists but is intentionally down.
		subState = "stopped"
	case !hasUsableEngine(p.registry, hwProfile, p.ollamaUsable):
		subState = "no_engine"
	case p.ollama != nil && p.ollama.Health(ctx).State == infruntime.StateStarting:
		// Engine restart in flight (e.g. just after a start request);
		// it isn't serving yet even if the active model is on disk.
		subState = "starting"
	case state.Active == nil:
		subState = "awaiting_model"
	default:
		ms, ok := state.Models[state.Active.ModelID]
		switch {
		case !ok:
			subState = "awaiting_model"
		case ms.State == catalog.ModelStateFailed:
			subState = "pull_failed"
		case ms.State != catalog.ModelStateReady:
			subState = "loading"
		}
	}
	desiredStateStr := ""
	if p.inferenceState != nil {
		_, desired := p.inferenceState()
		desiredStateStr = string(desired)
	}
	p.benchMu.Lock()
	depth := p.lastDepthBench
	p.benchMu.Unlock()
	return management.InferenceStatus{
		SubsystemState:  subState,
		Runtimes:        rs,
		Models:          models,
		ActiveEndpoints: endpoints,
		Active:          activeFromCatalog(state.Active),
		AvailableUpdate: computeAvailableUpdate(ctx, p.store, p.profiler, p.manifests, p.effectiveCfg(), p.ollamaEngineVersion(ctx)),
		LongContext:     longContextBenchFor(depth),
		DesiredState:    desiredStateStr,
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
// actually serve. A local engine (ollama / vllm) is usable only when its
// binary is installed — the adapter is registered unconditionally at boot
// (see the OllamaAdapter wiring in setupInference) even when the binary is
// missing, so "registered" alone does not imply "usable". External
// openai-compat adapters need no local install and are always treated as
// usable. When nothing is usable the caller reports SubsystemState
// "no_engine", which the tray / CLI surface as an "Install Ollama" prompt.
func hasUsableEngine(reg *infruntime.Registry, hw hardware.Profile, ollamaUsable func() bool) bool {
	for _, name := range reg.Names() {
		switch name {
		case "ollama":
			// Prefer the agent's resolver (knows the bundled binary under
			// state-dir, which the PATH-based profiler can't see). Fall
			// back to the profiler's PATH detection when no resolver was
			// wired (e.g. unit tests constructing the provider directly).
			if ollamaUsable != nil {
				if ollamaUsable() {
					return true
				}
			} else if hw.Engines.Ollama.Installed {
				return true
			}
		case "vllm":
			if hw.Engines.VLLM.Installed {
				return true
			}
		default:
			// External (e.g. operator-configured openai-compat) adapter:
			// no local binary to install, so its mere registration counts.
			return true
		}
	}
	return false
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
	// coordinator must not advertise capacity that would 503.
	if p.ollama != nil && p.ollama.IsParked() {
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
func (p *agentInferenceProvider) ActiveModelID() (string, bool) {
	st, _ := p.store.Load()
	if st.Active == nil || st.Active.ModelID == "" {
		return "", false
	}
	return st.Active.ModelID, true
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
	host := 0
	if p.ollama != nil {
		if t := p.ollama.AppliedTuning(); t.ContextLength > 0 && t.ModelID == m.ModelID {
			host = t.ContextLength
		}
	}
	if tuner, ok := p.vllm.(interface {
		AppliedTuning() infruntime.ModelTuning
	}); ok {
		if t := tuner.AppliedTuning(); t.ContextLength > 0 && t.ModelID == m.ModelID {
			host = t.ContextLength
		}
	}

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
func (p *agentInferenceProvider) runtimeStatusFor(ctx context.Context, name string, hwProfile hardware.Profile) management.RuntimeStatus {
	ad, _ := p.registry.Lookup(name)
	h := ad.Health(ctx)
	entry := management.RuntimeStatus{Name: name, Installed: true, State: h.State}
	if h.State == infruntime.StateFailed {
		entry.LastError = h.LastErr
	}
	switch name {
	case "ollama":
		if hwProfile.Engines.Ollama.Installed {
			entry.Version = hwProfile.Engines.Ollama.Version
		}
		if p.ollama != nil {
			// #290: surface the resolved GPU backend so a silent CPU
			// fallback (GPU present but not engaged) is visible.
			entry.Backend = string(p.ollama.ResolvedBackend())
			entry.Mode = string(p.ollama.Mode())
			entry.LiveVersion = p.ollama.EngineVersion()
			if !p.ollama.Borrowed() {
				entry.PinnedVersion = infruntime.OllamaPinnedVersion
			}
			entry.VersionWarning = ollamaVersionWarning(p.ollama.Borrowed(), entry.LiveVersion)
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
			}
		}
	case "vllm":
		if hwProfile.Engines.VLLM.Installed {
			entry.Version = hwProfile.Engines.VLLM.Version
		}
		// #675: surface the exported max-model-len sizing and its
		// warning, ollama parity. The adapter is the linux-only
		// VLLMAdapter behind the Adapter interface, so reach the
		// tuning through an assertion this untagged file can compile
		// on every platform.
		if tuner, ok := p.vllm.(interface{ AppliedTuning() infruntime.ModelTuning }); ok {
			if tune := tuner.AppliedTuning(); tune != (infruntime.ModelTuning{}) {
				entry.ContextLength = tune.ContextLength
				entry.TuningWarning = tune.Warning
			}
		}
	}
	return entry
}

// ollamaEngineVersion is the serving-engine version used against
// per-variant MinEngineVersion floors: the adapter's live
// /api/version when the engine has been ready once, else the
// boot-time binary probe. "" when neither is known (floored variants
// then fail closed).
func (p *agentInferenceProvider) ollamaEngineVersion(ctx context.Context) string {
	if p.ollama != nil {
		if v := p.ollama.EngineVersion(); v != "" {
			return v
		}
	}
	if p.profiler == nil {
		return ""
	}
	return p.profiler.Profile(ctx).Engines.Ollama.Version
}

// ollamaVersionWarning derives the agent-side version warning. Bundled
// engines must serve exactly the pin (anything else means waired is
// not in control of what answers requests); reuse engines are the
// user's own and only warned below the supported floor. An unknown
// live version ("") yields no warning — absence of data, not evidence
// of mismatch.
func ollamaVersionWarning(borrowed bool, live string) string {
	if live == "" {
		return ""
	}
	if borrowed {
		if !infruntime.OllamaVersionAtLeast(live, infruntime.OllamaSupportedMinVersion) {
			return fmt.Sprintf("engine version %s is below waired's supported minimum %s",
				live, infruntime.OllamaSupportedMinVersion)
		}
		return ""
	}
	if live != infruntime.OllamaPinnedVersion {
		return fmt.Sprintf("engine version %s does not match the bundled pin %s — restart waired-agent or %s",
			live, infruntime.OllamaPinnedVersion, elevation.Hint("waired runtimes install ollama"))
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

// servingEngine is the engine the agent actually serves from. The empty
// string — the zero value in unit tests and pre-#557 code paths — means
// ollama, preserving the historical default so existing behaviour is
// unchanged for hosts that never opt into vLLM.
func (p *agentInferenceProvider) servingEngine() string {
	if p.engine == "" {
		return catalog.RuntimeOllama
	}
	return p.engine
}

// engineVersionFor returns the installed version of the given serving
// engine, used to gate variant selection by MinEngineVersion. "" means
// unknown (the gate then fails closed only for floored variants). vLLM's
// version comes from the venv the installer activated; ollama's from the
// live engine probe.
func (p *agentInferenceProvider) engineVersionFor(ctx context.Context, engine string) string {
	if engine == catalog.RuntimeVLLM {
		if active, ok := infruntime.NewVLLMInstallerAt(filepath.Join(p.stateDir, "runtimes", "vllm")).Active(); ok {
			return active.Version
		}
		return ""
	}
	return p.ollamaEngineVersion(ctx)
}

func (p *agentInferenceProvider) PullModel(ctx context.Context, modelOrAlias string) (management.PullJob, error) {
	if !p.cfg.AllowPull {
		return management.PullJob{}, errors.New("pulls are disabled by config (allow_pull=false)")
	}
	manifest, ok := catalog.LookupByAlias(modelOrAlias, p.manifests)
	if !ok {
		return management.PullJob{}, fmt.Errorf("unknown model %q", modelOrAlias)
	}
	if len(manifest.Variants) == 0 {
		return management.PullJob{}, fmt.Errorf("manifest %s has no variants", manifest.ModelID)
	}
	// First variant the serving engine supports AND is new enough to
	// load (generalizes the historical Variants[0] rule): a too-old
	// engine pulls the plain variant instead of an mtp tag its registry
	// would refuse server-side with no useful error.
	engine := p.servingEngine()
	engineVersion := p.engineVersionFor(ctx, engine)
	variant, pullable := router.FirstPullableVariant(manifest, engine, engineVersion)
	if !pullable {
		floor := manifest.Variants[0].MinEngineVersion
		have := engineVersion
		if have == "" {
			have = "unknown"
		}
		return management.PullJob{}, fmt.Errorf(
			"model %s requires %s >= %s (engine reports %s); upgrade the engine or choose another model",
			manifest.ModelID, engine, floor, have)
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
	switch variant.Source.Type {
	case catalog.SourceOllama:
		// A refresh pull of a model that is already ready on disk must not
		// downgrade it to queued: serving continues from the on-disk blobs
		// while the pull runs, and a failed re-pull keeps it ready (#614).
		// Only re-pull a non-ready model through the queued/downloading path.
		refresh := false
		if err := p.store.Update(func(s *catalog.State) {
			if s.Models[manifest.ModelID].State == catalog.ModelStateReady {
				refresh = true
				return
			}
			s.Models[manifest.ModelID] = catalog.ModelState{
				VariantID: variant.VariantID,
				OllamaTag: variant.Source.Tag,
				State:     catalog.ModelStateQueued,
			}
		}); err != nil {
			return management.PullJob{}, err
		}
		p.pullsWG.Add(1)
		go func() {
			defer p.pullsWG.Done()
			p.runPullJob(ctx, manifest.ModelID, variant.VariantID, variant.Source.Tag, jobID, refresh)
		}()
	case catalog.SourceHuggingFace:
		// #557: vLLM safetensors. dispatchHFPull is defined per-OS — the
		// Linux build downloads the weights under <stateDir>/models/hf/
		// <repo> and records LocalPath so the next boot's bootstrapVLLM
		// spawns the engine against them; non-Linux returns an error
		// (vLLM serving is Linux-only). It writes the queued state itself.
		if err := p.dispatchHFPull(ctx, manifest, variant, jobID); err != nil {
			return management.PullJob{}, err
		}
	default:
		return management.PullJob{}, fmt.Errorf("unsupported variant source type %q for engine %q", variant.Source.Type, engine)
	}
	return management.PullJob{JobID: jobID, ModelID: manifest.ModelID, Status: "queued"}, nil
}

// waitForPulls blocks until all background pull goroutines started by
// PullModel have returned. Tests use it to join the writer goroutine
// before t.TempDir() cleanup (#377).
func (p *agentInferenceProvider) waitForPulls() { p.pullsWG.Wait() }

func (p *agentInferenceProvider) runPullJob(parent context.Context, modelID, variantID, tag, jobID string, refresh bool) {
	// Decouple from the request context so a CLI disconnect doesn't
	// abort the pull mid-flight. We still respect agent shutdown
	// because the parent wraps it.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-parent.Done()
		cancel()
	}()

	// A refresh pull of an already-ready model keeps the model servable
	// (state=ready) throughout: skip the downloading/verifying downgrades
	// so a transient registry error can't take healthy serving down (#614).
	if !refresh {
		_ = p.store.Update(func(s *catalog.State) {
			m := s.Models[modelID]
			m.State = catalog.ModelStateDownloading
			s.Models[modelID] = m
		})
	}

	// Forget live progress once the pull terminates (success or failure)
	// so a finished/failed model never lingers as a stale "downloading".
	defer p.dlProgress.forget(modelID)
	err := p.puller.Pull(ctx, tag, func(pr download.Progress) {
		p.dlProgress.observe(modelID, pr)
		if pr.State == download.StateVerifying && !refresh {
			_ = p.store.Update(func(s *catalog.State) {
				m := s.Models[modelID]
				m.State = catalog.ModelStateVerifying
				s.Models[modelID] = m
			})
		}
	})
	if err != nil {
		p.logger.Warn("ollama pull failed", "model", modelID, "tag", tag, "err", err, "refresh", refresh)
		_ = p.store.Update(func(s *catalog.State) {
			m := s.Models[modelID]
			// A failed refresh pull keeps the model ready — the on-disk
			// blobs still serve; record the error for observability only.
			if !refresh {
				m.State = catalog.ModelStateFailed
			}
			m.Error = err.Error()
			s.Models[modelID] = m
		})
		return
	}
	_ = p.store.Update(func(s *catalog.State) {
		m := s.Models[modelID]
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
	if modelID == p.cfg.BundledModelID {
		p.activateBundledIfUnset(modelID, variantID)
	}
	// Preferred-model switch: when the model that just became ready is
	// the operator's chosen one, commit it as Active. Before this the
	// switch never landed — nothing wrote Active after the restart, so
	// the agent came back up serving the old model (issue #347).
	p.activatePreferredIfNeeded(modelID, variantID)
	// #812: if this pull completed an operator's in-process model switch
	// (SwapPreferredModel recorded pendingSwapModel while the weights
	// downloaded), bounce the engine now so the new model's per-model serve
	// env applies — the same in-process swap the on-disk path takes, just
	// deferred until the download finished. Boot-time / unrelated pulls never
	// set pendingSwapModel, so they don't trigger a spurious bounce.
	if psm := p.pendingSwapModel.Load(); psm != nil && *psm == modelID {
		p.pendingSwapModel.CompareAndSwap(psm, nil)
		p.requestEngineReconcile(true)
	}
}

// bootstrapBundledModel kicks off the agent-startup pre-pull described
// in spec waired_inference_spec.md §11.1 (background download so that
// inference requests can succeed without the user invoking
// `waired models pull` explicitly).
func (p *agentInferenceProvider) bootstrapBundledModel(ctx context.Context) {
	manifest, ok := catalog.LookupByAlias(p.cfg.BundledModelID, p.manifests)
	if !ok {
		p.logger.Warn("bundled model not found in manifests; skipping pre-pull", "model", p.cfg.BundledModelID)
		return
	}
	state, _ := p.store.Load()
	if cur := state.Models[manifest.ModelID]; cur.State == catalog.ModelStateReady &&
		p.engineServesTag(ctx, cur.OllamaTag) {
		p.logger.Info("bundled model already ready; skipping pre-pull", "model", manifest.ModelID)
		// A fresh install pre-pulls the bundled model during `waired init`
		// (setup.Deploy), so the agent reaches here with the model Ready but
		// no ActiveSelection — commit it so the subsystem leaves
		// "awaiting_model". See activateBundledIfUnset.
		p.activateBundledIfUnset(manifest.ModelID, cur.VariantID)
		return
	}
	if _, err := p.PullModel(ctx, manifest.ModelID); err != nil {
		p.logger.Warn("bundled model pre-pull dispatch failed", "model", manifest.ModelID, "err", err)
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
			DecidedBy:      "auto",
			DecisionReason: []string{"bundled model auto-activated on first run (no prior selection)"},
		}
		committed = true
	}); err != nil {
		p.logger.Warn("auto-activate bundled model failed", "model", modelID, "err", err)
		return
	}
	if committed {
		p.logger.Info("auto-activated bundled model", "model", modelID, "variant", variantID)
	}
}

// preferredManifest resolves cfg.PreferredModelID (a model_id from
// preferred-model.json, or any catalog alias when set via flag/env)
// against the bundled manifests. ok=false when no preference is set
// or it names nothing in the catalog.
func (p *agentInferenceProvider) preferredManifest() (catalog.Manifest, bool) {
	pref := p.effectivePreferredModelID()
	if pref == "" {
		return catalog.Manifest{}, false
	}
	return catalog.LookupByAlias(pref, p.manifests)
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
// the next boot. Runs after bootstrapBundledModel in the
// engine-startup goroutine: re-pulls the preferred model when it is
// missing, or commits it as Active when it is already on disk.
func (p *agentInferenceProvider) bootstrapPreferredModel(ctx context.Context) {
	manifest, ok := p.preferredManifest()
	if !ok {
		return
	}
	state, _ := p.store.Load()
	if cur := state.Models[manifest.ModelID]; cur.State == catalog.ModelStateReady &&
		p.engineServesTag(ctx, cur.OllamaTag) {
		p.activatePreferredIfNeeded(manifest.ModelID, cur.VariantID)
		return
	}
	if _, err := p.PullModel(ctx, manifest.ModelID); err != nil {
		p.logger.Warn("preferred model re-pull dispatch failed", "model", manifest.ModelID, "err", err)
	}
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
// errSwapNeedsRestart for a cross-engine target so the caller restart-falls-back.
func (p *agentInferenceProvider) SwapPreferredModel(ctx context.Context, modelOrAlias string) (downloading bool, err error) {
	manifest, ok := catalog.LookupByAlias(modelOrAlias, p.manifests)
	if !ok {
		return false, fmt.Errorf("swap preferred model: unknown model %q", modelOrAlias)
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

	st, _ := p.store.Load()
	if ms, found := st.Models[manifest.ModelID]; found && ms.State == catalog.ModelStateReady {
		// On disk: flip Active + bounce the engine now.
		p.requestEngineReconcile(true)
		return false, nil
	}
	// Not on disk: record the pending switch and start the pull. The bounce
	// fires from runPullJob's completion once the weights reach Ready; the old
	// model keeps serving until then. Best-effort — the preference is already
	// published and self-heals on the next boot if the dispatch fails.
	p.pendingSwapModel.Store(&id)
	if _, perr := p.PullModel(ctx, manifest.ModelID); perr != nil {
		p.pendingSwapModel.CompareAndSwap(&id, nil)
		p.logger.Warn("swap preferred model: pull dispatch failed", "model", manifest.ModelID, "err", perr)
		return false, nil
	}
	return true, nil
}

func (p *agentInferenceProvider) DeleteModel(ctx context.Context, modelID string) error {
	state, err := p.store.Load()
	if err != nil {
		return err
	}
	m, ok := state.Models[modelID]
	if !ok {
		return fmt.Errorf("model %q not present", modelID)
	}
	// For Phase A, deletion just drops the state.json record. The
	// Ollama-side `ollama rm <tag>` shellout could be added once we
	// have a clear policy on shared models (multiple manifests, one
	// tag) — until then, leaving the bytes on disk is the safer
	// default.
	delete(state.Models, modelID)
	for k, e := range state.Endpoints {
		if e.ModelID == modelID {
			delete(state.Endpoints, k)
		}
	}
	if err := p.store.Save(state); err != nil {
		return err
	}
	p.logger.Info("model record removed", "model", modelID, "tag", m.OllamaTag)
	return nil
}

func (p *agentInferenceProvider) buildSelector(ctx context.Context) *router.Selector {
	// Routing preference snapshot — read once per SelectK so a
	// concurrent operator transition cannot tear the (mode, peer) pair.
	var pref state.RoutingPreference
	if p.routing != nil {
		pref = p.routing()
	}
	return p.buildSelectorWith(ctx, pref)
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
	}
}

// buildSelectorWith builds the loopback Selector with an explicit
// routing preference instead of the operator's live worker preference.
// The Claude surface's claudeSelector uses it to apply a per-class
// preference (#647) without duplicating the provider's Inputs wiring.
func (p *agentInferenceProvider) buildSelectorWith(ctx context.Context, pref state.RoutingPreference) *router.Selector {
	in := p.baseRouterInputs(ctx)
	in.MeshSnapshotFn = p.meshSnapshotFn
	// Phase 5: the loopback Selector may fall back through
	// registered openai-compat adapters before consulting the
	// mesh. localOnlySelector pins this false for overlay traffic.
	in.AllowExternal = true
	// Phase 7 routing signals — all four are nil-safe inside
	// the Selector. localOnlySelector deliberately leaves them
	// unset so an overlay-arriving peer request never affects
	// in-flight bookkeeping or sticky bindings for the local
	// agent's outbound traffic.
	in.Sticky = p.sticky
	in.LocalInFlight = p.localInFlight
	in.LocalRTT = p.localRTT
	in.LocalErrors = p.localErrors
	// Phase 8: disco-prober-based hard exclusion. nil keeps the
	// pre-Phase-8 behaviour (no exclusions) — main.go wires this
	// once the disco service is up.
	in.LocalReachable = p.localReachable
	// Phase 9 telemetry: emit RecordSelection on every SelectK
	// return. nil disables emission. The composite Recorder is
	// supplied via inferenceSubsystemDeps from main.go.
	in.Recorder = p.recorder
	// Tailscale-exit-node-style manual routing override (Phase
	// "worker-pin"). Empty mode == RoutingModeAuto == current
	// pre-feature behaviour.
	in.RoutingMode = pref.Mode
	in.PinnedPeerDeviceID = pref.PinnedPeerDeviceID
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

// activeEngineTag resolves state.Active to the engine-side tag the
// peer will see in the probe response (Ollama /api/tags name, or
// vLLM /v1/models id). Returns ("", false) when no Active is set,
// the active model is not present in state.Models, or the runtime
// has no usable tag recorded.
//
// Backs the "1 agent = 1 model" invariant: agent publishes only the
// active variant's tag in InferenceState.Models even when extra
// models happen to be pulled locally.
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
	// the --inference-preferred-engine flag, or the matching env var. This
	// is THE switch for local vLLM serving (#557): the hardware auto-picker
	// keeps vLLM off by default (router.VLLMAutoSelectable=false, #574)
	// because a single-stream user is better served by Ollama, so vLLM only
	// runs when the operator deliberately asks for it. A viable preference
	// wins over a stale persisted Active so setting preferred_engine="vllm"
	// takes effect on the next boot without hand-editing state.json (the
	// bootstrap clears the mismatched ActiveSelection — see
	// startInferenceSubsystem).
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

	// Auto-pick chain. vLLM stays OUT of the chain unless the hardware
	// auto-picker is explicitly enabled (router.VLLMAutoSelectable, false
	// today per #574): on a merely vLLM-capable host we must not silently
	// switch a user off Ollama. Local vLLM is opt-in via preferred_engine
	// above (#557); the chain is the default path for everyone else.
	chain := []string{catalog.RuntimeOllama}
	if router.VLLMAutoSelectable {
		chain = []string{catalog.RuntimeVLLM, catalog.RuntimeOllama}
	}
	walked := []string{}
	for _, e := range chain {
		walked = append(walked, e)
		if engineViable(e, hw, stateDir) {
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
			"no engine viable: vllm needs GPU, ollama needs binary",
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
// Doesn't actually start the engine; the bootstrap's EnsureRunning
// still has to succeed.
func engineViable(name string, hw hardware.Profile, stateDir string) bool {
	switch name {
	case catalog.RuntimeVLLM:
		if !hw.Accelerators.CUDA {
			return false
		}
		// We approximate "venv installed" by checking VLLMInstaller.Active()
		// rooted at <state-dir>/runtimes/vllm — the same path the installer
		// writes (#525). A $HOME-relative default would diverge from a
		// sudo-run install (root's home ≠ the User=waired daemon's home).
		_, ok := infruntime.NewVLLMInstallerAt(filepath.Join(stateDir, "runtimes", "vllm")).Active()
		return ok
	case catalog.RuntimeOllama:
		return hw.Engines.Ollama.Installed
	default:
		return false
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
func computeAvailableUpdate(ctx context.Context, store *catalog.Store, profiler *hardware.Profiler, manifests []catalog.Manifest, cfg agentconfig.InferenceConfig, engineVersion string) *management.AvailableUpdate {
	state, err := store.Load()
	if err != nil {
		return nil
	}
	hw := profiler.Profile(ctx)

	enginePick, err := router.PickEngine(router.EnginePickInput{
		Hardware:   hw,
		Preference: cfg.PreferredEngine,
	})
	if err != nil {
		return nil
	}
	modelPick, err := router.PickModel(router.PickInput{
		Catalog:          manifests,
		Hardware:         hw,
		Engine:           enginePick.Engine,
		EngineVersion:    engineVersion,
		PreferredModelID: cfg.PreferredModelID,
	})
	if err != nil {
		return nil
	}

	// No active yet → the picker output is itself the "update".
	if state.Active == nil {
		return availableUpdateFromPick(enginePick.Engine, modelPick, state)
	}
	// Same engine + same model → nothing to suggest.
	if state.Active.Runtime == enginePick.Engine && state.Active.ModelID == modelPick.Manifest.ModelID && state.Active.VariantID == modelPick.Variant.VariantID {
		return nil
	}
	return availableUpdateFromPick(enginePick.Engine, modelPick, state)
}

func availableUpdateFromPick(engine string, mp router.Pick, state catalog.State) *management.AvailableUpdate {
	precached := false
	if ms, ok := state.Models[mp.Manifest.ModelID]; ok && ms.State == catalog.ModelStateReady {
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
	upd := computeAvailableUpdate(ctx, p.store, p.profiler, p.manifests, p.effectiveCfg(), p.ollamaEngineVersion(ctx))
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

// loadGatewayAuthToken loads (or creates) the per-install Local Gateway
// token at <stateDir>/secrets/gateway-token, which the loopback gateway
// (LocalGatewayPort) enforces. `waired link` exports the same value via
// env.sh for env-driven clients; the OpenCode data-plane listener and the
// Claude proxy overlay handler are token-less. Both the agent and `waired
// link` race-safely call LoadOrCreateGatewayToken; whichever runs first
// creates the file, the other reads it.
//
// On read errors we log and return "" — the gateway then runs without
// auth (logged as a warning). This is preferable to crashing the agent
// over a token-permission glitch on first boot; `waired doctor` will
// flag the missing token loudly.
func loadGatewayAuthToken(stateDir string, logger *slog.Logger) (string, error) {
	if stateDir == "" {
		logger.Warn("gateway auth disabled: empty state dir (dev/test mode)")
		return "", nil
	}
	paths, err := integration.PathsFor(stateDir)
	if err != nil {
		return "", err
	}
	tok, err := integration.LoadOrCreateGatewayToken(paths.GatewayToken)
	if err != nil {
		logger.Warn("gateway auth disabled: token load failed", "err", err, "path", paths.GatewayToken)
		return "", nil
	}
	return tok, nil
}

// buildExternalAdapters turns each non-disabled
// agentconfig.ExternalEndpoint into an openaicompat.Adapter and
// returns them in input order. Adapters that fail to construct
// (parse error / missing URL) are skipped with a warning; agentconfig
// validation rules out most of these at boot, but we tolerate runtime
// surprises so a single typo does not prevent the agent from coming
// up with its remaining endpoints.
func buildExternalAdapters(eps []agentconfig.ExternalEndpoint, logger *slog.Logger) []*openaicompat.Adapter {
	if len(eps) == 0 {
		return nil
	}
	out := make([]*openaicompat.Adapter, 0, len(eps))
	for _, ep := range eps {
		if ep.Disable {
			logger.Info("external openai-compat endpoint skipped (disabled)",
				"id", ep.ID, "url", ep.URL)
			continue
		}
		a, err := openaicompat.NewAdapter(openaicompat.Config{
			ID:         ep.ID,
			URL:        ep.URL,
			AuthEnvVar: ep.AuthEnvVar,
		})
		if err != nil {
			logger.Warn("external openai-compat endpoint configured but adapter not built",
				"id", ep.ID, "url", ep.URL, "err", err)
			continue
		}
		out = append(out, a)
	}
	return out
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
func probeTargetForActive(cfg agentconfig.InferenceConfig) (kind string, port int) {
	st, _ := catalog.NewStore(catalog.DefaultStatePath()).Load()
	if st.Active == nil {
		return signer.InferenceTypeOllama, cfg.ResolvedOllamaPort()
	}
	switch st.Active.Runtime {
	case catalog.RuntimeVLLM:
		return signer.InferenceTypeVLLM, cfg.VLLMPort
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
	manifests, err := catalog.BundledManifests()
	if err != nil {
		return ""
	}
	for _, m := range manifests {
		if m.ModelID != st.Active.ModelID {
			continue
		}
		for _, v := range m.Variants {
			if v.VariantID == st.Active.VariantID {
				return catalog.VariantSHA(v)
			}
		}
	}
	return ""
}

// activeEngineTagForActive is the main-side wrapper around
// activeEngineTag — loads the catalog state from the default path and
// resolves the engine tag for the agent's Active selection. Returns
// "" when no Active is set or the runtime has no usable tag recorded.
// Used by main.go to feed inferenceProbeDeps.ActiveTag so the probe
// loop can enforce the "1 agent = 1 model" invariant.
func activeEngineTagForActive() string {
	st, _ := catalog.NewStore(catalog.DefaultStatePath()).Load()
	tag, _ := activeEngineTag(st)
	return tag
}
