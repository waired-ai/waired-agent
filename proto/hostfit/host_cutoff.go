// #496: the install-time host cutoff.
//
// Everything else in this package answers "can this machine serve this
// MODEL". This file answers a different question, and only that one: can
// this host run an inference engine usefully at all, or should local
// inference start off with the #465 opt-in? The answer is one measurement
// on one small model, and it is deliberately not an input to any ranking.
//
// Why it lives in proto rather than in the agent. The threshold and the
// arithmetic below are the whole verdict, and the control plane has to
// reach the same one from the figure the agent publishes
// (signer.InferenceState.HostSpeed). The control plane can import proto
// and nothing else, so a copy in internal/router would be a second 45 s
// that nobody would notice drifting — the identical failure this package
// was created to end (waired-ai/waired#942, see the package doc).
//
// Why a measurement and not a computation. #496 was written to feed a
// measured decode rate into the recommendation. Stage 1 measured that
// premise on real hardware and it did not hold: a ~1 GB probe's effective
// bandwidth spans 74-79 % across the dense ladder, so extrapolating from
// it to another model's rate produced +1 % and -8 % on two targets but
// +62 %, -50 % and +93 % on three others — the last of which would cut a
// working GPU host. Extrapolation was dropped with the rest of #496's
// scope item 2. What survived is the probe's OWN turn time, which needs
// no extrapolation, no roofline, and no ActiveBytesPerToken.
//
// Why it is a host question and not a model question. At a 21k context on
// the reference CPU-only host, one turn costs 227 s with the model the
// picker chooses today (tier 89) and 186 s with the smallest coding model
// in the catalog (tier 30); the same measurement on a 24 GB card is
// 17.8 s. Picking a smaller model does not rescue that host, so no model
// ranking can be the answer to it.
//
// Why it does not run through OllamaRecommend. The agent's
// internal/router/model_picker.go narrow helper keeps the previous
// candidate set whenever a filter rejects everything (`if len(pass) > 0`),
// so a speed verdict routed there is a no-op on exactly the hosts it
// exists to catch. The verdict reaches the same place
// router.SelectInstallModel's ok=false reaches — local inference off,
// with a working opt-in.
//
// Full derivation: docs/knowledges/20260805/1513-probe-predicts-decode-rate.md.
// Ratified in docs/decisions/20260805/1620-host-cutoff-is-a-measured-probe.md
// (owner, 2026-08-05); threshold fixed at 45 s the same day.
package hostfit

const (
	// HostCutoffProbeModelID is the model the cutoff is measured on. It
	// is chosen for its behaviour as a CLASSIFIER, not for its size:
	// scored against the measured floor over both legs of the stage-1
	// data it is 22/22, where granite4-350m is 9/11 on CPU (two
	// false-blocks a hair under the floor) and the population constant
	// hostfit.BandwidthSystemRAMGBs is 16/22.
	//
	// It doubles as the below-floor model a host that fails the cutoff
	// can still opt in to (quality_tier 12, the smallest offered entry),
	// so the ~1 GB the probe downloads is not thrown away.
	HostCutoffProbeModelID = "qwen3.5-0.8b"

	// HostCutoffProbeDepthTokens is the context depth the probe measures
	// at, and the depth the verdict is normalised to. Coding-agent
	// sessions run far deeper (#624 sizes for ~200k), but depth is the
	// input that changes the answer most and the probe has to be cheap:
	// 21k is deep enough that prefill dominates the way it does in a real
	// session (50-70 % of a CPU turn) while costing ~40 s on the slowest
	// host we expect to measure and ~4 s on a card.
	//
	// A shallow 200-token boot benchmark cannot substitute: it
	// overstates real-session decode by 24-60 % on CPU and 10-16 % on
	// GPU, and on a card a 1 GB model at that length is dominated by
	// fixed request overhead.
	HostCutoffProbeDepthTokens = 21000

	// HostCutoffCompletionSampleTokens is how many tokens the probe
	// actually decodes to measure the decode rate. The turn time is
	// computed from the rate, so there is no reason to decode a whole
	// turn's worth: 200 matches the boot benchmark's sample length and
	// the stage-1 harness that fixed the threshold.
	HostCutoffCompletionSampleTokens = 200

	// HostCutoffPromptCompletionRatio is the prompt:completion ratio of a
	// coding-agent turn, taken from the same 21:1 the lighter/upgrade
	// picker sizes with (model_picker.go). One turn is therefore a
	// HostCutoffProbeDepthTokens prefill plus a
	// HostCutoffProbeDepthTokens/21 decode.
	HostCutoffPromptCompletionRatio = 21

	// HostCutoffTurnBudgetSeconds is the cutoff: a host whose probe turn
	// takes longer than this starts with local inference off (owner
	// decision, 2026-08-05).
	//
	// Calibration point: the reference host (Ryzen 9 9950X 16C/32T, dual
	// channel DDR5, 121 GB) measured CPU-only takes 66.6 s, median of 5
	// cold runs (65.3 / 65.9 / 66.6 / 80.4 / 67.5). That machine is near
	// the ceiling for desktop CPU-only AND is demonstrably unusable at
	// 227 s per real turn, so it is a proven-bad anchor rather than a
	// guess. 45 s is two thirds of it; the 24 GB card at the other end
	// measures 4.5 s, a factor of 10 clear.
	//
	// The margin absorbs contention, which is the only variation the
	// repeat found: idle runs spread ±2 %, while the one run that shared
	// the machine with another job landed at 80.4 s (+21 %).
	//
	// Strict rather than generous, because the two errors do not cost the
	// same. Cutting a host that could have served costs one
	// `waired inference on` (#465's opt-in — this decides a default and
	// forbids nothing, waired-ai/waired#1056). Passing a host that cannot
	// serve costs a 20-45 GB download followed by minutes per turn.
	//
	// UNMEASURED: the 45-25 s band. Multi-channel server CPUs have 4-8x
	// the bandwidth of this anchor and should pass — correct, but not yet
	// confirmed — and where weak discrete GPUs and iGPUs land is unknown.
	// Revisit when there is data.
	HostCutoffTurnBudgetSeconds = 45.0

	// hostCutoffDepthToleranceLow / High bound the prompt length the
	// probe will accept as a valid measurement of
	// HostCutoffProbeDepthTokens. The engine silently truncates a prompt
	// that overflows the serve window, and a truncated prefill measures
	// its own truncation rather than the host — a 4096-window engine
	// would report a fast turn for a host that has no hope. Read the
	// depth back and refuse to judge on one that is not the depth asked
	// for.
	hostCutoffDepthToleranceLow  = 0.7
	hostCutoffDepthToleranceHigh = 1.5
)

// HostProbe is one measurement of HostCutoffProbeModelID at
// HostCutoffProbeDepthTokens: the depth the engine actually prefilled,
// and the two rates it reported for doing so.
//
// The rates are the engine's own counters (prompt_eval_* / eval_* from
// ollama's /api/generate), never wall clock: wall clock on a 1 GB model
// is dominated by model load and request overhead, which is how the
// pre-#764 benchmark under-measured fast hosts by ~35 %.
type HostProbe struct {
	// PromptTokens is prompt_eval_count — the depth the engine reports
	// having prefilled, which is the only trustworthy statement about
	// how deep the measurement actually went.
	PromptTokens int

	// PrefillTokps is prompt_eval_count / prompt_eval_duration.
	PrefillTokps float64

	// DecodeTokps is eval_count / eval_duration.
	DecodeTokps float64
}

// Measured reports whether this probe is a usable measurement of the
// canonical depth. A probe that is not Measured says nothing about the
// host and must leave the install path exactly as it found it — an
// unrun, failed or truncated measurement is not evidence that a host is
// slow.
func (p HostProbe) Measured() bool {
	if p.PrefillTokps <= 0 || p.DecodeTokps <= 0 || p.PromptTokens <= 0 {
		return false
	}
	target := float64(HostCutoffProbeDepthTokens)
	got := float64(p.PromptTokens)
	return got >= target*hostCutoffDepthToleranceLow && got <= target*hostCutoffDepthToleranceHigh
}

// TurnSeconds is what the cutoff judges: one coding-agent turn at the
// canonical depth, prefill plus decode, at the rates this host measured.
//
//	turn = P/prefill + (P/ratio)/decode
//
// P is HostCutoffProbeDepthTokens rather than the measured PromptTokens,
// so the verdict is a property of the host and not of how many tokens
// this build's prompt builder happened to emit. The 66.6 s calibration
// point was taken at 21066 tokens and maps to 66.4 s under this
// normalisation.
//
// Zero when the probe is not Measured.
func (p HostProbe) TurnSeconds() float64 {
	if !p.Measured() {
		return 0
	}
	depth := float64(HostCutoffProbeDepthTokens)
	return depth/p.PrefillTokps + (depth/HostCutoffPromptCompletionRatio)/p.DecodeTokps
}

// TurnFloorSeconds is a LOWER BOUND on the TurnSeconds of a host whose
// prefill rate is prefillTokps — the same turn with the decode term
// dropped:
//
//	floor = P/prefill        against        turn = P/prefill + (P/ratio)/decode
//
// It exists so a host can be found below the cutoff WITHOUT paying for a
// full-depth measurement. A full measurement is a ~21k-token prefill plus
// a 200-token decode, which on the hosts the cutoff exists to catch is
// minutes standing in front of the model download (waired-agent#579): the
// GitHub macos-14 runner takes 7 min 12 s for one sample. A prefill rate
// comes far cheaper — the agent's own calibration request already measures
// one at ~2.8k tokens and discards the timing.
//
// One-sided, and used in one direction only:
//
//	floor >  budget  =>  turn > budget    sound; this is a verdict
//	floor <= budget  =>  nothing at all   falls through to the measurement
//
// Two independent facts make that direction sound, and both point the
// same way:
//
//  1. The decode term is strictly positive on any host that decodes at
//     all, so dropping it can only make the number smaller.
//  2. Prefill rate is monotone non-increasing in depth, so a rate measured
//     SHALLOWER than HostCutoffProbeDepthTokens over-estimates the rate at
//     the canonical depth, and dividing by an over-estimate under-estimates
//     the time. Measured on this repo's reference host: 833 tok/s at 68 %
//     of the depth against 671 tok/s at the full depth — the same pair of
//     figures that made the agent measure its prompt's token cost rather
//     than assume it (hostCutoffCalibrationLines, host_cutoff_probe.go).
//
// A caller that publishes this figure has to say so. It rides
// signer.HostSpeed.TurnSeconds with Method =
// signer.BenchmarkMethodOllamaPrefillFloor, and a consumer that reads it
// as an exact turn time believes the host is FASTER than it is. That is
// tolerable only because the agent publishes this shape solely when the
// bound already exceeds HostCutoffTurnBudgetSeconds, so every threshold
// comparison at or below the budget still lands where the full
// measurement would have put it.
//
// Zero when the rate cannot be used, matching TurnSeconds: zero is "no
// claim", never "instant".
func TurnFloorSeconds(prefillTokps float64) float64 {
	if prefillTokps <= 0 {
		return 0
	}
	return float64(HostCutoffProbeDepthTokens) / prefillTokps
}

// MeetsRecommendedSpec is the verdict. ok is whether this host clears the
// cutoff; decided is whether the probe was able to judge at all.
//
// Callers must treat decided=false as "carry on unchanged", never as a
// failure: no engine, an engine with no timing counters, a truncated
// prefill and a machine too busy to answer all land there, and none of
// them is evidence about the host.
func (p HostProbe) MeetsRecommendedSpec() (ok, decided bool) {
	if !p.Measured() {
		return false, false
	}
	return p.TurnSeconds() <= HostCutoffTurnBudgetSeconds, true
}
