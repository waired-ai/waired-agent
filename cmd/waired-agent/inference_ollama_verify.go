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
	// quantized (q8_0 / q4_0) request — the engine fell back (no flash
	// attention).
	tuningF16Fallback
	// tuningSpill: the loaded model reports size_vram < size on a
	// discrete GPU: layers spilled to system RAM beyond what the
	// tuning planned (the #624 intentional spill widens the tolerance).
	tuningSpill
	// tuningOKPlannedSpill: the model spilled, but within the bound the
	// #624 intentional-spill tuning planned for — a working
	// configuration, reported informationally, never degraded.
	tuningOKPlannedSpill
	// tuningVRAMExhausted: the load left the accelerator without room to
	// serve a real prompt — an allocation probe came back with an engine
	// out-of-memory, or the free reading fell below
	// ollamaPostLoadFreeVRAMFloorMB.
	//
	// Distinct from tuningSpill because the remedy is different: a spill
	// costs decode speed and steps the WINDOW, this costs service
	// outright and steps the forced generation BATCH first
	// (waired-agent#1038).
	tuningVRAMExhausted
	// tuningGPUNotEngaged: a host with GPU-addressable memory loaded the
	// model with no layer on it — the engine's own placement says
	// "offloaded 0/M layers", or, with no engine log to read, /api/ps
	// reports no bytes in GPU memory. The cause is a backend or driver that
	// did not engage, not the window, so no smaller rung helps and the pass
	// neither restarts nor steps down (waired-agent#71).
	tuningGPUNotEngaged
)

// placementTailBytes bounds the engine-log read the placement witness
// parses: one runner's load transcript at --log-verbosity 4 (the fit
// dry-run, the tensor and buffer lines, the draft context) plus whatever
// the engine logged between the load and this read.
const placementTailBytes = 2 << 20

// placementEvidence reads llama.cpp's own record of where it put the model
// this tuning was verified against (waired-agent#1337). ok=false is "no
// evidence": no log seam, a log at its cap (the writer keeps the start, so
// the tail is not this load), no complete load in the tail, or a load that
// is not this runner's — a different model file than the live runner's
// --model, or a different context than the tuning asked for.
func placementEvidence(t ollamaTuning, deps ollamaVerifyDeps) (infruntime.LlamaPlacement, bool) {
	if deps.EngineLog == nil {
		return infruntime.LlamaPlacement{}, false
	}
	tail := deps.EngineLog(placementTailBytes)
	if infruntime.EngineLogTailIsStale(tail) {
		return infruntime.LlamaPlacement{}, false
	}
	p, ok := infruntime.ParseLlamaPlacement(tail)
	if !ok {
		return infruntime.LlamaPlacement{}, false
	}
	if t.ContextLength > 0 && p.ContextCells != t.ContextLength && p.ContextCells != t.ContextLength*max(t.NumParallel, 1) {
		return infruntime.LlamaPlacement{}, false
	}
	if f, ok := observeRunnerFlags(t, deps.ListProcs); ok && f.ModelPath != "" && p.ModelPath != "" &&
		pathBase(f.ModelPath) != pathBase(p.ModelPath) {
		return infruntime.LlamaPlacement{}, false
	}
	return p, true
}

// pathBase is the last element of a path written by either OS's engine.
func pathBase(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// f16DetectMinMarginBytes is the minimum gap between the expected q8_0
// and f16 KV sizes for the size heuristic to be meaningful; below it,
// graph-buffer noise dominates and the check abstains.
const f16DetectMinMarginBytes = 1_500_000_000

// The free-VRAM reading is EVIDENCE, not a threshold. It is recorded on
// the tuning (ModelTuning.PostLoadFreeVRAMMB) and degrades nothing on
// its own.
//
// There was a floor here — 768 MB, below which a load was declared
// unservable for hosts where the allocation probe could not answer. It
// was calibrated on the reproduction host against waired#642's forced
// generation ubatch: 491 MB free could not serve 2,000 tokens, 945 MB
// served 152k. Retiring that override (waired-agent#1079) invalidated
// the calibration in both directions:
//
//   - every failing reading it was drawn from belonged to a
//     configuration this agent no longer produces, so the floor has no
//     failing case left to separate; and
//   - the configuration that replaced it loads with 506 MB free on that
//     same host and serves a 171,449-token prompt, so the floor would
//     now condemn the working configuration it was written to protect.
//
// A number with no failing case and one known false positive is worse
// than no number. The allocation probe below asks the engine the actual
// question, and three other paths still catch a host that cannot serve:
// the spill-fraction check, the depth benchmark's out-of-memory verdict
// (waired-agent#1058), and a request-time out-of-memory
// (agentInferenceProvider.onEngineFitFailure).

// ollamaFitProbePromptTokens is how long the post-load allocation
// probe's prompt is: long enough that the runner allocates the working
// set a real prompt would, rather than the one a warm-up does. On the
// reproduction host the cliff sat at ~2,000 tokens, well inside this.
//
// It was 4 × waired#642's forced generation ubatch until that override was
// retired (waired-agent#1079); the engine sizes its own batch now, so
// the figure is stated directly. The quantity it has to exceed is a
// prompt long enough to span several of whatever ubatch the engine
// picked, and 2048 is the largest it picks.
const ollamaFitProbePromptTokens = 8192

// ollamaVerifyDeps are the post-load evidence and repair seams. The zero
// value opts out of all of them, which is what a host with no GPU — and
// every test that does not exercise them — passes.
type ollamaVerifyDeps struct {
	// FreeVRAMMB reads the tightest card's CURRENT free memory
	// (hardware.TightestGPUFreeMB). ok=false means "no evidence", never
	// zero: unified memory, an AMD host, or a driver that rejected the
	// query. nil skips the reading.
	FreeVRAMMB func(context.Context) (int, bool)
	// Allocate runs one generation spanning several ubatches against tag
	// and returns the engine's error verbatim. nil skips the probe.
	Allocate func(ctx context.Context, tag string, promptTokens int) error
	// ListProcs is the #763 runner-parallelism read.
	ListProcs runnerProcLister
	// EngineLog reads the end of the engine's own log, where llama.cpp
	// records where it placed each load (placementEvidence). nil means no
	// placement witness.
	EngineLog func(maxBytes int) string
	// RestampDraft pulls tag again so it carries a draft of draft tokens
	// (0 = none) when the runner's draft is not the one this host should
	// run (draftToRewrite). It returns at once; the pull runs in the
	// background. nil skips the repair.
	RestampDraft func(tag string, v catalog.Variant, draft int)
}

// draftToRewrite reports the draft v's tag should be pulled again with,
// when the runner serving it runs a different one than this host should
// (waired-ai/waired#1433). want is hostfit.OllamaDraftTokens for the
// applied tuning.
//
// Two ways a tag's draft goes stale, both fixed by the same re-pull,
// because Pull writes the draft this host should run and a re-pull first
// resets the tag to what the publisher shipped:
//
//   - A tag pulled before the catalog set MTPDraftTokens never got the
//     draft: the write happens only inside download.Puller.Pull.
//   - A tag written with a draft on a host whose load no longer fits with
//     it (a smaller window rung, another KV cache type, less free memory)
//     keeps it, and a load that spills gets slower with a draft.
//
// ollama reloads the runner on the next request either way, because
// draft_num_predict is a runner option and a changed runner option
// reloads (server/sched.go needsReload, v0.34.0).
//
// Only a draft the product writes is the product's to change: a tag whose
// publisher set draft_num_predict (GGUF.DraftMaxTokens > 0) is left as it is.
func draftToRewrite(v catalog.Variant, want int, f proclist.RunnerFlags) (int, bool) {
	if v.GGUF == nil || v.GGUF.DraftMaxTokens > 0 || v.GGUF.NextNLayers <= 0 || v.MTPDraftTokens <= 0 {
		return 0, false
	}
	return want, max(f.SpecDraftTokens, 0) != max(want, 0)
}

// verifyOllamaTuning inspects the loaded model and classifies the
// outcome. tag is the Ollama tag the tuning was sized for. Modern Ollama
// runs a per-model llama-server with its own -c, so verification is
// per-model: when the target tag is not the loaded model — e.g. a previous
// model still resident in /api/ps right after a model swap — the pass
// abstains (tuningInconclusive) instead of comparing the configured window
// against a FOREIGN runner, which used to emit a false "OLLAMA_CONTEXT_LENGTH
// did not apply" warning (waired#763). The returned detail is human-readable
// (log / warning material).
func verifyOllamaTuning(ctx context.Context, client *http.Client, baseURL string, t ollamaTuning, tag string, hw hardware.Profile, deps ollamaVerifyDeps) (tuningVerdict, string) {
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

	// Placement: where llama.cpp says it put this load. It is the witness
	// for everything below — /api/ps's size / size_vram are ollama's own
	// parse of the same buffer lines, and they leave out both the MTP draft
	// context and the CPU_Mapped weights (waired-agent#1337, #1330).
	accelerated := hw.UnifiedMemory || len(hw.GPUs) > 0
	discrete := !hw.UnifiedMemory && len(hw.GPUs) > 0
	place, placed := placementEvidence(t, deps)
	switch {
	case accelerated && placed && (place.OffloadedLayers == 0 || place.DeviceWeightsMiB == 0):
		return tuningGPUNotEngaged, fmt.Sprintf(
			"%s loaded with no layer in VRAM (offloaded %d/%d layers, %.0f MiB of weights in system RAM)",
			psm.Name, place.OffloadedLayers, place.TotalLayers, place.HostWeightsMiB)
	case discrete && !placed && psm.Size > 0 && psm.SizeVRAM == 0:
		return tuningGPUNotEngaged, fmt.Sprintf(
			"%s loaded with no bytes in VRAM (size_vram=0 of %.1f GB)", psm.Name, float64(psm.Size)/1e9)
	}

	// waired-agent#1038: the spill FRACTION does not separate a working
	// configuration from a dead one. On the reproduction host the same
	// model at the same window measured 21.9 % spilled and served a
	// 152k-token prompt at 966 tok/s, and 28.5 % spilled and could not
	// serve 2,000 tokens — with the tolerance landing between them by
	// luck. What does separate them is whether the runner can allocate
	// the working set, which is what the probe below asks. freeMB rides
	// along for the record and for the sentence a spilling host shows;
	// it decides nothing (waired-agent#1079).
	plannedSpillDetail := ""
	freeMB := 0
	if deps.FreeVRAMMB != nil && discrete {
		freeMB, _ = deps.FreeVRAMMB(ctx)
	}

	// The allocation probe is the only signal that puts the actual
	// question to the engine: can this configuration allocate the working
	// set a real prompt needs? Everything else here is a proxy, and both
	// proxies have now been shown to straddle the answer — the fraction
	// above, and the free reading twice over (waired-agent#1079).
	// Measured on the reproduction host, ollama 0.32.15: the base tag
	// loaded with 506 MB free and served a 171,449-token prompt, while
	// the retired forced-batch configuration loaded with 52 MB free and
	// could not serve 2,000 — two readings a few hundred MB apart, on
	// opposite sides of working (waired-agent#1079).
	probeServed := false
	if deps.Allocate != nil && discrete && t.ExpectedSpillFraction > 0 {
		err := deps.Allocate(ctx, psm.Name, ollamaFitProbePromptTokens)
		switch {
		case err == nil:
			probeServed = true
		case infruntime.EngineOutOfMemory(err.Error()):
			return tuningVRAMExhausted, fmt.Sprintf(
				"%s could not serve a %d-token prompt at this configuration: %v",
				psm.Name, ollamaFitProbePromptTokens, err)
		}
		// Anything else — a timeout, a transport error, a model the probe
		// could not address — says nothing about the fit, and leaves
		// probeServed false so the reading below still gets its say.
	}
	// serves is "this configuration was SHOWN to work". Only the probe
	// can show it: a free reading is a number about the card, not an
	// answer about the request (see the note above the floor's removal).
	serves := probeServed

	// Spill: the weights the fit moved to system RAM, against the weights
	// the plan predicted it would move, with one layer of tolerance — the
	// granularity the fit itself works in. The input-layer weights that
	// live in system RAM on every load are not a spill and are taken off
	// first. Bytes rather than a layer count, because on a mixture of
	// experts the fit moves expert tensors and still reports every layer
	// as offloaded. Without the engine's placement there is no witness:
	// /api/ps cannot tell a spill from its own omissions, so the check
	// abstains rather than guess.
	if accelerated && placed {
		moved := place.HostWeightsMiB - float64(t.HostWeightsMB)
		if t.PlannedDeviceWeightMB > 0 && place.DeviceWeightsMiB > 0 {
			// The device side is the witness where the plan can name it:
			// a mixture of experts whose experts spilled maps the whole
			// file into system RAM (CPU_Mapped reads 20,294 MiB for a
			// 35B-A3B the fit moved 2,349 MiB of), while its device
			// buffer shrinks by exactly what left.
			moved = float64(t.PlannedDeviceWeightMB) - place.DeviceWeightsMiB
		}
		moved = max(moved, 0)
		tolerance := float64(t.PlannedLayerWeightMB)
		if tolerance <= 0 {
			tolerance = 0.05 * (place.DeviceWeightsMiB + place.HostWeightsMiB)
		}
		layers := fmt.Sprintf("%d of %d layers", place.CPULayers(), place.TotalLayers)
		switch {
		case moved > float64(t.PlannedCPUWeightMB)+tolerance && !serves:
			return tuningSpill, fmt.Sprintf(
				"%s partially CPU-resident: %s and %.0f MiB of weights in system RAM, planned %d MiB",
				psm.Name, layers, moved, t.PlannedCPUWeightMB)
		case moved > float64(t.PlannedCPUWeightMB)+tolerance:
			// The configuration was shown to serve, so this is the planned
			// trade running hot rather than a broken host. Report it, do
			// not degrade into it.
			plannedSpillDetail = fmt.Sprintf(
				"serving a %d-token window with %s and %.0f MiB of weights in system RAM (planned %d MiB), %d MB of GPU memory still free",
				t.ContextLength, layers, moved, t.PlannedCPUWeightMB, freeMB)
		case t.ExpectedSpillFraction > 0 && moved > tolerance:
			plannedSpillDetail = fmt.Sprintf(
				"serving a %d-token window with %s and %.0f MiB of weights in system RAM — within the plan",
				t.ContextLength, layers, moved)
		}
	}

	// The engine names the KV cache type it allocated; that is the f16
	// fallback's witness when the log is there.
	if placed && place.KVCacheType != "" {
		if t.KVCacheType != "f16" && t.KVCacheType != "" && place.KVCacheType == "f16" {
			return tuningF16Fallback, fmt.Sprintf(
				"engine allocated an f16 KV cache although %s was requested", t.KVCacheType)
		}
		if ctxDetail == "" && plannedSpillDetail != "" {
			return tuningOKPlannedSpill, plannedSpillDetail
		}
		return tuningOK, ctxDetail
	}

	// f16-fallback size heuristic, only meaningful for the model we
	// sized: excess = live size − on-disk weights ≈ KV + graph buffers.
	// The manifest's per-token KV figure can overestimate architectures
	// with sliding-window / linear layers, which biases this check
	// toward false NEGATIVES (missed fallback) — never toward a
	// needless restart.
	//
	// Any quantized cache type, not only q8_0: the default is q4_0 since
	// waired-agent#1348, and a q4_0 request falls back to f16 the same way
	// on a model without flash attention.
	if quant := t.KVCacheType == catalog.KVCacheQ8_0 || t.KVCacheType == catalog.KVCacheQ4_0; psm.Name == tag && quant && t.ContextLength > 0 {
		if weight, err := ollamaTagSize(ctx, client, baseURL, tag); err == nil && weight > 0 {
			ctxTotal := psm.ContextLength
			if ctxTotal <= 0 {
				ctxTotal = t.ContextLength * t.NumParallel
			}
			kvBpt := float64(t.kvBytesPerTokFP16)
			expQ := kvBpt * kvFactorFor(t.KVCacheType) * float64(ctxTotal)
			expF16 := kvBpt * float64(ctxTotal)
			if expF16-expQ >= f16DetectMinMarginBytes {
				if excess := float64(psm.Size - weight); excess > (expQ+expF16)/2 {
					return tuningF16Fallback, fmt.Sprintf(
						"KV cache looks f16-sized despite %s (live %.1f GB − weights %.1f GB = %.1f GB, expected ~%.1f GB at %s)",
						t.KVCacheType, float64(psm.Size)/1e9, float64(weight)/1e9, excess/1e9, expQ/1e9, t.KVCacheType)
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
// The premise also requires that the tail IS the end of the file, which a
// capped log's is not — the caller checks EngineLogTailIsStale before
// passing anything here (waired-agent#951).
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

// observeRunnerFlags reads the command line the Ollama runner is ACTUALLY
// serving tuning t with, by correlating a live llama-server /
// `ollama runner` process against the tuning's context (waired#763).
// Two flags are wanted off it: -np, the request parallelism, and -b, the
// prompt batch (waired-agent#1127).
// /api/ps does not expose num_parallel and Ollama silently reduces
// OLLAMA_NUM_PARALLEL — at v0.34.0 for embedding models and for the model
// families its scheduler starts with one slot (server/sched.go
// Scheduler.load), and in any later build for whatever it decides at load
// time — so status would otherwise report the intent, not the truth. The
// catalog's build limit keeps the product from asking for those families
// (waired-ai/waired-agent#1423); this read is what still sees a reduction
// the catalog does not know about. Likewise the batch: the agent exports no batch
// variable, so the runner's own choice is the only figure there is. This
// reads what was launched and nothing else — the engine's REASON for a
// load-time decision is in its own log, not in the process table
// (waired-ai/waired-agent#877).
//
// Correlation: llama.cpp's -c is the TOTAL context across parallel slots,
// so the runner serving t has -c == t.ContextLength (parallelism reduced to
// 1) or -c == t.ContextLength × its own -np. A UNIQUE runner matching that
// wins; zero or several matches → not ok, and the caller keeps the intent.
func observeRunnerFlags(t ollamaTuning, listProcs runnerProcLister) (proclist.RunnerFlags, bool) {
	if listProcs == nil || t.ContextLength <= 0 {
		return proclist.RunnerFlags{}, false
	}
	procs, err := listProcs()
	if err != nil {
		return proclist.RunnerFlags{}, false
	}
	matches := 0
	var found proclist.RunnerFlags
	// LiveRunners, not every process that looks like a runner
	// (waired-agent#1303). Two things had to be true before the flags could
	// be read at all, and neither was:
	//
	//   - argv[0] has to be the program. On macOS and Windows the command
	//     line is one space-joined string, so a program path with a space
	//     in it — every macOS install: /Library/Application Support/waired
	//     — left argv[0] as a fragment and nothing matched. proclist now
	//     rebuilds it from the path the OS reports separately.
	//   - the runner has to belong to a LIVE engine. Measured on
	//     sv-macmini: a runner orphaned fourteen hours earlier still
	//     carried the same window and -np as the real one, so the
	//     unique-match test below saw two and abstained — and the host
	//     recorded no observation at all.
	for _, p := range proclist.LiveRunners(procs) {
		f := proclist.ParseRunnerFlags(p.Argv)
		if f.ContextLen <= 0 || f.NumParallel <= 0 {
			continue
		}
		if f.ContextLen == t.ContextLength || f.ContextLen == t.ContextLength*f.NumParallel {
			matches++
			found = f
		}
	}
	if matches != 1 {
		return proclist.RunnerFlags{}, false
	}
	return found, true
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
// unobservedParallelismNote is what a host says when its engine is serving
// but its own process table did not yield the runner: the advertised
// capacity falls back to one conversation rather than to the parallelism
// that was requested (waired-agent#1303).
const unobservedParallelismNote = "Waired could not read how many conversations Ollama can hold at once, " +
	"so this computer offers one at a time."

func applyOllamaTuningVerification(ctx context.Context, sw modelEnvSwitcher, t ollamaTuning, m catalog.Manifest, v catalog.Variant, hw hardware.Profile, tag, baseURL string, client *http.Client, deps ollamaVerifyDeps, logger *slog.Logger) {
	listProcs := deps.ListProcs
	freeMB := func() int {
		if deps.FreeVRAMMB == nil {
			return 0
		}
		mb, ok := deps.FreeVRAMMB(ctx)
		if !ok {
			return 0
		}
		return mb
	}

	// Verified once, and the verdict is carried down to the decision
	// below: the pass includes an allocation probe, which is a real
	// prefill, so re-asking the same question of the same configuration
	// would cost a second one for nothing.
	verdict, detail := verifyOllamaTuning(ctx, client, baseURL, t, tag, hw, deps)

	// record stores the applied tuning and returns what it stored, so a
	// caller can report what the runner was seen doing.
	record := func(tn ollamaTuning, verified bool, warning string) infruntime.ModelTuning {
		mt := tn.ModelTuning
		mt.PostLoadFreeVRAMMB = freeMB()
		mt.Verified = verified
		// #763: record the runner's ACTUAL request parallelism — Ollama
		// silently caps OLLAMA_NUM_PARALLEL for reasons it decides at load
		// time — and note the reduction rather than surfacing stale intent.
		//
		// Read regardless of the verdict (waired-agent#1303). "Is the
		// tuning what we planned" and "what -np is the runner actually
		// running" are independent questions, and the process table is
		// readable either way; gating the read on `verified` left an
		// inconclusive pass with no observation at all, and the advertised
		// capacity then fell back to the intent.
		{
			if f, ok := observeRunnerFlags(tn, listProcs); ok {
				np := f.NumParallel
				mt.ObservedNumParallel = np
				// The prompt batch comes off the same command line
				// (waired-agent#1127). The agent never asked for it —
				// ollama exports no batch variable — so this read is the
				// only way to know what the prefill measurement is
				// measuring against.
				mt.PromptBatchTokens = f.BatchTokens
				// Same for the draft: ollama decides it from the tag's
				// draft_num_predict, and the runner's arguments are the
				// only place it shows (waired-ai/waired#1433).
				mt.SpeculativeMethod, mt.SpeculativeTokens = f.SpecType, f.SpecDraftTokens
				want := hostfit.OllamaDraftTokens(v, hw.HostFit(), tn.KVCacheType, tn.ContextLength)
				if draft, rewrite := draftToRewrite(v, want, f); rewrite && deps.RestampDraft != nil {
					deps.RestampDraft(tag, v, draft)
				}
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
					// Past the cap the writer keeps the START of the file,
					// so the "tail" is text from the first minutes of this
					// engine process — with no error and no empty string to
					// notice. On a long-lived engine (residency now holds
					// the model indefinitely) and an in-process model switch
					// (#812) that is reachable, and it would quote ANOTHER
					// model's load as the reason for this one — #877's
					// defect reintroduced one layer down
					// (waired-agent#951). Degrade to the honest branch.
					if infruntime.EngineLogTailIsStale(tail) {
						logger.Debug("engine log is at its cap; the tail is not this load's",
							"engine_log_tail_bytes", len(tail))
						tail = ""
					}
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
			} else if verified && listProcs != nil {
				// The engine IS serving — the verification just said so —
				// and the process table still did not yield a unique
				// runner. Say so rather than letting the advertised
				// capacity quietly become the requested parallelism: that
				// silence is how two macOS hosts advertised two warm
				// conversations into a runner with one slot, and a pinned
				// turn was admitted behind the peer owner's own
				// (waired-agent#1303).
				logger.Info("could not read the model runner's command line",
					"requested_parallel", tn.NumParallel)
				warning = joinTuningWarn(warning, unobservedParallelismNote)
			}
		}
		// Join, do not replace. mt.Warning arrives carrying whatever the
		// sizing decided (modelDecisionReasons' extra warning — the
		// below-context-floor note, the forced-rung note), and the
		// verification's own warning is a different fact about the same
		// model: what was predicted versus what the runner actually did.
		//
		// So `warning` is what the VERIFICATION observed, and never what
		// the tuning already said. Three call sites used to pass t.Warning
		// back in, which joined it to itself and printed the sizing
		// sentence twice — and again on every re-verification, since
		// nothing resets it between the boot spawn and the in-process
		// model switch (waired-agent#1043).
		// Replacing dropped the first one silently, which is how a host
		// serving under the coding-agent context floor could show only a
		// spill warning and no mention of the floor at all. The two
		// branches above already join for the same reason.
		if warning != "" {
			mt.Warning = joinTuningWarn(mt.Warning, warning)
		}
		sw.SetAppliedTuning(mt)
		return mt
	}

	next, restartWarn, kind := degradeStep(t, m, v, hw, verdict, detail)
	switch {
	case verdict == tuningInconclusive:
		logger.Info("ollama tuning verification inconclusive", "detail", detail)
		// "" — the verification observed nothing to add. Passing t.Warning
		// here made record join the tuning's own warning to itself, so the
		// sizing sentence was shown twice on every surface that quotes it
		// (waired-agent#1043). record joins onto mt.Warning, and mt IS t.
		record(t, false, "")
		return
	case verdict == tuningOK:
		if detail != "" { // context mismatch: warn, nothing to restart into
			logger.Warn("ollama tuning verification", "detail", detail)
			record(t, true, detail)
			return
		}
		// parallel is what was asked for and observed_parallel what the
		// runner serves (0 = its command line was not read); logging only
		// the first read as the engine's answer when it was not
		// (waired-ai/waired-agent#1423).
		applied := record(t, true, "")
		logger.Info("ollama tuning verified",
			"ctx", t.ContextLength, "kv", t.KVCacheType, "parallel", t.NumParallel,
			"observed_parallel", applied.ObservedNumParallel)
		return
	case verdict == tuningGPUNotEngaged:
		// The GPU did not take the model at all. That is a backend or
		// driver fact, so stepping the window down cannot help and a
		// restart would load the same way (waired-agent#71). The engine
		// keeps serving from system RAM, the window stays declared (#657),
		// and the warning says what happened instead of calling it a spill.
		logger.Warn("ollama loaded the model without the GPU", "detail", detail)
		latched := t
		latched.Degraded = true
		record(latched, true, "the GPU was not used: every layer of the model is in system RAM, so replies are much slower ("+detail+"); `waired logs` shows the engine's load")
		return
	case verdict == tuningOKPlannedSpill:
		// The planned #624 spill, measured within its bound: a working
		// configuration. Informational log level; the measured detail is
		// appended to (never replaces) the intentional-spill warning.
		logger.Info("ollama tuning verified (planned spill within bound)", "detail", detail)
		record(t, true, detail)
		return
	case kind == stepNone || (next.ContextLength == t.ContextLength && next.KVCacheType == t.KVCacheType):
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
		// waired-agent#1038: the ladder is spent. Surfaces that must not
		// claim this model fits key on this, never on "a warning exists" —
		// the planned #624 spill sets a warning on a host that works.
		latched.Degraded = true
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
	verdict2, detail2 := verifyOllamaTuning(ctx, client, baseURL, next, tag, hw, deps)
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
		latched.Degraded = true
		record(latched, true, restartWarn+"; still degraded after restart: "+detail2)
	}
}

// tuningStepKind says how a degrade step reaches the engine.
//
// One rung has a cost worth naming: the window lives in
// OLLAMA_CONTEXT_LENGTH, which only a restart of `ollama serve` changes.
// A second kind existed for waired#642's forced generation ubatch, which
// rode the serving TAG and so cost a state write instead of a restart;
// retiring that override removed it (waired-agent#1079).
type tuningStepKind int

const (
	// stepNone: the bottom of the ladder. The caller latches.
	stepNone tuningStepKind = iota
	// stepEnv: an OLLAMA_* value changes; one engine restart.
	stepEnv
)

// The degrade sequence is bounded by shape rather than by a counter: one
// degradeStep, one restart, one re-verification, and then the outcome is
// recorded whatever it is. There was a counter here (2) while the ladder
// had a batch rung to walk before the window rung; retiring waired#642's
// forced generation ubatch removed that rung and the loop with it
// (waired-agent#1079). The engine-restart budget is #621's and unchanged:
// at most ONE Stop/EnsureRunning per pass.

// degradeStep returns the configuration strictly BELOW t on the ladder.
//
// The ladder is the window rung, descending:
//
//  1. window down one rung of hostfit.OllamaServedWindows (stepEnv)
//  2. nothing (stepNone) — the caller latches
//
// waired-agent#1038 was a host that had nowhere to go: a 262144-native
// model has a ONE-element window ladder, so the only rung did not exist
// and the pass latched into a configuration it had just measured as
// broken. The rung added for it dropped waired#642's forced generation
// ubatch — and retiring that override (waired-agent#1079) removes the
// configuration that had no rung, rather than giving it one. The engine
// sizes its own batch now, against its own memory prediction, and steps
// it down itself when it does not fit.
//
// The f16 fallback is unchanged and orthogonal: it re-sizes the whole
// budget at the f16 factor (explicit beats a knowingly-ignored q8_0),
// capped at the current rung so a degrade can only hold or step down.
//
// Every accepted step is strictly descending: a step never raises
// ContextLength, and the suppression is carried into the recompute
// (ceilingCtx), so a rung cannot be re-entered and the sequence cannot
// oscillate.
func degradeStep(t ollamaTuning, m catalog.Manifest, v catalog.Variant, hw hardware.Profile, verdict tuningVerdict, detail string) (ollamaTuning, string, tuningStepKind) {
	// operatorParallel=0 on every recompute: a degrade drops any operator
	// concurrency override back to the VRAM-safe auto value — the backstop
	// that keeps an over-aggressive override from leaving the engine
	// spilling or unloadable.
	switch verdict {
	case tuningF16Fallback:
		// No observation is carried in: a degrade lands on a different
		// window than the one the runner answered for, so grantedFor would
		// reject it anyway (waired-ai/waired-agent#846).
		next := computeOllamaTuningOpts(m, v, hw, "f16", t.ContextLength, 0, ollamaObservedServe{})
		return next, fmt.Sprintf(
			"this model runs its KV cache at f16 (%s needs flash attention, which it doesn't support); context window sized accordingly at %d tokens",
			t.KVCacheType, next.ContextLength), stepEnv

	case tuningSpill, tuningVRAMExhausted:
		below := rungBelow(m, t.ContextLength)
		if below <= 0 {
			// The bottom: there is no smaller rung. The caller latches —
			// the engine keeps serving, and keeps declaring, the window it
			// is on (#657).
			if verdict == tuningVRAMExhausted {
				return t, "this computer's GPU has no room left to serve a request at this model and window; requests will fail with an out-of-memory error (" + detail + ")", stepNone
			}
			if t.ExpectedSpillFraction > 0 {
				return t, "model spills to system RAM beyond the planned bound even at the fallback window; inference will be slower (" + detail + ")", stepNone
			}
			return t, "model spills to system RAM even at the minimum context window on this host; inference will be slower (" + detail + ")", stepNone
		}
		next := computeOllamaTuningOpts(m, v, hw, t.KVCacheType, below, 0, ollamaObservedServe{})
		if verdict == tuningVRAMExhausted {
			return next, fmt.Sprintf(
				"a %d-token window left this computer's GPU with no room to serve a request; context window reduced to %d tokens",
				t.ContextLength, next.ContextLength), stepEnv
		}
		if t.ExpectedSpillFraction > 0 {
			return next, fmt.Sprintf(
				"measured spill exceeded the planned bound at a %d-token window; context window reduced to %d tokens to keep the model GPU-resident",
				t.ContextLength, next.ContextLength), stepEnv
		}
		return next, fmt.Sprintf(
			"model spilled to system RAM at a %d-token window; context window reduced to %d tokens to keep the model GPU-resident",
			t.ContextLength, next.ContextLength), stepEnv

	default:
		return t, "", stepNone
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
