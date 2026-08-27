//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/download"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/router"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// resolveVLLMTensorParallel returns the --tensor-parallel-size for this
// host: the operator override (vllm_tensor_parallel ≥ 1) clamped to the
// detected NVIDIA GPU count, else the auto rule
// (router.VLLMTensorParallelSize). The clamp exists because an
// over-sized override makes vLLM die during NCCL world setup — a
// clamped-but-running engine plus a warning is strictly more
// diagnosable. An explicit 1 is the "force single GPU" escape hatch
// and is never auto-upgraded.
func resolveVLLMTensorParallel(cfgTP int, hw hardware.Profile, logger *slog.Logger) int {
	nvidia := 0
	for _, g := range hw.GPUs {
		if g.Vendor == "nvidia" {
			nvidia++
		}
	}
	if cfgTP >= 1 {
		if cfgTP > nvidia && nvidia >= 1 {
			logger.Warn("vllm_tensor_parallel exceeds detected NVIDIA GPU count; clamping",
				"configured", cfgTP, "gpus", nvidia)
			return nvidia
		}
		if nvidia == 0 {
			// No NVIDIA GPU detected at all — engineViable should have
			// stopped us earlier; fall back to 1 rather than crash vLLM.
			logger.Warn("vllm_tensor_parallel set but no NVIDIA GPU detected; using 1",
				"configured", cfgTP)
			return 1
		}
		return cfgTP
	}
	return router.VLLMTensorParallelSize(hw)
}

// resolveVenvHFCLI returns the HF CLI the agent shells out to for the
// safetensors download, preferring the vLLM venv's own `hf` (huggingface_hub
// 1.0+) then its `huggingface-cli`, and finally whatever is on PATH. Using
// the venv binary keeps the downloader version-matched to the engine
// (vllm_install.go pins huggingface_hub[cli] into the same venv).
func resolveVenvHFCLI(binDir string) string {
	for _, name := range []string{"hf", "huggingface-cli"} {
		cand := filepath.Join(binDir, name)
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
			return cand
		}
	}
	// Fall back to PATH lookup; ResolveHFCLI("") errors out cleanly if
	// nothing is found, which the caller surfaces.
	resolved, err := download.ResolveHFCLI("")
	if err != nil {
		return ""
	}
	return resolved
}

// vllmServingDeps resolves the venv the operator installed and returns an
// HFPuller wired to its CLI plus the venv's python interpreter. err is
// non-nil when the venv isn't active (the operator opted into vLLM but never
// ran `waired runtimes install vllm`, or it landed under the wrong home; cf.
// #525).
func (p *agentInferenceProvider) vllmServingDeps() (*download.HFPuller, string, error) {
	inst := infruntime.NewVLLMInstallerAt(filepath.Join(p.stateDir, "runtimes", "vllm"))
	active, ok := inst.Active()
	if !ok {
		return nil, "", fmt.Errorf("vllm venv not active under %s (run `waired runtimes install vllm`)",
			filepath.Join(p.stateDir, "runtimes", "vllm"))
	}
	hfBin := resolveVenvHFCLI(active.BinDir)
	python := filepath.Join(active.BinDir, "python")
	return download.NewHFPuller(hfBin, download.DefaultHFRunner{}), python, nil
}

// vllmTarget resolves the model the agent should serve on vLLM: the
// operator's preferred model when set, else the bundled model — and only
// when that model ships a vLLM (safetensors) variant this engine version
// can load. ok=false means no vLLM-capable model is selected, which is the
// common "opted into vLLM but the chosen model is ollama-only" mistake.
func (p *agentInferenceProvider) vllmTarget() (catalog.Manifest, catalog.Variant, bool) {
	engineVersion := p.engineVersionFor(context.Background(), catalog.RuntimeVLLM)
	candidates := []string{}
	if m, ok := p.preferredManifest(); ok {
		candidates = append(candidates, m.ModelID)
	}
	if p.cfg.BundledModelID != "" {
		candidates = append(candidates, p.cfg.BundledModelID)
	}
	for _, id := range candidates {
		m, ok := catalog.LookupByAlias(id, p.manifests)
		if !ok {
			continue
		}
		if v, pullable := router.FirstPullableVariant(m, catalog.RuntimeVLLM, engineVersion); pullable {
			return m, v, true
		}
	}
	return catalog.Manifest{}, catalog.Variant{}, false
}

// hfLocalDir is the on-disk directory the safetensors for repoID land in.
// The repo id's "/" is flattened to "__" so the whole repo maps to a single
// directory under <stateDir>/models/hf without nesting or traversal risk.
func (p *agentInferenceProvider) hfLocalDir(repoID string) string {
	return filepath.Join(p.stateDir, "models", "hf", strings.ReplaceAll(repoID, "/", "__"))
}

// downloadHFWeights fetches the safetensors for variant into hfLocalDir and
// drives the model's state through downloading → verifying → ready, then
// records a local vLLM endpoint. Returns the local dir on success. Synchronous
// (callers run it either in the bootstrap goroutine or a pull-job goroutine).
func (p *agentInferenceProvider) downloadHFWeights(ctx context.Context, modelID string, variant catalog.Variant, puller *download.HFPuller, refresh bool) (string, error) {
	localDir := p.hfLocalDir(variant.Source.RepoID)
	// A refresh pull of an already-ready model keeps it servable
	// (state=ready) throughout so a transient error can't take healthy
	// serving down (#614); skip the downloading/verifying downgrades.
	if !refresh {
		_ = p.store.Update(func(s *catalog.State) {
			m := s.Models[modelID]
			m.State = catalog.ModelStateDownloading
			s.Models[modelID] = m
		})
	}
	defer p.dlProgress.forget(modelID)

	err := puller.Pull(ctx, variant.Source.RepoID, download.HFPullOpts{
		LocalDir:     localDir,
		Revision:     variant.Source.Revision,
		FastTransfer: true,
	}, func(pr download.Progress) {
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
		p.logger.Warn("hf pull failed", "model", modelID, "repo", variant.Source.RepoID, "err", err, "refresh", refresh)
		_ = p.store.Update(func(s *catalog.State) {
			m := s.Models[modelID]
			// A failed refresh pull keeps the model ready — the on-disk
			// weights still serve; record the error for observability only.
			if !refresh {
				m.State = catalog.ModelStateFailed
			}
			m.Error = err.Error()
			s.Models[modelID] = m
		})
		return "", err
	}

	_ = p.store.Update(func(s *catalog.State) {
		m := s.Models[modelID]
		m.State = catalog.ModelStateReady
		m.Error = ""
		m.HFRepo = variant.Source.RepoID
		m.LocalPath = localDir
		m.VariantID = variant.VariantID
		m.PulledAt = time.Now().UTC()
		s.Models[modelID] = m

		epID := "ep_local_vllm_" + sanitiseModelID(modelID)
		s.Endpoints[epID] = catalog.EndpointState{
			Runtime:   catalog.RuntimeVLLM,
			ModelID:   modelID,
			VariantID: variant.VariantID,
			State:     "ready",
			Since:     time.Now().UTC(),
		}
	})
	p.logger.Info("hf pull completed", "model", modelID, "repo", variant.Source.RepoID, "path", localDir)
	return localDir, nil
}

// dispatchHFPull is the management-API pull path for a vLLM/HF variant. It
// writes the queued state and launches the download in the background; the
// serving swap to the new weights happens on the next agent restart (the same
// restart-to-swap contract ollama uses for a model change, #347).
func (p *agentInferenceProvider) dispatchHFPull(ctx context.Context, job *pullJob, manifest catalog.Manifest, variant catalog.Variant) error {
	puller, _, err := p.vllmServingDeps()
	if err != nil {
		return fmt.Errorf("vllm HF pull unavailable: %w", err)
	}
	// A refresh pull of an already-ready model must not downgrade it —
	// serving continues from the on-disk weights and a failed re-pull
	// keeps it ready (#614). Mirrors the ollama path in PullModel.
	refresh := false
	if err := p.store.Update(func(s *catalog.State) {
		if s.Models[manifest.ModelID].State == catalog.ModelStateReady {
			refresh = true
			return
		}
		s.Models[manifest.ModelID] = catalog.ModelState{
			VariantID: variant.VariantID,
			HFRepo:    variant.Source.RepoID,
			State:     catalog.ModelStateQueued,
		}
	}); err != nil {
		return err
	}
	// spawnPull, not a bare `go`: it releases the model's in-flight slot
	// (#305b), puts HF downloads under pullsWG, which they escaped — so
	// waitForPulls() now joins them too (#377) — and clears the row a
	// cancelled job leaves behind (waired-agent#641).
	p.spawnPull(job, func() {
		p.runHFPullJob(ctx, manifest.ModelID, variant, puller, job.jobID, refresh)
	})
	return nil
}

// runHFPullJob runs downloadHFWeights and commits activation on success —
// mirroring runPullJob for the ollama path. ctx MUST be the daemon's
// long-lived context: PullModel dispatches on backgroundCtx(), never on a
// request ctx, which net/http cancels the moment the handler returns
// (#305a). It is deliberately not re-wrapped in a self-cancelling ctx.
func (p *agentInferenceProvider) runHFPullJob(ctx context.Context, modelID string, variant catalog.Variant, puller *download.HFPuller, jobID string, refresh bool) {
	if _, err := p.downloadHFWeights(ctx, modelID, variant, puller, refresh); err != nil {
		return
	}
	p.logger.Info("hf pull job completed", "model", modelID, "job", jobID)
	if p.isBundledModel(modelID) {
		p.activateBundledIfUnset(modelID, variant.VariantID)
	}
	p.activatePreferredIfNeeded(modelID, variant.VariantID)
}

// bootstrapVLLM is the vLLM counterpart of the ollama startup path: resolve
// the venv, ensure the target model's safetensors are on disk (downloading if
// needed), then spawn the vLLM subprocess bound to that model, register the
// adapter, and activate. Runs in the engine-startup goroutine.
//
// Safe to call more than once (#339): a previous call's engine is left alone
// while it is up, and stopped before this one spawns over it otherwise. That
// is decided by decideVLLMBootstrap so the rule is table-testable off this
// Linux-only path.
func (p *agentInferenceProvider) bootstrapVLLM(ctx context.Context) {
	existing := p.vllmAdapter()
	existingState := ""
	if existing != nil {
		existingState = existing.Health(ctx).State
	}
	switch decideVLLMBootstrap(existing, existingState, p.vllmIsParked()) {
	case vllmBootstrapParked:
		p.logger.Info("vllm bootstrap: the engine is stopped by the operator; not starting it",
			"state", existingState, "fix", "waired inference engine start")
		return
	case vllmBootstrapSkip:
		p.logger.Info("vllm bootstrap: an engine is already running; leaving it alone",
			"state", existingState, "endpoint", existing.BaseURL())
		return
	case vllmBootstrapStopFirst:
		p.logger.Warn("vllm bootstrap: stopping the previous engine before spawning a new one",
			"state", existingState, "endpoint", existing.BaseURL())
		if err := existing.Stop(ctx); err != nil {
			p.logger.Warn("vllm bootstrap: stopping the previous engine returned error", "err", err)
		}
	}

	// A fresh attempt: whatever the last one refused for is no longer the
	// current answer (waired-agent#1075). Cleared here rather than on
	// success so a refusal later in this function is the one that stands.
	p.clearEngineBootstrapRefusal()

	puller, python, err := p.vllmServingDeps()
	if err != nil {
		p.logger.Error("vllm bootstrap: venv not ready; local inference unavailable", "err", err)
		p.refuseEngineBootstrap(err.Error())
		return
	}
	manifest, variant, ok := p.vllmTarget()
	if !ok {
		const noModel = "no vLLM-capable model selected — set a preferred model that ships a" +
			" vllm/safetensors variant (e.g. gpt-oss-20b)"
		p.logger.Error("vllm bootstrap: "+noModel, "bundled", p.bundledModelID())
		p.refuseEngineBootstrap(noModel)
		return
	}

	// Ensure the weights are present. A prior run (or `waired models pull`)
	// may have already downloaded them.
	localPath := ""
	if st, _ := p.store.Load(); st.Models[manifest.ModelID].State == catalog.ModelStateReady {
		if lp := st.Models[manifest.ModelID].LocalPath; lp != "" {
			if fi, statErr := os.Stat(lp); statErr == nil && fi.IsDir() {
				localPath = lp
			}
		}
	}
	if localPath == "" {
		if !p.cfg.AllowPull {
			p.logger.Error("vllm bootstrap: weights absent and pulls disabled (allow_pull=false)", "model", manifest.ModelID)
			p.refuseEngineBootstrap(fmt.Sprintf(
				"the weights for %s are not on this computer and downloads are turned off"+
					" (inference.allow_pull=false in agent.json)", manifest.ModelID))
			return
		}
		// Boot-time fetch: the weights are absent (localPath == ""), so this
		// is a genuine download, not a refresh of a ready model.
		localPath, err = p.downloadHFWeights(ctx, manifest.ModelID, variant, puller, false)
		if err != nil {
			p.logger.Error("vllm bootstrap: model download failed", "model", manifest.ModelID, "err", err)
			p.refuseEngineBootstrap(fmt.Sprintf("downloading the weights for %s failed: %v",
				manifest.ModelID, err))
			return
		}
	}

	hwProfile := p.profiler.Profile(ctx)
	tp := resolveVLLMTensorParallel(p.cfg.VLLMTensorParallel, hwProfile, p.logger)
	// #676: fp8 (e4m3) KV cache on Ada+ (compute_cap ≥ 8.9) halves KV to
	// roughly double the fittable window, unless the operator opted out.
	// The serve-time KV factor must match what the engine will actually
	// use so the #675 clamp sizes correctly (an fp8 host with an f16-sized
	// window would leave capacity on the table; the reverse would abort).
	kvCacheDType, kvFactor := resolveVLLMKVCache(hwProfile, p.cfg.VLLMDisableFP8KV)
	// #675: clamp --max-model-len to what the utilization budget fits
	// instead of forwarding the manifest window verbatim (an unfittable
	// window aborts vLLM startup — no spill-style degradation exists).
	maxLen, tuning := computeVLLMTuning(manifest, variant, hwProfile, tp, p.cfg.VLLMGPUMemoryUtilization, kvFactor)
	if tuning.Warning != "" {
		p.logger.Warn("vllm context sizing", "model", manifest.ModelID,
			"max_model_len", maxLen, "native", manifest.ContextLength, "note", tuning.Warning)
	}
	// #677: ngram speculative decoding accelerates single-stream decode
	// (coding agents) with no draft weights, when the operator enables it.
	specConfig := vllmSpeculativeConfigJSON(p.cfg.VLLMSpeculativeNgram)
	// #410: without a parser vLLM never populates tool_calls, so a coding
	// agent gets the model's call as prose. Resolved from the served
	// model, overridable per host.
	toolParser := resolveVLLMToolParser(manifest, p.cfg.VLLMToolParser)
	if toolParser == "" {
		p.logger.Warn("vllm tool calling disabled: no --tool-call-parser is known for this model; "+
			"the model's tool calls will arrive as text (set inference.vllm_tool_parser to override)",
			"model", manifest.ModelID)
	}
	// The serve-flag gate (waired-agent#885). activeVer is the "current"
	// symlink's version, which may predate this build on a host installed
	// by an older agent — and an unrecognised flag is an argparse exit 2
	// that costs the whole engine, not one feature.
	activeVer, _ := vllmActiveVersion(p.stateDir)
	serveFlags := vllmServeFlagsSupported(activeVer)
	if !serveFlags {
		p.logger.Warn("vllm venv predates this build's serve flags; starting without them",
			"venv_version", activeVer, "pinned", infruntime.VLLMPinnedVersion,
			"fix", "waired runtimes install vllm")
	}
	batchedTokens := 0
	kvOffloadGiB := 0.0
	if serveFlags {
		batchedTokens = vllmMaxNumBatchedTokens(maxLen, hwProfile, p.cfg.VLLMMaxNumBatchedTokens)
		var note string
		kvOffloadGiB, note = vllmKVOffloadingGiB(p.cfg.VLLMKVOffloadingGiB, hwProfile)
		if note != "" {
			p.logger.Warn("vllm kv offloading adjusted", "detail", note)
		}
	}
	logDir := filepath.Join(p.stateDir, "runtimes", "vllm", "logs")
	adapter := infruntime.NewVLLMAdapter(infruntime.VLLMConfig{
		Python:                    python,
		Host:                      "127.0.0.1",
		Port:                      p.cfg.ResolvedVLLMPort(),
		Model:                     localPath,
		ServedModelName:           variant.Source.RepoID,
		MaxModelLen:               maxLen,
		DType:                     variant.DType,
		GPUMemoryUtilization:      p.cfg.VLLMGPUMemoryUtilization,
		TensorParallelSize:        tp,
		KVCacheDType:              kvCacheDType,
		SpeculativeConfig:         specConfig,
		ToolCallParser:            toolParser,
		EnablePromptTokensDetails: serveFlags,
		MaxNumBatchedTokens:       batchedTokens,
		KVOffloadingGiB:           kvOffloadGiB,
		LogDir:                    logDir,
		Spawner:                   infruntime.DefaultSpawner{},
		// The operator's hard stop (#881). Read live, and read by the
		// adapter itself, so request traffic through the gateway cannot
		// revive an engine that was stopped to free VRAM — and so a park
		// that lands while this bootstrap is between its own check and here
		// still refuses the spawn.
		Parked: p.vllmIsParked,
		// Crash recovery (#946): until this existed a vLLM that died after
		// reaching Ready stayed latched StateReady for the life of the
		// daemon and nothing ever restarted it.
		OnUnhealthy: p.onVLLMEngineUnhealthy,
		// The start that never reaches Ready (waired-agent#1026). The
		// ollama adapter has had this since #310; vLLM had the callback
		// declared and nothing wired to it, so a vLLM that could not bind
		// its port charged no strike, never latched, and every later
		// trigger — a gateway request, a desired-state apply, a benchmark
		// — re-entered the same failing bootstrap for the life of the
		// daemon. On real hardware that was an unbounded loop whose only
		// user-visible symptom was a wizard benchmark that never started.
		OnStartFailed: p.onVLLMEngineStartFailed,
	})
	adapter.SetAppliedTuning(tuning)
	p.registry.Register(adapter)
	p.setVLLM(adapter)

	const maxAttempts = 3
	var ensureErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if ensureErr = adapter.EnsureRunning(ctx); ensureErr == nil {
			break
		}
		// A park that raced this bootstrap, or a give-up latch: neither
		// clears on its own and neither should be retried from here. The
		// ollama arm treats both the same way (engine_bootstrap.go), and
		// without this a park landing mid-bootstrap burned 30s of backoff
		// and then logged "did not become ready", which is a false
		// diagnosis of a stop that worked.
		if errors.Is(ensureErr, infruntime.ErrEngineParked) ||
			errors.Is(ensureErr, infruntime.ErrEngineUnrecoverable) {
			p.logger.Info("vllm bootstrap: start refused by a latch; leaving it set", "err", ensureErr)
			return
		}
		p.logger.Warn("vllm EnsureRunning failed", "attempt", attempt, "max", maxAttempts, "err", ensureErr)
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
		// The engine's own log is the only place the cause is written.
		// Every attempt above is in it now, each behind its own banner
		// (#878); the hint names the cause of the attempt the loop ended
		// on, and engine_log below is where a reader finds the others —
		// which is what a run whose attempts failed differently needs.
		raw, _ := os.ReadFile(filepath.Join(logDir, "engine.log"))
		hint := vllmStartupHint(string(raw), p.cfg.ResolvedVLLMPort())
		p.logger.Error("vllm did not become ready after retries; local inference unavailable until restart",
			"err", ensureErr, "hint", hint, "engine_log", filepath.Join(logDir, "engine.log"))
		// The hint used to end here, in a log line nobody reads on a
		// desktop. It is the only sentence that names a cause, so it goes
		// where the surfaces look: Health().LastErr, which runtimeStatusFor
		// publishes as runtimes[].last_error and `waired status` renders as
		// the ⚠ line (waired-agent#1026).
		adapter.SetStartFailureReason(hint)
		// The argv only on the failure path, and only here: it carries
		// paths and no secrets, and without it a flag rejection cannot
		// be matched to the flag that caused it.
		p.logger.Warn("vllm start-up argv", "args", adapter.CommandArgsForDiagnostics())
		return
	}
	// #675 read-back: the engine logs its measured KV pool capacity
	// during startup; record it as the tuning's verification (the ollama
	// /api/ps verify analogue). Best-effort — an unreadable or
	// capacity-less log leaves the tuning unverified.
	if raw, err := os.ReadFile(filepath.Join(logDir, "engine.log")); err == nil {
		tuning = applyVLLMTuningVerification(tuning, string(raw))
		adapter.SetAppliedTuning(tuning)
	}
	p.logger.Info("vllm engine ready",
		"model", manifest.ModelID, "variant", variant.VariantID,
		"served_as", variant.Source.RepoID, "endpoint", adapter.BaseURL(),
		"tensor_parallel_size", tp, "max_model_len", maxLen,
		"kv_cache_dtype", kvCacheDType, "speculative_ngram", specConfig != "",
		"tool_call_parser", toolParser,
		"prompt_tokens_details", serveFlags,
		"max_num_batched_tokens", batchedTokens,
		"kv_offloading_gib", kvOffloadGiB)

	// Commit the ActiveSelection (Runtime is derived from servingEngine(),
	// == vllm here). activateBundledIfUnset fills a fresh install's empty
	// slot; activatePreferredIfNeeded lands an explicit preferred choice.
	p.activateBundledIfUnset(manifest.ModelID, variant.VariantID)
	p.activatePreferredIfNeeded(manifest.ModelID, variant.VariantID)
}
