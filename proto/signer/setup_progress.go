package signer

// SetupProgress captures the agent-side progress of the NAVI-driven
// onboarding flow (waired#835 §7). It travels two places:
//
//   - agent → CP push body (POST /v1/devices/self/setup-progress)
//   - Spanner Device.setup_progress JSON column
//
// Like ConnectivityState it is NOT distributed to peers in the network
// map: it is admin-facing telemetry only (the NAVI setup wizard polls
// it back via the device detail endpoint), so it never rides on a
// signed NetworkMap and has no canonical-form constraints beyond the
// RFC3339Nano timestamp kept for consistency. The additive-only proto
// rules still apply.
type SetupProgress struct {
	// Steps is the typed step list for the current setup run, in
	// execution order. The CP validator bounds the array length and
	// per-field sizes; the NAVI wizard maps IDs and error codes to
	// copy and recovery affordances.
	Steps []SetupStep `json:"steps,omitempty"`

	// Benchmark reports the most recent benchmark completion for the
	// declarative generation counter (InferenceState.
	// DesiredBenchmarkGen): Gen echoes the generation the measurement
	// belongs to so the CP/NAVI can tell a stale result from the one
	// they asked for.
	Benchmark *SetupBenchmark `json:"benchmark,omitempty"`

	// LastCheck is the agent's wall-clock time at the snapshot,
	// formatted as RFC3339Nano. The UI ignores states older than its
	// staleness threshold so a crashed agent ages out of the display.
	LastCheck string `json:"last_check"`

	// Driver names the surface currently driving setup — one of the
	// SetupDriver* constants, empty when the agent does not report it
	// (an onboarding-v1 agent, or a run nobody has claimed yet).
	//
	// Without it neither surface can observe the other: the CLI's "setup
	// is active" and the reconciler's "active" are both local booleans
	// that never reach the wire, so a deliberate terminal takeover is
	// indistinguishable from the executor dying — both surface as
	// executor_gone (waired#932 G2). Publishing it lets the wizard show
	// a real handoff and lets the terminal switch to its "installation
	// in progress, keep this window open" display.
	Driver string `json:"driver,omitempty"`
}

// SetupStep is one step of the onboarding run.
type SetupStep struct {
	// ID identifies the step. The settled set is five (waired#835 §7,
	// revised by waired#934): "engine_download", "engine_install",
	// "model_pull", "benchmark", "integration". Completion is NOT a
	// step — it is derived control-plane side, so there is no
	// "complete" id.
	//
	// The field stays free-form for forward compatibility: the CP
	// clamps its length and treats it as opaque, and NAVI appends ids
	// it does not know in the order they arrived rather than dropping
	// them, so a newer agent can add a step without an older wizard
	// hiding it. That is also why adding ids costs nothing on the wire
	// — what constrains the set is the spec and the label map, not this
	// type.
	ID string `json:"id"`

	// Status is one of the SetupStatus* constants.
	Status string `json:"status"`

	// CompletedBytes / TotalBytes carry download progress for byte-
	// denominated steps (engine_download, model_pull). 0/omitted when
	// not applicable or unknown.
	CompletedBytes int64 `json:"completed_bytes,omitempty"`
	TotalBytes     int64 `json:"total_bytes,omitempty"`

	// RateBps is the current transfer rate in bytes/s for a byte-
	// denominated step. 0/omitted means unknown.
	//
	// Carried rather than differenced by the reader: the reporter
	// pushes only when its snapshot changes and the CP admits one push
	// per 2 s, so a consumer subtracting two polled samples measures
	// the push/poll spacing, not the transfer. The terminal already
	// renders this number from the same source, and publishing it is
	// what keeps the two surfaces from quoting different speeds for one
	// download.
	//
	// A stall is derived from CompletedBytes not advancing, NOT from a
	// zero here: omitempty collapses 0 into absent, so "stalled" and
	// "unknown" cannot be distinguished in this field.
	RateBps int64 `json:"rate_bps,omitempty"`

	// ErrorCode is one of the SetupError* constants when Status is
	// failed, empty otherwise. An enum rather than free text so NAVI
	// can map it to copy and a recovery affordance without parsing.
	ErrorCode string `json:"error_code,omitempty"`

	// ErrorDetail is the free-form diagnostic string accompanying
	// ErrorCode, for the collapsed "details" view only. Bounded by the
	// CP validator; never parsed.
	ErrorDetail string `json:"error_detail,omitempty"`
}

// SetupBenchmark is the benchmark result attached to a SetupProgress
// push. See SetupProgress.Benchmark.
type SetupBenchmark struct {
	// Gen is the DesiredBenchmarkGen generation this measurement
	// belongs to (0 for runs not requested via the declarative
	// counter, e.g. the CLI-triggered installer benchmark).
	Gen int `json:"gen"`

	// MeasuredTokps is the FINAL measured decode throughput in tokens/s,
	// written when the run completes. It deliberately stays empty while
	// a run is in flight: consumers already render it as the device's
	// speed, so putting a provisional figure here would silently change
	// what a published field means for every build already shipped. The
	// in-flight value lives in MedianTokps below.
	MeasuredTokps float64 `json:"measured_tokps,omitempty"`

	// Trial / Trials report per-sample progress so the wizard can render
	// a real measurement instead of an unbounded spinner: Trial is the
	// 1-based index of the sample in flight or just completed, Trials
	// the number planned. Both 0/omitted for an agent that does not
	// report trials.
	Trial  int `json:"trial,omitempty"`
	Trials int `json:"trials,omitempty"`

	// SampleTokps is the most recent single sample's decode throughput,
	// MedianTokps the running median across the samples completed so
	// far, and SpreadPct their spread as a percentage. MedianTokps is
	// what MeasuredTokps becomes once the run finishes.
	SampleTokps float64 `json:"sample_tokps,omitempty"`
	MedianTokps float64 `json:"median_tokps,omitempty"`
	SpreadPct   float64 `json:"spread_pct,omitempty"`

	// Method is how the throughput was obtained — one of the
	// BenchmarkMethod* constants, in the fallback order the agent tries
	// them. It travels because it changes what the number may be used
	// for downstream: a wall_clock sample is contaminated by request
	// overhead and must not drive model re-classification, which is a
	// filter the consumer can only apply if the method is on the wire.
	Method string `json:"method,omitempty"`
}

// Setup step status values — accepted values for SetupStep.Status.
const (
	SetupStatusPending = "pending"
	SetupStatusRunning = "running"
	SetupStatusDone    = "done"
	SetupStatusFailed  = "failed"
	SetupStatusSkipped = "skipped"
)

// IsValidSetupStatus reports whether s is one of the accepted step
// status values. Used by the CP API validator and by the agent push
// client's pre-flight check.
func IsValidSetupStatus(s string) bool {
	switch s {
	case SetupStatusPending, SetupStatusRunning, SetupStatusDone,
		SetupStatusFailed, SetupStatusSkipped:
		return true
	}
	return false
}

// Setup drivers — accepted values for SetupProgress.Driver. Which
// surface is driving is a two-valued fact: the browser wizard, or the
// terminal that ran init. "Nobody" is the empty string, which is also
// what an agent that predates the field reports.
const (
	SetupDriverBrowser  = "browser"
	SetupDriverTerminal = "terminal"
)

// IsValidSetupDriver reports whether d is an accepted driver value
// (empty is valid: "not reported"). Used by the CP API validator and
// by the agent push client's pre-flight check.
func IsValidSetupDriver(d string) bool {
	switch d {
	case "", SetupDriverBrowser, SetupDriverTerminal:
		return true
	}
	return false
}

// Benchmark methods — accepted values for SetupBenchmark.Method, in the
// agent's fallback order. The distinction is not cosmetic:
// BenchmarkMethodWallClock times the whole request, so it carries queue
// and prompt-processing overhead and understates a fast host; results
// measured that way must be excluded from anything that re-classifies
// models by speed.
const (
	BenchmarkMethodOllamaEval  = "ollama_eval"
	BenchmarkMethodOpenAISlope = "openai_slope"
	BenchmarkMethodWallClock   = "wall_clock"
)

// IsValidBenchmarkMethod reports whether m is an accepted method value
// (empty is valid: "not reported").
func IsValidBenchmarkMethod(m string) bool {
	switch m {
	case "", BenchmarkMethodOllamaEval, BenchmarkMethodOpenAISlope,
		BenchmarkMethodWallClock:
		return true
	}
	return false
}

// Setup error codes — accepted values for SetupStep.ErrorCode. The
// enum is the wire contract NAVI maps to user-facing copy; additions
// are fine (unknown codes render as a generic failure), removals and
// meaning changes are wire breaks.
const (
	SetupErrorEngineNotReady   = "engine_not_ready"
	SetupErrorDiskFull         = "disk_full"
	SetupErrorModelNotFound    = "model_not_found"
	SetupErrorNetworkError     = "network_error"
	SetupErrorPermissionDenied = "permission_denied"
	SetupErrorExecutorGone     = "executor_gone"
	SetupErrorTimeout          = "timeout"
	SetupErrorInternal         = "internal"
)

// IsValidSetupErrorCode reports whether c is one of the accepted
// error code values (empty is valid: "no error").
func IsValidSetupErrorCode(c string) bool {
	switch c {
	case "", SetupErrorEngineNotReady, SetupErrorDiskFull,
		SetupErrorModelNotFound, SetupErrorNetworkError,
		SetupErrorPermissionDenied, SetupErrorExecutorGone,
		SetupErrorTimeout, SetupErrorInternal:
		return true
	}
	return false
}
