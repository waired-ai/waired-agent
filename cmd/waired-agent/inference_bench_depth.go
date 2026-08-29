// Depth-aware long-context benchmark (#624).
//
// The boot benchmark measures an empty-context 200-token decode — it says
// nothing about how the host behaves at the depths coding agents actually
// run at (64k–200k of filled context), where prefill dominates latency
// and an intentional spill (#624) trades decode speed for window. This
// file measures prefill + decode at canonical depths against the ollama
// NATIVE API (/api/generate exposes prompt_eval_* / eval_* counters; the
// OpenAI-compat surface does not), in the background after boot, cached
// per (host GPU, variant, applied window, KV type).
//
// Methodology mirrors the #625 harness (docs/reports/
// 20260704-mtp-vs-spill-24gb.md): synthetic numbered filler lines with a
// per-run nonce so consecutive runs share no prefix (defeats the engine
// prompt cache), num_predict=200 at temperature 0, real depth read back
// from prompt_eval_count.
package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// depthStageTargets are the canonical measurement depths: the two
// mid-band depths real sessions live at, plus the #624 coding floor.
var depthStageTargets = []int{65536, 131072, 200704}

const (
	// depthStagePromptMarginTokens keeps each stage under the applied
	// window: target completion + chat template + tokenizer estimate
	// error must never overflow (overflow = silent truncation = the
	// measurement lies about its own depth).
	depthStagePromptMarginTokens = 2048

	// depthStageCompletionTokens matches the boot benchmark's decode
	// sample length.
	depthStageCompletionTokens = 200

	// depthStageTimeout bounds one stage: a 200k prefill at the anchor
	// host's measured ~1100-1750 tok/s takes ~2-3 min; 10 min leaves
	// room for slower cards without letting a wedged engine pin the
	// goroutine forever.
	depthStageTimeout = 10 * time.Minute

	// depthBenchTotalBudget bounds the whole run (all stages).
	depthBenchTotalBudget = 25 * time.Minute

	// depthPromptTokensPerLine is the measured token cost of one filler
	// line (calibrated in the #625 harness: 42431 tok / 1228 lines).
	depthPromptTokensPerLine = 35
)

// DepthStageResult is one (depth → prefill/decode) measurement.
type DepthStageResult struct {
	TargetTokens int     `json:"target_tokens"`
	PromptTokens int     `json:"prompt_tokens"`
	PrefillTokps float64 `json:"prefill_tok_s"`
	DecodeTokps  float64 `json:"decode_tok_s"`
	Failed       bool    `json:"failed,omitempty"`
	Err          string  `json:"err,omitempty"`
	// OutOfMemory says the accelerator ran out of memory serving this
	// stage — the strongest evidence this sweep can produce, and until
	// waired-agent#1058 the one it threw away.
	//
	// Failed alone cannot carry it. Every consumer skipped a failed
	// stage, so a host that died at its very first depth reached
	// interactiveFloorVerdict indistinguishable from a host nobody had
	// measured, and was told local inference works.
	OutOfMemory bool `json:"out_of_memory,omitempty"`
}

// DepthBenchResult is the full depth sweep for one (variant, window,
// KV type) on this host.
type DepthBenchResult struct {
	VariantID     string             `json:"variant_id"`
	EngineModel   string             `json:"engine_model"`
	ContextLength int                `json:"context_length"`
	KVCacheType   string             `json:"kv_cache_type,omitempty"`
	Stages        []DepthStageResult `json:"stages"`
	Completed     bool               `json:"completed"`
	MeasuredAt    time.Time          `json:"measured_at"`
}

// DepthBenchDeps is the injectable world of RunDepthBenchmark.
type DepthBenchDeps struct {
	EnginePort    int
	EngineModel   string
	VariantID     string
	ContextLength int    // the applied serve window (AppliedTuning)
	KVCacheType   string // applied KV type, for the cache key / record

	// EngineVersion is the engine release the sweep ran on, and a cache
	// key input for the same reason as BenchDeps.EngineVersion
	// (waired-agent#1131). Empty disables caching.
	EngineVersion string

	// Cache key inputs + handle (same convention as BenchDeps): empty
	// GPUModel/VariantSHA or a nil Cache disables caching.
	GPUModel      string
	VRAMTotalMB   int
	DriverVersion string
	VariantSHA    string
	Cache         *benchCache

	HTTPClient *http.Client
	Logger     *slog.Logger
	Now        func() time.Time
	// Nonce varies the synthetic prompt between runs so no two runs
	// share a prefix. Production passes something unique-ish (the boot
	// timestamp); tests pin it.
	Nonce string
	// OnFitFailure, when set, is called once with the engine's own words
	// if a stage dies of an accelerator out-of-memory
	// (waired-agent#1058).
	//
	// This sweep talks to the engine directly rather than through the
	// gateway — /api/generate is the only surface reporting the counters
	// it exists to read — so its failures never reach the adapter's
	// ReportUpstreamFailure and nothing stepped the fit ladder for them.
	// Production wires OllamaAdapter.ReportFitFailure, which is the same
	// debounce and the same handler a served request's out-of-memory
	// takes.
	OnFitFailure func(detail string)
}

// depthStagePlan clips the canonical stages to the applied window minus
// the safety margin, deduplicates, and returns them ascending. An
// unknown window plans nothing — a depth benchmark that overflows the
// window measures its own truncation, not the model.
func depthStagePlan(appliedCtx int) []int {
	usable := appliedCtx - depthStagePromptMarginTokens
	if appliedCtx <= 0 || usable <= 0 {
		return nil
	}
	var plan []int
	for _, target := range depthStageTargets {
		d := target
		if d > usable {
			d = usable
		}
		if len(plan) > 0 && plan[len(plan)-1] >= d {
			continue // clipped into a duplicate of the previous stage
		}
		plan = append(plan, d)
	}
	return plan
}

// depthPromptWords are the subsystem fillers (NATO alphabet, matching
// the #625 harness so the tokens-per-line calibration carries over).
var depthPromptWords = []string{
	"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf",
	"hotel", "india", "juliet", "kilo", "lima", "mike", "november",
	"oscar", "papa", "quebec", "romeo", "sierra", "tango", "uniform",
	"victor", "whiskey", "xray", "yankee", "zulu",
}

// depthBenchPrompt builds a ~targetTokens synthetic prompt of numbered
// filler lines. The nonce leads every line so runs never share a prefix.
//
// tokensPerLine is the caller's calibration, not a constant, because the
// line above tokenizes differently per model family: the #625 figure
// (depthPromptTokensPerLine) is 35, while the #496 cutoff's probe model
// measures 19.2 on the same text. Baking one in silently produced a
// prompt at 55 % of the requested depth.
func depthBenchPrompt(targetTokens, tokensPerLine int, nonce string) string {
	if tokensPerLine <= 0 {
		tokensPerLine = 1
	}
	return depthBenchPromptLines((targetTokens+tokensPerLine-1)/tokensPerLine, nonce)
}

// depthBenchPromptLines builds the same prompt from an exact LINE count.
//
// The line count is what a caller controls; the token count is what the
// model decides, and only the model can say what the exchange rate is
// (#496's probe measures it rather than assuming — see
// calibrateHostCutoffPrompt). Splitting the two apart is what lets a
// caller read a prefill count back and correct its own estimate.
func depthBenchPromptLines(lines int, nonce string) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "session %s log begin\n", nonce)
	for i := 0; i < lines; i++ {
		fmt.Fprintf(&b, "entry %s-%06d: subsystem %s reported state %d with latency %d ms and checksum %d\n",
			nonce, i, depthPromptWords[i%len(depthPromptWords)], i%7, (i*13)%997, (i*31+7)%65521)
	}
	b.WriteString("Question: summarize the three most frequent subsystems above in one short paragraph.")
	return b.String()
}

// RunDepthBenchmark measures prefill/decode at each planned depth via
// the ollama-native /api/generate. A stage failure records the stage
// and aborts the rest (partial result, Completed=false) — callers must
// not cache incomplete runs.
func RunDepthBenchmark(ctx context.Context, deps DepthBenchDeps) DepthBenchResult {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.HTTPClient == nil {
		deps.HTTPClient = http.DefaultClient
	}
	res := DepthBenchResult{
		VariantID:     deps.VariantID,
		EngineModel:   deps.EngineModel,
		ContextLength: deps.ContextLength,
		KVCacheType:   deps.KVCacheType,
		MeasuredAt:    deps.Now().UTC(),
	}
	plan := depthStagePlan(deps.ContextLength)
	if deps.EnginePort == 0 || len(plan) == 0 {
		return res
	}

	cacheKey := depthBenchCacheKey(deps)
	if cacheKey == "" && deps.Cache != nil {
		// Same three inputs, same silence, same reason (waired-agent#1150).
		// A sweep is 25 minutes of engine, so an uncacheable one is worth
		// more than the boot benchmark's.
		deps.Logger.Info("long-context benchmark: caching is off",
			"reason", benchCacheDisabledReason(deps.GPUModel, deps.VariantSHA, deps.EngineVersion))
	}
	if cacheKey != "" && deps.Cache != nil {
		if cached, hit, err := deps.Cache.LoadDepth(cacheKey); err != nil {
			deps.Logger.Warn("long-context benchmark: cache load failed; will measure", "err", err)
		} else if hit {
			deps.Logger.Info("long-context benchmark: cache hit",
				"key", cacheKey, "stages", len(cached.Stages),
				"measured_at", cached.MeasuredAt.UTC().Format(time.RFC3339))
			return cached
		}
	}
	deps.Logger.Info("long-context benchmark starting in background; local inference may be slower for a few minutes",
		"stages", len(plan), "window", deps.ContextLength)

	ctx, cancel := context.WithTimeout(ctx, depthBenchTotalBudget)
	defer cancel()

	base := fmt.Sprintf("http://127.0.0.1:%d", deps.EnginePort)
	for _, depth := range plan {
		stage, err := runDepthStage(ctx, deps, base, depth)
		res.Stages = append(res.Stages, stage)
		if err != nil {
			if stage.OutOfMemory {
				// Not "a stage failed" — this host cannot serve the
				// window it is configured for, which is the same fact a
				// request-time out-of-memory carries and takes the same
				// route (waired-agent#1058). Deeper stages are not worth
				// running: they can only fail harder.
				deps.Logger.Warn("long-context benchmark: the accelerator ran out of memory at this depth; this window does not work on this host",
					"depth", depth, "window", deps.ContextLength, "err", err)
				if deps.OnFitFailure != nil {
					deps.OnFitFailure(fmt.Sprintf("the long-context benchmark ran out of accelerator memory at ~%dk tokens: %v",
						depth/1024, err))
				}
				return res // Completed stays false
			}
			deps.Logger.Warn("long-context benchmark stage failed; aborting remaining stages",
				"depth", depth, "err", err)
			return res // Completed stays false
		}
		deps.Logger.Info("long-context benchmark stage",
			"depth_target", depth, "prompt_tokens", stage.PromptTokens,
			"prefill_tok_s", fmt.Sprintf("%.0f", stage.PrefillTokps),
			"decode_tok_s", fmt.Sprintf("%.1f", stage.DecodeTokps))
	}
	res.Completed = true
	if cacheKey != "" && deps.Cache != nil {
		if err := deps.Cache.StoreDepth(cacheKey, res, deps.GPUModel, deps.VRAMTotalMB, deps.DriverVersion, deps.EngineVersion); err != nil {
			deps.Logger.Warn("long-context benchmark: cache store failed", "err", err)
		}
	}
	return res
}

func runDepthStage(ctx context.Context, deps DepthBenchDeps, baseURL string, depth int) (DepthStageResult, error) {
	st := DepthStageResult{TargetTokens: depth}
	sctx, cancel := context.WithTimeout(ctx, depthStageTimeout)
	defer cancel()

	counters, err := postOllamaGenerate(sctx, deps.HTTPClient, baseURL, map[string]any{
		"model":  deps.EngineModel,
		"prompt": depthBenchPrompt(depth, depthPromptTokensPerLine, deps.Nonce),
		"stream": false,
		"options": map[string]any{
			"num_predict": depthStageCompletionTokens,
			"temperature": 0,
		},
	})
	if err != nil {
		st.Failed, st.Err = true, err.Error()
		// Classified here rather than inside postOllamaGenerate, which
		// the host-cutoff probe also calls against a ~1 GB probe model
		// (#496). An out-of-memory there says nothing about the tuning
		// of the model this host actually serves, and routing it to the
		// fit ladder would blame the wrong configuration.
		st.OutOfMemory = infruntime.EngineOutOfMemory(err.Error())
		return st, err
	}
	prefill, decode, err := counters.rates()
	if err != nil {
		st.Failed, st.Err = true, err.Error()
		return st, err
	}
	st.PromptTokens = counters.PromptEvalCount
	st.PrefillTokps, st.DecodeTokps = prefill, decode
	return st, nil
}

// worstCompletedDepthDecode returns the slowest decode rate among the
// successfully-measured stages (and its target depth) — the number the
// #133 lighter-model recommendation compares against the interactive
// floor, since a session AT depth is exactly where slowness hurts.
func worstCompletedDepthDecode(d *DepthBenchResult) (decodeTokps float64, targetTokens int, ok bool) {
	if d == nil {
		return 0, 0, false
	}
	for _, st := range d.Stages {
		if st.Failed || st.DecodeTokps <= 0 {
			continue
		}
		if !ok || st.DecodeTokps < decodeTokps {
			decodeTokps, targetTokens, ok = st.DecodeTokps, st.TargetTokens, true
		}
	}
	return decodeTokps, targetTokens, ok
}

// depthOutOfMemory reports the shallowest depth at which the
// accelerator ran out of memory, if any stage did.
//
// The companion to worstCompletedDepthDecode, and the reason it needed
// one: that function answers "how slow was this host", and a host that
// could not answer at all is not slow. Keeping the two apart is what
// lets interactiveFloorVerdict say which of the two happened
// (waired-agent#1058).
func depthOutOfMemory(d *DepthBenchResult) (targetTokens int, ok bool) {
	if d == nil {
		return 0, false
	}
	for _, st := range d.Stages {
		if !st.OutOfMemory {
			continue
		}
		if !ok || st.TargetTokens < targetTokens {
			targetTokens, ok = st.TargetTokens, true
		}
	}
	return targetTokens, ok
}

// appliedTuningReader is the slice of *infruntime.OllamaAdapter the
// depth scheduler needs.
type appliedTuningReader interface {
	AppliedTuning() infruntime.ModelTuning
}

// depthBenchTuningWait bounds how long the background depth run waits
// for the #621 tuning verification to settle before reading the
// applied window. Generous: a fresh install pulls a 20+ GB model
// before the verify pass can even load it.
const depthBenchTuningWait = 15 * time.Minute

// waitForAppliedTuning polls until the applied tuning reports
// Verified (the one-shot verify/degrade cycle is over — starting a
// multi-minute prefill mid-restart would measure a dying engine), or
// the deadline passes; it returns the latest tuning either way. The
// caller skips the run when ContextLength is 0 (untuned engine).
func waitForAppliedTuning(ctx context.Context, r appliedTuningReader, poll, timeout time.Duration) infruntime.ModelTuning {
	deadline := time.Now().Add(timeout)
	for {
		t := r.AppliedTuning()
		if t.Verified || time.Now().After(deadline) {
			return t
		}
		select {
		case <-ctx.Done():
			return r.AppliedTuning()
		case <-time.After(poll):
		}
	}
}
