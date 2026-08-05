package main

import "time"

// benchProbeTokens is the completion length of the FIRST measurement
// sample, before anything is known about how fast this host decodes.
//
// It exists because the benchmark used to ask every host for the same
// benchPromptCompletionTokens (200) inside the same benchTimeout (30 s),
// which is a request a slow host cannot satisfy: at ~2 tok/s, 200 tokens
// needs ~100 s, so sample 0 timed out, measureOllamaNative returned with
// zero samples, and the whole benchmark reported failure on an engine
// that was working correctly. Observed on the nightly
// `install+inference (linux)` leg — a 4-vCPU CPU-only runner serving
// qwen3.5-9b (run 30998191050).
//
// 32 tokens is not a throwaway: ollama's eval_count / eval_duration are
// pure decode counters, so a short run is an unbiased rate sample. It is
// simply the smallest one that still carries a usable pair.
const benchProbeTokens = 32

// benchProbeTimeout bounds that first sample. Generous relative to
// benchTimeout because it is the one request with no rate estimate
// behind it — 32 tokens at 0.5 tok/s is 64 s.
const benchProbeTimeout = 90 * time.Second

// benchSizingBudgetFraction is how much of a sample's share of the
// shared measurement budget the planner is willing to spend on decode.
// The remainder absorbs per-request overhead (HTTP, scheduling, prompt
// prefill) that the token count does not account for.
const benchSizingBudgetFraction = 0.7

// benchSizingTimeoutSlack multiplies the predicted decode time to get a
// per-request timeout. The prediction comes from previous samples on the
// same host, so it only has to cover ordinary variance.
const benchSizingTimeoutSlack = 2.0

// benchSizingFacts is everything the sizing decision depends on. Passed
// in rather than read, so the decision is table-testable without an
// engine (repo rule: put the seam below the behaviour).
type benchSizingFacts struct {
	// ObservedTokps is the rate measured so far on this host, or 0 when
	// nothing has been measured yet.
	ObservedTokps float64
	// Remaining is what is left of the shared measurement budget.
	Remaining time.Duration
	// SamplesLeft counts this sample and every one after it.
	SamplesLeft int
}

// benchSizingPlan is one measurement request's shape.
type benchSizingPlan struct {
	CompletionTokens int
	RequestTimeout   time.Duration
}

// planBenchSizing sizes the next measurement request so that a host slow
// enough to matter still returns a sample, instead of timing out and
// reporting a working engine as a failure (#203).
//
// Deliberately OS-invariant: nothing about decode speed varies by GOOS,
// so there is no (GOOS, facts) seam here — the facts are the whole input.
func planBenchSizing(f benchSizingFacts) benchSizingPlan {
	// Nothing measured yet, or a caller that lost count: take the probe.
	if f.ObservedTokps <= 0 || f.SamplesLeft <= 0 {
		return benchSizingPlan{
			CompletionTokens: benchProbeTokens,
			RequestTimeout:   benchProbeTimeout,
		}
	}

	// Split what is left of the shared budget evenly across the samples
	// still to take. A budget already spent leaves the probe size, which
	// the shared deadline will cut short anyway — sizing up into a window
	// that does not exist is how sample 0 failed in the first place.
	tokens := benchProbeTokens
	if f.Remaining > 0 {
		perSample := f.Remaining.Seconds() / float64(f.SamplesLeft)
		affordable := int(perSample * benchSizingBudgetFraction * f.ObservedTokps)
		tokens = max(tokens, affordable)
	}
	tokens = min(tokens, benchPromptCompletionTokens)

	// The timeout has to cover the tokens just planned, or the request is
	// one nobody intends to wait out. Never shorter than benchTimeout, so
	// a fast host keeps exactly today's behaviour.
	timeout := max(
		time.Duration(float64(tokens)/f.ObservedTokps*benchSizingTimeoutSlack*float64(time.Second)),
		benchTimeout,
	)
	// The shared budget is the real bound; this cap only keeps a wild
	// rate estimate from producing an absurd per-request deadline.
	timeout = min(timeout, benchMeasureBudget)
	return benchSizingPlan{CompletionTokens: tokens, RequestTimeout: timeout}
}
