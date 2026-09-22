//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/download"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/router"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/proto/signer"
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
// HFPuller wired to its CLI plus the venv itself, held in use until release
// is called (vllmVenvKeeper, waired-agent#1431): whatever runs from it — an
// engine, a download — must not have it reclaimed underneath. err is non-nil
// when the venv isn't active (the operator opted into vLLM but never ran
// `waired runtimes install vllm`, or it landed under the wrong home; cf.
// #525), and release is then a no-op.
func (p *agentInferenceProvider) vllmServingDeps() (*download.HFPuller, infruntime.InstallResult, func(), error) {
	venv, release, ok := p.vllmVenvKeeper().hold()
	if !ok {
		return nil, venv, release, fmt.Errorf("vllm venv not active under %s (run `waired runtimes install vllm`)",
			filepath.Join(p.stateDir, "runtimes", "vllm"))
	}
	hfBin := resolveVenvHFCLI(venv.BinDir)
	return download.NewHFPuller(hfBin, download.DefaultHFRunner{}), venv, release, nil
}

// errVLLMNoModelChosen is "nobody has picked a model for this computer
// yet", as distinct from every other reason a vLLM start cannot begin.
//
// It is not a fault, and the caller that starts engines at boot must not
// record it as one: setup installs the engine first and asks for a model
// after (docs/decisions/20260808/0530), so on the browser wizard's path
// this is the ordinary state for as long as the operator is reading the
// picker. Reported to whoever asked for a start explicitly, ignored by
// the bootstrap.
var errVLLMNoModelChosen = errors.New("no model has been chosen for this computer yet; the engine starts when one is")

// vllmTarget resolves the model the agent should serve on vLLM: the
// operator's chosen model, and only when it ships a vLLM (safetensors)
// variant this engine version can load.
//
// chosen=false means nothing has been chosen at all. It used to fall back
// to cfg.BundledModelID — the model the hardware auto-selector named at
// boot — and that is how a wizard-driven vLLM install tried to start on a
// gguf-only model nobody had asked for, seconds after the venv appeared
// and minutes before the operator reached the picker (waired-agent#1298).
// The bundled id is a RECOMMENDATION computed against whichever engine the
// picker named; it is not a selection, and starting an engine is a
// decision only a selection may drive.
func (p *agentInferenceProvider) vllmTarget() (catalog.Manifest, catalog.Variant, bool, error) {
	m, v, chosen, fits, err := p.vllmTargetBuild()
	if err == nil && !fits {
		p.logger.Warn("vllm: no variant of the chosen model fits this host; starting on the first one it can load",
			"model", m.ModelID, "variant", v.VariantID, "min_vram_mb", v.MinVRAMMB)
	}
	return m, v, chosen, err
}

// vllmTargetBuild is vllmTarget without its log line, for the readers that
// ask on every status poll (waired-agent#1515): the build of the chosen
// model this engine would start, and whether it fits this computer.
func (p *agentInferenceProvider) vllmTargetBuild() (catalog.Manifest, catalog.Variant, bool, bool, error) {
	m, ok := p.preferredManifest()
	if !ok {
		return catalog.Manifest{}, catalog.Variant{}, false, false, errVLLMNoModelChosen
	}
	ctx := context.Background()
	engineVersion := p.engineVersionFor(ctx, catalog.RuntimeVLLM)
	// Which BUILD of it, asked of the host rather than read off manifest
	// order. FirstPullableVariant answers "can this engine load it at
	// all" and returns the first row that says yes, which is the right
	// question only while a model ships one variant per engine.
	// qwen3.6-27b ships two safetensors builds (fp8 then nvfp4), and #575
	// adds more, so the first row would have been served to hosts it does
	// not fit. The ollama side asked the same question and was moved onto
	// FamilyBestFit in waired-agent#1265; this is the vLLM half of it.
	//
	// A build the user chose is served as chosen when this engine can load
	// it, and with no choice the owner's default build is served wherever
	// this host is recommended it (waired-agent#1348).
	if want := p.chosenVariantFor(m.ModelID); want != "" {
		if v, ok := variantByID(m, want); ok && router.VariantLoadable(v, catalog.RuntimeVLLM, engineVersion) {
			return m, v, true, true, nil
		}
	}
	if p.profiler != nil {
		if best := router.FamilyDefaultBuild(m, catalog.RuntimeVLLM, engineVersion, p.Hardware(ctx)); best.Fits {
			return m, best.Variant, true, true, nil
		}
	}
	// No variant FITS. Falling back to the loadable-at-all answer keeps
	// today's behaviour rather than adding a refusal: min_vram_mb is a
	// catalog estimate, the engine's own sizing is the authority, and a
	// host refused here would lose local inference over a number nobody
	// measured on it. vLLM's clamp and its start-up abort are the real
	// gates, and they run either way.
	v, pullable := router.FirstPullableVariant(m, catalog.RuntimeVLLM, engineVersion)
	if !pullable {
		return catalog.Manifest{}, catalog.Variant{}, true, false, fmt.Errorf(
			"the model chosen for this computer (%s) has no vllm/safetensors variant this engine can load;"+
				" choose a model that does, or switch this computer to ollama", m.ModelID)
	}
	return m, v, true, false, nil
}

// vllmSwitchTarget is the build a switch to modelID will start and the two
// figures that say whether it fits: the build's catalog minimum and this
// computer's vLLM VRAM budget, as the model catalog's row compares them.
func (p *agentInferenceProvider) vllmSwitchTarget(ctx context.Context, modelID string) (string, int, int, bool) {
	m, v, _, _, err := p.vllmTargetBuild()
	if err != nil || m.ModelID != modelID {
		return "", 0, 0, false
	}
	have := 0
	if p.profiler != nil {
		have = router.VLLMVRAMBudgetMB(p.Hardware(ctx))
	}
	return v.VariantID, v.MinVRAMMB, have, true
}

// vllmStartPlan resolves everything a vLLM start needs before it can spawn:
// the venv it runs from, and the model it serves.
//
// It exists so the reason a start CANNOT begin is one string in one place.
// bootstrapVLLM refuses into a provider record that only the next bootstrap
// can clear, which is invisible to whoever asked for the start; the operator's
// explicit `waired inference engine start` asks the same question up front
// through vllmStartRefusal and answers with it instead of printing "ok."
// (waired-agent#1170, and the same class of lie the 2026-08-28 engine-power
// decision settled for the local-inference-off refusal).
//
// The download-policy arm (weights absent with inference.allow_pull=false) is
// deliberately NOT here: it needs the resolved local path, which is the
// spawn's own input, so it stays beside the spawn.
//
// The venv comes back held (release); every error return has released it.
func (p *agentInferenceProvider) vllmStartPlan() (*download.HFPuller, infruntime.InstallResult, func(), catalog.Manifest, catalog.Variant, error) {
	puller, venv, release, err := p.vllmServingDeps()
	if err != nil {
		return nil, venv, release, catalog.Manifest{}, catalog.Variant{},
			fmt.Errorf("venv not ready; local inference unavailable: %w", err)
	}
	manifest, variant, _, err := p.vllmTarget()
	if err != nil {
		release()
		return nil, venv, func() {}, catalog.Manifest{}, catalog.Variant{}, err
	}
	return puller, venv, release, manifest, variant, nil
}

// vllmStartResolution is everything a vLLM start resolved before it acts:
// the venv (held), the chosen model and the build of it this engine loads,
// the model this host was running when it may answer in the meantime, and
// the plan (planVLLMTarget, waired-agent#1515).
type vllmStartResolution struct {
	puller            *download.HFPuller
	venv              infruntime.InstallResult
	release           func()
	target            catalog.Manifest
	variant           catalog.Variant
	targetPath        string
	targetDownloading bool
	prev              catalog.Manifest
	prevVariant       catalog.Variant
	prevPath          string
	plan              vllmTargetPlan
}

// resolveVLLMStart gathers the facts and makes the plan. engineUp and the
// serving model are what the bootstrap found registered. On an error the
// venv is already released.
func (p *agentInferenceProvider) resolveVLLMStart(ctx context.Context, engineUp bool) (vllmStartResolution, error) {
	puller, venv, release, manifest, variant, err := p.vllmStartPlan()
	if err != nil {
		return vllmStartResolution{release: release}, err
	}
	r := vllmStartResolution{puller: puller, venv: venv, release: release, target: manifest, variant: variant}
	st, _ := p.store.Load()
	if ms := st.VLLMModels[manifest.ModelID]; ms.State == catalog.ModelStateReady && ms.LocalPath != "" &&
		(ms.VariantID == "" || ms.VariantID == variant.VariantID) && dirExists(ms.LocalPath) {
		r.targetPath = ms.LocalPath
	}
	r.targetDownloading = p.pullInFlight(manifest.ModelID)
	var hasPrev bool
	r.prev, r.prevVariant, r.prevPath, hasPrev = vllmPreviousCandidate(st.Active, p.catalogManifests(), st,
		manifest.ModelID, venv.Version, dirExists, p.vllmStartableNow(ctx, st))
	facts := vllmTargetFacts{
		EngineUp:          engineUp,
		TargetModel:       manifest.ModelID,
		TargetVariant:     variant.VariantID,
		TargetOnDisk:      r.targetPath != "",
		TargetDownloading: r.targetDownloading,
		AllowPull:         p.cfg.AllowPull,
		AlreadyDispatched: p.vllmDispatched.seen(manifest.ModelID, variant.VariantID),
		HasPrevious:       hasPrev,
	}
	if s := p.vllmServing.Load(); s != nil {
		facts.ServingModel, facts.ServingVariant = s.ModelID, s.VariantID
	}
	r.plan = planVLLMTarget(facts)
	return r, nil
}

// vllmNoPullRefusal is the reason a start cannot begin when the chosen
// model's weights are absent and downloads are turned off.
func vllmNoPullRefusal(modelID string) string {
	return fmt.Sprintf("the weights for %s are not on this computer and downloads are turned off"+
		" (inference.allow_pull=false in agent.json)", modelID)
}

// vllmStartRefusal reports why a vLLM start cannot begin, or nil when it can.
//
// The untagged half of vllmStartPlan: engineController lives in an untagged
// file and must not name download.HFPuller, which exists only on this leg.
func (p *agentInferenceProvider) vllmStartRefusal() error {
	up := false
	if a := p.vllmAdapter(); a != nil {
		st := a.Health(context.Background()).State
		up = st == infruntime.StateReady || st == infruntime.StateStarting
	}
	r, err := p.resolveVLLMStart(context.Background(), up)
	r.release()
	if err != nil {
		return err
	}
	switch {
	case r.plan.Action == vllmRefuseNoPull:
		return errors.New(vllmNoPullRefusal(r.target.ModelID))
	case r.plan.Action == vllmWait && r.targetDownloading:
		// Nothing to start until they land; the finished download asks for
		// the engine on its way out (noteWeightsLanded).
		return fmt.Errorf("the weights for %s are still downloading; the engine starts when they land",
			r.target.ModelID)
	}
	return nil
}

// hfProgressPollInterval is how often the weights download's byte progress
// is re-read off disk. Matched to the executor's own reporting cadence —
// a faster poll would be discarded downstream, and a slower one would make
// a multi-gigabyte shard look stalled.
const hfProgressPollInterval = 2 * time.Second

// hfLister reads the repository's top level. A field so a test can answer
// without a network; nil is the real Hub client.
func (p *agentInferenceProvider) hfLister() download.HFFileLister {
	if p.hfFiles != nil {
		return p.hfFiles
	}
	return download.DefaultHFFileLister{}
}

// failHFPull records a pull that stopped before it began, the way a failed
// `hf download` is recorded below: a refresh keeps the model ready.
func (p *agentInferenceProvider) failHFPull(modelID string, refresh bool, why string) {
	_ = p.store.Update(func(s *catalog.State) {
		m := s.VLLMModels[modelID]
		if !refresh {
			m.State = catalog.ModelStateFailed
		}
		m.Error = why
		s.VLLMModels[modelID] = m
	})
}

// hfModelsRootOrState is the directory whose filesystem the weights will land
// on: the weights root once it exists, the state directory before the first
// download creates it.
func hfModelsRootOrState(stateDir string) string {
	if dirExists(hfModelsRoot(stateDir)) {
		return hfModelsRoot(stateDir)
	}
	return stateDir
}

// downloadHFWeights fetches the safetensors for variant into hfLocalDir and
// drives the model's state through downloading → verifying → ready, then
// records a local vLLM endpoint. Returns the local dir on success. Synchronous
// (callers run it either in the bootstrap goroutine or a pull-job goroutine).
//
// stopRequested, when non-nil, tells a stop somebody asked for (`waired
// models cancel`, `models rm`) from a download that failed: the first records
// nothing, because settleCancelledPull is about to drop the row, and "failed"
// would be a wrong answer — the same rule runPullJob follows for ollama.
func (p *agentInferenceProvider) downloadHFWeights(ctx context.Context, modelID string, variant catalog.Variant, puller *download.HFPuller, refresh bool, stopRequested func() bool) (string, error) {
	localDir := p.hfLocalDir(modelID, variant)
	// The directory is this download's until it returns, so every partial
	// file in it now is one a killed download left: huggingface_hub never
	// resumes them (download.SweepHFIncomplete), and they would otherwise
	// both hold the disk and count as progress in WatchHFLocalDir
	// (waired-agent#1519).
	release, err := p.hfDirs.acquire(ctx, localDir, func() {
		p.logger.Info("another download is writing to this model's directory; waiting for it",
			"model", modelID, "dir", localDir)
	})
	if err != nil {
		return "", err
	}
	defer release()
	sweepHFPartials(p.logger, localDir, "before download")

	// A refresh pull of an already-ready model keeps it servable
	// (state=ready) throughout so a transient error can't take healthy
	// serving down (#614); skip the downloading/verifying downgrades.
	if !refresh {
		_ = p.store.Update(func(s *catalog.State) {
			m := s.VLLMModels[modelID]
			m.State = catalog.ModelStateDownloading
			s.VLLMModels[modelID] = m
		})
	}
	defer p.dlProgress.forget(modelID)

	// Which files, and how many bytes they are. Both answers come from the
	// same listing, and neither existed before waired-agent#1298: the pull
	// took the whole repository (41.30 GB of openai/gpt-oss-20b against the
	// 13.79 GB a vLLM host loads), and the CLI's own output carries only a
	// per-file percentage, which the byte aggregator drops — so the wizard's
	// model row read 0 / 0 for the entire download.
	//
	// A listing that cannot be read is not a reason to refuse the pull: the
	// fetch falls back to the whole repository, exactly as it behaved
	// before, and the row goes back to reporting nothing.
	files, listErr := p.hfLister().ListTopLevel(ctx, variant.Source.RepoID, variant.Source.Revision)
	custom := catalog.IsCustomModelID(modelID)
	if custom && listErr == nil {
		files = download.CustomHFFiles(files)
	}
	switch {
	case custom && (listErr != nil || !download.HFHasSafetensors(files)):
		// A custom model is pulled narrowly or not at all: the whole-repository
		// fallback below would fetch whatever the repository holds, and the
		// import only vouched for its top-level safetensors
		// (waired-ai/waired#1480).
		err := fmt.Errorf("download: %s: the repository's top level has no safetensors weights at the imported commit, so there is nothing vLLM can load — choose another model, or import one whose safetensors weights are at the repository's top level", variant.Source.RepoID)
		if listErr != nil {
			err = fmt.Errorf("download: %s: could not list the repository's files on Hugging Face at the imported commit (%v); try again later", variant.Source.RepoID, listErr)
		}
		p.failHFPull(modelID, refresh, err.Error())
		return "", err
	case listErr != nil:
		p.logger.Warn("hf file listing unavailable; fetching the whole repository and reporting no byte progress",
			"model", modelID, "repo", variant.Source.RepoID, "err", listErr)
		files = nil
	case !download.HFHasWeights(files):
		// The top level is the right set for every repository the catalog
		// names today, and wrong for one that keeps its shards in a
		// subdirectory. Narrowing there would fetch the config and the
		// tokenizer, report 100%, and leave the engine to fail on a model
		// with no weights — so take the whole repository, as an unreadable
		// listing does.
		p.logger.Warn("hf file listing has no weights at the top level; fetching the whole repository",
			"model", modelID, "repo", variant.Source.RepoID, "files", len(files))
		files = nil
	default:
		p.logger.Info("hf pull scope", "model", modelID, "repo", variant.Source.RepoID,
			"files", len(files), "bytes", download.HFTotalBytes(files))
	}
	// Before a byte is fetched, as the ollama pull does (pull_size.go): a
	// multi-GB download that ends in a full disk costs the time and leaves
	// the disk full (waired-ai/waired#1480).
	if short := diskShortfallAt(hfModelsRootOrState(p.stateDir), download.HFTotalBytes(files)); short != "" {
		p.failHFPull(modelID, refresh, short)
		return "", fmt.Errorf("%w: %s", errDiskShort, short)
	}

	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	watchDone := make(chan struct{})
	if len(files) > 0 {
		download.AnnounceHFFiles(files, func(pr download.Progress) { p.dlProgress.observe(modelID, pr) })
		go func() {
			defer close(watchDone)
			download.WatchHFLocalDir(watchCtx, localDir, files, hfProgressPollInterval, func(pr download.Progress) {
				p.dlProgress.observe(modelID, pr)
			})
		}()
	} else {
		close(watchDone)
	}

	err = puller.Pull(ctx, variant.Source.RepoID, download.HFPullOpts{
		LocalDir: localDir,
		Revision: variant.Source.Revision,
		Files:    download.HFFileNames(files),
	}, func(pr download.Progress) {
		p.dlProgress.observe(modelID, pr)
		if pr.State == download.StateVerifying && !refresh {
			_ = p.store.Update(func(s *catalog.State) {
				m := s.VLLMModels[modelID]
				m.State = catalog.ModelStateVerifying
				s.VLLMModels[modelID] = m
			})
		}
	})
	// Cancel AND join. The deferred forget below drops this model's
	// progress, and a watcher still inside an emit pass would write a
	// pulling entry back after it — leaving a finished model reading as
	// downloading, which is what the wizard and `waired models ls` show.
	stopWatch()
	<-watchDone
	if err != nil {
		// A killed `hf download` cannot delete its own partial file, and
		// nothing will ever resume it; free the disk now rather than at
		// the next attempt.
		sweepHFPartials(p.logger, localDir, "after the download stopped")
		if stopRequested != nil && stopRequested() {
			p.logger.Info("hf pull stopped on request", "model", modelID, "repo", variant.Source.RepoID)
			return "", err
		}
		p.logger.Warn("hf pull failed", "model", modelID, "repo", variant.Source.RepoID, "err", err, "refresh", refresh)
		_ = p.store.Update(func(s *catalog.State) {
			m := s.VLLMModels[modelID]
			// A failed refresh pull keeps the model ready — the on-disk
			// weights still serve; record the error for observability only.
			if !refresh {
				m.State = catalog.ModelStateFailed
			}
			m.Error = err.Error()
			s.VLLMModels[modelID] = m
		})
		return "", err
	}

	_ = p.store.Update(func(s *catalog.State) {
		m := s.VLLMModels[modelID]
		m.State = catalog.ModelStateReady
		m.Error = ""
		m.HFRepo = variant.Source.RepoID
		m.LocalPath = localDir
		m.VariantID = variant.VariantID
		m.PulledAt = time.Now().UTC()
		s.VLLMModels[modelID] = m

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
	// The download runs the venv's own hf CLI, so the venv is held until
	// the job ends (waired-agent#1431).
	puller, _, release, err := p.vllmServingDeps()
	if err != nil {
		return fmt.Errorf("vllm HF pull unavailable: %w", err)
	}
	// A refresh pull of an already-ready model must not downgrade it —
	// serving continues from the on-disk weights and a failed re-pull
	// keeps it ready (#614). Mirrors the ollama path in PullModel.
	refresh := false
	if err := p.store.Update(func(s *catalog.State) {
		if s.VLLMModels[manifest.ModelID].State == catalog.ModelStateReady {
			refresh = true
			return
		}
		s.VLLMModels[manifest.ModelID] = catalog.ModelState{
			VariantID: variant.VariantID,
			HFRepo:    variant.Source.RepoID,
			State:     catalog.ModelStateQueued,
		}
	}); err != nil {
		release()
		return err
	}
	// spawnPull, not a bare `go`: it releases the model's in-flight slot
	// (#305b), puts HF downloads under pullsWG, which they escaped — so
	// waitForPulls() now joins them too (#377) — and clears the row a
	// cancelled job leaves behind (waired-agent#641).
	p.spawnPull(job, func() {
		defer release()
		p.runHFPullJob(ctx, job, variant, puller, refresh)
	})
	return nil
}

// runHFPullJob runs downloadHFWeights and commits activation on success —
// mirroring runPullJob for the ollama path. ctx MUST be the daemon's
// long-lived context: PullModel dispatches on backgroundCtx(), never on a
// request ctx, which net/http cancels the moment the handler returns
// (#305a). It is deliberately not re-wrapped in a self-cancelling ctx.
func (p *agentInferenceProvider) runHFPullJob(ctx context.Context, job *pullJob, variant catalog.Variant, puller *download.HFPuller, refresh bool) {
	if _, err := p.downloadHFWeights(ctx, job.modelID, variant, puller, refresh, job.requestedStop); err != nil {
		return
	}
	p.logger.Info("hf pull job completed", "model", job.modelID, "job", job.jobID)
	p.hfWeightsLanded(ctx, job.modelID, variant.VariantID)
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
	latched, latchedReason := false, ""
	if existing != nil {
		existingState = existing.Health(ctx).State
		if l, ok := existing.(interface{ FailureLatchedReason() (bool, string) }); ok {
			latched, latchedReason = l.FailureLatchedReason()
		}
	}
	engineUp := false
	switch decideVLLMBootstrap(existing, existingState, p.vllmIsParked(), latched, p.vllmProbeEngineUp.Load()) {
	case vllmBootstrapParked:
		// Two causes share the latch, and the log named only the
		// operator's: a stop for a model that did not fit read as one
		// someone asked for (waired-agent#1515).
		if p.parkedForLoadFailure() {
			p.logger.Info("vllm bootstrap: the engine is stopped because the chosen model did not start on this computer; not starting it",
				"state", existingState, "cause", p.parkedBecause().String(), "fix", "choose a different model")
			return
		}
		p.logger.Info("vllm bootstrap: the engine is stopped by the operator; not starting it",
			"state", existingState, "fix", "waired inference engine start")
		return
	case vllmBootstrapProbeHoldsTheCard:
		// The host-speed probe has its own engine up on this host's vLLM
		// port. It asks for a start when it stops, so nothing is lost by
		// standing down (waired-agent#1298).
		p.logger.Info("vllm bootstrap: the host-speed probe has an engine on the card; " +
			"starting when it stops")
		return
	case vllmBootstrapGaveUp:
		// Asked before the stop-and-respawn below, because that path
		// replaces the adapter and the latch does not survive the
		// replacement (waired-agent#1109). The ollama arm refuses the same
		// triggers in engine_bootstrap.go; this is its missing half.
		p.logger.Info("vllm bootstrap: automatic recovery has given up on this engine; leaving the latch set",
			"reason", latchedReason, "fix", "waired inference engine start")
		return
	case vllmBootstrapSkip:
		// Up — but it may be serving a model other than the chosen one,
		// which the plan below decides (waired-agent#1515).
		engineUp = true
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

	r, err := p.resolveVLLMStart(ctx, engineUp)
	// Held until an adapter is registered on it, which then holds it for as
	// long as it is the registered one; every return before that releases
	// it (waired-agent#1431).
	held := false
	defer func() {
		if !held {
			r.release()
		}
	}()
	if errors.Is(err, errVLLMNoModelChosen) {
		// Not a fault, so nothing is recorded: a refusal reaches the
		// surfaces as engine_failed, and telling an operator who is
		// still reading the model picker that their engine has failed
		// is how the vLLM wizard ended in an ERR box (waired-agent#1298).
		// The next trigger asks again — choosing a model is one.
		p.logger.Info("vllm bootstrap: " + err.Error())
		return
	}
	if err != nil {
		p.logger.Error("vllm bootstrap: "+err.Error(), "bundled", p.bundledModelID())
		p.refuseEngineBootstrap(err.Error())
		return
	}

	// The chosen model's weights, when they are not here, are fetched by
	// the ordinary pull path — cancellable, listed, one per model — and not
	// inside this start (waired-agent#1515). The engine keeps answering in
	// the meantime, or the previous model does; the finished download asks
	// for the engine again (noteWeightsLanded), which lands on the switch
	// below.
	if r.plan.Download {
		p.startVLLMTargetDownload(r.target, r.variant)
	}
	m, v, localPath, chosen := r.target, r.variant, r.targetPath, true
	switch r.plan.Action {
	case vllmKeep:
		if engineUp {
			p.logger.Info("vllm bootstrap: the running engine keeps answering",
				"serving", servingModelID(p.vllmServing.Load()), "chosen", r.target.ModelID,
				"chosen_on_disk", r.targetPath != "")
		}
		return
	case vllmWait:
		p.logger.Info("vllm bootstrap: nothing to start until the chosen model's weights are on disk",
			"model", r.target.ModelID, "downloading", r.targetDownloading || r.plan.Download)
		return
	case vllmRefuseNoPull:
		p.logger.Error("vllm bootstrap: weights absent and pulls disabled (allow_pull=false)", "model", r.target.ModelID)
		p.refuseEngineBootstrap(vllmNoPullRefusal(r.target.ModelID))
		return
	case vllmStartPrevious:
		p.logger.Info("vllm bootstrap: starting the previous model until the chosen one is on disk",
			"previous", r.prev.ModelID, "chosen", r.target.ModelID)
		m, v, localPath, chosen = r.prev, r.prevVariant, r.prevPath, false
	case vllmSwitchToTarget:
		// Drained first: the switch is minutes with nothing answering
		// here, and a turn cut mid-answer is worse than one that waits a
		// little for the drain budget.
		p.drainBeforeBounce(ctx, "vllm model switch")
		p.logger.Info("vllm bootstrap: switching the engine to the chosen model",
			"from", servingModelID(p.vllmServing.Load()), "to", r.target.ModelID)
		if existing != nil {
			if err := existing.Stop(ctx); err != nil {
				p.logger.Warn("vllm bootstrap: stopping the previous engine returned error", "err", err)
			}
		}
	}
	held = true // spawnVLLM hands it to the adapter it registers
	p.spawnVLLM(ctx, r.venv, r.release, m, v, localPath, chosen)
}

// servingModelID is the model id in s, or "" for none.
func servingModelID(s *vllmServingModel) string {
	if s == nil {
		return ""
	}
	return s.ModelID
}

// startVLLMTargetDownload starts the chosen model's download through the
// ordinary pull path, once per build per process (planVLLMTarget).
func (p *agentInferenceProvider) startVLLMTargetDownload(m catalog.Manifest, v catalog.Variant) {
	p.vllmDispatched.mark(m.ModelID, v.VariantID)
	if _, err := p.pullModelBuild(p.backgroundCtx(), m.ModelID, v.VariantID); err != nil {
		p.logger.Warn("vllm bootstrap: starting the chosen model's download failed", "model", m.ModelID, "err", err)
		return
	}
	p.logger.Info("vllm bootstrap: downloading the chosen model; the engine switches to it once it is on disk",
		"model", m.ModelID, "variant", v.VariantID)
}

// spawnVLLM builds the adapter for one model and brings it up. chosen says
// the model is the one chosen for this computer rather than the previous
// one answering in the meantime: only the chosen build takes the KV-cache
// type the person chose with it. It takes over the venv hold (release).
func (p *agentInferenceProvider) spawnVLLM(ctx context.Context, venv infruntime.InstallResult, release func(),
	manifest catalog.Manifest, variant catalog.Variant, localPath string, chosen bool) {
	python := filepath.Join(venv.BinDir, "python")
	hwProfile := p.profiler.Profile(ctx)
	tp := resolveVLLMTensorParallel(p.cfg.VLLMTensorParallel, hwProfile, p.logger)
	// #676: fp8 (e4m3) KV cache on Ada+ (compute_cap ≥ 8.9) halves KV to
	// roughly double the fittable window, unless the operator opted out.
	// The serve-time KV factor must match what the engine will actually
	// use so the #675 clamp sizes correctly (an fp8 host with an f16-sized
	// window would leave capacity on the table; the reverse would abort).
	// A user who chose fp16 is served fp16 exactly as the operator opt-out
	// is (waired-agent#1348): the choice and the setting are the same
	// instruction from two places.
	kvCacheDType, kvFactor := resolveVLLMKVCache(hwProfile,
		p.cfg.VLLMDisableFP8KV || (chosen && p.effectiveBuildChoice().KVCacheType == catalog.KVCacheFP16))
	// The serve-flag gate (waired-agent#885). activeVer is the "current"
	// symlink's version, which may predate this build on a host installed
	// by an older agent — and an unrecognised flag is an argparse exit 2
	// that costs the whole engine, not one feature. Decided before the
	// sizing, because it decides whether an MTP draft runs and the draft
	// is sized into the window (waired-ai/waired#1432).
	// The held venv's version, not a fresh read of `current`: that is the
	// venv the engine is spawned from, and a converge may have swapped
	// `current` since.
	activeVer := venv.Version
	serveFlags := vllmServeFlagsSupported(activeVer)
	if !serveFlags {
		p.logger.Warn("vllm venv predates this build's serve flags; starting without them",
			"venv_version", activeVer, "pinned", infruntime.VLLMPinnedVersion,
			"fix", "waired runtimes install vllm")
	}
	// Speculative decoding: ngram when the operator turned it on (#677),
	// else the build's own MTP head when the catalog gives it a draft
	// length (waired-ai/waired#1432).
	spec := router.VLLMSpeculative(variant, p.cfg.VLLMSpeculativeNgram, p.cfg.VLLMDisableMTP, serveFlags)
	// #675, #1434: --max-model-len is 200,704 or 1,048,576, whichever the
	// utilization budget holds, rather than the manifest window (an
	// unfittable window aborts vLLM startup — no spill-style degradation
	// exists). computeVLLMTuning has the cases outside the two.
	maxLen, tuning := computeVLLMTuning(manifest, variant, hwProfile, tp, p.cfg.VLLMGPUMemoryUtilization, kvFactor, spec)
	if tuning.Warning != "" {
		p.logger.Warn("vllm context sizing", "model", manifest.ModelID,
			"max_model_len", maxLen, "native", manifest.ContextLength, "note", tuning.Warning)
	}
	// #410: without a parser vLLM never populates tool_calls, so a coding
	// agent gets the model's call as prose. Resolved from the served
	// model, overridable per host.
	toolParser := resolveVLLMToolParser(manifest, p.cfg.VLLMToolParser)
	if toolParser == "" {
		p.logger.Warn("vllm tool calling disabled: no --tool-call-parser is known for this model; "+
			"the model's tool calls will arrive as text (set inference.vllm_tool_parser to override)",
			"model", manifest.ModelID)
	}
	batchedTokens := 0
	kvOffloadGiB := 0.0
	if serveFlags {
		batchedTokens = router.VLLMMaxNumBatchedTokens(maxLen, hwProfile, p.cfg.VLLMMaxNumBatchedTokens)
		var note string
		kvOffloadGiB, note = router.VLLMKVOffloadingGiB(p.cfg.VLLMKVOffloadingGiB, hwProfile)
		if note != "" {
			p.logger.Warn("vllm kv offloading adjusted", "detail", note)
		}
	}
	// Record what the engine is being launched with, not just what it is
	// told (waired-agent#1127). Before this the figure existed only in a
	// local and in one log line, so nothing downstream could ask how many
	// prompt tokens this engine prefills per step — which is what a
	// prefill measurement has to span several of. 0 when the venv predates
	// this build's serve flags: the flag is not passed, so the engine uses
	// its own default and we do not know it.
	tuning.PromptBatchTokens = batchedTokens
	// A build this computer already could not start, in this same shape
	// on this same machine, is not tried again: the engine is held off
	// with the reason instead (waired-agent#1515). Choosing the model
	// again, or a change to the computer, is what lifts it.
	shape := vllmLoadShape(tuning, kvCacheDType, router.VLLMMaxNumSeqs(p.cfg.VLLMMaxNumSeqs))
	if chosen {
		if rec, blocked := p.vllmLoadBlocked(ctx, manifest, variant, shape); blocked {
			p.logger.Info("vllm bootstrap: this computer already could not start this model in this configuration; not starting it again",
				"model", manifest.ModelID, "variant", variant.VariantID, "reason", rec.Reason, "failed_at", rec.FailedAt)
			p.parkVLLMForOutOfMemory(p.vllmBlockKey(manifest, variant, shape), rec.Reason)
			release()
			return
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
		SpeculativeConfig:         spec.Config,
		ToolCallParser:            toolParser,
		ReasoningParser:           resolveVLLMReasoningParser(manifest),
		LoadFormat:                vllmLoadFormat(manifest),
		EnablePromptTokensDetails: serveFlags,
		MaxNumBatchedTokens:       batchedTokens,
		MaxNumSeqs:                router.VLLMMaxNumSeqs(p.cfg.VLLMMaxNumSeqs),
		KVOffloadingGiB:           kvOffloadGiB,
		LogDir:                    logDir,
		Spawner:                   infruntime.DefaultSpawner{},
		PendingExits:              p.engineExits,
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
	p.vllmServing.Store(&vllmServingModel{ModelID: manifest.ModelID, VariantID: variant.VariantID})
	p.holdVLLMVenvForAdapter(venv.Dir, release)

	// Same reason the ollama arm clears it before its loop: while these
	// attempts are in flight the honest answer is "still trying"
	// (waired-agent#1093).
	p.clearEngineStartExhausted()

	movedOn := func() bool { return chosen && p.vllmChoiceMovedOn(manifest.ModelID) }
	end, ensureErr := runVLLMStartAttempts(ctx, p.logger, adapter.EnsureRunning, movedOn, vllmRetryWait)
	switch end {
	case vllmAttemptsLatched:
		p.logger.Info("vllm bootstrap: start refused by a latch; leaving it set", "err", ensureErr)
		return
	case vllmAttemptsCancelled:
		return
	case vllmAttemptsMovedOn:
		raw, _ := os.ReadFile(filepath.Join(logDir, "engine.log"))
		lastSpawn := infruntime.LastEngineLogSpawn(string(raw))
		if mem, reason := vllmStartFailedForMemory(lastSpawn, tuning.WeightsOverBudget); mem {
			p.noteVLLMLoadFailure(ctx, manifest, variant, shape, reason, "", signer.LoadFailureMemory, vllmEngineMaxWindow(lastSpawn))
		} else if kind := vllmStartFailureKind(lastSpawn); kind != "" {
			p.noteVLLMLoadFailure(ctx, manifest, variant, shape, vllmModelFailureHint(lastSpawn), "", kind, 0)
		}
		p.logger.Info("vllm bootstrap: a different model was chosen while this one was starting; starting that one instead",
			"model", manifest.ModelID, "err", ensureErr)
		p.requestEngineStart("a different model was chosen")
		return
	}
	if end == vllmAttemptsFailed {
		// The engine's own log is the only place the cause is written.
		// Every attempt above is in it now, each behind its own banner
		// (#878); the hint names the cause of the attempt the loop ended
		// on, and engine_log below is where a reader finds the others —
		// which is what a run whose attempts failed differently needs.
		raw, _ := os.ReadFile(filepath.Join(logDir, "engine.log"))
		hint := vllmStartupHint(string(raw), p.cfg.ResolvedVLLMPort())
		if tuning.WeightsOverBudget {
			// Not the KV cache: the weights alone are larger than what vLLM
			// may use here, so no KV setting helps (waired-agent#1515).
			hint = "the model's weights are larger than the GPU memory vLLM may use on this computer — choose a smaller model"
		}
		p.logger.Error("vllm did not become ready after retries; local inference unavailable until restart",
			"err", ensureErr, "hint", hint, "engine_log", filepath.Join(logDir, "engine.log"))
		// The hint used to end here, in a log line nobody reads on a
		// desktop. It is the only sentence that names a cause, so it goes
		// where the surfaces look: Health().LastErr, which runtimeStatusFor
		// publishes as runtimes[].last_error and `waired status` renders as
		// the ⚠ line (waired-agent#1026).
		adapter.SetStartFailureReason(hint)
		// And on the provider: this loop spends three strikes against a
		// give-up budget of four, so it stops trying without latching, and
		// the one reader that asks only about a give-up — the wizard's
		// engine row — reported the install DONE over an engine that could
		// not start (waired-agent#1093). Read back off the adapter so that
		// row quotes the same bytes as runtimes[].last_error.
		p.noteEngineStartExhausted(adapter.Health(ctx).LastErr)
		// The argv only on the failure path, and only here: it carries
		// paths and no secrets, and without it a flag rejection cannot
		// be matched to the flag that caused it.
		p.logger.Warn("vllm start-up argv", "args", adapter.CommandArgsForDiagnostics())
		// A chosen model that did not fit is remembered and the engine held
		// off with the reason, so a restart does not repeat the same failed
		// start (waired-agent#1515). The previous model answering in the
		// meantime is not the choice and is not recorded against.
		// A build this engine cannot run here at all fails every attempt
		// the same way, and is recorded and held off the same way, with its
		// own words (waired-ai/waired#1480).
		if chosen {
			lastSpawn := infruntime.LastEngineLogSpawn(string(raw))
			if mem, reason := vllmStartFailedForMemory(lastSpawn, tuning.WeightsOverBudget); mem {
				p.recordVLLMLoadFailure(ctx, manifest, variant, shape, reason, hint, signer.LoadFailureMemory, vllmEngineMaxWindow(lastSpawn))
			} else if kind := vllmStartFailureKind(lastSpawn); kind != "" {
				p.recordVLLMLoadFailure(ctx, manifest, variant, shape, hint, "", kind, 0)
			}
		}
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
		"kv_cache_dtype", kvCacheDType,
		"speculative", spec.Method, "speculative_tokens", spec.Tokens,
		"tool_call_parser", toolParser,
		"prompt_tokens_details", serveFlags,
		"max_num_batched_tokens", batchedTokens,
		"kv_offloading_gib", kvOffloadGiB)
	// The engine now runs from the venv `current` names, and the previous
	// adapter's hold went when this one replaced it: whatever a converge
	// superseded can go (waired-agent#1431).
	go p.vllmVenvKeeper().reclaim(context.WithoutCancel(ctx), p.logger)

	// Commit the ActiveSelection (Runtime is derived from servingEngine(),
	// == vllm here). activateBundledIfUnset fills a fresh install's empty
	// slot; activatePreferredIfNeeded lands an explicit preferred choice.
	p.activateBundledIfUnset(manifest.ModelID, variant.VariantID)
	p.activatePreferredIfNeeded(manifest.ModelID, variant.VariantID)
}
