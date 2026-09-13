package main

import (
	"context"
	"sync"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/management"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// speedTuningPatience is how long a measurement waits for the engine's
// post-load verification before measuring the tuning as applied. The
// verification can restart the engine with a smaller window, and a
// 32,768-token request sent into that restart measures a dying engine; but
// on an engine that never logs the line the verification reads (a vLLM build
// that words it differently) it never completes, and the host must still be
// measured. Fifteen minutes is the wait the prefill ladder this replaced
// used, for the same reason.
const speedTuningPatience = 15 * time.Minute

// speedTuningWatch remembers when this daemon first saw the served
// selection's tuning unverified, so the patience above is counted from then.
type speedTuningWatch struct {
	mu    sync.Mutex
	key   string
	since time.Time
}

// pending reports whether key's unverified tuning is still within its
// patience.
func (w *speedTuningWatch) pending(key string, verified bool, now time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if verified || key == "" {
		w.key, w.since = "", time.Time{}
		return false
	}
	if w.key != key {
		w.key, w.since = key, now
	}
	return now.Sub(w.since) < speedTuningPatience
}

// currentServeTuning is the serving engine's applied tuning, read without
// waiting.
func (p *agentInferenceProvider) currentServeTuning() infruntime.ModelTuning {
	if p == nil {
		return infruntime.ModelTuning{}
	}
	if p.servingEngine() == catalog.RuntimeVLLM {
		if r, ok := p.vllmAdapter().(appliedTuningReader); ok {
			return r.AppliedTuning()
		}
		return infruntime.ModelTuning{}
	}
	if p.ollama == nil {
		return infruntime.ModelTuning{}
	}
	return p.ollama.AppliedTuning()
}

// speedDeps is the one description of what a speed measurement measures
// with, read live: the daemon's own loop, a setup generation and a person's
// `waired runtimes benchmark` all measure through it, so the three cannot
// describe one selection three ways.
func (p *agentInferenceProvider) speedDeps(ctx context.Context, mode string) BenchDeps {
	kind, port := p.probeTarget(p.cfg)
	var gpu hardware.GPU
	if p.profiler != nil {
		if prof := p.profiler.Profile(ctx); len(prof.GPUs) > 0 {
			gpu = prof.GPUs[0]
		}
	}
	tuning := p.currentServeTuning()
	deps := BenchDeps{
		EngineKind:      kind,
		EnginePort:      port,
		EngineVersion:   p.servingEngineVersion(ctx),
		EngineReady:     p.EngineReady,
		EngineQuiet:     p.engineQuietForBench,
		EngineClaim:     p.claimBench,
		EngineGen:       p.engineProcessGen,
		EngineModel:     p.activeEngineModel(),
		VariantID:       p.activeVariantID(),
		ModelID:         p.activeModelID(),
		VariantSHA:      p.activeVariantSHA(),
		GPUModel:        gpu.Model,
		VRAMTotalMB:     gpu.VRAMTotalMB,
		DriverVersion:   gpu.DriverVersion,
		WarmSlots:       p.WarmConversationSlots,
		Cache:           p.benchCache,
		Logger:          p.logger,
		AppliedWindow:   tuning.ContextLength,
		KVCacheType:     tuning.KVCacheType,
		NumParallel:     tuning.NumParallel,
		ServingInFlight: p.servingInFlight,
		SkipCacheLoad:   mode == management.BenchmarkModeRerun,
		Selected:        p.activeVariantID,
		Progress:        p.publishBenchProgress,
	}
	deps.StoredMeasurement = func() (BenchResult, bool) { return p.storedSpeedMeasurement(deps) }
	if p.speedDepsHook != nil {
		p.speedDepsHook(&deps)
	}
	if tuning.ContextLength > 0 {
		deps.TuningPending = p.speedTuning.pending(
			bootBenchSelectionKey(deps)+"\x00"+itoa(int(p.engineProcessGen())), tuning.Verified, time.Now())
	}
	return deps
}

// storedSpeedMeasurement answers from the state ledger when the disk cache
// has nothing: bench.json may be cleared at any time, and the ledger is the
// record decision 7 names. Used only for the same weights on the same
// engine release, GPU and serving configuration — the key the cache uses —
// and only for a finished figure.
func (p *agentInferenceProvider) storedSpeedMeasurement(deps BenchDeps) (BenchResult, bool) {
	if p == nil || p.store == nil || deps.VariantSHA == "" || deps.EngineVersion == "" {
		return BenchResult{}, false
	}
	st, err := p.store.Load()
	if err != nil {
		return BenchResult{}, false
	}
	m, ok := st.MeasuredVariants[deps.VariantSHA]
	if !ok || m.TurnSeconds <= 0 ||
		m.EngineKind != deps.EngineKind || m.EngineVersion != deps.EngineVersion ||
		m.GPUModel != deps.GPUModel || m.VRAMTotalMB != deps.VRAMTotalMB || m.DriverVersion != deps.DriverVersion ||
		m.AppliedWindow != deps.AppliedWindow || m.KVCacheType != deps.KVCacheType || m.NumParallel != deps.NumParallel {
		return BenchResult{}, false
	}
	return BenchResult{
		TokensPerSec: m.MeasuredTokps,
		DecodeTokps:  m.MeasuredTokps,
		PrefillTokps: m.PrefillTokps,
		DepthTokens:  m.DepthTokens,
		TurnSeconds:  m.TurnSeconds,
		Samples:      m.Samples,
		SpreadPct:    m.SpreadPct,
		Method:       m.Method,
		VariantID:    m.VariantID,
		ModelID:      m.ModelID,
	}, true
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// activeModelID is the committed active selection, read from this
// provider's store. It used to have a twin, modelIDForActive, that answered
// the same question from the process-wide default path because the
// benchmark deps were built without a provider; the deps take one now
// (waired-agent#1206) and this is the only reader left.
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
