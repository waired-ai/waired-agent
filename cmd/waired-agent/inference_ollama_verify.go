// Post-load verification of the Ollama serve tuning (#621).
//
// Exporting OLLAMA_CONTEXT_LENGTH / OLLAMA_KV_CACHE_TYPE is necessary but
// not sufficient: KV-cache quantization silently degrades to f16 on
// models/backends without flash attention (ollama/ollama#13337), and a
// sizing estimate that ran slightly hot spills layers to system RAM —
// measured at −39..48% decode on discrete GPUs. Both failure modes are
// invisible from the request path, so after the first model load we
// inspect /api/ps and, on positive evidence, recompute the sizing and
// restart the engine ONCE. Every uncertain outcome keeps the engine as-is
// (the same "never make it worse" constraint as the #290 backend probe).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/platform/proclist"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// tuningVerdict classifies one post-load /api/ps inspection.
type tuningVerdict int

const (
	// tuningOK: the tuning applied and the model is fully resident.
	tuningOK tuningVerdict = iota
	// tuningInconclusive: no model could be loaded / ps unreachable /
	// the size signal is too small to discriminate. Never acted on.
	tuningInconclusive
	// tuningF16Fallback: the KV cache came out ~f16-sized despite a
	// q8_0 request — the engine fell back (no flash attention).
	tuningF16Fallback
	// tuningSpill: the loaded model reports size_vram < size on a
	// discrete GPU: layers spilled to system RAM beyond what the
	// tuning planned (the #624 intentional spill widens the tolerance).
	tuningSpill
	// tuningOKPlannedSpill: the model spilled, but within the bound the
	// #624 intentional-spill tuning planned for — a working
	// configuration, reported informationally, never degraded.
	tuningOKPlannedSpill
	// tuningGpuNotEngaged: the model reports size_vram == 0 on a
	// discrete GPU: no VRAM is being used for the KV cache (full
	// CPU fallback). This is not a spill; the tuning is ineffective
	// because the GPU is not engaged.
	tuningGpuNotEngaged
)

// f16DetectMinMarginBytes is the minimum gap between the expected q8_0
// and f16 KV sizes for the size heuristic to be meaningful; below it,
// graph-buffer noise dominates and the check abstains.
const f16DetectMinMarginBytes = 1_500_000_000

// spillAbsoluteToleranceMax caps the measured spill fraction the verify
// pass tolerates around an intentional spill, even when 2× the expected
// fraction would allow more. Above ~25% the decode penalty stops being
// "some speed traded for window" and the no-spill fallback is better.
const spillAbsoluteToleranceMax = 0.25

// generationBatchBufferBytes is the extra VRAM the larger #642 ubatch
// (ollamaLargeBatch) allocates for its generation compute buffer —
// Ollama's own surcharge estimate for a 2048 batch. When the tuning
// forced that batch (NumBatch >= ollamaLargeBatch), this buffer displaces
// weights into system RAM, so the verify pass widens its spill tolerance
// by this many bytes' worth of the model before treating the spill as an
// unplanned f16/oversize failure. Measured on the 24 GB reference host:
// 512→2048 moved spill 13.45 %→18.69 %, ~1.36 GB, within this 2 GiB bound
// (docs/reports/20260705-num-batch-512-vs-2048-24gb.md).
const generationBatchBufferBytes = 2 << 30

// verifyOllamaTuning inspects the loaded model and classifies the
// outcome. tag is the Ollama tag the tuning was sized for. Modern Ollama
// runs a per-model llama-server with its own -c, so verification is
// per-model: when the target tag is not the loaded model — e.g. a previous
// model still resident in /api/ps right after a model swap — the pass
// abstains (tuningInconclusive) instead of comparing the configured window
// against a FOREIGN runner, which used to emit a false "OLLAMA_CONTEXT_LENGTH
// did not apply" warning (waired#763). The returned detail is human-readable
// (log / warning material).
func verifyOllamaTuning(ctx context.Context, client *http.Client, baseURL string, t ollamaTuning, tag string, hw hardware.Profile) (tuningVerdict, string) {
	var ps psResponse
	if err := getJSON(ctx, client, baseURL+"/api/ps", probeHTTPTimeout, &ps); err != nil {
		return tuningInconclusive, fmt.Sprintf("/api/ps error: %v", err)
	}
	// target is the model this tuning is verified against: the tag it was
	// sized for. When nothing is loaded we load it; if the caller gave no
	// tag we fall back to whatever can be loaded and verify THAT model.
	target := tag
	if len(ps.Models) == 0 {
		if target == "" {
			var err error
			if target, err = firstOllamaTag(ctx, client, baseURL); err != nil || target == "" {
				return tuningInconclusive, "no model available to verify tuning"
			}
		}
		if err := loadOllamaModel(ctx, client, baseURL, target); err != nil {
			return tuningInconclusive, fmt.Sprintf("verify model load failed: %v", err)
		}
		if err := getJSON(ctx, client, baseURL+"/api/ps", probeHTTPTimeout, &ps); err != nil || len(ps.Models) == 0 {
			return tuningInconclusive, "model not visible in /api/ps after load"
		}
	}

	// Match the target model's own runner. A different model still resident
	// (the model-swap race) is not a valid witness for this tuning, so we
	// abstain rather than cross-wire two models (waired#763).
	psm, found := psModel{}, false
	for _, m := range ps.Models {
		if m.Name == target {
			psm, found = m, true
			break
		}
	}
	if !found {
		return tuningInconclusive, fmt.Sprintf(
			"target model %q not loaded (loaded: %s); deferring tuning verification",
			target, loadedModelNames(ps))
	}

	// Context application check for the target model's own runner. Ollama
	// has reported both num_ctx and num_ctx × num_parallel in /api/ps across
	// versions — accept either before concluding the env was ignored.
	ctxDetail := ""
	if t.ContextLength > 0 && psm.ContextLength > 0 &&
		psm.ContextLength != t.ContextLength &&
		psm.ContextLength != t.ContextLength*t.NumParallel {
		ctxDetail = fmt.Sprintf(
			"engine is serving a %d-token context, not the configured %d — OLLAMA_CONTEXT_LENGTH did not apply",
			psm.ContextLength, t.ContextLength)
	}

	// Spill check: on a discrete GPU, size_vram < size means layers live
	// in system RAM. UMA hosts share one physical pool — the field's
	// semantics differ and partial "spill" there is not the discrete
	// decode cliff, so this check only runs off unified memory. An
	// intentional spill (#624) widens the tolerance to 2× the planned
	// fraction (capped at spillAbsoluteToleranceMax): the single-point
	// spill calibration is allowed to be off by that much before the
	// prediction counts as wrong and the no-spill fallback kicks in.
	plannedSpillDetail := ""
	if !hw.UnifiedMemory && len(hw.GPUs) > 0 && psm.Size > 0 {
			if psm.SizeVRAM == 0 {
				return tuningGpuNotEngaged, fmt.Sprintf(
					"%s not using GPU (size_vram==0)", psm.Name)
			}
		allowed := 0.01
		if t.ExpectedSpillFraction > 0 {
			allowed = 2 * t.ExpectedSpillFraction
			if allowed < 0.01 {
				allowed = 0.01
			}
		}
		// #642: the forced larger ubatch adds a known generation compute
		// buffer that pushes weights to RAM; count it as expected spill so
		// the intentional-spill config isn't degraded for a planned cost.
		if t.NumBatch >= ollamaLargeBatch && psm.Size > 0 {
			allowed += float64(generationBatchBufferBytes) / float64(psm.Size)
		}
		if allowed > spillAbsoluteToleranceMax {
			allowed = spillAbsoluteToleranceMax
		}
		spilled := psm.Size - psm.SizeVRAM
		frac := float64(spilled) / float64(psm.Size)
		if frac > allowed {
			return tuningSpill, fmt.Sprintf(
				"%s partially CPU-resident: %.1f of %.1f GB (%.1f%%) spilled to system RAM (size_vram=%d, tolerated %.0f%%)",
				psm.Name, float64(spilled)/1e9, float64(psm.Size)/1e9, frac*100, psm.SizeVRAM, allowed*100)
		}
		if t.ExpectedSpillFraction > 0 && frac > 0.01 {
			plannedSpillDetail = fmt.Sprintf(
				"serving a %d-token window with %.1f%% of the model in system RAM (expected ~%.0f%%) — within the planned bound",
				t.ContextLength, frac*100, t.ExpectedSpillFraction*100)
		}
	}

	// f16-fallback size heuristic, only meaningful for the model we
	// sized: excess = live size − on-disk weights ≈ KV + graph buffers.
	// The manifest's per-token KV figure can overestimate architectures
	// with sliding-window / linear layers, which biases this check
	// toward false NEGATIVES (missed fallback) — never toward a
	// needless restart.
	if psm.Name == tag && t.KVCacheType == "q8_0" && t.ContextLength > 0 {
		if weight, err := ollamaTagSize(ctx, client, baseURL, tag); err == nil && weight > 0 {
			ctxTotal := psm.ContextLength
			if ctxTotal <= 0 {
				ctxTotal = t.ContextLength * t.NumParallel
			}
			kvBpt := float64(t.kvBytesPerTokFP16)
			expQ8 := kvBpt * 0.5 * float64(ctxTotal)
			expF16 := kvBpt * float64(ctxTotal)
			if expF16-expQ8 >= f16DetectMinMarginBytes {
				if excess := float64(psm.Size - weight); excess > (expQ8+expF16)/2 {
					return tuningF16Fallback, fmt.Sprintf(
						"KV cache looks f16-sized despite q8_0 (live %.1f GB − weights %.1f GB = %.1f GB, expected ~%.1f GB at q8_0)",
						float64(psm.Size)/1e9, float64(weight)/1e9, excess/1e9, expQ8/1e9)
				}
			}
		}
	}

	if ctxDetail == "" && plannedSpillDetail != "" {
		return tuningOKPlannedSpill, plannedSpillDetail
	}
	return tuningOK, ctxDetail
}

// ollamaTagSize returns the on-disk size of tag from /api/tags, the
// live-size baseline for the f16 heuristic (more accurate than the
// manifest's estimated weight).
func ollamaTagSize(ctx context.Context, client *http.Client, baseURL, tag string) (int64, error) {
	var tags ollamaTagsResponse
	if err := getJSON(ctx, client, baseURL+"/api/tags", probeHTTPTimeout, &tags); err != nil {
		return 0, err
	}
	for _, m := range tags.Models {
		if m.Name == tag {
			return m.Size, nil
		}
	}
	return 0, fmt.Errorf("tag %q not in /api/tags", tag)
}

// joinTuningWarn concatenates two warning fragments, skipping empties.
func joinTuningWarn(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "; " + b
	}
}

// loadedModelNames lists the /api/ps model names for an abstain detail
// message, so the log says which foreign model was resident instead.
func loadedModelNames(ps psResponse) string {
	if len(ps.Models) == 0 {
		return "none"
	}
	names := make([]string, 0, len(ps.Models))
	for _, m := range ps.Models {
		names = append(names, m.Name)
	}
	return strings.Join(names, ", ")
}

// runnerProcLister enumerates the local process table (proclist.List in
// production; a fake in tests) so verification can read the model runner's
// real flags.
type runnerProcLister func() ([]proclist.ProcInfo, error)

// observeRunnerParallel reads the num_parallel (-np) the Ollama runner is
// ACTUALLY serving for tuning t, by correlating a live llama-server /
// `ollama runner` process against the tuning's context (waired#763).
// /api/ps does not expose num_parallel and Ollama silently reduces
// OLLAMA_NUM_PARALLEL when the per-slot KV won't fit, so status would
// otherwise report the intent, not the truth.
//
// Correlation: llama.cpp's -c is the TOTAL context across parallel slots,
// so the runner serving t has -c == t.ContextLength (parallelism reduced to
// 1) or -c == t.ContextLength × its own -np. A UNIQUE runner matching that
// wins; zero or several matches → not ok, and the caller keeps the intent.
func observeRunnerParallel(t ollamaTuning, listProcs runnerProcLister) (int, bool) {
	if listProcs == nil || t.ContextLength <= 0 {
		return 0, false
	}
	procs, err := listProcs()
	if err != nil {
		return 0, false
	}
	matches, np := 0, 0
	for _, p := range procs {
		if !proclist.IsRunnerProc(p.Argv) {
			continue
		}
		f := proclist.ParseRunnerFlags(p.Argv)
		if f.ContextLen <= 0 || f.NumParallel <= 0 {
			continue
		}
		if f.ContextLen == t.ContextLength || f.ContextLen == t.ContextLength*f.NumParallel {
			matches++
			np = f.NumParallel
		}
	}
	if matches != 1 {
		return 0, false
	}
	return np, true
}

// modelEnvSwitcher is the slice of *infruntime.OllamaAdapter the verify
// pass needs to relaunch the engine with recomputed tuning.
type modelEnvSwitcher interface {
	SetModelEnv([]string)
	SetAppliedTuning(infruntime.ModelTuning)
	Stop(context.Context) error
	EnsureRunning(context.Context) error
}

// applyOllamaTuningVerification verifies the exported tuning once the
// engine is serving and, on positive evidence of an f16 fallback or a
// spill, recomputes the sizing, swaps the model env, restarts the engine
// ONCE, and re-verifies. It never restarts twice: if the degraded sizing
// still misbehaves the outcome is recorded as a user-visible warning and
// the engine is left alone. Every path ends in SetAppliedTuning. listProcs
// reads the local process table so the recorded tuning carries the runner's
// ACTUAL request parallelism (waired#763); nil disables that read.
func applyOllamaTuningVerification(ctx context.Context, sw modelEnvSwitcher, t ollamaTuning, m catalog.Manifest, v catalog.Variant, hw hardware.Profile, tag, baseURL string, client *http.Client, listProcs runnerProcLister, logger *slog.Logger) {
	verdict, detail := verifyOllamaTuning(ctx, client, baseURL, t, tag, hw)

	record := func(tn ollamaTuning, verified bool, warning string) {
		mt := tn.ModelTuning
		mt.Verified = verified
		if verified {
			// #763: record the runner's ACTUAL request parallelism — Ollama
			// silently caps OLLAMA_NUM_PARALLEL when the per-slot KV won't
			// fit — and note the reduction rather than surfacing stale intent.
			if np, ok := observeRunnerParallel(tn, listProcs); ok {
				mt.ObservedNumParallel = np
				if np < tn.NumParallel {
					warning = joinTuningWarn(warning, fmt.Sprintf(
						"ollama reduced request parallelism from %d to %d (per-slot KV did not fit the %d-token window)",
						tn.NumParallel, np, tn.ContextLength))
				}
			}
		}
		if warning != "" {
			mt.Warning = warning
		}
		sw.SetAppliedTuning(mt)
	}

	next, restartWarn := degradedTuning(t, m, v, hw, verdict, detail)
	switch {
	case verdict == tuningInconclusive:
		logger.Info("ollama tuning verification inconclusive", "detail", detail)
		record(t, false, t.Warning)
		return
	case verdict == tuningOK:
		if detail != "" { // context mismatch: warn, nothing to restart into
			logger.Warn("ollama tuning verification", "detail", detail)
			record(t, true, detail)
			return
		}
		logger.Info("ollama tuning verified",
			"ctx", t.ContextLength, "kv", t.KVCacheType, "parallel", t.NumParallel)
		record(t, true, t.Warning)
		return
	case verdict == tuningOKPlannedSpill:
		// The planned #624 spill, measured within its bound: a working
		// configuration. Informational log level; the measured detail is
		// appended to (never replaces) the intentional-spill warning.
		logger.Info("ollama tuning verified (planned spill within bound)", "detail", detail)
		record(t, true, joinTuningWarn(t.Warning, detail))
		return
	case verdict == tuningGpuNotEngaged:
		logger.Info("ollama tuning verification GPU not engaged", "detail", detail)
		record(t, false, t.Warning)
		return
	case next.ContextLength == t.ContextLength && next.KVCacheType == t.KVCacheType:
		// The recompute changed nothing (already at the floor): a
		// restart would land in the same place, so warn and keep going.
		logger.Warn("ollama tuning degraded but no smaller sizing available", "detail", detail)
		record(t, true, restartWarn)
		return
	}

	logger.Warn("ollama tuning verification failed; restarting engine once with recomputed sizing",
		"detail", detail,
		"ctx", fmt.Sprintf("%d→%d", t.ContextLength, next.ContextLength),
		"kv", fmt.Sprintf("%s→%s", t.KVCacheType, next.KVCacheType))
	sw.SetModelEnv(next.Env())
	if err := sw.Stop(ctx); err != nil {
		logger.Warn("stop for tuning restart failed; keeping current engine", "err", err)
		record(t, true, restartWarn)
		return
	}
	if err := sw.EnsureRunning(ctx); err != nil {
		logger.Warn("restart with recomputed tuning failed; engine down until retry/restart", "err", err)
		record(next, true, restartWarn)
		return
	}

	// Single re-verify; never a second restart.
	verdict2, detail2 := verifyOllamaTuning(ctx, client, baseURL, next, tag, hw)
	switch verdict2 {
	case tuningOK:
		if detail2 != "" {
			restartWarn = restartWarn + "; " + detail2
		}
		logger.Info("ollama tuning re-verified after restart",
			"ctx", next.ContextLength, "kv", next.KVCacheType)
		record(next, true, restartWarn)
	case tuningInconclusive:
		record(next, false, restartWarn)
	default:
		logger.Warn("ollama tuning still degraded after one restart; leaving engine as-is",
			"detail", detail2)
		record(next, true, restartWarn+"; still degraded after restart: "+detail2)
	}
}

// degradedTuning recomputes the sizing for a failed verification. For an
// f16 fallback the whole budget is re-sized at the f16 factor (and the
// exported KV type flips to f16 — explicit beats a knowingly-ignored
// q8_0). For a spill the window shrinks by the observed overshoot plus a
// 25% safety margin. The returned warning is the user-visible record of
// what happened; callers compare the result against the current tuning
// to detect a no-op (already at the floor).
func degradedTuning(t ollamaTuning, m catalog.Manifest, v catalog.Variant, hw hardware.Profile, verdict tuningVerdict, detail string) (ollamaTuning, string) {
	switch verdict {
	case tuningF16Fallback:
		// operatorParallel=0: a degrade recompute drops any operator concurrency
		// override back to the VRAM-safe auto value — the backstop that keeps an
		// over-aggressive override from leaving the engine spilling/unloadable.
		next := computeOllamaTuningOpts(m, v, hw, "f16", false, 0)
		warn := fmt.Sprintf(
			"this model runs its KV cache at f16 (q8_0 needs flash attention, which it doesn't support); context window sized accordingly at %d tokens",
			next.ContextLength)
		return next, warn
	case tuningSpill:
		if t.ExpectedSpillFraction > 0 {
			// The intentional spill overshot its bound: the prediction
			// was wrong on this host, so fall back to the no-spill
			// sizing rather than proportional shrinking around a number
			// that just proved unreliable. The recompute never re-takes
			// the intentional-spill branch (allowIntentionalSpill=false),
			// so its ExpectedSpillFraction is 0 and the re-verify runs
			// the strict 1% check.
			next := computeOllamaTuningOpts(m, v, hw, t.KVCacheType, false, 0)
			warn := fmt.Sprintf(
				"measured spill exceeded the planned bound at a %d-token window; context window reduced to %d tokens to keep the model GPU-resident",
				t.ContextLength, next.ContextLength)
			if next.ContextLength == t.ContextLength {
				warn = "model spills to system RAM beyond the planned bound even at the fallback window; inference will be slower (" + detail + ")"
			}
			return next, warn
		}
		next := t
		next.ContextLength = spillShrunkContext(t, detail)
		warn := fmt.Sprintf(
			"model spilled to system RAM at a %d-token window; context window reduced to %d tokens to keep the model GPU-resident",
			t.ContextLength, next.ContextLength)
		if next.ContextLength == t.ContextLength {
			warn = "model spills to system RAM even at the minimum context window on this host; inference will be slower (" + detail + ")"
		}
		return next, warn
	default:
		return t, ""
	}
}

// spillOvershootBytes is set by verifyOllamaTuning via its detail — but
// parsing prose is brittle, so the shrink instead recomputes from the
// live /api/ps numbers captured in lastSpillBytes. To keep the flow
// testable and free of hidden state, the overshoot is re-derived from
// the sizing itself: shrink by 25% of the current window per pass,
// floored at ollamaContextFloor and 1024-aligned. One restart means at
// most one shrink, so a fixed proportional step is both simple and
// sufficient — the re-verify records a warning if it still spills.
func spillShrunkContext(t ollamaTuning, _ string) int {
	shrunk := t.ContextLength * 3 / 4
	shrunk = shrunk / 1024 * 1024
	if shrunk < ollamaContextFloor {
		shrunk = ollamaContextFloor
	}
	if shrunk > t.ContextLength {
		shrunk = t.ContextLength
	}
	return shrunk
}
