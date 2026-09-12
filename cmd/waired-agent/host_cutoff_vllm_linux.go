//go:build linux

package main

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/router"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// The vLLM half of the host-speed probe.
//
// ollama loads a model per request, so its probe only had to ask. vLLM
// holds exactly one model per process and reserves the KV pool at
// start-up, so measuring the probe model means running an engine on it and
// then stopping it — which is also why this has to happen BEFORE the
// operator chooses, not after: after the choice the engine is serving the
// chosen model, and displacing it to measure a 1.7 GB probe would cost a
// restart and a reload of the model the operator is waiting for.
//
// That ordering is the point of waired-agent#1298. The ollama path is
// engine install -> small model -> measure -> choose -> download -> serve,
// and the vLLM path was engine install -> [start refused] -> choose ->
// download -> serve, with no measurement anywhere in it. This is the step
// that was missing.

// vllmProbeWindow is the context window the probe engine is started with:
// two probe depths plus the margin the ollama path asks ollama for, which
// is what makes a full-depth prompt fit alongside its own completion.
//
// Deliberately small. The KV pool is reserved at start-up, so asking for
// the coding window here would make the probe refuse to start on exactly
// the cards the measurement is for — and nothing is served from this
// engine, so a wide window would buy nothing.
const vllmProbeWindow = hostCutoffWindowSlots*hostfit.HostCutoffProbeDepthTokens + hostCutoffWindowMargin

// vllmProbeServedName is what the probe engine answers to. It is not the
// repo id the serving path uses, so a request that reached this engine by
// accident fails on the model name rather than being answered by a model
// nobody selected (waired-agent#736: a probe is not a selection).
const vllmProbeServedName = "waired-host-speed-probe"

// vllmProbeStartTimeout bounds bringing the probe engine up. The first
// start on a host also pays flashinfer's CUDA kernel compilation, which is
// the cost this step exists to expose early — measured at 13.7 s to first
// token cold against 1.5 s warm on an RTX PRO 4000 Blackwell — so the
// budget is the engine's ordinary start budget rather than a tight one.
const vllmProbeStartTimeout = 15 * time.Minute

// measureHostCutoffVLLM downloads the probe model's vLLM variant, runs an
// engine on it, measures, and stops it. It returns the same shape the
// ollama probe returns.
//
// The engine it starts is NOT registered in the runtime registry: nothing
// must route to it, nothing must report it as this host's engine, and
// stopping it must not look like the serving engine going down.
func (p *agentInferenceProvider) measureHostCutoffVLLM(ctx context.Context, variant catalog.Variant, onWeightsReady func()) (hostCutoffMeasurement, error) {
	puller, python, err := p.vllmServingDeps()
	if err != nil {
		return hostCutoffMeasurement{}, fmt.Errorf("probe engine unavailable: %w", err)
	}
	localPath, err := p.downloadHFWeights(ctx, hostfit.HostCutoffProbeModelID, variant, puller, false)
	if err != nil {
		return hostCutoffMeasurement{}, fmt.Errorf("probe weights: %w", err)
	}

	if onWeightsReady != nil {
		onWeightsReady()
	}

	hw := p.profiler.Profile(ctx)
	kvCacheDType, _ := resolveVLLMKVCache(hw, p.cfg.VLLMDisableFP8KV)
	adapter := infruntime.NewVLLMAdapter(infruntime.VLLMConfig{
		Python:               python,
		Host:                 "127.0.0.1",
		Port:                 p.cfg.ResolvedVLLMPort(),
		Model:                localPath,
		ServedModelName:      vllmProbeServedName,
		MaxModelLen:          vllmProbeWindow,
		DType:                variant.DType,
		GPUMemoryUtilization: p.cfg.VLLMGPUMemoryUtilization,
		TensorParallelSize:   resolveVLLMTensorParallel(p.cfg.VLLMTensorParallel, hw, p.logger),
		KVCacheDType:         kvCacheDType,
		// The probe engine gets the same ceiling the serving one does. On
		// a hybrid-mamba model vLLM's default of 256 is a start-up
		// refusal rather than a concurrency choice (waired-agent#1298),
		// and a probe that could not start would report the host
		// unmeasurable for a reason that is ours.
		MaxNumSeqs: router.VLLMMaxNumSeqs(p.cfg.VLLMMaxNumSeqs),
		LogDir:     filepath.Join(p.stateDir, "runtimes", "vllm", "logs"),
		Spawner:    infruntime.DefaultSpawner{},
		Parked:     p.vllmIsParked,
		// No OnUnhealthy / OnStartFailed. Those record strikes and set the
		// give-up latch for the engine this host SERVES with; a probe that
		// could not start has said what it needs to say by returning an
		// error, and latching the serving engine over it would take local
		// inference away from a host that has not tried to serve yet.
	})

	// Claimed BEFORE the spawn and cleared after the stop, so a model
	// chosen while this runs cannot spawn the serving engine over the
	// probe's on the same port (waired-agent#1298). bootstrapVLLM reads it
	// and stands down; the release below asks it to try again.
	p.vllmProbeEngineUp.Store(true)
	defer func() {
		p.vllmProbeEngineUp.Store(false)
		p.requestEngineStart("host speed: the probe engine has stopped")
	}()

	startCtx, cancelStart := context.WithTimeout(ctx, vllmProbeStartTimeout)
	defer cancelStart()
	if err := adapter.EnsureRunning(startCtx); err != nil {
		// Stop anyway: EnsureRunning can fail after the process is up
		// (a readiness or served-name check), and a probe engine nobody
		// registered would then hold the port and the VRAM with no owner.
		_ = adapter.Stop(context.WithoutCancel(ctx))
		return hostCutoffMeasurement{}, fmt.Errorf("probe engine did not start: %w", err)
	}
	// WithoutCancel so a cancelled measurement still tears the engine
	// down. A probe that outlived its own context would hold the whole KV
	// pool against the model the operator is about to choose.
	defer func() {
		if err := adapter.Stop(context.WithoutCancel(ctx)); err != nil {
			p.logger.Warn("host speed: stopping the probe engine returned an error", "err", err)
		}
	}()

	p.logger.Info("host speed: the probe engine is up; measuring",
		"model", hostfit.HostCutoffProbeModelID, "variant", variant.VariantID,
		"max_model_len", vllmProbeWindow, "endpoint", adapter.BaseURL())

	return measureHostCutoffOpenAI(ctx, openAICutoffDeps{
		BaseURL:    adapter.BaseURL(),
		Model:      vllmProbeServedName,
		Logger:     p.logger,
		HTTPClient: p.hostCutoffClient,
		Nonce:      fmt.Sprintf("hostcutoff-%d", time.Now().UnixNano()),
	})
}
