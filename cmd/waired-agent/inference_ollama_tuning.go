// Ollama per-model serve tuning (#621).
//
// The bundled `ollama serve` historically got no context configuration at
// all, so every model silently loaded at Ollama's default context window
// (32768 in the pinned 0.31 line) regardless of the manifest's
// context_length — Claude Code's 35–50k-token opening prompt was then
// front-truncated and the model lost its tool schemas and instructions.
// This file computes the env that fixes that:
//
//	OLLAMA_CONTEXT_LENGTH  the rung of hostfit.OllamaServedWindows this
//	                       host serves the model at (waired-agent#587)
//	OLLAMA_KV_CACHE_TYPE   q8_0 (near-lossless, halves KV) only where
//	                       halving KV actually buys context — see
//	                       planOllamaKV; f16 otherwise
//	OLLAMA_NUM_PARALLEL    1 unless doubling KV still costs no context
//	OLLAMA_FLASH_ATTENTION 1 ONLY alongside a quantized KV cache (KV
//	                       quantization is a silent f16 no-op without it);
//	                       omitted on f16 so the engine chooses
//
// The values are computed once per process spawn from the tuning target
// (preferred > active > bundled model) and the hardware profile, and
// verified after the first model load (inference_ollama_verify.go).
package main

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/router"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// ollamaContextFloor is the pinned engine's own default window. The
// verify pass's size heuristics still reference it; the sizing itself
// stopped using a separate floor when the serve window became a rung of
// hostfit.OllamaServedWindows (waired-agent#587) — rungs never sit below
// the model's own window, so there is nothing to floor.
//
// "The pinned engine's own default" is a claim about a version, so it is
// re-taken with the pin rather than inherited: on 0.33.2, loading a
// model with no num_ctx spawned the runner with -c 32768 and /api/ps
// reported context_length 32768 (waired-agent#1132).
//
// On 0.33.3 that default is no longer one number. The engine logs
// "vram-based default context" at startup and derives it from the VRAM
// it found: 32768 on a 37.4 GiB Mac, 262144 on a 102.2 GiB Strix Halo
// (waired-agent#1193, 2026-09-06). The value below is unaffected, because
// it is a floor this agent applies to its OWN request and the agent
// always exports OLLAMA_CONTEXT_LENGTH — the engine's default never
// governs a window waired asked for. What changed is only the sentence
// above: the number it names is now the small-VRAM end of a range.
const ollamaContextFloor = 32768

// ollamaMaxAutoParallel is the most request slots the sizing ever grants
// itself (see the NumParallel branch below). planOllamaKV uses the same
// figure, which is what makes "choosing f16 cannot cost a slot" a proof
// rather than a hope.
const ollamaMaxAutoParallel = 2

// ollamaKVAuto is the kvType value meaning "decide": planOllamaKV then takes
// the default the owner decided — q4_0 wherever the build allows it, the
// smallest type above it where it does not, on a CPU-only host as on any
// other (hostfit.ResolveKVCacheType; decision 2 of
// docs/decisions/20260913/2355-catalog-variant-kv-and-residency-rulings.md,
// and docs/decisions/20260916/2250-cpu-kv-cache-defaults-to-q4-0.md).
// Any explicit type is a PIN — that is how the verify pass's f16 degrade, the
// ollamaKVOverrideEnv test lane and a user's chosen type all express intent,
// and it is why every existing caller that passes "q8_0"/"f16" keeps its
// exact behaviour.
const ollamaKVAuto = "auto"

// ollamaKVOverrideEnv pins the KV/flash-attention pair the tuning would
// otherwise choose. It exists so the q8_0 + flash-attention combination keeps
// a real nightly exercise against a real engine even on hosts where the auto
// decision drops it: the only CI legs that run ollama at all are CPU-only
// (the GPU runner is vLLM-only), so without this the combination would be
// covered by env-string unit tests and nothing else.
// Values: f16 | q8_0 | q4_0; anything else is ignored.
const ollamaKVOverrideEnv = "WAIRED_OLLAMA_KV_CACHE_TYPE"

// ollamaKVPlan is the KV half of the tuning decision. The two fields are
// INSEPARABLE: Ollama silently degrades a quantized KV cache to f16 without
// flash attention, so "q8_0" without FA is a lie, and FA without a quantized
// cache is a forced code path bought for nothing.
type ollamaKVPlan struct {
	Type           string
	FlashAttention bool
}

// planOllamaKV turns the kvType request into the KV/flash-attention pair.
// Pure: the env pin and the user's choice are resolved by the caller
// (ollamaKVRequestFor).
//
// "auto" is the default ladder: hostfit.ResolveKVCacheType with no request,
// which is q4_0 when the build lists it (kv_cache_types) and q8_0 when it
// does not, with flash attention, on every host. A CPU-only host was held at
// f16 until 2026-09-16 to stay off the CPU + flash-attention + quantized-KV
// path waired-agent#29's segfault was suspected on; 404 requests with a
// quantised cache on ollama 0.34.0 found no crash, and the owner moved CPU
// hosts onto the ladder
// (docs/decisions/20260916/2250-cpu-kv-cache-defaults-to-q4-0.md).
func planOllamaKV(m catalog.Manifest, v catalog.Variant, hw hardware.Profile, requested string) ollamaKVPlan {
	if requested != ollamaKVAuto {
		// A pin. f16 needs no flash attention; a quantized pin does.
		return ollamaKVPlan{Type: requested, FlashAttention: requested != catalog.KVCacheF16}
	}
	t := hostfit.ResolveKVCacheType(catalog.RuntimeOllama, v, hw.HostFit(), nil, "")
	return ollamaKVPlan{Type: t, FlashAttention: t != catalog.KVCacheF16}
}

// ollamaKVRequestFor is the kvType production passes to the sizing for
// serving v of m: the env pin when a test lane set one, the KV-cache type
// the user chose with this model when there is one (resolved against the
// build and host, so a type the build cannot serve falls to the default
// rung), and "auto" otherwise. Impure by design, so computeOllamaTuning*
// stays pure.
func ollamaKVRequestFor(cfg agentconfig.InferenceConfig, m catalog.Manifest, v catalog.Variant, hw hardware.Profile) string {
	if pin := ollamaKVRequest(); pin != ollamaKVAuto {
		return pin
	}
	if cfg.PreferredKVCacheType == "" || cfg.PreferredModelID == "" {
		return ollamaKVAuto
	}
	if _, ok := catalog.LookupByAlias(cfg.PreferredModelID, []catalog.Manifest{m}); !ok {
		return ollamaKVAuto
	}
	return hostfit.ResolveKVCacheType(catalog.RuntimeOllama, v, hw.HostFit(), nil, cfg.PreferredKVCacheType)
}

// ollamaKVRequest is the kvType production passes to the sizing: auto unless a
// test lane pinned one. Impure by design, so computeOllamaTuning* stays pure.
func ollamaKVRequest() string {
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv(ollamaKVOverrideEnv))); v {
	case "f16", "q8_0", "q4_0":
		return v
	default:
		return ollamaKVAuto
	}
}

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
	// window (bounded-spill gate passed): the predicted share of the
	// weights llama.cpp's fit puts in system RAM
	// (hostfit.OllamaPredictPlacement, waired-agent#1337). The verify pass
	// compares the engine's own placement with it instead of treating the
	// planned spill as a failure.
	ExpectedSpillFraction float64

	// PlannedGPULayers / PlannedTotalLayers are the predicted llama.cpp
	// "offloaded N/M layers" at ContextLength; PlannedCPUWeightMB is the
	// predicted weight the fit moves to system RAM, and HostWeightsMB the
	// input-layer weights that live there regardless. PlannedLayerWeightMB
	// is one repeating layer's weight, the verify pass's tolerance.
	// PlannedDeviceWeightMB is the model buffer the engine is predicted to
	// place in GPU-addressable memory when nothing spills (the projector
	// blob, which loads outside load_tensors, excluded) — the figure the
	// verify pass subtracts the logged device buffer from. All 0 when the
	// variant carries no GGUF layout or the host no accelerator.
	PlannedGPULayers      int
	PlannedTotalLayers    int
	PlannedCPUWeightMB    int
	PlannedDeviceWeightMB int
	HostWeightsMB         int
	PlannedLayerWeightMB  int
}

// kvFactorFor maps an OLLAMA_KV_CACHE_TYPE value to the KV-cache size it
// allocates relative to f16, from ggml's block layout (q8_0 is 34/64, not a
// half — waired-agent#1337).
func kvFactorFor(kvType string) float64 {
	return hostfit.OllamaKVCacheFactor(kvType)
}

// computeOllamaTuning sizes the serve tuning for the given model/variant
// on this host. kvType is the OLLAMA_KV_CACHE_TYPE to assume ("q8_0" on
// the first pass; the verify pass retries with "f16" after a fallback).
//
// When any sizing input is unknown (no per-token KV figure, no weight
// estimate, no memory budget) ContextLength stays 0 — the context var is
// then NOT exported and the engine keeps its own default, which is
// exactly the pre-#621 behavior. We never guess a window we can't size.
func computeOllamaTuning(m catalog.Manifest, v catalog.Variant, hw hardware.Profile, kvType string, observed ollamaObservedServe) ollamaTuning {
	return computeOllamaTuningOpts(m, v, hw, kvType, 0, 0, observed)
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

// ollamaObservedServe is what the engine was last seen actually serving,
// so the sizing can stop re-requesting a slot count the runner already
// declined. Ollama silently caps OLLAMA_NUM_PARALLEL when the per-slot KV
// cache does not fit the configured window; #763 reads the runner's own
// -np back off its command line, and this carries that reading into the
// next sizing pass rather than leaving it as status-only telemetry
// (waired-ai/waired-agent#846).
//
// The zero value means "nothing observed", which is what every caller
// that is not the serve reconcile passes.
type ollamaObservedServe struct {
	ModelID       string
	VariantID     string
	ContextLength int
	// NumParallel is the runner's OWN parallelism (ModelTuning's
	// ObservedNumParallel), never the value we asked for.
	NumParallel int
}

// grantedFor returns the observed slot count when the observation was
// made for this exact model, variant and window, and 0 otherwise. The
// identity check is the whole safety of the feedback: a slot count the
// engine declined at one window says nothing about another, and a degrade
// recompute deliberately moves to a different window.
func (o ollamaObservedServe) grantedFor(m catalog.Manifest, v catalog.Variant, ctx int) int {
	if o.NumParallel <= 0 || ctx <= 0 {
		return 0
	}
	if o.ModelID != m.ModelID || o.VariantID != v.VariantID || o.ContextLength != ctx {
		return 0
	}
	return o.NumParallel
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

// computeOllamaTuningOpts is computeOllamaTuning with the rung ladder
// cappable: ceilingCtx > 0 drops every rung above it, which is how the
// verify pass's degrade recomputes step DOWN — a rung that just proved
// unreliable is never re-entered, and never stepped back up to
// (waired-agent#587). 0 considers the full ladder.
//
// operatorParallel is the admin's max-concurrent-requests override (0 = auto):
// when > 0 it replaces the auto-sized NumParallel (see finalizeParallel), and
// RecommendedMaxParallel is reported regardless so the UI can advise the trade.
//
// observed is what the engine was last seen serving; the zero value opts
// out. It only ever lowers the auto-sized slot count, and never the
// operator's override — see the clamp below.
func computeOllamaTuningOpts(m catalog.Manifest, v catalog.Variant, hw hardware.Profile, kvType string, ceilingCtx, operatorParallel int, observed ollamaObservedServe) (t ollamaTuning) {
	// The operator override is applied at every exit (named return + defer) so
	// each sizing branch just records its RecommendedMaxParallel and returns.
	defer func() { finalizeParallel(&t, operatorParallel) }()
	// Resolve the KV/flash-attention pair FIRST: every sizing branch below is
	// a function of KVFactor, and nothing downstream may ever see "auto".
	kv := planOllamaKV(m, v, hw, kvType)
	t = ollamaTuning{
		ModelTuning: infruntime.ModelTuning{
			ModelID:        m.ModelID,
			VariantID:      v.VariantID,
			NumParallel:    1,
			KVCacheType:    kv.Type,
			FlashAttention: kv.FlashAttention,
		},
		KVFactor:          kvFactorFor(kv.Type),
		kvBytesPerTokFP16: v.KVBytesPerTokenFP16,
	}

	// The sizing itself lives in proto/hostfit, because the control
	// plane's wizard and the agent's picker have to reach the same answer
	// about this host as the engine is actually started with — that is
	// what "recommended = declares the coding window here" means
	// (waired-ai/waired#1056 decision 3). This function's job is the
	// engine-facing consequences: which ubatch, how many slots, and what
	// to tell the user.
	hf := hw.HostFit()
	plan := hostfit.OllamaPlannedRungFor(m, v, hf, kv.Type, ceilingCtx)
	if plan.ContextLength <= 0 {
		// Unknown sizing: recommend a single slot (we cannot prove more fit).
		t.RecommendedMaxParallel = 1
		// And still the 200k tier, not the engine's own default: ollama
		// picks 4,096 to 262,144 from VRAM when no OLLAMA_CONTEXT_LENGTH is
		// set (docs/knowledges/20260906/0230-ollama-pin-0333.md §5), and a
		// window between the tiers is one this product does not
		// serve (owner decision 2026-09-16, waired-agent#1396; #1434).
		// Unproven, so WindowFits stays false. A model whose own window is
		// under the tier — only CI's internal_only one — keeps the engine
		// default, as it always did.
		if m.ContextLength >= hostfit.ServingWindow200k {
			t.ContextLength = hostfit.ServingWindow200k
		}
		return t
	}
	maxCtx, ctx := plan.NoSpillCapacityTokens, plan.ContextLength
	t.ContextLength = ctx
	t.WindowFits = plan.Fits
	t.ExpectedSpillFraction = plan.ExpectedSpillFraction
	if hf.HasGPU() && hf.OllamaVRAMBudgetMB() > 0 {
		place := hostfit.OllamaPredictPlacement(v, hf, kv.Type, ctx, 1)
		t.PlannedGPULayers, t.PlannedTotalLayers = place.GPULayers, place.TotalLayers
		t.PlannedCPUWeightMB = place.CPUWeightMB
		est := hostfit.OllamaEstimateMemory(v, hf, kv.Type, ctx, 1)
		t.HostWeightsMB = est.HostWeightsMB
		if g := v.GGUF; g != nil && g.BlockCount > g.NextNLayers {
			t.PlannedLayerWeightMB = int(g.RepeatingBytes / int64(g.BlockCount-g.NextNLayers) >> 20)
			t.PlannedDeviceWeightMB = est.DeviceWeightsMB - int(g.ProjectorBytes>>20)
		}
	}

	if plan.ExpectedSpillFraction > 0 {
		t.Warning = fmt.Sprintf(
			"context window set to %d tokens for coding-agent workloads; %s expected to sit in system RAM (larger window traded for some decode speed)",
			ctx, plannedSpillAmount(t))
		// Already spilling to reach the window: a single slot only (adding
		// parallel slots would multiply the spill).
		t.RecommendedMaxParallel = 1
		return t
	}
	if maxCtx < ctx {
		// The rung is being kept though the budget does not hold it
		// un-spilled (a CPU-only or unified host below the lowest rung —
		// Fits=false above): a truncated-context model is broken
		// silently, a spilling one is slow visibly.
		t.Warning = fmt.Sprintf(
			"context window kept at %d though host memory fits ~%d tokens un-spilled; the model may spill to system RAM and slow down",
			ctx, maxCtx)
	}

	// Parallelism never costs context: only serve >1 request slot when
	// the full window is already granted AND doubling the KV allocation
	// (Ollama reserves num_ctx × num_parallel) still fits.
	//
	// "Full" is the window this product would serve — the top rung of
	// hostfit.OllamaServedWindows — not the manifest's native figure. The
	// two used to be the same thing; since #552 capped the sizing at the
	// rung, comparing against the manifest would never be equal on a
	// 262144-native model and would silently withdraw the second slot
	// from every host that has one.
	if ceiling := hostfit.OllamaCeilingWindow(m); ceiling > 0 &&
		ctx == ceiling && ollamaSlotsFit(v, hf, kv.Type, ctx, ollamaMaxAutoParallel, maxCtx) {
		t.NumParallel = ollamaMaxAutoParallel
	}
	// The VRAM-safe ceiling the admin's override is advised against exceeding:
	// how many full-window slots the KV budget holds.
	t.RecommendedMaxParallel = recommendedParallel(maxCtx, ctx)
	// The engine's own answer wins over the estimate above. Ours prices a
	// slot at its KV cache; the runner also charges a prompt cache, context
	// checkpoints and, on a multimodal variant, a vision tower — so on some
	// hosts it grants fewer slots than this arithmetic offers and logs the
	// reduction. Re-asking every reconcile reserves memory the runner will
	// decline again and republishes the same warning, so once the runner has
	// answered for THIS model at THIS window, that answer is the ceiling
	// (waired-ai/waired-agent#846). RecommendedMaxParallel follows it too: a
	// measured refusal is better evidence of the ceiling than the estimate,
	// and leaving it high would advise an operator toward a slot the engine
	// has already refused. This narrows the gap's damage, not the gap: what
	// the per-slot price omits is still open on #846.
	if granted := observed.grantedFor(m, v, ctx); granted > 0 {
		t.NumParallel = min(t.NumParallel, granted)
		t.RecommendedMaxParallel = min(t.RecommendedMaxParallel, granted)
	}
	return t
}

// ollamaSlotsFit reports whether slots full-window request slots fit the
// accelerator outright. With a GGUF layout the whole load is priced at that
// slot count — every slot keeps its own KV cache and recurrent state — and
// without one the older rule stands: the no-spill capacity holds the windows.
func ollamaSlotsFit(v catalog.Variant, h hostfit.Host, kvType string, ctx, slots, maxCtx int) bool {
	if v.GGUF != nil && h.HasGPU() && h.OllamaVRAMBudgetMB() > 0 {
		return hostfit.OllamaEstimateMemory(v, h, kvType, ctx, slots).DeviceMB() <= h.OllamaVRAMBudgetMB()
	}
	return maxCtx >= slots*ctx
}

// plannedSpillAmount words the predicted placement of a spilling plan: in
// layers where the variant's layout is known, as a share of the model
// otherwise.
func plannedSpillAmount(t ollamaTuning) string {
	if t.PlannedTotalLayers > 0 {
		return fmt.Sprintf("%d of %d layers", t.PlannedTotalLayers-t.PlannedGPULayers, t.PlannedTotalLayers)
	}
	return fmt.Sprintf("about %.0f%% of the model", t.ExpectedSpillFraction*100)
}

// Env renders the OLLAMA_* variables for OllamaAdapter.SetModelEnv.
// ContextLength 0 (unknown sizing) omits the context var so the engine
// keeps its own default. There is deliberately no generation-batch var:
// the engine sizes that itself from the window and its own memory
// prediction, and overriding it is what waired-agent#1079 retired.
func (t ollamaTuning) Env() []string {
	env := make([]string, 0, 4)
	if t.ContextLength > 0 {
		env = append(env, fmt.Sprintf("OLLAMA_CONTEXT_LENGTH=%d", t.ContextLength))
	}
	env = append(env,
		"OLLAMA_KV_CACHE_TYPE="+t.KVCacheType,
		fmt.Sprintf("OLLAMA_NUM_PARALLEL=%d", t.NumParallel),
	)
	if t.FlashAttention {
		// KV-cache quantization silently degrades to f16 without flash
		// attention; the engine still auto-disables FA per-model where
		// unsupported (that case is what the post-load verify catches).
		// Omitted on an f16 cache: there is no KV saving to protect, and
		// forcing FA there buys llama.cpp's least-exercised path for free.
		env = append(env, "OLLAMA_FLASH_ATTENTION=1")
	}
	return env
}

// applyModelDecisionReasons folds modelDecisionReasons' output into a
// freshly computed tuning: the extra warning joins whatever the sizing
// already put there, and the reasons go to the log.
//
// It exists because the same six lines were written out at the boot and
// spawn-time tuning sites and NOT at the third one — the in-process
// model switch (#812) — so a model switched without a restart served
// with no decision warning at all until something restarted the agent,
// including the below-context-floor warning. effectiveCfg's own doc
// already names modelDecisionReasons as a helper that has to observe the
// post-switch preference; there was simply no call there to observe it.
func applyModelDecisionReasons(cfg agentconfig.InferenceConfig, m catalog.Manifest, tune ollamaTuning, logger *slog.Logger) ollamaTuning {
	reasons, extraWarn := modelDecisionReasons(cfg, m, tune)
	if extraWarn != "" {
		tune.Warning = joinTuningWarn(tune.Warning, extraWarn)
	}
	if logger != nil {
		for _, r := range reasons {
			logger.Info("model decision", "reason", r)
		}
	}
	return tune
}

// resolveTuningTarget picks the model the serve tuning is sized for:
// the preferred model (already folded into cfg from preferred-model.json
// — an operator switch always bounces ollama serve, so spawn-time
// resolution tracks it; before waired#812 the whole agent restarted and
// it tracked for that reason instead), else the persisted active
// selection, else the bundled default.
// The variant is the one actually on disk when state records a Ready
// pull; otherwise the one the pinned engine would pull first. ok=false
// (no resolvable model or variant) means "export no tuning env" — we
// never size for a guessed model.
//
// Two of the three legs take a retired name to its successor (#200) and
// the middle one deliberately does not. The config legs are INSTRUCTIONS
// — "size the engine for the model I asked for" — and a name that outran
// the catalog should still size for something. state.Active is an
// OBSERVATION of what this host actually downloaded and is serving; a
// host running the old weights must be tuned for the old weights, and
// substituting there would size the engine for a model that is not on
// disk.
func resolveTuningTarget(cfg agentconfig.InferenceConfig, manifests []catalog.Manifest, state catalog.State) (catalog.Manifest, catalog.Variant, bool) {
	var m catalog.Manifest
	ok := false
	if cfg.PreferredModelID != "" {
		m, _, ok = catalog.ResolveModel(cfg.PreferredModelID, manifests)
	}
	if !ok && state.Active != nil && state.Active.Runtime == catalog.RuntimeOllama {
		m, ok = catalog.LookupByAlias(state.Active.ModelID, manifests)
	}
	if !ok && cfg.BundledModelID != "" {
		m, _, ok = catalog.ResolveModel(cfg.BundledModelID, manifests)
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
// resolved tuning target in the engine-decision log idiom, and returns an
// extra warning for a rung this host's memory was not shown to hold, so
// `waired status` / doctor carry it via TuningWarning too. Informational
// tone throughout — every case is a working configuration.
//
// It also used to warn about a target whose OWN window was below the ~200k
// floor ("preferred model overrides …" / "configured model is below …").
// That left with waired-ai/waired-agent#1400: the catalog admits only
// builds whose window reaches the floor, and the owner had the machinery
// built around sub-200k models removed (decisions 3 and 4 of
// docs/decisions/20260916/0340). cfg stays in the signature for the
// callers and is no longer read.
func modelDecisionReasons(cfg agentconfig.InferenceConfig, m catalog.Manifest, t ollamaTuning) (reasons []string, extraWarning string) {
	switch {
	case t.ExpectedSpillFraction > 0:
		reasons = append(reasons, fmt.Sprintf(
			"%s serves a ~%dk coding window with %s expected in system RAM",
			m.ModelID, t.ContextLength/1024, plannedSpillAmount(t)))
	case t.ContextLength >= router.CodingAgentContextFloorTokens && t.WindowFits:
		// A prediction, and worded as one: whether the layers really landed
		// in GPU memory is the engine's to report, and the verify pass logs
		// what it reported (waired-agent#1330). A host with no accelerator
		// makes no GPU claim at all.
		switch {
		case t.PlannedTotalLayers > 0:
			reason := fmt.Sprintf(
				"%s is sized for the ~200k coding window (ctx %d), predicted to hold %d of %d layers in VRAM",
				m.ModelID, t.ContextLength, t.PlannedGPULayers, t.PlannedTotalLayers)
			if t.HostWeightsMB > 0 {
				reason += fmt.Sprintf("; %.1f GB of input embedding weights stay in system RAM", float64(t.HostWeightsMB)*(1<<20)/1e9)
			}
			reasons = append(reasons, reason)
		default:
			reasons = append(reasons, fmt.Sprintf(
				"%s is sized for the ~200k coding window (ctx %d)",
				m.ModelID, t.ContextLength))
		}
	case t.ContextLength > 0:
		// A rung this host's memory was not shown to hold (WindowFits
		// false — the forced lowest rung, waired-agent#587). Served, and
		// therefore declared: what the spill costs is decode speed, not
		// window size, so the warning names the slowdown rather than
		// claiming the device takes no work (waired-ai/waired-agent#657).
		extraWarning = fmt.Sprintf(
			"%s serves a %d-token window with part of the model in system RAM, which this "+
				"host's memory could not be shown to hold — the window is declared to the "+
				"mesh and Claude Code sessions can route here, but turns are slower than on "+
				"a host holding the model in GPU memory",
			m.ModelID, t.ContextLength)
		reasons = append(reasons, extraWarning)
	}
	return reasons, extraWarning
}
