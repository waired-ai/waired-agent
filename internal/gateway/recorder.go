package gateway

import "github.com/waired-ai/waired-agent/internal/observability"

// Recorder is the narrow telemetry interface the gateway emits into.
// The observability package supplies the concrete composite that
// satisfies it (alongside the router and inference interfaces) via
// duck typing.
//
// nil is supported at every call site: Deps.Recorder may be left
// zero and the gateway will skip emission. The Phase 8 slog.Warn
// behaviour is preserved when Recorder == nil so existing tests
// observe unchanged journal lines.
type Recorder interface {
	// RecordRequest is emitted once per gateway request at the
	// terminal point (success or error). Pre-selection failures
	// (malformed body, missing model) are intentionally skipped:
	// the gateway helper drops emits whose Model field is empty.
	RecordRequest(observability.RequestEvent)

	// RecordFallback is emitted in addition to RecordRequest when
	// the probe-then-commit winner was not the top-1 candidate.
	// Lets consumers filter "kind=fallback" without parsing every
	// request payload.
	RecordFallback(observability.FallbackEvent)

	// RecordBriefQueueRetry is emitted at most once per request:
	// when the first ParallelProbe pass found no ready candidate,
	// after the 250 ms wait and the second pass complete. result
	// is "succeeded" if the second pass produced a winner,
	// "failed" otherwise.
	RecordBriefQueueRetry(result string)

	// RecordProbe is emitted once per real /healthz probe (i.e.
	// excluding the synthetic fast-path results ParallelProbe
	// returns for local / external candidates). outcome is the
	// router.ProbeOutcome.String() tag.
	RecordProbe(outcome string, latencyMs uint32)
}
