package main

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
)

// umaCarveOutHost is the reported Ryzen AI Max+ 395: 128 GB installed, 96 GB
// handed to the iGPU at the firmware level, so the OS sees ~31.6 GB
// (waired-ai/waired-agent#837, waired-ai/waired#762).
func umaCarveOutHost() hardware.Profile {
	return hardware.Profile{
		RAMTotalGB:     31,
		UnifiedMemory:  true,
		UsableVRAMMB:   98304,
		CarveOutVRAMMB: 98304,
	}
}

// TestOllamaTuning_NoMmapOnlyOnCarveOutHosts pins every half of the mapping
// decision.
//
// A firmware carve-out is subtracted from what the OS can see, so the weights
// sit outside RAMTotalGB while the mapping is charged inside it — a genuine
// second copy in the smaller half, measured at 21.6 GB of runner working set
// for a 22.6 GB fully GPU-resident model on a host with 31.6 GB visible
// (waired-ai/waired#762).
//
// Unified memory alone is NOT that shape, and the negative cases are the point
// of this test rather than filler. Apple Silicon holds nothing back: measured
// on an M5 Pro / 48 GB, the same 25 GB model loaded in 17.1 s mapped and 15.3 s
// unmapped, both fully resident, runner RSS 24.5 vs 24.7 GB — one copy either
// way. Firing there would change which code path runs on every Mac and buy
// nothing. Discrete and CPU-only hosts keep the mapping too: system RAM is not
// scarce there and it makes reloads cheap.
func TestOllamaTuning_NoMmapOnlyOnCarveOutHosts(t *testing.T) {
	m := tuningTestManifest()
	v := m.Variants[0]

	cases := []struct {
		name string
		hw   hardware.Profile
		want bool
	}{
		{"carve-out uma host", umaCarveOutHost(), true},
		{"uma without a carve-out", hardware.Profile{RAMTotalGB: 128, UnifiedMemory: true, UsableVRAMMB: 98304}, false},
		// Apple Silicon as the profiler actually reports it: a synthesized
		// 75%-of-RAM budget and no carve-out at all, which is what
		// internal/hardware/profiler_darwin.go leaves unset on both branches.
		{"apple silicon 48gb", hardware.Profile{RAMTotalGB: 48, UnifiedMemory: true, UsableVRAMMB: 36864}, false},
		{"discrete card", discrete24GB(), false},
		{"cpu only", hardware.Profile{RAMTotalGB: 32}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := computeOllamaTuning(m, v, c.hw, "q8_0").NoMmap; got != c.want {
				t.Errorf("NoMmap = %v, want %v", got, c.want)
			}
		})
	}
}

// TestOllamaTuning_NoMmapSurvivesEverySizingBranch guards the placement rather
// than the value. Where the weights live is a fact about the host, not about
// the window sizing, but computeOllamaTuningOpts returns from three different
// places — an unknown sizing, a spilling plan, and the normal path. A decision
// written into only one of them would be silently absent exactly on the hosts
// too tight to size, which are the ones this is for.
func TestOllamaTuning_NoMmapSurvivesEverySizingBranch(t *testing.T) {
	hw := umaCarveOutHost()

	t.Run("unknown sizing", func(t *testing.T) {
		// No weight annotation and no KV annotation: the rung planner cannot
		// price anything, so the tuning returns before it sizes a window.
		m := catalog.Manifest{ModelID: "unsizable", ContextLength: 262144}
		v := catalog.Variant{VariantID: "q4"}
		got := computeOllamaTuning(m, v, hw, "q8_0")
		if got.ContextLength != 0 {
			t.Fatalf("ContextLength = %d, want 0 — this case is meant to take the unknown-sizing return", got.ContextLength)
		}
		if !got.NoMmap {
			t.Error("NoMmap = false on the unknown-sizing return; a host too tight to size still maps its weights into the small OS half")
		}
	})

	t.Run("model larger than the pool", func(t *testing.T) {
		// Weights beyond the carve-out: whatever the sizing decides about the
		// window, the mapping decision must already be recorded.
		m := catalog.Manifest{ModelID: "huge", ContextLength: 262144}
		v := catalog.Variant{VariantID: "q4", EstimatedWeightGB: 400, KVBytesPerTokenFP16: 24576}
		if !computeOllamaTuning(m, v, hw, "q8_0").NoMmap {
			t.Error("NoMmap = false for a model past the pool")
		}
	})
}
