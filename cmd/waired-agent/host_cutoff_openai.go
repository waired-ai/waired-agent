package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/proto/hostfit"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// The host-speed probe, measured over an OpenAI-compatible surface
// instead of ollama's counters.
//
// host_cutoff_probe.go reads prompt_eval_* / eval_* off /api/generate,
// which only ollama reports. A vLLM host therefore had no probe at all:
// hostCutoffProbeTag refused the engine outright, the stage machine never
// left its zero value, and the wizard's "measuring this computer with a
// small model" note — which is shown to agents that report NO row — stayed
// up for the life of the setup while nothing was being measured
// (waired-agent#1298).
//
// What replaces the counters is the client's own clock over a STREAMED
// completion, which is the only place the two phases can still be told
// apart: everything up to the first content token is prefill, everything
// after it is decode. The token counts come from the response's usage
// block (vLLM fills it in streaming mode when stream_options.include_usage
// is set), so neither figure is estimated from characters.
//
// The DEPTH is the probe's, not the boot benchmark's. inference_bench.go
// measures a decode rate from 64- and 256-token completions, and
// proto/hostfit/host_cutoff.go records why that cannot stand in here: a
// shallow benchmark overstates real-session decode by 24-60% on CPU and
// 10-16% on GPU, and the 45 s turn budget this feeds is calibrated at
// hostfit.HostCutoffProbeDepthTokens. So what is borrowed from the
// benchmark is client-side timing, and nothing else.

// openAICutoffDeps is what one client-timed measurement needs.
type openAICutoffDeps struct {
	// BaseURL is the engine's loopback root ("http://127.0.0.1:9479").
	BaseURL string
	// Model is the name the engine serves under (--served-model-name).
	Model string

	HTTPClient *http.Client
	Logger     *slog.Logger

	// Nonce leads the prompt so no two runs share a prefix. vLLM's prefix
	// cache is on by design for coding agents (--enable-prefix-caching is
	// pinned), so a repeat that shared an opening would be answered at a
	// rate no host can achieve.
	Nonce string

	// MeasureBudget bounds the whole sampled measurement; 0 means
	// hostCutoffMeasureBudget. Injected so the early exits can be tested
	// without a test that waits minutes.
	MeasureBudget time.Duration

	// RequestTimeout is the per-request ceiling; 0 means
	// hostCutoffProbeTimeout.
	RequestTimeout time.Duration
}

func (d openAICutoffDeps) client() *http.Client {
	if d.HTTPClient != nil {
		return d.HTTPClient
	}
	return http.DefaultClient
}

func (d openAICutoffDeps) requestTimeout() time.Duration {
	if d.RequestTimeout > 0 {
		return d.RequestTimeout
	}
	return hostCutoffProbeTimeout
}

// measureHostCutoffOpenAI takes the host-speed measurement over the
// OpenAI-compatible surface and returns it in the same shape the ollama
// probe produces, so everything downstream — the verdict, the published
// record, the setup rows — is the one implementation.
//
// There is no screen arm. The screen exists to end a run early on a host
// far below the budget without paying for a full-depth sample, and it
// concludes from ollama's prefill counter; a host reaching this function
// is an NVIDIA GPU host with a vLLM venv, where the full sample is seconds
// rather than the minutes the screen was written to avoid.
func measureHostCutoffOpenAI(ctx context.Context, deps openAICutoffDeps) (hostCutoffMeasurement, error) {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.BaseURL == "" || deps.Model == "" {
		return hostCutoffMeasurement{}, fmt.Errorf("host cutoff probe: no engine to measure")
	}
	budget := deps.MeasureBudget
	if budget <= 0 {
		budget = hostCutoffMeasureBudget
	}
	ctx, cancel := context.WithDeadline(ctx, time.Now().Add(budget))
	defer cancel()

	deps.Logger.Info("measuring whether this host can serve local inference usefully; this takes a minute or two",
		"model", deps.Model, "depth_tokens", hostfit.HostCutoffProbeDepthTokens,
		"samples", benchSampleCount, "budget", budget, "surface", "openai")

	// What a filler line costs in tokens, measured rather than assumed —
	// the same correction the ollama path makes, for the same reason: the
	// depth is a token count and the prompt is built out of lines.
	tokensPerLine, err := calibrateOpenAICutoffPrompt(ctx, deps)
	if err != nil {
		return hostCutoffMeasurement{}, err
	}

	samples := make([]hostfit.HostProbe, 0, benchSampleCount)
	for sample := 1; sample <= benchSampleCount; sample++ {
		if err := ctx.Err(); err != nil {
			break
		}
		probe, err := measureHostCutoffOpenAIOnce(ctx, deps, tokensPerLine,
			hostCutoffNonce(deps.Nonce, sample, 1))
		if err != nil {
			if len(samples) > 0 {
				deps.Logger.Info("host cutoff: a sample failed; reducing what was measured",
					"samples", len(samples), "err", err)
				break
			}
			return hostCutoffMeasurement{}, err
		}
		samples = append(samples, probe)
	}
	if len(samples) == 0 {
		return hostCutoffMeasurement{}, fmt.Errorf("host cutoff probe: no sample completed")
	}
	m := reduceHostCutoffSamples(samples)
	// reduceHostCutoffSamples stamps the ollama method because it was
	// written for the one caller that existed. The numbers are the shape
	// it says they are; the clock is not, and the record has to say so.
	m.Method = signer.BenchmarkMethodOpenAIStreamTTFT
	return m, nil
}

// calibrateOpenAICutoffPrompt measures the tokens-per-line exchange rate
// with one cheap request, so the depth the samples ask for is the depth
// they get.
func calibrateOpenAICutoffPrompt(ctx context.Context, deps openAICutoffDeps) (int, error) {
	reqCtx, cancel := context.WithTimeout(ctx, hostCutoffCalibrationTimeout)
	defer cancel()

	turn, err := streamOpenAICompletion(reqCtx, deps,
		syntheticPromptLines(hostCutoffCalibrationLines, hostCutoffNonce(deps.Nonce, 0, 1)), 1)
	if err != nil {
		return 0, fmt.Errorf("host cutoff probe: calibration: %w", err)
	}
	if turn.promptTokens <= 0 {
		return 0, fmt.Errorf("host cutoff probe: calibration: the engine reported no prompt tokens")
	}
	perLine := turn.promptTokens / hostCutoffCalibrationLines
	if perLine <= 0 {
		perLine = 1
	}
	deps.Logger.Info("host cutoff: measured what a filler line costs",
		"tokens_per_line", perLine, "prompt_tokens", turn.promptTokens,
		"lines", hostCutoffCalibrationLines)
	return perLine, nil
}

// measureHostCutoffOpenAIOnce is one full-depth streamed completion.
func measureHostCutoffOpenAIOnce(ctx context.Context, deps openAICutoffDeps, tokensPerLine int, nonce string) (hostfit.HostProbe, error) {
	reqCtx, cancel := context.WithTimeout(ctx, deps.requestTimeout())
	defer cancel()

	turn, err := streamOpenAICompletion(reqCtx, deps,
		syntheticPrompt(hostfit.HostCutoffProbeDepthTokens, tokensPerLine, nonce),
		hostfit.HostCutoffCompletionSampleTokens)
	if err != nil {
		return hostfit.HostProbe{}, err
	}
	return turn.probe()
}

// openAITurn is one streamed completion, timed.
type openAITurn struct {
	promptTokens     int
	completionTokens int
	// prefill is request-sent to first content token. It carries the HTTP
	// round trip and the engine's scheduling, both of which are noise
	// beside a 21,000-token prefill and neither of which can be separated
	// out from outside the engine.
	prefill time.Duration
	// decode is first content token to last.
	decode time.Duration
	// chunks is how many content deltas arrived. Used only to tell a
	// stream that produced tokens from one that produced none.
	chunks int
}

// probe converts a timed turn into the shape the verdict is computed from.
func (t openAITurn) probe() (hostfit.HostProbe, error) {
	if t.promptTokens <= 0 {
		return hostfit.HostProbe{}, fmt.Errorf("host cutoff probe: the engine reported no prompt tokens")
	}
	if t.chunks < 2 || t.decode <= 0 {
		return hostfit.HostProbe{}, fmt.Errorf("host cutoff probe: the engine streamed %d content tokens", t.chunks)
	}
	if t.prefill <= 0 {
		return hostfit.HostProbe{}, fmt.Errorf("host cutoff probe: no time passed before the first token")
	}
	decoded := t.completionTokens
	if decoded <= 0 {
		decoded = t.chunks
	}
	// decoded-1, not decoded: the first token's cost is in the prefill
	// term, and counting it twice would inflate a fast host's decode rate
	// by a whole token's worth of prefill.
	return hostfit.HostProbe{
		PromptTokens: t.promptTokens,
		PrefillTokps: float64(t.promptTokens) / t.prefill.Seconds(),
		DecodeTokps:  float64(decoded-1) / t.decode.Seconds(),
	}, nil
}

// streamOpenAICompletion posts one streamed chat completion and times it.
func streamOpenAICompletion(ctx context.Context, deps openAICutoffDeps, prompt string, maxTokens int) (openAITurn, error) {
	body, err := json.Marshal(map[string]any{
		"model":       deps.Model,
		"stream":      true,
		"max_tokens":  maxTokens,
		"temperature": 0,
		// Without this vLLM streams no usage block at all, and the token
		// counts would have to be estimated from the text — which is the
		// estimate hostfit refuses to build a verdict on.
		"stream_options": map[string]any{"include_usage": true},
		"messages":       []map[string]string{{"role": "user", "content": prompt}},
	})
	if err != nil {
		return openAITurn{}, err
	}
	url := strings.TrimRight(deps.BaseURL, "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return openAITurn{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	start := time.Now()
	resp, err := deps.client().Do(req)
	if err != nil {
		return openAITurn{}, fmt.Errorf("host cutoff probe: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return openAITurn{}, fmt.Errorf("host cutoff probe: status %d: %s",
			resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	var turn openAITurn
	var first, last time.Time
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if chunk.Usage != nil {
			turn.promptTokens = chunk.Usage.PromptTokens
			turn.completionTokens = chunk.Usage.CompletionTokens
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content == "" {
				continue
			}
			now := time.Now()
			if first.IsZero() {
				first = now
			}
			last = now
			turn.chunks++
		}
	}
	if err := sc.Err(); err != nil {
		return openAITurn{}, fmt.Errorf("host cutoff probe: reading the stream: %w", err)
	}
	if !first.IsZero() {
		turn.prefill = first.Sub(start)
		turn.decode = last.Sub(first)
	}
	return turn, nil
}
