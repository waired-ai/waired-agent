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
	"encoding/json"
	"fmt"
	"io"
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
	NumBatch      int    // applied generation ubatch (#642), for the cache key

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
func depthBenchPrompt(targetTokens int, nonce string) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "session %s log begin\n", nonce)
	for n, i := 0, 0; n < targetTokens; n, i = n+depthPromptTokensPerLine, i+1 {
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
		if err := deps.Cache.StoreDepth(cacheKey, res, deps.GPUModel, deps.VRAMTotalMB, deps.DriverVersion); err != nil {
			deps.Logger.Warn("long-context benchmark: cache store failed", "err", err)
		}
	}
	return res
}

func runDepthStage(ctx context.Context, deps DepthBenchDeps, baseURL string, depth int) (DepthStageResult, error) {
	st := DepthStageResult{TargetTokens: depth}
	sctx, cancel := context.WithTimeout(ctx, depthStageTimeout)
	defer cancel()

	body, err := json.Marshal(map[string]any{
		"model":  deps.EngineModel,
		"prompt": depthBenchPrompt(depth, deps.Nonce),
		"stream": false,
		"options": map[string]any{
			"num_predict": depthStageCompletionTokens,
			"temperature": 0,
		},
	})
	if err != nil {
		st.Failed, st.Err = true, err.Error()
		return st, err
	}
	req, err := http.NewRequestWithContext(sctx, http.MethodPost, baseURL+"/api/generate", bytes.NewReader(body))
	if err != nil {
		st.Failed, st.Err = true, err.Error()
		return st, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := deps.HTTPClient.Do(req)
	if err != nil {
		st.Failed, st.Err = true, err.Error()
		return st, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		err := fmt.Errorf("engine returned %d: %s", resp.StatusCode, bytes.TrimSpace(raw))
		st.Failed, st.Err = true, err.Error()
		return st, err
	}
	var gen struct {
		PromptEvalCount    int   `json:"prompt_eval_count"`
		PromptEvalDuration int64 `json:"prompt_eval_duration"` // ns
		EvalCount          int   `json:"eval_count"`
		EvalDuration       int64 `json:"eval_duration"` // ns
	}
	if err := json.NewDecoder(resp.Body).Decode(&gen); err != nil {
		st.Failed, st.Err = true, err.Error()
		return st, err
	}
	if gen.PromptEvalDuration <= 0 || gen.EvalDuration <= 0 || gen.EvalCount <= 0 {
		err := fmt.Errorf("engine returned no timing counters (prompt_eval_duration=%d eval_duration=%d)",
			gen.PromptEvalDuration, gen.EvalDuration)
		st.Failed, st.Err = true, err.Error()
		return st, err
	}
	st.PromptTokens = gen.PromptEvalCount
	st.PrefillTokps = float64(gen.PromptEvalCount) / (float64(gen.PromptEvalDuration) / 1e9)
	st.DecodeTokps = float64(gen.EvalCount) / (float64(gen.EvalDuration) / 1e9)
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
