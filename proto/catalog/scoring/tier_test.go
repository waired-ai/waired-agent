package scoring

import (
	"math"
	"testing"
)

// Record of today's behaviour: the composite's two terms and their relative
// weight. The benchmark term was deleted on 2026-08-05
// (docs/decisions/20260805/1427-quality-tier-is-a-curated-ladder.md), so size
// and footprint are the whole formula and a placement they get wrong is
// corrected by a tier_override, not by a third input.
func TestCompositeScore_Monotonic(t *testing.T) {
	base := CompositeScore(30_000_000_000, 24000)

	// More parameters → higher score (capability).
	if CompositeScore(80_000_000_000, 24000) <= base {
		t.Error("higher params should raise the composite")
	}
	// Larger memory footprint → lower score (penalty).
	if CompositeScore(30_000_000_000, 48000) >= base {
		t.Error("larger footprint should lower the composite")
	}
}

// The footprint term is a tie-breaker, not a rival to size: it must separate
// two variants of the same model, and must not overturn a real size gap.
// Record of today's behaviour (the coefficients are 10.0 and 5.0).
func TestCompositeScore_FootprintIsTheWeakerTerm(t *testing.T) {
	// Same weights, one needs twice the memory: the leaner one wins.
	lean := CompositeScore(27_000_000_000, 24000)
	fat := CompositeScore(27_000_000_000, 48000)
	if lean <= fat {
		t.Errorf("same params, less memory should rank higher: %.2f vs %.2f", lean, fat)
	}
	if d := lean - fat; math.Abs(d-1.505) > 0.01 {
		t.Errorf("doubling the footprint should cost ~1.51, got %.3f", d)
	}

	// A 3x size gap is worth ~4.77, which four footprint doublings (~6.02)
	// could overturn — so the guard is that the realistic case does not.
	// 9B in 12 GB vs 27B in 24 GB: the bigger model wins despite the penalty.
	small := CompositeScore(9_000_000_000, 12288)
	big := CompositeScore(27_000_000_000, 24576)
	if big <= small {
		t.Errorf("27B/24GB (%.2f) should outrank 9B/12GB (%.2f)", big, small)
	}
}

// A MoE outranks a dense model of similar TOTAL size here regardless of how the
// two actually compare, because the composite reads total parameters by design
// (see the coefficient block's note). Pinned because it is the single most
// likely place for the ordering to be wrong, and the thing tier_override is
// there to correct — qwen3.6-35b-a3b sits above qwen3.6-27b in the shipped
// ladder while the one accepted source covering both puts it 6.5 points below.
// Record of today's behaviour.
func TestCompositeScore_TotalParamsFavourMoEOverDense(t *testing.T) {
	moe := CompositeScore(35_000_000_000, 32*1024)   // 3.3B active
	dense := CompositeScore(27_000_000_000, 24*1024) // 27B active
	if moe <= dense {
		t.Errorf("total-params composite should place the 35B-A3B (%.2f) above the 27B dense (%.2f); "+
			"if this flipped, the note in the coefficient block is stale", moe, dense)
	}
}

func TestCompositeScore_Degenerate(t *testing.T) {
	if CompositeScore(0, 8000) != 0 {
		t.Error("zero params should give 0")
	}
	// footprint floor must not panic / produce -Inf.
	if got := CompositeScore(7_000_000_000, 0); got <= 0 {
		t.Errorf("footprint floor failed: %v", got)
	}
}
