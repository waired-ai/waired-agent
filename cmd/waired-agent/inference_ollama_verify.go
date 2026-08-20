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
	"regexp"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/platform/proclist"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/proto/hostfit"
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
		if err := loadOllamaModel(ctx, client, baseURL, target, ""); err != nil {
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

// ollamaTagSizes returns every tag's on-disk size from /api/tags, keyed by
// tag. One request covers the whole engine, so a caller that wants sizes
// for several models asks once rather than once per model.
//
// Tags the engine reports without a size are left out rather than recorded
// as zero: absent means "the engine did not say", which a caller has to be
// able to tell apart from "empty".
func ollamaTagSizes(ctx context.Context, client *http.Client, baseURL string, timeout time.Duration) (map[string]int64, error) {
	var tags ollamaTagsResponse
	if err := getJSON(ctx, client, baseURL+"/api/tags", timeout, &tags); err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(tags.Models))
	for _, m := range tags.Models {
		if m.Name != "" && m.Size > 0 {
			out[m.Name] = m.Size
		}
	}
	return out, nil
}

// ollamaTagSize returns the on-disk size of tag from /api/tags, the
// live-size baseline for the f16 heuristic (more accurate than the
// manifest's estimated weight).
func ollamaTagSize(ctx context.Context, client *http.Client, baseURL, tag string) (int64, error) {
	sizes, err := ollamaTagSizes(ctx, client, baseURL, probeHTTPTimeout)
	if err != nil {
		return 0, err
	}
	if size, ok := sizes[tag]; ok {
		return size, nil
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

// engineReasonTailBytes bounds the engine-log read behind a parallelism
// note. The file's total size does not matter — the read seeks from the
// end — but the volume written between the scheduler's sentence and this
// read does, and two things push that up: Ollama writes the sentence
// after llama.cpp's whole load transcript, and the runner is spawned
// with --log-verbosity 4, so serving traffic keeps appending. The 4 KiB
// used for startup failures does not reach back past either. 256 KiB is
// still a trivial read and leaves room for a busy engine.
//
// Missing the line is not a failure: the caller then states only what it
// observed, which is the whole point of waired-ai/waired-agent#877.
const engineReasonTailBytes = 256 << 10

// engineReasonMaxChars bounds what any single engine sentence may
// contribute to a user-visible warning.
const engineReasonMaxChars = 160

// engineMsgRe pulls the msg="…" field out of an Ollama log line. Ollama
// logs in logfmt, and msg is the only field that is prose; everything
// else on the line is a key=value whose value may be a filesystem path.
var engineMsgRe = regexp.MustCompile(`\bmsg="([^"]*)"`)

// parallelReductionReason returns the engine's own sentence explaining a
// request-parallelism reduction, quoted verbatim, from a tail of
// engine.log.
//
// It quotes rather than interprets on purpose. The agent has no way to
// enumerate the reasons a given Ollama build can reduce
// OLLAMA_NUM_PARALLEL — the one that prompted this read was an
// architecture limit, not the KV-capacity shortfall the note used to
// assert (waired-ai/waired-agent#877) — and a mapping table would go
// stale against an engine that ships its own wording.
//
// The LAST matching line wins. Verification runs immediately after the
// load it is verifying, so the most recent scheduler sentence is that
// load's. This is a record of today's behaviour, not a guarantee: a
// second model loaded in between would leave its own line later in the
// file.
//
// Returns ok=false when the tail carries no such line, which is also
// what a build that logs nothing produces. Callers then say only what
// they observed.
func parallelReductionReason(tail string) (string, bool) {
	if tail == "" {
		return "", false
	}
	found := ""
	for _, line := range strings.Split(tail, "\n") {
		if !strings.Contains(line, "parallel") {
			continue
		}
		mm := engineMsgRe.FindStringSubmatch(line)
		if len(mm) != 2 || !strings.Contains(mm[1], "parallel") {
			continue
		}
		found = mm[1]
	}
	if found == "" {
		return "", false
	}
	return sanitizeEngineReason(found), true
}

// sanitizeEngineReason makes another program's log text safe to place in
// a warning: control characters out, length bounded. The caller has
// already restricted itself to the msg field, so no key=value path rides
// along, but the value itself is still not ours.
func sanitizeEngineReason(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if len(s) > engineReasonMaxChars {
		s = strings.TrimSpace(s[:engineReasonMaxChars]) + "…"
	}
	return s
}

// observeRunnerParallel reads the num_parallel (-np) the Ollama runner is
// ACTUALLY serving for tuning t, by correlating a live llama-server /
// `ollama runner` process against the tuning's context (waired#763).
// /api/ps does not expose num_parallel and Ollama silently reduces
// OLLAMA_NUM_PARALLEL — for a per-slot KV cache that will not fit, for
// an architecture its build serves single-slot only, or for anything
// else it decides at load time — so status would otherwise report the
// intent, not the truth. This reads the count and nothing else: the
// engine's reason is in its own log, not in the process table
// (waired-ai/waired-agent#877).
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
	// EngineLogTail reads the end of the engine's own log, which is the
	// only place its reason for a load-time decision is recorded
	// (waired-ai/waired-agent#877).
	EngineLogTail(maxBytes int) string
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
			// #763: record the runner's ACTUAL request parallelism —
			// Ollama silently caps OLLAMA_NUM_PARALLEL for reasons it
			// decides at load time — and note the reduction rather than
			// surfacing stale intent.
			if np, ok := observeRunnerParallel(tn, listProcs); ok {
				mt.ObservedNumParallel = np
				if np < tn.NumParallel {
					// The count comes from the process table; the CAUSE
					// comes from the engine or from nowhere. This note
					// used to assert a KV-capacity shortfall inferred
					// from the count alone, and on the first host it
					// fired on that was wrong — the engine had reduced
					// for an architecture limit while the model sat
					// fully GPU-resident with room to spare
					// (waired-ai/waired-agent#877).
					note := fmt.Sprintf("ollama reduced request parallelism from %d to %d",
						tn.NumParallel, np)
					tail := sw.EngineLogTail(engineReasonTailBytes)
					if reason, ok := parallelReductionReason(tail); ok {
						note += fmt.Sprintf(" — the engine's reason: %q", reason)
					} else {
						// An unreadable log and a log that carries no
						// such line both arrive here as "no reason", and
						// the note reads the same either way. Record the
						// size so the two are still tellable apart from
						// the agent's own log: 0 is "read nothing",
						// non-zero is "read it, the line was not there".
						logger.Debug("no engine reason for the parallelism reduction",
							"engine_log_tail_bytes", len(tail),
							"requested", tn.NumParallel, "observed", np)
						note += "; the engine records why when it loads the model (`waired logs`)"
					}
					warning = joinTuningWarn(warning, note)
				}
			}
		}
		// Join, do not replace. mt.Warning arrives carrying whatever the
		// sizing decided (modelDecisionReasons' extra warning — the
		// below-context-floor note, the forced-rung note), and the
		// verification's own warning is a different fact about the same
		// model: what was predicted versus what the runner actually did.
		// Replacing dropped the first one silently, which is how a host
		// serving under the coding-agent context floor could show only a
		// spill warning and no mention of the floor at all. The two
		// branches above already join for the same reason.
		if warning != "" {
			mt.Warning = joinTuningWarn(mt.Warning, warning)
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
	case next.ContextLength == t.ContextLength && next.KVCacheType == t.KVCacheType:
		// The recompute changed nothing (already at the ladder's lowest
		// rung): a restart would land in the same place, so the failure
		// LATCHES — the engine keeps serving the rung, the warning
		// records it, and WindowFits drops to record WHY the host is on
		// that rung (waired-agent#587).
		//
		// The window stays declared to the mesh. The host serves it; what
		// the spill costs is decode speed, and withholding the window for
		// that made a machine answering real requests invisible at every
		// session size (waired-ai/waired-agent#657).
		logger.Warn("ollama tuning degraded but no smaller sizing available", "detail", detail)
		latched := t
		latched.WindowFits = false
		record(latched, true, restartWarn)
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
		// Still degraded after the one restart: the same latch as the
		// no-smaller-sizing path — keep serving, and keep declaring the
		// window that is being served (#657).
		logger.Warn("ollama tuning still degraded after one restart; leaving engine as-is",
			"detail", detail2)
		latched := next
		latched.WindowFits = false
		record(latched, true, restartWarn+"; still degraded after restart: "+detail2)
	}
}

// degradedTuning recomputes the sizing for a failed verification. For an
// f16 fallback the whole budget is re-sized at the f16 factor (and the
// exported KV type flips to f16 — explicit beats a knowingly-ignored
// q8_0), capped at the current rung so a degrade can only hold or step
// down. For a spill the window steps down ONE RUNG of
// hostfit.OllamaServedWindows; at the ladder's lowest rung there is
// nothing to step to and the recompute is a no-op — the caller's
// no-smaller-sizing path then records the warning and leaves the engine
// serving the rung, which is the whole of what a degrade may do now that
// sub-rung windows are not served (waired-agent#587). The returned
// warning is the user-visible record of what happened; callers compare
// the result against the current tuning to detect that no-op.
func degradedTuning(t ollamaTuning, m catalog.Manifest, v catalog.Variant, hw hardware.Profile, verdict tuningVerdict, detail string) (ollamaTuning, string) {
	switch verdict {
	case tuningF16Fallback:
		// operatorParallel=0: a degrade recompute drops any operator concurrency
		// override back to the VRAM-safe auto value — the backstop that keeps an
		// over-aggressive override from leaving the engine spilling/unloadable.
		// No observation is carried in: a degrade lands on a different
		// window than the one the runner answered for, so grantedFor would
		// reject it anyway (waired-ai/waired-agent#846).
		next := computeOllamaTuningOpts(m, v, hw, "f16", t.ContextLength, 0, ollamaObservedServe{})
		warn := fmt.Sprintf(
			"this model runs its KV cache at f16 (q8_0 needs flash attention, which it doesn't support); context window sized accordingly at %d tokens",
			next.ContextLength)
		return next, warn
	case tuningSpill:
		below := rungBelow(m, t.ContextLength)
		if below <= 0 {
			// Already at the lowest rung: nothing to step down to.
			if t.ExpectedSpillFraction > 0 {
				return t, "model spills to system RAM beyond the planned bound even at the fallback window; inference will be slower (" + detail + ")"
			}
			return t, "model spills to system RAM even at the minimum context window on this host; inference will be slower (" + detail + ")"
		}
		next := computeOllamaTuningOpts(m, v, hw, t.KVCacheType, below, 0, ollamaObservedServe{})
		if t.ExpectedSpillFraction > 0 {
			return next, fmt.Sprintf(
				"measured spill exceeded the planned bound at a %d-token window; context window reduced to %d tokens to keep the model GPU-resident",
				t.ContextLength, next.ContextLength)
		}
		return next, fmt.Sprintf(
			"model spilled to system RAM at a %d-token window; context window reduced to %d tokens to keep the model GPU-resident",
			t.ContextLength, next.ContextLength)
	default:
		return t, ""
	}
}

// rungBelow returns the highest rung of hostfit.OllamaServedWindows(m)
// strictly below ctx, or 0 when ctx already sits at (or below) the
// ladder's lowest rung — the point where a spill degrade has nowhere
// left to step and latches into a warning instead of a restart.
func rungBelow(m catalog.Manifest, ctx int) int {
	for _, rung := range hostfit.OllamaServedWindows(m) {
		if rung < ctx {
			return rung
		}
	}
	return 0
}
