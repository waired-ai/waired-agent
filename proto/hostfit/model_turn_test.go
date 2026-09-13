package hostfit

import (
	"math"
	"testing"
)

// The decode half of a 32,768-token request is the coding-agent ratio
// applied in whole tokens — the constant the ruling names (1,560) and the
// ratio the host cutoff already uses must stay one fact.
func TestTurnCompletionTokens_IsTheRatioAtTheMeasurementDepth(t *testing.T) {
	if TurnCompletionTokens != 1560 {
		t.Fatalf("TurnCompletionTokens = %d, want 1560", TurnCompletionTokens)
	}
	if want := SpeedMeasurementDepthTokens / HostCutoffPromptCompletionRatio; TurnCompletionTokens != want {
		t.Errorf("TurnCompletionTokens = %d, want depth/ratio = %d", TurnCompletionTokens, want)
	}
}

// PRODUCT CONTRACT (docs/decisions/20260913/2245 calibration): the two
// figures the switch line was set against reproduce from their rates.
func TestTurnSecondsAt_ReproducesTheCalibrationPoints(t *testing.T) {
	cases := []struct {
		name            string
		prefill, decode float64
		want            float64
	}{
		{"qwen3.8-27b on the 48 GB laptop", 252.9, 15.8, 228},
		{"qwen3.6-35b-a3b on the 48 GB laptop", 901.3, 45.7, 70},
	}
	for _, c := range cases {
		got := TurnSecondsAt(SpeedMeasurementDepthTokens, c.prefill, c.decode)
		if math.Round(got) != c.want {
			t.Errorf("%s: TurnSecondsAt = %.1f, want %.0f", c.name, got, c.want)
		}
	}
	slow := TurnSecondsAt(SpeedMeasurementDepthTokens, 252.9, 15.8)
	fast := TurnSecondsAt(SpeedMeasurementDepthTokens, 901.3, 45.7)
	if !(slow > ModelTurnBudgetSeconds) || !(fast <= ModelTurnBudgetSeconds) {
		t.Errorf("line %.0f s: 27B %.0f s should be over it and 35B-A3B %.0f s under it",
			ModelTurnBudgetSeconds, slow, fast)
	}
}

// The host cutoff's figure is the same formula at its own depth; moving the
// formula into TurnSecondsAt must not move a single published HostSpeed.
func TestTurnSecondsAt_HostProbeIsTheSameFormula(t *testing.T) {
	p := HostProbe{PromptTokens: HostCutoffProbeDepthTokens, PrefillTokps: 671, DecodeTokps: 23.5}
	depth := float64(HostCutoffProbeDepthTokens)
	want := depth/p.PrefillTokps + (depth/HostCutoffPromptCompletionRatio)/p.DecodeTokps
	if got := p.TurnSeconds(); math.Abs(got-want) > 1e-9 {
		t.Errorf("HostProbe.TurnSeconds = %v, want %v", got, want)
	}
}

func TestTurnSecondsAt_NoClaimWithoutUsableInputs(t *testing.T) {
	for _, c := range []struct {
		depth           int
		prefill, decode float64
	}{
		{0, 250, 15}, {32768, 0, 15}, {32768, 250, 0}, {-1, 250, 15}, {32768, -1, 15},
	} {
		if got := TurnSecondsAt(c.depth, c.prefill, c.decode); got != 0 {
			t.Errorf("TurnSecondsAt(%d, %v, %v) = %v, want 0", c.depth, c.prefill, c.decode, got)
		}
	}
}
