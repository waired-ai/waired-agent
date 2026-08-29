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

	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// The prefill measurement (waired-agent#1127).
//
// Nothing in this repository turned an observed wait into a prefill rate,
// and peer selection had no term for what a turn costs the person waiting
// for it: on the rc4 review mesh an engine-less host's turns went to the
// peer running the biggest model, which answered a 30k-token first turn in
// 9 min 10 s while another of the same person's machines answered it in 43
// (waired-agent#1082). Prefill is the half that decides the wait — that
// same turn had 709 ms of decode behind it.
//
// The figures already in the tree could not answer it. HostSpeed.PrefillTokps
// is a FIXED 0.8 B probe model by design ("the point of a fixed probe is
// comparability"), is ollama-only, and is stripped from the served
// NetworkMap. DepthStageResult measures the served model but is ollama-only,
// local, and its 10-minute stage timeout excludes exactly the hosts this
// exists for — 64k at 54 tok/s is 1,214 s.
//
// # Why the depths are fixed, and shared by every host
//
// Prefill throughput is not independent of prompt depth: the attention term
// grows with position, so tokens/second falls as the prompt gets longer.
// Measured in this repository on ONE machine with ONE model
// (docs/knowledges/20260805/1830-ollama-prompt-depth-two-traps.md):
//
//	11,526 tokens → 833 tok/s
//	21,247 tokens → 583 tok/s
//
// A 1.84x deeper prompt reads 30 % slower. That note's own conclusion is
// "浅いプレフィルはホストを速く見せる" — a shallow prefill makes a host
// look fast.
//
// So a depth chosen per host would make the depth a confounder of exactly
// the quantity being compared, and a first draft of this file did that: it
// scaled the depth by the host's own speed, which measured a fast peer
// DEEPER and a slow one SHALLOWER — biased in favour of the slow peer, in
// the one comparison this whole programme exists to get right. The rungs
// below are constants for that reason, and it is the same reason
// hostfit.HostCutoffProbeDepthTokens is one: "the point of a FIXED probe is
// comparability".
//
// A host publishes every rung it completed, and a requester compares two
// peers at the DEEPEST RUNG BOTH REACHED. Unequal depths are never compared.
//
// The depths live in internal/router because BOTH sides need the same
// list: this side to climb it, the requester to know which rung an
// observed turn belongs to. Two copies would drift, and the failure would
// be silent — observations would simply stop merging with published
// readings.
var prefillRungs = router.PrefillRungDepths

const (
	// prefillWarmupTokens is the discarded first run. It has two jobs.
	//
	// It absorbs what is about the first request rather than about the
	// machine — the cold page cache, graph capture, kernel selection, the
	// top of the boost curve.
	//
	// And it CALIBRATES the prompt. The synthetic prompt is built from
	// numbered filler lines, and the tokens-per-line rate is the model's to
	// decide: the same text measured 35 tokens/line on the #625 harness and
	// 51.6-55.7 on a 0.8 B model, so a prompt built with the wrong constant
	// landed at 55 % of the requested depth — and a shallow prefill reads
	// fast (docs/knowledges/20260805/1830). The warm-up reads its own
	// prompt_eval_count back and the rungs are built from that.
	prefillWarmupTokens = 1024

	// prefillSamplesPerRung / prefillSpreadTarget: keep sampling a rung
	// until two readings agree within 10 %, up to three. The figures come
	// from the host-cutoff probe's own calibration — idle runs of one host
	// sat within +/-2 % of each other, while the single run that shared the
	// machine with another job landed +21 %.
	prefillSamplesPerRung = 3
	prefillSpreadTarget   = 0.10

	// prefillDepthMarginTokens keeps a rung clear of the served window.
	// The engine silently truncates a prompt that overflows it, and a
	// truncated prefill measures the truncation rather than the host — the
	// same margin the depth benchmark leaves.
	prefillDepthMarginTokens = 2048

	// prefillDepthToleranceLow / High bound what counts as having measured
	// the rung that was asked for, read back off the engine's own
	// prompt_eval_count. Same band, for the same reason, as the host-cutoff
	// probe's: read the depth back and refuse to judge on one that is not
	// the depth asked for.
	prefillDepthToleranceLow  = 0.7
	prefillDepthToleranceHigh = 1.5

	// prefillMeasureBudget is the wall clock one host may spend. Owner
	// ruling, 2026-08-29: init WAITS for this measurement, so that a node is
	// not handed to the operator — or to the mesh — in a state where nothing
	// knows what it costs to use. Three minutes is what that wait is worth.
	//
	// The model load is NOT in it: this runs against an engine that is
	// already serving.
	//
	// A host too slow to finish even the first rung publishes a BOUND for
	// it instead of a rate. That is the shape waired-agent#579 already
	// settled for the host-cutoff probe (owner ruling, 2026-08-09) — a bound
	// goes in its own field, so a consumer that has not been taught it reads
	// "no measurement" and declines to judge.
	prefillMeasureBudget = 3 * time.Minute
)

// PrefillRung is one host's reading at one of the fixed depths.
type PrefillRung struct {
	// Depth is the rung — one of prefillRungs. Two peers' rates are
	// comparable only at an equal Depth.
	Depth int
	// Tokps is prompt tokens per second. When Bound is true it is an UPPER
	// bound (the host did not get through Depth tokens in the time it had,
	// so it is no faster than this) and not a measurement.
	Tokps float64
	Bound bool
	// PromptTokens is what the engine reported actually prefilling, read
	// back rather than assumed.
	PromptTokens int
	// Samples is how many readings the figure is the median of; SpreadPct
	// is (max-min)/median across them. Samples <= 1 is a reading that was
	// never checked against another — the meaning signer.HostSpeed.Samples
	// already carries.
	Samples   int
	SpreadPct float64
}

// PrefillMeasurement is what one host learned about prefilling the model
// it is serving: one entry per rung it completed, shallowest first.
type PrefillMeasurement struct {
	Rungs []PrefillRung
	// VariantID is the catalog variant this was measured on. The figures
	// are meaningless against any other, so they travel with it.
	VariantID string

	Failed bool
	Err    string
}

// Known reports whether this measurement says anything at all. An unknown
// measurement must never be read as "slow" — see the nil rule in
// docs/decisions/20260822/0218-residency-breaks-ties-only.md.
func (m PrefillMeasurement) Known() bool { return !m.Failed && len(m.Rungs) > 0 }

// prefillLinesFor converts a wanted depth into a line count using a
// measured tokens-per-line rate. tokensPerLine <= 0 falls back to the
// #625 harness figure, which is only right for that model family — the
// caller is expected to have calibrated.
func prefillLinesFor(depth int, tokensPerLine float64) int {
	if tokensPerLine <= 0 {
		tokensPerLine = depthPromptTokensPerLine
	}
	lines := int(float64(depth)/tokensPerLine + 0.9999)
	if lines < 1 {
		lines = 1
	}
	return lines
}

// prefillRungsFor is the ladder this host can actually climb: the fixed
// rungs that fit inside its served window, with the margin left clear.
// appliedWindow of 0 means the window is unknown and every rung is
// attempted.
func prefillRungsFor(appliedWindow int) []int {
	if appliedWindow <= 0 {
		return prefillRungs
	}
	usable := appliedWindow - prefillDepthMarginTokens
	var out []int
	for _, d := range prefillRungs {
		if d <= usable {
			out = append(out, d)
		}
	}
	return out
}

// prefillSettled reports the median of the samples, their spread, and
// whether they have agreed closely enough to stop asking. Two readings are
// the minimum that can agree at all — one has nothing to be checked
// against, which is what Samples <= 1 tells a consumer.
func prefillSettled(samples []float64) (median, spreadPct float64, settled bool) {
	if len(samples) == 0 {
		return 0, 0, false
	}
	sorted := append([]float64(nil), samples...)
	sort.Float64s(sorted)
	median = sorted[len(sorted)/2]
	if len(sorted)%2 == 0 {
		median = (sorted[len(sorted)/2-1] + sorted[len(sorted)/2]) / 2
	}
	if median <= 0 {
		return median, 0, false
	}
	spreadPct = (sorted[len(sorted)-1] - sorted[0]) / median * 100
	return median, spreadPct, len(sorted) >= 2 && spreadPct <= prefillSpreadTarget*100
}

// prefillDepthAccepted reports whether a read-back prompt length counts as
// having measured the rung that was asked for.
func prefillDepthAccepted(want, got int) bool {
	if want <= 0 || got <= 0 {
		return false
	}
	ratio := float64(got) / float64(want)
	return ratio >= prefillDepthToleranceLow && ratio <= prefillDepthToleranceHigh
}

// prefillSampler takes one reading from a prompt of the given LINE count,
// and reports the rate and the prompt length the engine says it prefilled.
// Lines rather than tokens because only the engine can say what the
// exchange rate is — see prefillWarmupTokens.
type prefillSampler func(ctx context.Context, lines int) (tokps float64, promptTokens int, err error)

// PrefillDeps is what MeasurePrefillRate needs.
type PrefillDeps struct {
	EngineKind  string
	EnginePort  int
	EngineModel string
	VariantID   string
	// AppliedWindow is the serve window the engine was given; 0 = unknown.
	AppliedWindow int
	// Nonce leads every prompt line so two runs never share a prefix — a
	// shared prefix is served from the engine's cache and measures nothing.
	Nonce string

	Budget     time.Duration
	Now        func() time.Time
	HTTPClient *http.Client
	Logger     *slog.Logger

	// Sample overrides the engine-derived sampler. Tests drive the protocol
	// through it; production leaves it nil.
	Sample prefillSampler
}

func (d PrefillDeps) now() time.Time {
	if d.Now == nil {
		return time.Now()
	}
	return d.Now()
}

func (d PrefillDeps) budget() time.Duration {
	if d.Budget <= 0 {
		return prefillMeasureBudget
	}
	return d.Budget
}

// MeasurePrefillRate climbs the fixed ladder as far as the budget allows.
//
//  1. One warm-up run, DISCARDED — and read back, to calibrate how many
//     tokens a line of the synthetic prompt is worth on THIS model.
//  2. For each rung that fits the served window, shallowest first: sample
//     until two readings agree, up to prefillSamplesPerRung.
//  3. Move to the next rung only while the budget can hold one sample of
//     it, estimated from the rate just measured.
//
// A rung whose read-back depth misses the tolerance band is dropped rather
// than published: a truncated prefill measures its own truncation. A first
// rung that could not be finished at all publishes a bound — the host did
// not get through Depth tokens in the time it had.
func MeasurePrefillRate(ctx context.Context, deps PrefillDeps) PrefillMeasurement {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	out := PrefillMeasurement{VariantID: deps.VariantID}

	sample := deps.Sample
	if sample == nil {
		var err error
		sample, err = prefillSamplerFor(deps)
		if err != nil {
			out.Failed, out.Err = true, err.Error()
			return out
		}
	}

	ladder := prefillRungsFor(deps.AppliedWindow)
	if len(ladder) == 0 {
		out.Failed = true
		out.Err = fmt.Sprintf("served window %d holds none of the measurement depths", deps.AppliedWindow)
		return out
	}

	deadline := deps.now().Add(deps.budget())
	left := func() time.Duration { return deadline.Sub(deps.now()) }

	// --- warm-up, discarded, and the prompt calibration -----------------
	warmLines := prefillLinesFor(prefillWarmupTokens, 0)
	_, warmTokens, err := sample(ctx, warmLines)
	if err != nil {
		// An engine that cannot answer is not a slow engine.
		out.Failed, out.Err = true, err.Error()
		return out
	}
	tokensPerLine := float64(warmTokens) / float64(warmLines)
	if tokensPerLine <= 0 {
		out.Failed = true
		out.Err = "engine reported no prompt tokens for the calibration run"
		return out
	}
	deps.Logger.Debug("prefill measurement: prompt calibrated",
		"variant", deps.VariantID, "tokens_per_line", tokensPerLine)

	// --- the ladder ------------------------------------------------------
	lastTokps := 0.0
	for _, depth := range ladder {
		if left() <= 0 {
			break
		}
		// Do not start a rung the budget cannot hold one sample of. The
		// estimate comes from the rung just measured, or from the warm-up
		// for the first one.
		if lastTokps > 0 {
			want := time.Duration(float64(depth) / lastTokps * float64(time.Second))
			if want > left() {
				deps.Logger.Debug("prefill measurement: stopping short of the next rung",
					"variant", deps.VariantID, "depth", depth,
					"estimated", want.String(), "budget_left", left().String())
				break
			}
		}

		lines := prefillLinesFor(depth, tokensPerLine)
		var kept []float64
		lastTokens := 0
		var rungErr error
		startedAt := deps.now()
		for len(kept) < prefillSamplesPerRung {
			if len(kept) > 0 && left() <= 0 {
				break
			}
			tokps, tokens, err := sample(ctx, lines)
			if err != nil {
				rungErr = err
				break
			}
			if !prefillDepthAccepted(depth, tokens) {
				rungErr = fmt.Errorf("engine prefilled %d tokens for a %d-token rung", tokens, depth)
				break
			}
			kept = append(kept, tokps)
			lastTokens = tokens
			if _, _, settled := prefillSettled(kept); settled {
				break
			}
		}

		if len(kept) == 0 {
			if len(out.Rungs) == 0 && rungErr != nil {
				// Nothing has been measured at all, and the shallowest rung
				// did not finish. What IS known is that this host did not
				// get through `depth` tokens in the time it spent trying.
				if elapsed := deps.now().Sub(startedAt).Seconds(); elapsed > 0 {
					out.Rungs = append(out.Rungs, PrefillRung{
						Depth: depth, Tokps: float64(depth) / elapsed, Bound: true,
					})
					deps.Logger.Info("prefill measurement: bounded rather than measured",
						"variant", deps.VariantID, "depth", depth,
						"at_most_tokps", out.Rungs[0].Tokps, "err", rungErr)
					return out
				}
				out.Failed, out.Err = true, rungErr.Error()
			}
			break
		}

		median, spread, _ := prefillSettled(kept)
		out.Rungs = append(out.Rungs, PrefillRung{
			Depth: depth, Tokps: median, PromptTokens: lastTokens,
			Samples: len(kept), SpreadPct: spread,
		})
		lastTokps = median
		deps.Logger.Info("prefill measurement: rung completed",
			"variant", deps.VariantID, "engine_kind", deps.EngineKind,
			"depth", depth, "tokps", median, "samples", len(kept),
			"spread_pct", fmt.Sprintf("%.1f", spread))
	}

	if len(out.Rungs) == 0 && !out.Failed {
		out.Failed = true
		out.Err = "no rung completed inside the budget"
	}
	return out
}

// prefillSamplerFor picks how to read a prefill rate off this engine.
//
// ollama has native counters (prompt_eval_count / prompt_eval_duration off
// /api/generate) and they are strictly better than a wall clock: they
// exclude request overhead and queueing, which is how the pre-#764
// benchmark under-measured fast hosts by ~35 %.
//
// vLLM exposes no equivalent on its OpenAI-compatible surface, so the same
// rate is taken as prompt tokens over the wall clock of a request that
// decodes ONE token. At these depths the decode term is noise, and the
// quotient is the same quantity in the same units.
func prefillSamplerFor(deps PrefillDeps) (prefillSampler, error) {
	if deps.EnginePort <= 0 || deps.EngineModel == "" {
		return nil, fmt.Errorf("no engine to measure (port=%d model=%q)", deps.EnginePort, deps.EngineModel)
	}
	client := deps.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", deps.EnginePort)
	switch deps.EngineKind {
	case signer.InferenceTypeOllama:
		return ollamaPrefillSampler(client, base, deps.EngineModel, deps.Nonce), nil
	case signer.InferenceTypeVLLM:
		return openAIPrefillSampler(client, base, deps.EngineModel, deps.Nonce, deps.Now), nil
	default:
		return nil, fmt.Errorf("engine kind %q has no prefill sampler", deps.EngineKind)
	}
}

// ollamaPrefillSampler reads the engine's OWN prefill counters off
// /api/generate. Shared response shape with the depth benchmark and the
// host-cutoff probe (postOllamaGenerate), so there is one place that knows
// it.
//
// num_predict is 1 rather than the depth benchmark's 200: this measures
// prefill, and every decoded token is time spent measuring something else.
func ollamaPrefillSampler(client *http.Client, baseURL, model, nonce string) prefillSampler {
	call := 0
	return func(ctx context.Context, lines int) (float64, int, error) {
		call++
		counters, err := postOllamaGenerate(ctx, client, baseURL, map[string]any{
			"model": model,
			// A per-call nonce: two samples that shared a prefix would be
			// served from the engine's cache and measure nothing.
			"prompt": depthBenchPromptLines(lines, fmt.Sprintf("%s-%d", nonce, call)),
			"stream": false,
			"options": map[string]any{
				"num_predict": 1,
				"temperature": 0,
			},
		})
		if err != nil {
			return 0, 0, err
		}
		if counters.PromptEvalCount <= 0 || counters.PromptEvalDuration <= 0 {
			return 0, 0, fmt.Errorf("no prefill counters in engine response: prompt_eval_count=%d prompt_eval_duration=%d",
				counters.PromptEvalCount, counters.PromptEvalDuration)
		}
		tokps := float64(counters.PromptEvalCount) / (float64(counters.PromptEvalDuration) / 1e9)
		return tokps, counters.PromptEvalCount, nil
	}
}

// openAIPrefillSampler is the vLLM path: prompt tokens over the wall clock
// of a request that decodes one token.
//
// It reads usage.prompt_tokens rather than trusting the prompt builder's
// estimate, for the reason the host-cutoff probe reads its depth back — the
// tokens-per-line exchange rate is the model's to decide.
func openAIPrefillSampler(client *http.Client, baseURL, model, nonce string, now func() time.Time) prefillSampler {
	if now == nil {
		now = time.Now
	}
	call := 0
	return func(ctx context.Context, lines int) (float64, int, error) {
		call++
		body, err := json.Marshal(map[string]any{
			"model":      model,
			"max_tokens": 1,
			"stream":     false,
			"messages": []map[string]string{
				{"role": "user", "content": depthBenchPromptLines(lines, fmt.Sprintf("%s-%d", nonce, call))},
			},
		})
		if err != nil {
			return 0, 0, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			baseURL+"/v1/chat/completions", bytes.NewReader(body))
		if err != nil {
			return 0, 0, err
		}
		req.Header.Set("Content-Type", "application/json")

		start := now()
		resp, err := client.Do(req)
		if err != nil {
			return 0, 0, err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return 0, 0, engineHTTPError(resp)
		}
		var out struct {
			Usage struct {
				PromptTokens int `json:"prompt_tokens"`
			} `json:"usage"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
			return 0, 0, fmt.Errorf("decode engine response: %w", err)
		}
		elapsed := now().Sub(start).Seconds()
		if out.Usage.PromptTokens <= 0 {
			return 0, 0, fmt.Errorf("engine reported no prompt tokens")
		}
		if elapsed <= 0 {
			return 0, 0, fmt.Errorf("engine answered in no measurable time")
		}
		return float64(out.Usage.PromptTokens) / elapsed, out.Usage.PromptTokens, nil
	}
}
