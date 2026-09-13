package main

// The served model's speed measurement (waired-ai/waired-agent#1341).
//
// One request on the model this host serves — a
// hostfit.SpeedMeasurementDepthTokens prompt and a
// hostfit.SpeedMeasurementCompletionTokens decode — yields the engine's own
// prefill and decode rates at depth, and the verdict is what they cost one
// coding-agent request together: hostfit.TurnSecondsAt against
// hostfit.ModelTurnBudgetSeconds. It replaced two measurements that never
// read each other's results — a shallow decode benchmark and a
// 4,096 / 8,192 / 32,768 prefill ladder — and so could not see that the
// shallow decode overstated the rate at depth by 27-31 %, nor that no switch
// verdict read prefill at all (decisions 1-4 of
// docs/decisions/20260913/2245-speed-is-one-request-at-32768-tokens.md).

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/proto/hostfit"
	"github.com/waired-ai/waired-agent/proto/signer"
)

const (
	// modelSpeedStallCap is the one ceiling on a measurement request. It is
	// not a timeout on the verdict — a measurement past the line keeps going
	// (decision 4) — but the detector for an engine that has stopped
	// answering. Twenty minutes is a 32,768-token prefill at ~27 tok/s, a
	// host already six times over the line; what it ends in is kept in
	// memory only, like a failure, because a stopped engine is not a speed.
	modelSpeedStallCap = 20 * time.Minute

	// modelSpeedDepthMargin is the part of the served window the prompt
	// leaves clear, beside the decode: the chat template and the question
	// tail are not counted by the line estimate.
	modelSpeedDepthMargin = 2048

	// modelSpeedMinDepthTokens is the shallowest measurement worth taking at
	// all. Below it the request is dominated by fixed overhead rather than
	// by prefill and decode. A host measured shallower than the canonical
	// depth publishes the depth it reached beside the figure
	// (DepthTokens), so the difference stays visible.
	modelSpeedMinDepthTokens = 1024

	// modelSpeedResizeMargin is what a prompt resized to the engine's own
	// window leaves clear of it, beside the decode.
	modelSpeedResizeMargin = 256

	// modelSpeedCalibrationLines sizes the request that measures what one
	// filler line costs in tokens on this model — the exchange rate only the
	// engine can report (see syntheticPromptLines).
	modelSpeedCalibrationLines = 40

	// modelSpeedProgressEvery is how often a running request reports how
	// long it has been going.
	modelSpeedProgressEvery = 5 * time.Second

	// modelSpeedTrafficPoll is how often a running request checks whether
	// this host's own traffic wants the engine back.
	modelSpeedTrafficPoll = 500 * time.Millisecond

	// modelSpeedQuestion ends the prompt with a request that cannot be
	// answered in a sentence, so the decode reaches its sample length on
	// models that would otherwise stop after a short paragraph.
	modelSpeedQuestion = "Question: go through the log above entry by entry, from the first to the last, " +
		"and for each entry restate its subsystem, state, latency and checksum in words, one entry per line."
)

// errYieldedToTraffic ends a measurement that gave the engine back to this
// host's own serving traffic. Not a verdict and not a failure: nothing was
// learned, and the measurement is owed again once the host is idle.
var errYieldedToTraffic = errors.New("the measurement gave the engine back to serving traffic")

// errSelectionChanged ends a measurement whose model was switched while it
// ran: the figure would describe a model this host no longer serves.
var errSelectionChanged = errors.New("the model was switched during the measurement")

// modelSpeed is one finished measurement: a figure, or — for a request
// that stalled past modelSpeedStallCap — only its lower bound.
type modelSpeed struct {
	DepthTokens      int
	PrefillTokps     float64
	DecodeTokps      float64
	TurnSeconds      float64
	TurnFloorSeconds float64
	Samples          int
	SpreadPct        float64
	Method           string
}

// bound reports that the measurement produced only a lower bound.
func (m modelSpeed) bound() bool { return m.TurnSeconds <= 0 && m.TurnFloorSeconds > 0 }

// modelSpeedLine is the line the measurement is judged against, with the
// test seam folded in.
func (d BenchDeps) modelSpeedLine() float64 {
	if d.LineSeconds > 0 {
		return d.LineSeconds
	}
	return hostfit.ModelTurnBudgetSeconds
}

func (d BenchDeps) modelSpeedProgressEvery() time.Duration {
	if d.ProgressEvery > 0 {
		return d.ProgressEvery
	}
	return modelSpeedProgressEvery
}

func (d BenchDeps) modelSpeedStallCap() time.Duration {
	if d.StallCap > 0 {
		return d.StallCap
	}
	return modelSpeedStallCap
}

// modelSpeedDepth is how deep this host measures: the canonical depth, or
// as deep as its served window allows with the margin and the decode left
// clear. A host on the smallest ollama window (32,768) measures at 30,592 —
// about 7 % shallower than the constant depth of
// docs/decisions/20260829/1740, in the direction that flatters it; the depth
// is published beside the figure so the difference is visible, and
// TurnSeconds is still normalised to the canonical depth.
func modelSpeedDepth(appliedWindow int) int {
	depth := hostfit.SpeedMeasurementDepthTokens
	if appliedWindow > 0 {
		if usable := appliedWindow - modelSpeedDepthMargin - hostfit.SpeedMeasurementCompletionTokens; usable < depth {
			depth = usable
		}
	}
	return depth
}

// modelSpeedDepthAccepted is the read-back guard: an engine silently
// truncates a prompt that overflows its window, and a truncated prefill
// measures the truncation.
func modelSpeedDepthAccepted(want, got int) bool {
	if want <= 0 || got <= 0 {
		return false
	}
	ratio := float64(got) / float64(want)
	return ratio >= 0.7 && ratio <= 1.5
}

// modelSpeedSample is what one request reported.
type modelSpeedSample struct {
	promptTokens     int
	completionTokens int
	prefillTokps     float64
	decodeTokps      float64
}

// modelSpeedSampler sends one prompt of lines filler lines and a decode of
// maxTokens, and reports the engine's counters.
type modelSpeedSampler func(ctx context.Context, lines int, nonce string, maxTokens int) (modelSpeedSample, error)

// measureModelSpeed takes the measurement against an engine that is already
// warm. It reports progress while a request runs — elapsed seconds, and past
// the line the lower bound — through deps.Progress.
func measureModelSpeed(ctx context.Context, deps BenchDeps) (modelSpeed, error) {
	sampler, method, err := modelSpeedSamplerFor(deps)
	if err != nil {
		return modelSpeed{}, err
	}
	depth := modelSpeedDepth(deps.AppliedWindow)
	if depth < modelSpeedMinDepthTokens {
		return modelSpeed{}, fmt.Errorf("the served window (%d tokens) is too small to measure at depth", deps.AppliedWindow)
	}
	nonce := deps.Nonce
	if nonce == "" {
		nonce = fmt.Sprintf("speed%d", deps.Now().UnixNano())
	}

	// What a filler line costs, measured on this model.
	cal, err := sampler(ctx, modelSpeedCalibrationLines, nonce+"-cal", 1)
	if err != nil {
		return modelSpeed{}, fmt.Errorf("calibration: %w", err)
	}
	if cal.promptTokens <= 0 {
		return modelSpeed{}, errors.New("calibration: the engine reported no prompt tokens")
	}
	tokensPerLine := float64(cal.promptTokens) / float64(modelSpeedCalibrationLines)
	lines := int(math.Ceil(float64(depth) / tokensPerLine))

	line := deps.modelSpeedLine()
	var samples []modelSpeedSample
	resized := false
	for attempt := 0; len(samples) < 2 && attempt < 4; attempt++ {
		trial := len(samples) + 1
		s, stalledFor, err := runModelSpeedSample(ctx, deps, sampler, lines, fmt.Sprintf("%s-%d", nonce, attempt), depth, trial)
		if stalledFor > 0 {
			floor := stalledFor.Seconds() * float64(hostfit.SpeedMeasurementDepthTokens) / float64(depth)
			deps.Logger.Warn("model speed measurement: the engine stopped answering; keeping only a lower bound",
				"after", stalledFor.Truncate(time.Second).String(), "turn_floor_seconds", math.Round(floor))
			return modelSpeed{DepthTokens: depth, TurnFloorSeconds: floor, Samples: len(samples), Method: method}, nil
		}
		if err != nil {
			return modelSpeed{}, err
		}
		if !modelSpeedDepthAccepted(depth, s.promptTokens) {
			// The engine truncated the prompt to a window this host did not
			// report — an engine serving without an applied tuning, such as
			// a CI runner's tiny model at the engine's default context. What
			// it prefilled IS its window: measure once more inside it rather
			// than call an engine that answered a failure. Only once, and only
			// shallower: a prompt the engine still refuses is an error.
			if !resized && s.promptTokens > 0 && s.promptTokens < depth {
				resized = true
				next := s.promptTokens - modelSpeedResizeMargin - hostfit.SpeedMeasurementCompletionTokens
				if next >= modelSpeedMinDepthTokens {
					deps.Logger.Info("model speed measurement: the engine's window is smaller than the prompt; measuring inside it",
						"asked_tokens", depth, "prefilled_tokens", s.promptTokens, "depth_tokens", next)
					depth = next
					lines = int(math.Ceil(float64(depth) / tokensPerLine))
					continue
				}
			}
			return modelSpeed{}, fmt.Errorf("the engine prefilled %d tokens for a %d-token prompt (window %d); refusing to judge a truncated prompt",
				s.promptTokens, depth, deps.AppliedWindow)
		}
		if s.completionTokens < hostfit.SpeedMeasurementMinCompletionTokens {
			// A model that stopped early; one more try with another nonce.
			deps.Logger.Info("model speed measurement: the model stopped early; sampling again",
				"completion_tokens", s.completionTokens)
			continue
		}
		samples = append(samples, s)
		turn := hostfit.TurnSecondsAt(hostfit.SpeedMeasurementDepthTokens, s.prefillTokps, s.decodeTokps)
		if len(samples) == 1 && math.Abs(turn-line) > line*hostfit.SpeedMeasurementSecondSampleBand {
			break
		}
	}
	if len(samples) == 0 {
		return modelSpeed{}, fmt.Errorf("the model stopped before %d tokens on every attempt", hostfit.SpeedMeasurementMinCompletionTokens)
	}
	return combineModelSpeedSamples(samples, depth, method), nil
}

// combineModelSpeedSamples reduces one or two samples to the published
// figure: the mean of each rate, and the request time from those means.
func combineModelSpeedSamples(samples []modelSpeedSample, depth int, method string) modelSpeed {
	var prefill, decode float64
	var turns []float64
	for _, s := range samples {
		prefill += s.prefillTokps
		decode += s.decodeTokps
		turns = append(turns, hostfit.TurnSecondsAt(hostfit.SpeedMeasurementDepthTokens, s.prefillTokps, s.decodeTokps))
	}
	n := float64(len(samples))
	out := modelSpeed{
		DepthTokens:  samples[0].promptTokens,
		PrefillTokps: prefill / n,
		DecodeTokps:  decode / n,
		Samples:      len(samples),
		Method:       method,
	}
	out.TurnSeconds = hostfit.TurnSecondsAt(hostfit.SpeedMeasurementDepthTokens, out.PrefillTokps, out.DecodeTokps)
	if len(turns) > 1 {
		out.SpreadPct = spreadPercent(turns)
	}
	_ = depth
	return out
}

// runModelSpeedSample sends one measurement request and watches it: every
// modelSpeedProgressEvery it reports the elapsed seconds (and, past the line,
// the bound), and when this host's own serving traffic starts it takes the
// request back rather than making a person's turn wait behind it.
//
// stalledFor is non-zero when the request ran into the stall cap.
func runModelSpeedSample(ctx context.Context, deps BenchDeps, sampler modelSpeedSampler,
	lines int, nonce string, depth, trial int) (s modelSpeedSample, stalledFor time.Duration, err error) {
	line := deps.modelSpeedLine()
	stall := deps.modelSpeedStallCap()
	sctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	sctx, cancelStall := context.WithTimeout(sctx, stall)
	defer cancelStall()

	start := deps.Now()
	report := func() {
		elapsed := deps.Now().Sub(start).Seconds()
		p := BenchProgress{
			Phase:          benchPhaseMeasuring,
			Trial:          trial - 1,
			Trials:         trial,
			Method:         "",
			ElapsedSeconds: elapsed,
			BudgetSeconds:  line,
			DepthTokens:    depth,
		}
		if elapsed > line {
			p.OverBudget = true
			p.TurnFloorSeconds = elapsed * float64(hostfit.SpeedMeasurementDepthTokens) / float64(depth)
		}
		deps.report(p)
	}
	report()

	type result struct {
		s   modelSpeedSample
		err error
	}
	done := make(chan result, 1)
	go func() {
		s, err := sampler(sctx, lines, nonce, hostfit.SpeedMeasurementCompletionTokens)
		done <- result{s, err}
	}()

	progress := time.NewTicker(deps.modelSpeedProgressEvery())
	defer progress.Stop()
	traffic := time.NewTicker(modelSpeedTrafficPoll)
	defer traffic.Stop()
	for {
		select {
		case r := <-done:
			switch {
			case r.err == nil:
				return r.s, 0, nil
			case errors.Is(context.Cause(sctx), errYieldedToTraffic):
				return modelSpeedSample{}, 0, errYieldedToTraffic
			case errors.Is(context.Cause(sctx), errSelectionChanged):
				return modelSpeedSample{}, 0, errSelectionChanged
			case ctx.Err() != nil:
				return modelSpeedSample{}, 0, ctx.Err()
			case errors.Is(sctx.Err(), context.DeadlineExceeded):
				return modelSpeedSample{}, deps.Now().Sub(start), nil
			default:
				return modelSpeedSample{}, 0, r.err
			}
		case <-progress.C:
			report()
		case <-traffic.C:
			if deps.ServingInFlight != nil && deps.ServingInFlight() > 0 {
				cancel(errYieldedToTraffic)
			}
			if deps.Selected != nil && deps.Selected() != deps.VariantID {
				cancel(errSelectionChanged)
			}
		}
	}
}

// modelSpeedSamplerFor picks the engine's own counters: ollama's
// prompt_eval_* / eval_* on /api/generate, or on vLLM a streamed chat
// completion timed to its first token with the token counts from usage.
func modelSpeedSamplerFor(deps BenchDeps) (modelSpeedSampler, string, error) {
	base := fmt.Sprintf("http://127.0.0.1:%d", deps.EnginePort)
	switch deps.EngineKind {
	case signer.InferenceTypeOllama:
		return func(ctx context.Context, lines int, nonce string, maxTokens int) (modelSpeedSample, error) {
			c, err := postOllamaGenerate(ctx, deps.HTTPClient, base, map[string]any{
				"model":  deps.EngineModel,
				"prompt": syntheticPromptLinesAsking(lines, nonce, modelSpeedQuestion),
				"stream": false,
				// No num_ctx and no keep_alive: the request is served by the
				// runner as the host serves it, with its window and its
				// residency, which is the configuration being measured.
				"options": map[string]any{
					"num_predict": maxTokens,
					"temperature": 0,
				},
			})
			if err != nil {
				return modelSpeedSample{}, err
			}
			s := modelSpeedSample{promptTokens: c.PromptEvalCount, completionTokens: c.EvalCount}
			if maxTokens <= 1 {
				return s, nil
			}
			prefill, decode, err := c.rates()
			if err != nil {
				return modelSpeedSample{}, err
			}
			s.prefillTokps, s.decodeTokps = prefill, decode
			return s, nil
		}, signer.BenchmarkMethodOllamaEval, nil
	case signer.InferenceTypeVLLM:
		return func(ctx context.Context, lines int, nonce string, maxTokens int) (modelSpeedSample, error) {
			turn, err := streamOpenAICompletionWith(ctx, openAICutoffDeps{
				BaseURL: base, Model: deps.EngineModel, HTTPClient: deps.HTTPClient, Logger: deps.Logger,
			}, syntheticPromptLinesAsking(lines, nonce, modelSpeedQuestion), maxTokens,
				map[string]any{"min_tokens": maxTokens})
			if err != nil {
				return modelSpeedSample{}, err
			}
			s := modelSpeedSample{promptTokens: turn.promptTokens, completionTokens: turn.completionTokens}
			if maxTokens <= 1 {
				return s, nil
			}
			probe, err := turn.probe()
			if err != nil {
				return modelSpeedSample{}, err
			}
			if s.completionTokens <= 0 {
				s.completionTokens = turn.chunks
			}
			s.prefillTokps, s.decodeTokps = probe.PrefillTokps, probe.DecodeTokps
			return s, nil
		}, signer.BenchmarkMethodOpenAIStreamTTFT, nil
	default:
		return nil, "", fmt.Errorf("no speed measurement for engine %q", strings.TrimSpace(deps.EngineKind))
	}
}

// awaitServingIdle waits until this host has served nothing for idle,
// polling ServingInFlight. false when ctx ends first.
func awaitServingIdle(ctx context.Context, deps BenchDeps, idle time.Duration) bool {
	quietSince := deps.Now()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		if deps.ServingInFlight() > 0 {
			quietSince = deps.Now()
		} else if deps.Now().Sub(quietSince) >= idle {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-tick.C:
		}
	}
}
