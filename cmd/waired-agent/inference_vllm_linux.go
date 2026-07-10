//go:build linux

package main

import (
	"context"
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
func (p *agentInferenceProvider) dispatchHFPull(ctx context.Context, manifest catalog.Manifest, variant catalog.Variant, jobID string) error {
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
	go p.runHFPullJob(ctx, manifest.ModelID, variant, puller, jobID, refresh)
	return nil
}

// runHFPullJob decouples from the request context (a CLI disconnect must not
// abort the download), runs downloadHFWeights, and commits activation on
// success — mirroring runPullJob for the ollama path.
func (p *agentInferenceProvider) runHFPullJob(parent context.Context, modelID string, variant catalog.Variant, puller *download.HFPuller, jobID string, refresh bool) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-parent.Done()
		cancel()
	}()

	if _, err := p.downloadHFWeights(ctx, modelID, variant, puller, refresh); err != nil {
		return
	}
	p.logger.Info("hf pull job completed", "model", modelID, "job", jobID)
	if modelID == p.cfg.BundledModelID {
		p.activateBundledIfUnset(modelID, variant.VariantID)
	}
	p.activatePreferredIfNeeded(modelID, variant.VariantID)
}

// bootstrapVLLM is the vLLM counterpart of the ollama startup path: resolve
// the venv, ensure the target model's safetensors are on disk (downloading if
// needed), then spawn the vLLM subprocess bound to that model, register the
// adapter, and activate. Runs in the engine-startup goroutine.
func (p *agentInferenceProvider) bootstrapVLLM(ctx context.Context) {
	puller, python, err := p.vllmServingDeps()
	if err != nil {
		p.logger.Error("vllm bootstrap: venv not ready; local inference unavailable", "err", err)
		return
	}
	manifest, variant, ok := p.vllmTarget()
	if !ok {
		p.logger.Error("vllm bootstrap: no vLLM-capable model selected — set a preferred model that ships a vllm/safetensors variant (e.g. gpt-oss-20b)",
			"bundled", p.cfg.BundledModelID)
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
			return
		}
		// Boot-time fetch: the weights are absent (localPath == ""), so this
		// is a genuine download, not a refresh of a ready model.
		localPath, err = p.downloadHFWeights(ctx, manifest.ModelID, variant, puller, false)
		if err != nil {
			p.logger.Error("vllm bootstrap: model download failed", "model", manifest.ModelID, "err", err)
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
	logDir := filepath.Join(p.stateDir, "runtimes", "vllm", "logs")
	adapter := infruntime.NewVLLMAdapter(infruntime.VLLMConfig{
		Python:               python,
		Host:                 "127.0.0.1",
		Port:                 p.cfg.VLLMPort,
		Model:                localPath,
		ServedModelName:      variant.Source.RepoID,
		MaxModelLen:          maxLen,
		DType:                variant.DType,
		GPUMemoryUtilization: p.cfg.VLLMGPUMemoryUtilization,
		TensorParallelSize:   tp,
		KVCacheDType:         kvCacheDType,
		SpeculativeConfig:    specConfig,
		LogDir:               logDir,
		Spawner:              infruntime.DefaultSpawner{},
	})
	adapter.SetAppliedTuning(tuning)
	p.registry.Register(adapter)
	p.vllm = adapter

	const maxAttempts = 3
	var ensureErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if ensureErr = adapter.EnsureRunning(ctx); ensureErr == nil {
			break
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
		p.logger.Error("vllm did not become ready after retries; local inference unavailable until restart", "err", ensureErr)
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
		"kv_cache_dtype", kvCacheDType, "speculative_ngram", specConfig != "")

	// Commit the ActiveSelection (Runtime is derived from servingEngine(),
	// == vllm here). activateBundledIfUnset fills a fresh install's empty
	// slot; activatePreferredIfNeeded lands an explicit preferred choice.
	p.activateBundledIfUnset(manifest.ModelID, variant.VariantID)
	p.activatePreferredIfNeeded(manifest.ModelID, variant.VariantID)
}
