package router

import (
	"math"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
)

// The two rows the threshold was fixed against, straight off the stage-1
// repeat (docs/knowledges/20260805/1513-probe-predicts-decode-rate.md
// §12). Both legs measured qwen3.5-0.8b at 21066 tokens on the same
// machine; the only difference is whether the GPU was used.
var (
	referenceCPUOnly = HostProbe{PromptTokens: 21066, PrefillTokps: 671.17, DecodeTokps: 28.47}
	reference24GBGPU = HostProbe{PromptTokens: 21066, PrefillTokps: 20251.41, DecodeTokps: 291.77}
)

// PRODUCT CONTRACT: the arithmetic still reproduces the measurement the
// threshold was derived from. If this drifts, 45 s stops meaning what the
// decision record says it means and has to be re-derived, not re-fitted.
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

// PRODUCT CONTRACT: the reference host's two legs land on opposite sides
// of the cutoff, and by a margin. This is the whole claim: a 15x gap that
// needs no extrapolation to read.
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

// PRODUCT CONTRACT: an unrun, failed or truncated measurement yields NO
// verdict, never a "below spec" one. Every one of these shapes is an
// engine or a probe misbehaving, and none of them is evidence about the
// host — treating them as evidence would turn local inference off on
// hosts that can serve perfectly well.
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

// PRODUCT CONTRACT: a depth inside the tolerance IS judged. The band
// exists to catch truncation, not to make the probe fussy about ordinary
// tokenizer drift — a prompt builder that lands a few percent off must
// still produce a verdict.
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

// PRODUCT CONTRACT: the boundary is inclusive, and the verdict is a
// function of the turn alone. A host exactly at the budget passes — the
// budget is what we are willing to serve, not the first value we refuse.
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

// PRODUCT CONTRACT: the turn is normalised to the canonical depth, so two
// hosts with identical rates get identical verdicts however many tokens
// their prompt happened to tokenize to. Without this, a prompt-builder
// change would move the cutoff without anyone deciding to move it.
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

// PRODUCT CONTRACT (decision record 20260805/1620, decision 6): a speed
// verdict may NOT be routed through the recommendation gate. It has to
// reach SelectInstallModel's ok=false, which is why HostProbe returns a
// verdict for a CALLER to act on rather than a candidate filter.
//
// The reason is structural, not stylistic. model_picker.go's narrow helper
// keeps the previous candidate set whenever a filter rejects everything
// (`if len(pass) > 0`), and SelectInstallModel stands the whole
// recommendation pass down when nothing clears the tier floor. Between
// them, NOTHING placed in that pass can turn an ok=true host into an
// ok=false one — so a cutoff placed there would be silently inert on
// exactly the hosts it exists to catch.
//
// This asserts the property directly: standing the gate down changes no
// host's verdict, over the real catalog.
func TestRecommendGateCanNeverWithholdEveryModel(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	for _, ramGB := range []int{2, 4, 8, 16, 32, 64, 128} {
		hw := hardware.Profile{OS: "linux", Arch: "x86_64", RAMTotalGB: ramGB}
		in := PickInput{Catalog: manifests, Hardware: hw, Engine: catalog.RuntimeOllama}

		gated, gatedOK, err := SelectInstallModel(in, InstallQualityFloorTier)
		if err != nil {
			t.Fatalf("%d GB: SelectInstallModel: %v", ramGB, err)
		}
		ungated := in
		ungated.NoRecommendGate = true
		stoodDown, stoodDownOK, err := SelectInstallModel(ungated, InstallQualityFloorTier)
		if err != nil {
			t.Fatalf("%d GB: SelectInstallModel (gate stood down): %v", ramGB, err)
		}
		if gatedOK != stoodDownOK || len(gated) != len(stoodDown) {
			t.Fatalf("%d GB: the recommendation gate changed the verdict (ok %v→%v, %d→%d candidates). "+
				"If that is now possible the host cutoff could live there; until then it must not.",
				ramGB, stoodDownOK, gatedOK, len(stoodDown), len(gated))
		}
	}
}
