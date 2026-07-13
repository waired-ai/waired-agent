// Ollama per-model serve tuning (#621).
//
// The bundled `ollama serve` historically got no context configuration at
// all, so every model silently loaded at Ollama's default context window
// (32768 in the pinned 0.31 line) regardless of the manifest's
// context_length — Claude Code's 35–50k-token opening prompt was then
// front-truncated and the model lost its tool schemas and instructions.
// This file computes the env that fixes that:
//
//	OLLAMA_CONTEXT_LENGTH  manifest context_length, clamped down so
//	                       weights + KV cache fit host memory un-spilled
//	OLLAMA_KV_CACHE_TYPE   q8_0 by default (near-lossless, halves KV)
//	OLLAMA_NUM_PARALLEL    1 unless doubling KV still costs no context
//	OLLAMA_FLASH_ATTENTION 1 (KV quantization is a silent f16 no-op
//	                       without flash attention)
//
// The values are computed once per process spawn from the tuning target
// (preferred > active > bundled model) and the hardware profile, and
// verified after the first model load (inference_ollama_verify.go).
package main

import (
	"fmt"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/catalog/scoring"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/router"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// ollamaContextFloor is the smallest OLLAMA_CONTEXT_LENGTH we ever export:
// the pinned engine's own default. Setting less would make hosts strictly
// worse than the pre-#621 behavior; when even the floor doesn't fit
// un-spilled we keep it and warn instead (a truncated-context model is
// broken silently, a spilling one is slow visibly).
const ollamaContextFloor = 32768

// ollamaLargeBatch is the generation ubatch the tuning selects for the
// intentional-spill (#624) configuration on discrete GPUs (#642). At the
// 200k coding floor a 512→2048 ubatch raised prefill +38–44 % with a
// negligible decode cost on the 24 GB reference host
// (docs/reports/20260705-num-batch-512-vs-2048-24gb.md). It is only set
// where the model spills: Ollama's own automaticGenerationBatch already
// picks 2048 when the model is GPU-resident, and drops to 512 exactly on
// the spilled hosts this overrides. Delivered via a derived model
// (inference_ollama_derived.go), not an env var.
const ollamaLargeBatch = 2048

// ollamaCPUOnlyRAMHeadroomGB is the system headroom subtracted from total
// RAM when sizing the context budget on CPU-only hosts (consistent with
// scoring.SuggestMinRAMGB's 2 GB OS allowance plus margin for the agent
// itself).
const ollamaCPUOnlyRAMHeadroomGB = 4

// ollamaTuning is the env decision for one (manifest, variant, host).
type ollamaTuning struct {
	infruntime.ModelTuning
	// KVFactor is the scoring KV factor the ContextLength was sized with
	// (q8_0 = 0.5); the verify pass re-sizes with KVFactorF16 when the
	// engine fell back.
	KVFactor float64
	// kvBytesPerTokFP16 carries the variant's per-token KV figure so the
	// verify pass can build size expectations without a catalog re-lookup.
	kvBytesPerTokFP16 int
	// ExpectedSpillFraction is non-zero when the ContextLength was set
	// to the #624 coding floor DELIBERATELY overshooting the no-spill
	// window (bounded-spill gate passed): the predicted /api/ps spill
	// fraction. The verify pass widens its spill tolerance around it
	// instead of treating the planned spill as a failure.
	ExpectedSpillFraction float64
}

// kvFactorFor maps an OLLAMA_KV_CACHE_TYPE value to its scoring factor.
func kvFactorFor(kvType string) float64 {
	switch kvType {
	case "q8_0":
		return scoring.KVFactorQ8_0
	case "q4_0":
		return scoring.KVFactorQ4_0
	default:
		return scoring.KVFactorF16
	}
}

// ollamaTuningBudgetGB returns the decimal-GB memory budget available for
// weights + KV on this host: effective VRAM minus the same engine
// overhead the fit gate reserves (weight-scaled on discrete GPUs, flat
// on UMA), or, on CPU-only hosts (spilling to RAM is the design there),
// total RAM minus OS headroom. Returns 0 when the budget is unknown.
func ollamaTuningBudgetGB(hw hardware.Profile, weightGB float64) float64 {
	if eff := hw.EffectiveVRAMMB(); eff > 0 {
		mib := eff - router.OllamaVRAMOverheadMB(hw, weightGB)
		if mib <= 0 {
			return 0
		}
		return float64(mib) * (1 << 20) / 1e9
	}
	if hw.RAMTotalGB > ollamaCPUOnlyRAMHeadroomGB {
		return float64(hw.RAMTotalGB - ollamaCPUOnlyRAMHeadroomGB)
	}
	return 0
}

// computeOllamaTuning sizes the serve tuning for the given model/variant
// on this host. kvType is the OLLAMA_KV_CACHE_TYPE to assume ("q8_0" on
// the first pass; the verify pass retries with "f16" after a fallback).
//
// When any sizing input is unknown (no per-token KV figure, no weight
// estimate, no memory budget) ContextLength stays 0 — the context var is
// then NOT exported and the engine keeps its own default, which is
// exactly the pre-#621 behavior. We never guess a window we can't size.
func computeOllamaTuning(m catalog.Manifest, v catalog.Variant, hw hardware.Profile, kvType string) ollamaTuning {
	return computeOllamaTuningOpts(m, v, hw, kvType, true, 0)
}

// recommendedParallel is the VRAM-safe engine-parallelism ceiling: how many
// full-window request slots the KV budget holds. Ollama reserves ctx ×
// num_parallel tokens of KV, and the sizing already knows the budget holds
// maxCtx tokens total, so floor(maxCtx/ctx) slots fit without spilling. It
// never shrinks the window to add slots (the "parallelism never costs context"
// invariant / the ~200k coding-window policy). Floors at 1; also 1 when the
// host is spilling (maxCtx < ctx) or unsizable.
func recommendedParallel(maxCtx, ctx int) int {
	if ctx <= 0 || maxCtx <= 0 {
		return 1
	}
	if n := maxCtx / ctx; n > 1 {
		return n
	}
	return 1
}

// finalizeParallel applies the operator's max-concurrent-requests override to
// the computed tuning. operatorParallel <= 0 keeps the auto-sized NumParallel.
// A positive value is HONORED even above RecommendedMaxParallel — the admin
// accepted the trade in the UI (informed override), and the post-load
// verify-degrade recompute (which carries no override) backstops it down to the
// safe auto value if the requested parallelism can't load. A Warning is attached
// when it exceeds the recommendation so `waired doctor` / the status surface it.
func finalizeParallel(t *ollamaTuning, operatorParallel int) {
	if operatorParallel <= 0 {
		return
	}
	t.NumParallel = operatorParallel
	if t.RecommendedMaxParallel > 0 && operatorParallel > t.RecommendedMaxParallel {
		t.Warning = joinTuningWarn(t.Warning, fmt.Sprintf(
			"concurrency set to %d, above this host's recommended max of %d — each parallel slot reserves its own KV-cache VRAM, so this may spill to system RAM, slow every request, or fail to load",
			operatorParallel, t.RecommendedMaxParallel))
	}
}

// computeOllamaTuningOpts is computeOllamaTuning with the intentional-
// spill branch switchable: the verify pass's degrade recomputes pass
// false so a sizing that just proved unreliable is never re-entered
// (the f16 recompute also stays no-spill — compounding an f16 KV with
// a spill on an unmeasured GPU class is not a bet worth one restart).
//
// operatorParallel is the admin's max-concurrent-requests override (0 = auto):
// when > 0 it replaces the auto-sized NumParallel (see finalizeParallel), and
// RecommendedMaxParallel is reported regardless so the UI can advise the trade.
func computeOllamaTuningOpts(m catalog.Manifest, v catalog.Variant, hw hardware.Profile, kvType string, allowIntentionalSpill bool, operatorParallel int) (t ollamaTuning) {
	// The operator override is applied at every exit (named return + defer) so
	// each sizing branch just records its RecommendedMaxParallel and returns.
	defer func() { finalizeParallel(&t, operatorParallel) }()
	t = ollamaTuning{
		ModelTuning: infruntime.ModelTuning{
			ModelID:     m.ModelID,
			VariantID:   v.VariantID,
			NumParallel: 1,
			KVCacheType: kvType,
		},
		KVFactor:          kvFactorFor(kvType),
		kvBytesPerTokFP16: v.KVBytesPerTokenFP16,
	}

	budgetGB := ollamaTuningBudgetGB(hw, v.EstimatedWeightGB)
	maxCtx := scoring.MaxContextTokens(v.EstimatedWeightGB, v.KVBytesPerTokenFP16, t.KVFactor, budgetGB)
	if maxCtx <= 0 {
		if budgetGB > 0 && v.EstimatedWeightGB > 0 && v.KVBytesPerTokenFP16 > 0 {
			// Inputs were known and the weights alone exceed the budget:
			// the floor applies (spill is already certain, a truncated
			// window on top would break the model twice).
			maxCtx = 0
		} else {
			// Unknown sizing: recommend a single slot (we cannot prove more fit).
			t.RecommendedMaxParallel = 1
			return t // unknown sizing inputs: leave ContextLength 0
		}
	}

	ctx := maxCtx
	if m.ContextLength > 0 && ctx > m.ContextLength {
		ctx = m.ContextLength
	}
	// #624 intentional spill (discrete GPUs only): when the no-spill
	// window can't reach the coding floor, widen it anyway — but only
	// as far as decode stays at the coding-agent selection floor. The
	// spilled fraction executes on a single CPU thread in the bundled
	// engine (#664: 158.6 tok/s no-spill vs ~85 at 13.4% measured), so
	// spill past OllamaIntentionalSpillCapExpected costs more decode
	// than the extra window is worth under the 60 tok/s true-decode
	// floor (#670/#765; at that floor the cap clamps to the selection
	// gate's 0.20, so the anchor host serves the full 200704 floor at
	// ~85 tok/s instead of trimming to ~165k as it did under the 100
	// floor). When the full floor would still exceed the cap, serve
	// the largest window that holds it instead. UMA is excluded (one
	// memory pool, no spill semantics); the tone is informational —
	// every branch is a working configuration, not an error.
	if floorCtx := router.EffectiveContextFloor(m); allowIntentionalSpill && ctx < floorCtx && !hw.UnifiedMemory && len(hw.GPUs) > 0 {
		target := floorCtx
		expected := router.OllamaExpectedSpillFraction(
			v.EstimatedWeightGB, v.KVBytesPerTokenFP16, t.KVFactor, target, hw)
		if expected > router.OllamaIntentionalSpillCapExpected {
			// Full floor spills past the speed cap: fall back to the
			// biggest window the cap affords (< floorCtx here, since
			// the floor itself just exceeded the cap).
			target = router.OllamaMaxContextAtSpill(
				v.EstimatedWeightGB, v.KVBytesPerTokenFP16, t.KVFactor,
				router.OllamaIntentionalSpillCapExpected, hw)
			expected = router.OllamaExpectedSpillFraction(
				v.EstimatedWeightGB, v.KVBytesPerTokenFP16, t.KVFactor, target, hw)
		}
		if target > ctx && expected > 0 && expected <= router.OllamaIntentionalSpillCapExpected {
			ctx = target
			t.ExpectedSpillFraction = expected
			t.ContextLength = ctx
			// #642: this is the spilled discrete-GPU config where Ollama's
			// automatic batch sizing falls back to 512; force the larger
			// ubatch (delivered via a derived model) for the prefill win.
			// The verify pass widens its spill tolerance for the compute
			// buffer this adds (inference_ollama_verify.go).
			t.NumBatch = ollamaLargeBatch
			if ctx >= floorCtx {
				t.Warning = fmt.Sprintf(
					"context window set to %d tokens for coding-agent workloads; about %.0f%% of the model is expected to sit in system RAM (larger window traded for some decode speed)",
					ctx, expected*100)
			} else {
				t.Warning = fmt.Sprintf(
					"context window set to %d tokens (below the ~200k coding target: widening further would spill past ~%.1f%% of the model to system RAM and drop decode below the %.0f tok/s floor)",
					ctx, router.OllamaIntentionalSpillCapExpected*100, router.CodingAgentSelectionFloorTokps)
			}
			// Already spilling to reach the window: a single slot only (adding
			// parallel slots would multiply the spill).
			t.RecommendedMaxParallel = 1
			return t
		}
	}
	if ctx < ollamaContextFloor {
		floored := ollamaContextFloor
		if m.ContextLength > 0 && m.ContextLength < floored {
			floored = m.ContextLength
		}
		ctx = floored
		if maxCtx < ctx {
			t.Warning = fmt.Sprintf(
				"context window kept at %d though host memory fits ~%d tokens un-spilled; the model may spill to system RAM and slow down",
				ctx, maxCtx)
		}
	}
	t.ContextLength = ctx

	// Parallelism never costs context: only serve >1 request slot when
	// the full manifest window is already granted AND doubling the KV
	// allocation (Ollama reserves num_ctx × num_parallel) still fits.
	if m.ContextLength > 0 && ctx == m.ContextLength && maxCtx >= 2*ctx {
		t.NumParallel = 2
	}
	// The VRAM-safe ceiling the admin's override is advised against exceeding:
	// how many full-window slots the KV budget holds.
	t.RecommendedMaxParallel = recommendedParallel(maxCtx, ctx)
	return t
}

// Env renders the OLLAMA_* variables for OllamaAdapter.SetModelEnv.
// ContextLength 0 (unknown sizing) omits the context var so the engine
// keeps its own default. NumBatch is deliberately absent: the pinned
// engine has no batch env, so it is delivered through a derived model
// (inference_ollama_derived.go) instead.
func (t ollamaTuning) Env() []string {
	env := make([]string, 0, 4)
	if t.ContextLength > 0 {
		env = append(env, fmt.Sprintf("OLLAMA_CONTEXT_LENGTH=%d", t.ContextLength))
	}
	env = append(env,
		"OLLAMA_KV_CACHE_TYPE="+t.KVCacheType,
		fmt.Sprintf("OLLAMA_NUM_PARALLEL=%d", t.NumParallel),
		// KV-cache quantization silently degrades to f16 without flash
		// attention; the engine still auto-disables FA per-model where
		// unsupported (that case is what the post-load verify catches).
		"OLLAMA_FLASH_ATTENTION=1",
	)
	return env
}

// resolveTuningTarget picks the model the serve tuning is sized for:
// the preferred model (already folded into cfg from preferred-model.json
// — a tray switch restarts the agent, so spawn-time resolution tracks
// it), else the persisted active selection, else the bundled default.
// The variant is the one actually on disk when state records a Ready
// pull; otherwise the one the pinned engine would pull first. ok=false
// (no resolvable model or variant) means "export no tuning env" — we
// never size for a guessed model.
func resolveTuningTarget(cfg agentconfig.InferenceConfig, manifests []catalog.Manifest, state catalog.State) (catalog.Manifest, catalog.Variant, bool) {
	var m catalog.Manifest
	ok := false
	if cfg.PreferredModelID != "" {
		m, ok = catalog.LookupByAlias(cfg.PreferredModelID, manifests)
	}
	if !ok && state.Active != nil && state.Active.Runtime == catalog.RuntimeOllama {
		m, ok = catalog.LookupByAlias(state.Active.ModelID, manifests)
	}
	if !ok && cfg.BundledModelID != "" {
		m, ok = catalog.LookupByAlias(cfg.BundledModelID, manifests)
	}
	if !ok {
		return catalog.Manifest{}, catalog.Variant{}, false
	}

	if ms, found := state.Models[m.ModelID]; found && ms.State == catalog.ModelStateReady {
		for _, v := range m.Variants {
			if v.VariantID == ms.VariantID {
				return m, v, true
			}
		}
	}
	v, pullable := router.FirstPullableVariant(m, catalog.RuntimeOllama, infruntime.OllamaPinnedVersion)
	if !pullable {
		return catalog.Manifest{}, catalog.Variant{}, false
	}
	return m, v, true
}

// modelDecisionReasons renders the #624 context-floor status of the
// resolved tuning target in the engine-decision log idiom, and returns
// an extra warning to append for sub-floor targets (preferred override
// or a stale/best-effort config) so `waired status` / doctor carry it
// via TuningWarning too. Informational tone throughout — every case is
// a working configuration.
func modelDecisionReasons(cfg agentconfig.InferenceConfig, m catalog.Manifest, t ollamaTuning) (reasons []string, extraWarning string) {
	switch {
	case t.ExpectedSpillFraction > 0:
		reasons = append(reasons, fmt.Sprintf(
			"%s serves a ~%dk coding window with ~%.0f%% of the model expected in system RAM",
			m.ModelID, t.ContextLength/1024, t.ExpectedSpillFraction*100))
	case !router.MeetsNativeContextFloor(m):
		if cfg.PreferredModelID != "" {
			extraWarning = fmt.Sprintf(
				"preferred model overrides the ~200k coding-agent context floor (native window %d tokens)",
				m.ContextLength)
		} else {
			extraWarning = fmt.Sprintf(
				"configured model is below the ~200k coding-agent context floor (native window %d tokens); best-effort serving",
				m.ContextLength)
		}
		reasons = append(reasons, extraWarning)
	case t.ContextLength >= router.CodingAgentContextFloorTokens:
		reasons = append(reasons, fmt.Sprintf(
			"%s serves the ~200k coding window fully GPU-resident (ctx %d)",
			m.ModelID, t.ContextLength))
	case t.ContextLength > 0:
		reasons = append(reasons, fmt.Sprintf(
			"%s serves a %d-token window on this host (below the ~200k coding target, no spill)",
			m.ModelID, t.ContextLength))
	}
	return reasons, extraWarning
}
