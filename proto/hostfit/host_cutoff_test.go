package hostfit

import (
	"math"
	"testing"
)

// The two rows the threshold was fixed against, straight off the stage-1
// repeat (docs/knowledges/20260805/1513-probe-predicts-decode-rate.md
// §12). Both legs measured qwen3.5-0.8b at 21066 tokens on the same
// machine; the only difference is whether the GPU was used.
var (
	referenceCPUOnly = HostProbe{PromptTokens: 21066, PrefillTokps: 671.17, DecodeTokps: 28.47}
	reference24GBGPU = HostProbe{PromptTokens: 21066, PrefillTokps: 20251.41, DecodeTokps: 291.77}
)

// The arithmetic still reproduces the measurement the threshold was
// derived from. If this drifts, 45 s stops meaning what the decision
// record says it means and has to be re-derived, not re-fitted
// (docs/decisions/20260805/1620-host-cutoff-is-a-measured-probe.md).
//
// The recorded turns are 66.6 s and 4.5 s at the measured 21066 tokens;
// normalising to the canonical 21000 scales them by 21000/21066.
func TestHostProbe_TurnSeconds_ReproducesTheCalibration(t *testing.T) {
	for _, tc := range []struct {
		name  string
		probe HostProbe
		want  float64
	}{
		{"reference host, CPU-only", referenceCPUOnly, 66.4},
		{"reference host, 24 GB card", reference24GBGPU, 4.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.probe.TurnSeconds()
			if math.Abs(got-tc.want) > 0.1 {
				t.Fatalf("turn = %.2f s, want %.1f s (±0.1) — the recorded measurement", got, tc.want)
			}
		})
	}
}

// The reference host's two legs land on opposite sides of the cutoff, and
// by a margin. This is the whole claim: a 15x gap that needs no
// extrapolation to read.
func TestHostProbe_MeetsRecommendedSpec_TheReferenceHost(t *testing.T) {
	if ok, decided := referenceCPUOnly.MeetsRecommendedSpec(); !decided || ok {
		t.Fatalf("CPU-only leg: ok=%v decided=%v, want ok=false decided=true — %.1f s "+
			"against a %.0f s budget", ok, decided, referenceCPUOnly.TurnSeconds(), HostCutoffTurnBudgetSeconds)
	}
	if ok, decided := reference24GBGPU.MeetsRecommendedSpec(); !decided || !ok {
		t.Fatalf("24 GB card leg: ok=%v decided=%v, want ok=true decided=true — %.1f s "+
			"against a %.0f s budget", ok, decided, reference24GBGPU.TurnSeconds(), HostCutoffTurnBudgetSeconds)
	}
}

// An unrun, failed or truncated measurement yields NO verdict, never a
// "below spec" one. Every one of these shapes is an engine or a probe
// misbehaving, and none of them is evidence about the host — treating
// them as evidence would turn local inference off on hosts that can serve
// perfectly well.
//
// The truncation rows are the case that motivated reading the depth back
// at all: an engine serving its 4096 default silently truncates a 21k
// prompt, and the fast prefill it then reports is a measurement of the
// truncation.
func TestHostProbe_Measured_RefusesToJudgeWhatItDidNotMeasure(t *testing.T) {
	for _, tc := range []struct {
		name  string
		probe HostProbe
	}{
		{"zero value (never run)", HostProbe{}},
		{"no prefill counter", HostProbe{PromptTokens: 21000, DecodeTokps: 300}},
		{"no decode counter", HostProbe{PromptTokens: 21000, PrefillTokps: 20000}},
		{"truncated to a 4096 window", HostProbe{PromptTokens: 4096, PrefillTokps: 20000, DecodeTokps: 300}},
		{"truncated just below tolerance", HostProbe{
			PromptTokens: int(HostCutoffProbeDepthTokens*hostCutoffDepthToleranceLow) - 1,
			PrefillTokps: 20000, DecodeTokps: 300}},
		{"implausibly long prompt", HostProbe{
			PromptTokens: int(HostCutoffProbeDepthTokens*hostCutoffDepthToleranceHigh) + 1,
			PrefillTokps: 20000, DecodeTokps: 300}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.probe.Measured() {
				t.Fatal("Measured() = true, want false")
			}
			if got := tc.probe.TurnSeconds(); got != 0 {
				t.Fatalf("TurnSeconds() = %v, want 0 for an unusable probe", got)
			}
			ok, decided := tc.probe.MeetsRecommendedSpec()
			if decided {
				t.Fatal("decided = true, want false — an unusable probe is not evidence")
			}
			if ok {
				t.Fatal("ok = true with decided = false; callers read decided first, but the pair must not lie")
			}
		})
	}
}

// A depth inside the tolerance IS judged. The band exists to catch
// truncation, not to make the probe fussy about ordinary tokenizer drift
// — a prompt builder that lands a few percent off must still produce a
// verdict.
func TestHostProbe_Measured_AcceptsOrdinaryTokenizerDrift(t *testing.T) {
	for _, tokens := range []int{
		int(HostCutoffProbeDepthTokens * hostCutoffDepthToleranceLow),
		HostCutoffProbeDepthTokens - 500,
		HostCutoffProbeDepthTokens,
		HostCutoffProbeDepthTokens + 500,
		int(HostCutoffProbeDepthTokens * hostCutoffDepthToleranceHigh),
	} {
		probe := HostProbe{PromptTokens: tokens, PrefillTokps: 671.17, DecodeTokps: 28.47}
		if !probe.Measured() {
			t.Fatalf("prompt_eval_count %d: Measured() = false, want true", tokens)
		}
	}
}

// The boundary is inclusive, and the verdict is a function of the turn
// alone. A host exactly at the budget passes — the budget is what we are
// willing to serve, not the first value we refuse.
func TestHostProbe_MeetsRecommendedSpec_AtTheBoundary(t *testing.T) {
	// Prefill fast enough to be negligible, decode chosen so the decode
	// half alone spends the whole budget.
	decodeFor := func(turn float64) float64 {
		prefill := 1e6
		prefillSeconds := float64(HostCutoffProbeDepthTokens) / prefill
		return (float64(HostCutoffProbeDepthTokens) / HostCutoffPromptCompletionRatio) / (turn - prefillSeconds)
	}
	for _, tc := range []struct {
		name string
		turn float64
		want bool
	}{
		{"a hair under the budget", HostCutoffTurnBudgetSeconds - 0.01, true},
		{"exactly the budget", HostCutoffTurnBudgetSeconds, true},
		{"a hair over the budget", HostCutoffTurnBudgetSeconds + 0.01, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := HostProbe{
				PromptTokens: HostCutoffProbeDepthTokens,
				PrefillTokps: 1e6,
				DecodeTokps:  decodeFor(tc.turn),
			}
			ok, decided := probe.MeetsRecommendedSpec()
			if !decided {
				t.Fatalf("decided = false for a %.2f s turn", probe.TurnSeconds())
			}
			if ok != tc.want {
				t.Fatalf("turn %.2f s: ok = %v, want %v (budget %.0f s)",
					probe.TurnSeconds(), ok, tc.want, HostCutoffTurnBudgetSeconds)
			}
		})
	}
}

// The turn is normalised to the canonical depth, so two hosts with
// identical rates get identical verdicts however many tokens their prompt
// happened to tokenize to. Without this, a prompt-builder change would
// move the cutoff without anyone deciding to move it.
func TestHostProbe_TurnSeconds_DoesNotDependOnTheMeasuredPromptLength(t *testing.T) {
	base := HostProbe{PromptTokens: HostCutoffProbeDepthTokens, PrefillTokps: 671.17, DecodeTokps: 28.47}
	for _, tokens := range []int{
		HostCutoffProbeDepthTokens - 3000,
		HostCutoffProbeDepthTokens,
		HostCutoffProbeDepthTokens + 3000,
	} {
		drifted := base
		drifted.PromptTokens = tokens
		if got, want := drifted.TurnSeconds(), base.TurnSeconds(); got != want {
			t.Fatalf("prompt_eval_count %d: turn = %v, want %v — the verdict must be a "+
				"property of the host, not of the prompt builder", tokens, got, want)
		}
	}
}

// The soundness property, stated as a property rather than as examples:
// the floor is never above the turn it bounds. Everything the screen is
// allowed to conclude rests on this one inequality, so it is checked over
// a grid rather than at the two calibration points — a future edit that
// added a term, or divided by the wrong depth, would still reproduce those
// two.
//
// Product contract (waired-agent#579, the Stage 3 contract table on the
// issue): the agent publishes a floor in HostSpeed.TurnSeconds, and a
// consumer that compares it against a threshold is relying on exactly
// this.
func TestTurnFloorSeconds_IsNeverAboveTheTurnItBounds(t *testing.T) {
	for _, prefill := range []float64{12, 83.53, 130, 671.17, 833, 20251.41} {
		for _, decode := range []float64{0.5, 3.44, 28.47, 291.77, 4000} {
			p := HostProbe{PromptTokens: HostCutoffProbeDepthTokens, PrefillTokps: prefill, DecodeTokps: decode}
			floor, turn := TurnFloorSeconds(p.PrefillTokps), p.TurnSeconds()
			if floor > turn {
				t.Fatalf("prefill %.2f / decode %.2f tok/s: floor %.3f s > turn %.3f s — "+
					"the bound is not a bound, and every screen verdict built on it is unsound",
					prefill, decode, floor, turn)
			}
		}
	}
}

// The reference host is the anchor the 45 s threshold was set from, and it
// is a machine already proven unusable. The screen must not conclude
// anything about a host BETTER than that anchor, so the figure it would
// read there — 833 tok/s, this repo's own measurement at 68 % of the
// canonical depth — has to leave the floor under the budget with room.
//
// Product contract (waired-agent#579): the shallow reading is what the
// screen sees, and using it is only safe because it over-states the rate.
// If this ever inverts, the screen starts cutting hosts the full
// measurement would have passed.
func TestTurnFloorSeconds_TheProvenBadAnchorIsNotCutByItsShallowRate(t *testing.T) {
	const shallowPrefillTokps = 833.0 // reference host at ~68 % depth
	floor := TurnFloorSeconds(shallowPrefillTokps)
	if floor > HostCutoffTurnBudgetSeconds {
		t.Fatalf("floor %.1f s from the reference host's shallow prefill rate exceeds the "+
			"%.0f s budget — the screen would cut the anchor the budget was derived from",
			floor, HostCutoffTurnBudgetSeconds)
	}
	// Pinned rather than merely bounded, the way the calibration test above
	// pins 66.4 s: the gap between "proven bad" and "the screen fires" is
	// the whole design, so it is a number to notice moving and not just an
	// inequality to satisfy. 25.2 s against a 45 s budget — and the agent
	// fires above the budget, not at it, so the real clearance is wider.
	if math.Abs(floor-25.2) > 0.1 {
		t.Fatalf("floor = %.2f s, want 25.2 s (±0.1) from 833 tok/s at the canonical depth", floor)
	}
}

// The host the screen exists for. These are the counters the GitHub
// macos-14 runner published on 2026-08-09 (run 31316731884): a 542 s turn,
// twelve times the budget, that cost 7 min 12 s of one sample to reach
// while the model download waited behind it (waired-agent#579).
//
// The rate below is the FULL-depth one, which is the conservative
// substitute: a shallow reading is faster still (833/671 = 1.24x on the
// reference host), so the real screen would see an even larger rate and a
// smaller floor. Asserting on the full-depth rate therefore under-states
// how far over the line this host sits.
func TestTurnFloorSeconds_AHostFarPastTheBudgetIsCaughtWithoutTheDecode(t *testing.T) {
	const macOSRunnerPrefillTokps = 83.53
	floor := TurnFloorSeconds(macOSRunnerPrefillTokps)
	if floor <= HostCutoffTurnBudgetSeconds {
		t.Fatalf("floor %.1f s does not exceed the %.0f s budget — the prefill term alone "+
			"has to settle a host measured at 542 s per turn", floor, HostCutoffTurnBudgetSeconds)
	}
	// The full measurement's own answer, for the record: the bound is a
	// bound, and a loose one. It has to be — it is the price of not paying
	// for the decode.
	full := HostProbe{PromptTokens: 21000, PrefillTokps: macOSRunnerPrefillTokps, DecodeTokps: 3.4376796295033905}
	if floor > full.TurnSeconds() {
		t.Fatalf("floor %.1f s > measured turn %.1f s", floor, full.TurnSeconds())
	}
}

// No rate, no claim — the same rule Measured() enforces for the full
// measurement. A zero here must never read as an instant turn, which
// would pass every host including the ones that reported nothing.
func TestTurnFloorSeconds_RefusesToClaimWithoutARate(t *testing.T) {
	for _, prefill := range []float64{0, -1} {
		if got := TurnFloorSeconds(prefill); got != 0 {
			t.Fatalf("TurnFloorSeconds(%v) = %v, want 0 — an unmeasured rate is no claim", prefill, got)
		}
	}
}
