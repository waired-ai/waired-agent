package management

// SpeedMeasurement is the served model's speed in the shape every surface
// judges it: one request of hostfit.SpeedMeasurementDepthTokens costs
// TurnSeconds, held against BudgetSeconds (hostfit.ModelTurnBudgetSeconds).
// waired-ai/waired-agent#1341; decisions 1-5 of
// docs/decisions/20260913/2245-speed-is-one-request-at-32768-tokens.md.
//
// It is embedded, so its keys sit beside the older rate fields on the same
// JSON objects and a client that has not been taught them reads what it read
// before. Every field is a figure, never a sentence: the words live in one
// place (internal/notice.RequestSeconds / TargetClause) so every surface
// rounds the same measurement the same way.
type SpeedMeasurement struct {
	// TurnSeconds is the finished figure. Zero while running, and on a run
	// that ended without one.
	TurnSeconds float64 `json:"turn_seconds,omitempty"`
	// TurnFloorSeconds is a lower bound on TurnSeconds for a measurement
	// that has been running longer than the line: the prompt alone has
	// already taken this long. Set only while TurnSeconds is not.
	TurnFloorSeconds float64 `json:"turn_floor_seconds,omitempty"`
	// BudgetSeconds is the line the figure was judged against.
	BudgetSeconds float64 `json:"budget_seconds,omitempty"`
	// OverBudget is the verdict: while running, that the request has been
	// going longer than the line; once finished, that TurnSeconds is over it.
	OverBudget bool `json:"over_budget,omitempty"`
	// PrefillTokps and DecodeTokps are the engine's own rates at depth.
	PrefillTokps float64 `json:"prefill_tokps,omitempty"`
	DecodeTokps  float64 `json:"decode_tokps,omitempty"`
	// DepthTokens is the prompt depth the engine reported prefilling.
	DepthTokens int `json:"depth_tokens,omitempty"`
	// ElapsedSeconds is how long the measurement request has been running,
	// counted from when it was sent. Running only.
	ElapsedSeconds float64 `json:"elapsed_seconds,omitempty"`
	// Cached reports a stored measurement of the same weights on the same
	// engine and GPU, published without measuring again (decision 7).
	Cached bool `json:"cached,omitempty"`
}

// ModelSpeedStatus is InferenceStatus.ModelSpeed: which model the
// measurement describes, whether it is still running, and the figures.
type ModelSpeedStatus struct {
	ModelID   string `json:"model_id,omitempty"`
	VariantID string `json:"variant_id,omitempty"`
	// Running is true while the measurement request is in flight; the
	// figures are then ElapsedSeconds and, past the line, the bound.
	Running bool `json:"running,omitempty"`
	// MeasuredAt is when the finished figure was taken, RFC3339.
	MeasuredAt string `json:"measured_at,omitempty"`
	SpeedMeasurement
}

// Judged reports whether the measurement says anything about the line: a
// finished figure, or a running measurement already over it.
func (m SpeedMeasurement) Judged() bool {
	return m.TurnSeconds > 0 || m.TurnFloorSeconds > 0
}

// Benchmark modes for POST /waired/v1/inference/benchmark?mode=.
const (
	// BenchmarkModeRerun measures again and overwrites the stored figure for
	// this model, engine and GPU. The default: `waired runtimes benchmark`
	// is a person asking for a new number.
	BenchmarkModeRerun = "rerun"
	// BenchmarkModeEnsure answers from the stored figure when there is one,
	// or joins the measurement already running, and measures only when
	// neither exists. `waired init` asks this way, so a setup does not
	// measure twice what the daemon has just measured on its own.
	BenchmarkModeEnsure = "ensure"
)
