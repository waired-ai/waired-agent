// The install-time host cutoff's MEASUREMENT half (#496). The policy it
// feeds — what the numbers mean and where the threshold comes from —
// lives in internal/router/host_cutoff.go; the wiring that acts on the
// verdict lives in host_cutoff.go.
//
// One request, against the ollama NATIVE API: /api/generate is the only
// surface that reports prompt_eval_* / eval_* counters, and the counters
// are the whole point. Wall clock on a 1 GB model is dominated by model
// load and request overhead.
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

	"github.com/waired-ai/waired-agent/internal/router"
)

const (
	// hostCutoffPromptTokensPerLine is the probe model's token cost for
	// one line of the shared filler builder, measured against the probe
	// model with THIS file's nonce: 55.7 (412 lines → 22935 tokens).
	//
	// It is NOT depthPromptTokensPerLine (35), which was calibrated on the
	// #625 harness's much larger model — the line is dense in digits and
	// hyphens, and the two tokenizers disagree by 59 %. Getting this wrong
	// is not a rounding error in either direction: a prefill measured at
	// 11.5k reads FASTER than the same host at 21k (833 vs 671 tok/s on
	// the reference machine), so a short prompt flatters exactly the hosts
	// the cutoff exists to catch.
	//
	// The nonce is part of the calibration, not a detail: it repeats on
	// every line, so its length moves this figure (a 22-character nonce
	// measures 51.6 where the 32-character one here measures 55.7).
	//
	// Re-measure if HostCutoffProbeModelID or the nonce format changes.
	// The depth readback in router.HostProbe.Measured is what keeps a
	// stale figure from producing a WRONG verdict rather than no verdict.
	hostCutoffPromptTokensPerLine = 55

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
)

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
	// EngineModel is the ollama tag of router.HostCutoffProbeModelID —
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
}

// measureHostCutoff takes the one measurement the cutoff judges: a
// ~21k-token prefill and a 200-token decode on the probe model, reported
// by the engine's own counters.
//
// The returned HostProbe is only meaningful when it reports Measured();
// an error always yields a zero probe, and the caller must treat that as
// "no verdict" rather than as a slow host.
//
// keep_alive:0 unloads the probe model as soon as the response is
// written. The counters are unaffected (they cover prefill and decode,
// not load), and the alternative is leaving ~1 GB plus a 23k KV cache
// resident on the host least able to spare it, right as the real model
// starts downloading.
func measureHostCutoff(ctx context.Context, deps hostCutoffDeps) (router.HostProbe, error) {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.BaseURL == "" || deps.EngineModel == "" {
		return router.HostProbe{}, fmt.Errorf("host cutoff probe: no engine to measure")
	}
	deps.Logger.Info("measuring whether this host can serve local inference usefully; this takes about a minute",
		"model", deps.EngineModel, "depth_tokens", router.HostCutoffProbeDepthTokens)

	window := hostCutoffWindowSlots*router.HostCutoffProbeDepthTokens + hostCutoffWindowMargin
	var probe router.HostProbe
	for attempt := 1; attempt <= hostCutoffMaxAttempts; attempt++ {
		var err error
		// The attempt number goes in the nonce, not just the run's: a
		// retry that reused the first attempt's prompt would share its
		// prefix, and the engine would answer it out of the KV cache.
		probe, err = measureHostCutoffOnce(ctx, deps, window, fmt.Sprintf("%s-%d", deps.Nonce, attempt))
		if err != nil {
			return router.HostProbe{}, err
		}
		if probe.Measured() {
			return probe, nil
		}
		// Not a usable measurement. The only shape worth another request
		// is a SHORT one — the engine capped the prompt at what one of
		// its slots holds — and the cap tells us exactly how much wider
		// to ask. Anything else (a prompt somehow too long) is a
		// different fault and retrying it would only cost time.
		if probe.PromptTokens <= 0 || probe.PromptTokens >= router.HostCutoffProbeDepthTokens {
			return probe, nil
		}
		window = window * router.HostCutoffProbeDepthTokens / probe.PromptTokens
		window += hostCutoffWindowMargin
		deps.Logger.Info("host cutoff: the engine capped the prompt; measuring again with a wider window",
			"prompt_tokens", probe.PromptTokens, "want_tokens", router.HostCutoffProbeDepthTokens,
			"num_ctx", window)
	}
	return probe, nil
}

// measureHostCutoffOnce is one /api/generate at the given serve window.
func measureHostCutoffOnce(ctx context.Context, deps hostCutoffDeps, window int, nonce string) (router.HostProbe, error) {
	ctx, cancel := context.WithTimeout(ctx, hostCutoffProbeTimeout)
	defer cancel()

	counters, err := postOllamaGenerate(ctx, deps.HTTPClient, deps.BaseURL, map[string]any{
		"model":  deps.EngineModel,
		"prompt": depthBenchPrompt(router.HostCutoffProbeDepthTokens, hostCutoffPromptTokensPerLine, nonce),
		"stream": false,
		// Seconds, and 0 means "unload now" — not the duration string
		// form, which the API also accepts.
		"keep_alive": 0,
		"options": map[string]any{
			"num_predict": router.HostCutoffCompletionSampleTokens,
			"temperature": 0,
			"num_ctx":     window,
		},
	})
	if err != nil {
		return router.HostProbe{}, err
	}
	prefill, decode, err := counters.rates()
	if err != nil {
		return router.HostProbe{}, err
	}
	return router.HostProbe{
		PromptTokens: counters.PromptEvalCount,
		PrefillTokps: prefill,
		DecodeTokps:  decode,
	}, nil
}
