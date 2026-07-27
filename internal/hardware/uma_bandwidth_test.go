package hardware

import "testing"

// TestUnifiedMemoryBandwidthGBs walks every table entry plus the shapes
// that decide whether the table is safe: the prefix collision, the
// normalisation inputs, and the unknown part.
func TestUnifiedMemoryBandwidthGBs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model string
		want  float64
	}{
		{"m1", "Apple M1", 68.25},
		{"m1 pro", "Apple M1 Pro", 200},
		{"m1 max", "Apple M1 Max", 400},
		{"m1 ultra", "Apple M1 Ultra", 800},

		{"m2", "Apple M2", 100},
		{"m2 pro", "Apple M2 Pro", 200},
		{"m2 max", "Apple M2 Max", 400},
		{"m2 ultra", "Apple M2 Ultra", 800},

		{"m3", "Apple M3", 100},
		{"m3 pro", "Apple M3 Pro", 150},
		{"m3 max", "Apple M3 Max", 400},
		{"m3 ultra", "Apple M3 Ultra", 819},

		{"m4", "Apple M4", 120},
		{"m4 pro", "Apple M4 Pro", 273},
		{"m4 max", "Apple M4 Max", 546},

		{"m5", "Apple M5", 153},

		// Strix Halo is a family match on the full marketing string.
		{"strix halo", "AMD Ryzen AI Max+ PRO 395 w/ Radeon 8060S", 256},
		{"strix halo plain", "AMD Ryzen AI Max 395", 256},

		// Normalisation: the same part reached through different probes.
		{"lowercase", "apple m4 max", 546},
		{"padded", "  Apple   M4   Max  ", 546},

		// Unknown parts. 0 is a normal answer — hostfit falls back to its
		// population constant and declines to exclude anything.
		{"unreleased apple part", "Apple M9 Ultra", 0},
		{"board id, not a chip name", "Mac16,10", 0},
		{"intel mac", "Intel(R) Core(TM) i7-9750H CPU @ 2.60GHz", 0},
		{"a discrete-GPU host's cpu", "AMD Ryzen 9 9950X 16-Core Processor", 0},
		{"empty", "", 0},

		// A non-Strix AMD APU must NOT borrow Strix Halo's figure: its
		// iGPU hangs off ordinary dual-channel system memory, nowhere near
		// 256 GB/s. IsAMDMobileAPU recognises these separately and this
		// table deliberately has no entry for them.
		{"amd mobile apu is not strix halo", "AMD Ryzen 7 7840U w/ Radeon 780M Graphics", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := UnifiedMemoryBandwidthGBs(tc.model); got != tc.want {
				t.Errorf("UnifiedMemoryBandwidthGBs(%q) = %v, want %v", tc.model, got, tc.want)
			}
		})
	}
}

// TestUnifiedMemoryBandwidth_SuffixedPartsDoNotMatchTheBase is the bug
// this table is most likely to grow: "Apple M4 Max" contains "Apple M4",
// so any move to prefix or substring matching silently judges every Max
// and Pro as a base part — an UNDER-estimate, which is the direction that
// withholds models the machine runs.
func TestUnifiedMemoryBandwidth_SuffixedPartsDoNotMatchTheBase(t *testing.T) {
	for _, base := range []string{"Apple M1", "Apple M2", "Apple M3", "Apple M4"} {
		baseGBs := UnifiedMemoryBandwidthGBs(base)
		if baseGBs == 0 {
			t.Fatalf("%q is missing from the table; this test can no longer detect the collision", base)
		}
		for _, suffix := range []string{" Pro", " Max", " Ultra"} {
			variant := base + suffix
			got := UnifiedMemoryBandwidthGBs(variant)
			if got == 0 {
				continue // not a shipping part (e.g. M4 Ultra); nothing to collide
			}
			if got == baseGBs {
				t.Errorf("%q resolved to the base part's %v GB/s — the lookup is matching "+
					"a prefix, which under-estimates every larger part", variant, baseGBs)
			}
			if got < baseGBs {
				t.Errorf("%q = %v GB/s, below the base %q at %v — a larger part cannot "+
					"have less bandwidth; check the table",
					variant, got, base, baseGBs)
			}
		}
	}
}

// TestUnifiedMemoryBandwidth_FiguresArePlausible guards the table against
// a units slip or a transposed digit, which a per-entry test cannot catch
// because it asserts the same wrong constant it documents. The bounds are
// the shipping population's real extremes.
func TestUnifiedMemoryBandwidth_FiguresArePlausible(t *testing.T) {
	const (
		lowest  = 68.25 // Apple M1 base
		highest = 819.0 // Apple M3 Ultra
	)
	for name, gbs := range appleUnifiedBandwidthGBs {
		if gbs < lowest || gbs > highest {
			t.Errorf("%s = %v GB/s, outside the shipping unified population (%v..%v). "+
				"A figure outside this range is far more likely a units slip than a "+
				"real part — the value must be the vendor's published peak in GB/s",
				name, gbs, lowest, highest)
		}
	}
	if strixHaloBandwidthGBs < lowest || strixHaloBandwidthGBs > highest {
		t.Errorf("strixHaloBandwidthGBs = %v, outside %v..%v", strixHaloBandwidthGBs, lowest, highest)
	}
}

// TestUnifiedBandwidthFor pins the profiler's gate: the figure describes
// the UNIFIED pool, so a host the UMA hook did not classify as unified
// must not carry one — hostfit would be reasoning about a different
// memory term there entirely.
func TestUnifiedBandwidthFor(t *testing.T) {
	for _, tc := range []struct {
		name     string
		unified  bool
		cpuModel string
		want     float64
	}{
		{"unified apple part", true, "Apple M4 Max", 546},
		{"unified strix halo", true, "AMD Ryzen AI Max 395", 256},
		{"unified but unknown part", true, "Apple M9 Ultra", 0},

		// The case that matters: a Strix Halo whose iGPU was never
		// enumerated (Linux without rocm-smi) leaves UnifiedMemory false
		// and is judged CPU-only. Publishing a 256 GB/s unified figure
		// there would describe memory the fit rule is not using.
		{"strix halo whose igpu was not detected", false, "AMD Ryzen AI Max 395", 0},
		{"discrete host", false, "AMD Ryzen 9 9950X 16-Core Processor", 0},
		{"apple string on a non-unified host", false, "Apple M4", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := unifiedBandwidthFor(tc.unified, tc.cpuModel); got != tc.want {
				t.Errorf("unifiedBandwidthFor(%v, %q) = %v, want %v",
					tc.unified, tc.cpuModel, got, tc.want)
			}
		})
	}
}
