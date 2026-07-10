package scoring

import "testing"

func TestCompositeScore_Monotonic(t *testing.T) {
	base := CompositeScore(30_000_000_000, 60, 24000)

	// More parameters → higher score (capability).
	if CompositeScore(80_000_000_000, 60, 24000) <= base {
		t.Error("higher params should raise the composite")
	}
	// Higher SWE-bench → higher score.
	if CompositeScore(30_000_000_000, 77, 24000) <= base {
		t.Error("higher swe_bench should raise the composite")
	}
	// Larger memory footprint → lower score (penalty).
	if CompositeScore(30_000_000_000, 60, 48000) >= base {
		t.Error("larger footprint should lower the composite")
	}
}

func TestCompositeScore_BenchmarkMattersVsSize(t *testing.T) {
	// A 27B dense model with a strong SWE-bench (77.2) should outrank a larger
	// 30B model with no benchmark — the directional behaviour the user asked
	// for (quality_tier reflects SWE-bench, not just size).
	strong := CompositeScore(27_000_000_000, 77.2, 24000)
	biggerNoBench := CompositeScore(30_530_000_000, 0, 24000)
	if strong <= biggerNoBench {
		t.Errorf("strong-benchmark 27B (%.1f) should outrank no-benchmark 30B (%.1f)", strong, biggerNoBench)
	}
}

func TestCompositeScore_Degenerate(t *testing.T) {
	if CompositeScore(0, 50, 8000) != 0 {
		t.Error("zero params should give 0")
	}
	// footprint floor must not panic / produce -Inf.
	if got := CompositeScore(7_000_000_000, 0, 0); got <= 0 {
		t.Errorf("footprint floor failed: %v", got)
	}
}
