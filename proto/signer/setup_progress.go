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
}

// SetupStep is one step of the onboarding run.
type SetupStep struct {
	// ID identifies the step, e.g. "engine_install", "model_pull",
	// "benchmark", "engine_start". Free-form for forward compatibility
	// (the CP clamps length; NAVI ignores IDs it does not know).
	ID string `json:"id"`

	// Status is one of the SetupStatus* constants.
	Status string `json:"status"`

	// CompletedBytes / TotalBytes carry download progress for byte-
	// denominated steps (model_pull). 0/omitted when not applicable or
	// unknown.
	CompletedBytes int64 `json:"completed_bytes,omitempty"`
	TotalBytes     int64 `json:"total_bytes,omitempty"`

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

	// MeasuredTokps is the measured decode throughput in tokens/s.
	MeasuredTokps float64 `json:"measured_tokps,omitempty"`
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
