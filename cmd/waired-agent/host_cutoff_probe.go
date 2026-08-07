// The install-time host cutoff's MEASUREMENT half (#496). The policy it
// feeds — what the numbers mean and where the threshold comes from —
// lives in proto/hostfit/host_cutoff.go; the wiring that acts on the
// verdict lives in host_cutoff.go.
//
// Requests go to the ollama NATIVE API: /api/generate is the only surface
// that reports prompt_eval_* / eval_* counters, and the counters are the
// whole point. Wall clock on a 1 GB model is dominated by model load and
// request overhead.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/waired-ai/waired-agent/proto/hostfit"
)

const (
	// hostCutoffCalibrationLines is how many filler lines the probe sends
	// to find out what a line costs on this model, before it builds the
	// real prompt.
	//
	// It measures rather than assumes because a baked-in figure has now
	// been wrong twice, both times silently and both times in the
	// direction that flatters a slow host. The line is dense in digits and
	// hyphens, so the exchange rate moves with the tokenizer (the #625
	// harness's model reads 35 tokens/line where the probe model reads
	// ~56) AND with the nonce, which repeats on every line: a 19-character
	// nonce measures ~38 where a 32-character one measures ~56. A prompt
	// at 68 % of the requested depth reads FASTER than the same host at
	// full depth (833 vs 671 tok/s on the reference machine), so getting
	// this wrong lets through exactly the hosts the cutoff exists to
	// catch — or, once the depth readback guard is in place, reaches no
	// verdict at all on hosts that are perfectly fine.
	//
	// 50 lines is ~2.8k tokens: about 4 s on the slowest host expected and
	// a fraction of a second on a card. The nonce it uses is the same
	// WIDTH as the sampling nonces (hostCutoffNonce), because the width is
	// what the answer depends on, and a different one so the calibration
	// prompt is not a prefix of the real one.
	hostCutoffCalibrationLines = 50

	// hostCutoffPromptTokensPerLine is the seed estimate, used only when
	// the calibration above cannot be taken. 38 is what the probe model
	// reads at the current nonce width, measured on the reference host.
	//
	// A wrong value here no longer produces a wrong verdict — the depth
	// readback still refuses to judge a prompt that missed the depth — so
	// this is a starting point, not a calibration.
	hostCutoffPromptTokensPerLine = 38

	// hostCutoffWindowMargin covers the chat template and the residual
	// tokens-per-line estimate error on top of the depth itself.
	hostCutoffWindowMargin = 2048

	// hostCutoffWindowSlots multiplies the window we ask the engine for.
	//
	// num_ctx is not a per-request prompt budget: ollama divides it among
	// its parallel slots and truncates the prompt to what one slot holds.
	// Measured on the reference host, with the daemon's own
	// OLLAMA_NUM_PARALLEL unset (the fresh-install case, where the boot
	// plan is untuned): num_ctx 23048 capped the prompt at 11526 and
	// num_ctx 46096 capped it at 23050 — half, each time. Asking for
	// depth + margin alone therefore measured 55 % of the depth, and
	// silently.
	//
	// 2 covers the split seen in practice; measureHostCutoff reads the
	// depth back and retries wider if some host splits further, so this
	// is the common-case saving rather than the guarantee. Kept as small
	// as that allows for two reasons: the window is real memory (the
	// probe model costs 12 KB/token of KV, so each slot factor is
	// ~270 MB), and a wider window measures slightly SLOWER on a
	// CPU-only host — the reference machine reads 583 tok/s of prefill
	// at this window against the 671 the 45 s threshold was calibrated
	// at, i.e. a ~5 % conservative bias. Both effects point the same way
	// and both are far inside the threshold's margin.
	hostCutoffWindowSlots = 2

	// hostCutoffMaxAttempts bounds the widen-and-retry above. Two: one
	// ordinary measurement, and one correction for a host whose engine
	// splits the window further than hostCutoffWindowSlots expects.
	hostCutoffMaxAttempts = 2

	// hostCutoffProbeTimeout bounds each measuring request. Generous by
	// design: the reference CPU-only host spends ~40 s here, and a host
	// several times slower is exactly the host this measures. A run that
	// exceeds even this is not evidence of a slow host — a wedged engine
	// looks identical — so the timeout yields "undecided" and the install
	// path carries on unchanged.
	hostCutoffProbeTimeout = 10 * time.Minute

	// hostCutoffMeasureBudget bounds the whole sampled measurement, not
	// one request. The figure is published, and a published measurement is
	// the median of N samples with their spread rather than one reading
	// (proto/signer/inference_state.go, the memory_bandwidth_measured_gbs
	// doc) — but N samples on the slowest host is exactly where the cost
	// lands, and this runs before the model download rather than instead
	// of it. So: take as many of benchSampleCount as fit, never fewer than
	// one, and publish how many were actually taken.
	//
	// Three minutes buys all three samples on the reference CPU-only host
	// (~40 s each) and on anything faster; a host slower than that
	// publishes one or two and is, by the same token, nowhere near the
	// threshold.
	hostCutoffMeasureBudget = 3 * time.Minute
)

// hostCutoffMeasurement is a completed sampled measurement: the median
// sample, and the provenance that travels with it on the wire.
type hostCutoffMeasurement struct {
	// Probe is the MEDIAN SAMPLE — one run's own three numbers, not a
	// field-wise median of several. A field-wise median would put a
	// prefill/decode pair on the wire that no run ever produced, and the
	// turn time computed from it would belong to no measurement.
	Probe hostfit.HostProbe

	// Samples is how many usable samples the median was taken over, and
	// SpreadPct is (max−min)/median over their turn times. Samples is 0
	// when nothing usable was measured.
	Samples   int
	SpreadPct float64
}

// ollamaGenCounters are the timing counters ollama's /api/generate
// returns for a non-streaming request. Durations are nanoseconds.
type ollamaGenCounters struct {
	PromptEvalCount    int   `json:"prompt_eval_count"`
	PromptEvalDuration int64 `json:"prompt_eval_duration"`
	EvalCount          int   `json:"eval_count"`
	EvalDuration       int64 `json:"eval_duration"`
}

// rates converts the counters to (prefill, decode) tok/s. The error names
// the missing counter rather than returning zeros, because a build or
// backend that stops reporting them must be diagnosable: a silent 0 tok/s
// reads as an infinitely slow host.
func (c ollamaGenCounters) rates() (prefill, decode float64, err error) {
	if c.PromptEvalDuration <= 0 || c.EvalDuration <= 0 || c.EvalCount <= 0 {
		return 0, 0, fmt.Errorf("engine returned no timing counters (prompt_eval_duration=%d eval_duration=%d eval_count=%d)",
			c.PromptEvalDuration, c.EvalDuration, c.EvalCount)
	}
	return float64(c.PromptEvalCount) / (float64(c.PromptEvalDuration) / 1e9),
		float64(c.EvalCount) / (float64(c.EvalDuration) / 1e9),
		nil
}

// postOllamaGenerate issues one non-streaming /api/generate and returns
// its timing counters. Shared by the depth benchmark (#624) and the host
// cutoff probe (#496) so there is one place that knows the response shape.
func postOllamaGenerate(ctx context.Context, client *http.Client, baseURL string, payload map[string]any) (ollamaGenCounters, error) {
	var counters ollamaGenCounters
	body, err := json.Marshal(payload)
	if err != nil {
		return counters, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return counters, err
	}
	req.Header.Set("Content-Type", "application/json")
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return counters, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return counters, fmt.Errorf("engine returned %d: %s", resp.StatusCode, bytes.TrimSpace(raw))
	}
	if err := json.NewDecoder(resp.Body).Decode(&counters); err != nil {
		return counters, err
	}
	return counters, nil
}

// hostCutoffProbeDeps is the injectable world of measureHostCutoff.
type hostCutoffDeps struct {
	// BaseURL is the engine's loopback root ("http://127.0.0.1:11434").
	BaseURL string
	// EngineModel is the ollama tag of hostfit.HostCutoffProbeModelID —
	// the engine-native name, not the catalog id.
	EngineModel string

	HTTPClient *http.Client
	Logger     *slog.Logger

	// Nonce leads the prompt so no two runs share a prefix. Without it
	// ollama's prefix KV cache answers a repeat with the FULL
	// prompt_eval_count and a near-zero duration — measured at 697,222
	// tok/s on this repo's reference host, which would pass every machine
	// ever built.
	Nonce string

	// MeasureBudget bounds the whole sampled measurement. 0 means
	// hostCutoffMeasureBudget. Injected rather than read from the constant
	// so the early-exit can be tested without a test that waits minutes.
	MeasureBudget time.Duration
}

// measureHostCutoff takes the measurement the cutoff judges and the wire
// publishes: up to benchSampleCount runs of a ~21k-token prefill plus a
// 200-token decode on the probe model, reported by the engine's own
// counters, reduced to the median run.
//
// Sampling is what makes the number safe to act on. The repeat that fixed
// the 45 s threshold found idle runs within ±2 % of each other and a
// single run that shared the machine with another job at +21 % — enough,
// on its own, to move a host across the threshold. The median of three
// throws that run away; one reading cannot.
//
// The returned measurement is only meaningful when its Probe reports
// Measured(); an error always yields a zero measurement, and the caller
// must treat that as "no verdict" rather than as a slow host. A sample
// that fails outright ends the run: a host whose engine has stopped
// answering is not a host to publish a median for.
func measureHostCutoff(ctx context.Context, deps hostCutoffDeps) (hostCutoffMeasurement, error) {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.BaseURL == "" || deps.EngineModel == "" {
		return hostCutoffMeasurement{}, fmt.Errorf("host cutoff probe: no engine to measure")
	}
	budget := deps.MeasureBudget
	if budget <= 0 {
		budget = hostCutoffMeasureBudget
	}
	deps.Logger.Info("measuring whether this host can serve local inference usefully; this takes a minute or two",
		"model", deps.EngineModel, "depth_tokens", hostfit.HostCutoffProbeDepthTokens,
		"samples", benchSampleCount, "budget", budget)

	started := time.Now()
	tokensPerLine := hostCutoffPromptTokensPerLine
	if measured, err := calibrateHostCutoffPrompt(ctx, deps); err != nil {
		deps.Logger.Info("host cutoff: could not measure what a prompt line costs; using the seed estimate",
			"tokens_per_line", tokensPerLine, "err", err)
	} else {
		deps.Logger.Info("host cutoff: measured the prompt's token cost",
			"tokens_per_line", measured, "seed", hostCutoffPromptTokensPerLine)
		tokensPerLine = measured
	}

	var (
		usable  []hostfit.HostProbe
		slowest time.Duration
		last    hostfit.HostProbe
	)
	for sample := 1; sample <= benchSampleCount; sample++ {
		// Stop before starting a sample the budget cannot hold. The
		// slowest sample so far is the estimate: samples on one idle host
		// vary by a few percent, and the one case where they do not — a
		// machine that just got busy — is the case worth cutting short.
		if sample > 1 && time.Since(started)+slowest > budget {
			deps.Logger.Info("host cutoff: stopping at the sample budget",
				"samples", len(usable), "elapsed", time.Since(started).Round(time.Second))
			break
		}
		at := time.Now()
		probe, err := measureHostCutoffSample(ctx, deps, sample, tokensPerLine)
		if err != nil {
			if len(usable) == 0 {
				return hostCutoffMeasurement{}, err
			}
			// Some samples landed. Judging on them beats discarding a
			// measurement because the host got busy near the end.
			deps.Logger.Info("host cutoff: a sample did not complete; judging on the ones that did",
				"samples", len(usable), "err", err)
			break
		}
		if took := time.Since(at); took > slowest {
			slowest = took
		}
		last = probe
		if probe.Measured() {
			usable = append(usable, probe)
		}
	}
	if len(usable) == 0 {
		// Nothing usable, but the last probe still carries the depth the
		// engine reported — which is how the caller says "it prefilled
		// 11k of the 21k asked for" rather than just "no verdict".
		return hostCutoffMeasurement{Probe: last}, nil
	}
	return reduceHostCutoffSamples(usable), nil
}

// reduceHostCutoffSamples picks the median sample by turn time and
// records the spread across all of them.
//
// On an even count it takes the SLOWER of the two middle samples. The
// threshold is deliberately strict — a wrongly-cut host pays one
// `waired inference on`, a wrongly-passed one downloads 20-45 GB first —
// and this keeps that bias rather than reversing it in the one case where
// there is no single middle run.
func reduceHostCutoffSamples(samples []hostfit.HostProbe) hostCutoffMeasurement {
	sorted := append([]hostfit.HostProbe(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].TurnSeconds() < sorted[j].TurnSeconds() })
	turns := make([]float64, 0, len(sorted))
	for _, p := range sorted {
		turns = append(turns, p.TurnSeconds())
	}
	return hostCutoffMeasurement{
		Probe:     sorted[len(sorted)/2],
		Samples:   len(sorted),
		SpreadPct: spreadPercent(turns),
	}
}

// hostCutoffNonce is the per-request nonce. It leads the prompt, so the
// varying part goes FIRST: two prompts that differ only after a shared
// opening are answered from ollama's prefix KV cache.
//
// Fixed width by construction, because the nonce repeats on every filler
// line and its width is therefore part of what a line costs. A nonce that
// grew with the sample number would make every sample a slightly
// different depth.
func hostCutoffNonce(base string, sample, attempt int) string {
	return fmt.Sprintf("%02d%02d-%s", sample, attempt, base)
}

// calibrateHostCutoffPrompt measures what one filler line costs on this
// model, in tokens, by sending a short prompt and reading the prefill
// count back.
//
// num_predict:1 because only the prompt side is wanted here; the answer
// is thrown away. The count includes the prompt's fixed opening and
// closing lines, so it slightly OVERSTATES the per-line cost and the real
// prompt lands a percent or two short of the depth — immaterial next to
// the ±30 % the depth guard allows, and TurnSeconds normalises to the
// canonical depth regardless.
func calibrateHostCutoffPrompt(ctx context.Context, deps hostCutoffDeps) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, hostCutoffProbeTimeout)
	defer cancel()

	counters, err := postOllamaGenerate(ctx, deps.HTTPClient, deps.BaseURL, map[string]any{
		"model":      deps.EngineModel,
		"prompt":     depthBenchPromptLines(hostCutoffCalibrationLines, hostCutoffNonce(deps.Nonce, 0, 0)),
		"stream":     false,
		"keep_alive": 0,
		"options": map[string]any{
			"num_predict": 1,
			"temperature": 0,
			"num_ctx":     hostCutoffWindowSlots*hostfit.HostCutoffProbeDepthTokens + hostCutoffWindowMargin,
		},
	})
	if err != nil {
		return 0, err
	}
	if counters.PromptEvalCount <= 0 {
		return 0, fmt.Errorf("engine reported no prompt_eval_count for the calibration prompt")
	}
	perLine := counters.PromptEvalCount / hostCutoffCalibrationLines
	if perLine <= 0 {
		return 0, fmt.Errorf("calibration prompt measured %d tokens over %d lines",
			counters.PromptEvalCount, hostCutoffCalibrationLines)
	}
	return perLine, nil
}

// measureHostCutoffSample takes ONE sample, widening the serve window and
// re-measuring when the engine silently truncated the prompt.
//
// keep_alive:0 unloads the probe model as soon as the response is
// written. The counters are unaffected (they cover prefill and decode,
// not load), and the alternative is leaving ~1 GB plus a 23k KV cache
// resident on the host least able to spare it, right as the real model
// starts downloading.
func measureHostCutoffSample(ctx context.Context, deps hostCutoffDeps, sample, tokensPerLine int) (hostfit.HostProbe, error) {
	window := hostCutoffWindowSlots*hostfit.HostCutoffProbeDepthTokens + hostCutoffWindowMargin
	var probe hostfit.HostProbe
	for attempt := 1; attempt <= hostCutoffMaxAttempts; attempt++ {
		var err error
		probe, err = measureHostCutoffOnce(ctx, deps, window, tokensPerLine,
			hostCutoffNonce(deps.Nonce, sample, attempt))
		if err != nil {
			return hostfit.HostProbe{}, err
		}
		if probe.Measured() {
			return probe, nil
		}
		// Not a usable measurement. The only shape worth another request
		// is a SHORT one — the engine capped the prompt at what one of
		// its slots holds — and the cap tells us exactly how much wider
		// to ask. Anything else (a prompt somehow too long) is a
		// different fault and retrying it would only cost time.
		//
		// A prompt that was short because it was BUILT short cannot
		// reach here any more: the line cost is measured before the
		// samples, not assumed.
		if probe.PromptTokens <= 0 || probe.PromptTokens >= hostfit.HostCutoffProbeDepthTokens {
			return probe, nil
		}
		window = window * hostfit.HostCutoffProbeDepthTokens / probe.PromptTokens
		window += hostCutoffWindowMargin
		deps.Logger.Info("host cutoff: the engine capped the prompt; measuring again with a wider window",
			"prompt_tokens", probe.PromptTokens, "want_tokens", hostfit.HostCutoffProbeDepthTokens,
			"num_ctx", window)
	}
	return probe, nil
}

// measureHostCutoffOnce is one /api/generate at the given serve window.
func measureHostCutoffOnce(ctx context.Context, deps hostCutoffDeps, window, tokensPerLine int, nonce string) (hostfit.HostProbe, error) {
	ctx, cancel := context.WithTimeout(ctx, hostCutoffProbeTimeout)
	defer cancel()

	counters, err := postOllamaGenerate(ctx, deps.HTTPClient, deps.BaseURL, map[string]any{
		"model":  deps.EngineModel,
		"prompt": depthBenchPrompt(hostfit.HostCutoffProbeDepthTokens, tokensPerLine, nonce),
		"stream": false,
		// Seconds, and 0 means "unload now" — not the duration string
		// form, which the API also accepts.
		"keep_alive": 0,
		"options": map[string]any{
			"num_predict": hostfit.HostCutoffCompletionSampleTokens,
			"temperature": 0,
			"num_ctx":     window,
		},
	})
	if err != nil {
		return hostfit.HostProbe{}, err
	}
	prefill, decode, err := counters.rates()
	if err != nil {
		return hostfit.HostProbe{}, err
	}
	return hostfit.HostProbe{
		PromptTokens: counters.PromptEvalCount,
		PrefillTokps: prefill,
		DecodeTokps:  decode,
	}, nil
}
