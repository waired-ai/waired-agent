package hostfit

// The served model's speed measurement (waired-ai/waired-agent#1341).
//
// One request on the model this host serves: a SpeedMeasurementDepthTokens
// prompt plus a SpeedMeasurementCompletionTokens decode, which yields the
// engine's own prefill and decode rates. The verdict is not either rate but
// what they cost one coding-agent request together, TurnSecondsAt at
// SpeedMeasurementDepthTokens, held against ModelTurnBudgetSeconds.
//
// Owner ruling 2026-09-13: docs/decisions/20260913/2245-speed-is-one-request-at-32768-tokens.md
// (decisions 1-4). The host cutoff above is the same formula at its own depth
// on its own stand-in model; this is the same formula on the served model.
const (
	// SpeedMeasurementDepthTokens is the prompt depth of the measurement,
	// and the depth every published TurnSeconds is normalised to. One
	// constant for every host (decision 1; the fixed depth of
	// docs/decisions/20260829/1740-speed-is-measured-at-fixed-depths.md).
	SpeedMeasurementDepthTokens = 32768

	// SpeedMeasurementCompletionTokens is how many tokens the measurement
	// decodes to read the decode rate at depth. The request's cost is
	// computed from the rate, so there is no reason to decode a whole
	// request's worth.
	SpeedMeasurementCompletionTokens = 128

	// SpeedMeasurementMinCompletionTokens is the fewest decoded tokens a
	// sample may carry and still count: a model that stops early leaves a
	// decode rate dominated by the first token's scheduling.
	SpeedMeasurementMinCompletionTokens = 64

	// TurnCompletionTokens is the decode half of one request at
	// SpeedMeasurementDepthTokens under the coding-agent ratio:
	// 32768 / HostCutoffPromptCompletionRatio, in whole tokens.
	TurnCompletionTokens = SpeedMeasurementDepthTokens / HostCutoffPromptCompletionRatio

	// ModelTurnBudgetSeconds is the switch line: a served model whose
	// request takes longer than this is recommended to be switched for a
	// lighter one (decision 3), and a measurement still running this long
	// after its request was sent publishes that it is over the line
	// (decision 4). It sits beside HostCutoffTurnBudgetSeconds, which is the
	// install-time cutoff and a different question.
	//
	// Calibration: an Apple M5 Pro 48 GB laptop on ollama 0.33.3 measured
	// 228 s for qwen3.8-27b mtp-q4 (prefill 252.9 tok/s, decode at depth
	// 15.8 tok/s) and 70 s for qwen3.6-35b-a3b mtp-q4 (901.3 / 45.7). The
	// owner judged the first "the limit of usable" and set the line a
	// little stricter.
	ModelTurnBudgetSeconds = 190.0

	// SpeedMeasurementSecondSampleBand is the fraction of
	// ModelTurnBudgetSeconds either side of the line inside which one sample
	// is not enough to decide which side a host is on: a first sample whose
	// TurnSeconds lands within it earns a second (decision 1). Outside it
	// the samples at depth agree to under 1 %, far closer than the band.
	SpeedMeasurementSecondSampleBand = 0.10
)

// TurnSecondsAt is one coding-agent request at depthTokens, prefill plus
// decode, at the given rates:
//
//	seconds = depth/prefill + (depth/ratio)/decode
//
// with the decode half in whole tokens (depth / HostCutoffPromptCompletionRatio),
// so 21000 decodes 1000 exactly as HostProbe.TurnSeconds always has and
// 32768 decodes TurnCompletionTokens. This is the one place the formula
// lives; HostProbe.TurnSeconds calls it.
//
// Zero when any input cannot be used — zero is "no claim", never
// "instant", matching TurnSeconds and TurnFloorSeconds.
func TurnSecondsAt(depthTokens int, prefillTokps, decodeTokps float64) float64 {
	if depthTokens <= 0 || prefillTokps <= 0 || decodeTokps <= 0 {
		return 0
	}
	completion := depthTokens / HostCutoffPromptCompletionRatio
	return float64(depthTokens)/prefillTokps + float64(completion)/decodeTokps
}
