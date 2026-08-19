package main

import (
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
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

// TestOllamaTuning_NoMmapOnlyOnUnifiedMemory pins both halves of the mapping
// decision.
//
// On a unified host the GGUF mapping is charged to the OS-visible RAM half
// while the weights live in the carve-out, so a model the GPU pool holds can
// still pin free RAM at zero — measured at 21.6 GB of runner working set for a
// 22.6 GB fully GPU-resident model on a host with 31.6 GB visible
// (waired-ai/waired#762). Discrete and CPU-only hosts keep the mapping: system
// RAM is not scarce there and the mapping makes reloads cheap. A guard that
// fired everywhere would be a regression, not a fix, which is why the negative
// cases are asserted alongside the positive one.
func TestOllamaTuning_NoMmapOnlyOnUnifiedMemory(t *testing.T) {
	m := tuningTestManifest()
	v := m.Variants[0]

	cases := []struct {
		name string
		hw   hardware.Profile
		want bool
	}{
		{"carve-out uma host", umaCarveOutHost(), true},
		{"uma without a carve-out", hardware.Profile{RAMTotalGB: 128, UnifiedMemory: true, UsableVRAMMB: 98304}, true},
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

// TestWarmBudgetOutlastsTheEngineLoadDeadline pins the ordering the warm-up
// depends on.
//
// At a flat 4 minutes this budget sat below the engine's own 5-minute load
// deadline, so the warm-up cancelled loads the engine was still willing to
// finish — and a cancelled load leaves nothing resident, so the next real
// request paid the whole cost again (waired-ai/waired-agent#837). The
// relationship is what matters, not the digits: assert the ordering against
// the engine constant so moving either one keeps them consistent.
func TestWarmBudgetOutlastsTheEngineLoadDeadline(t *testing.T) {
	engine, err := time.ParseDuration(infruntime.OllamaLoadTimeout)
	if err != nil {
		t.Fatalf("engine load deadline %q is unparsable: %v", infruntime.OllamaLoadTimeout, err)
	}
	if warmBudget <= engine {
		t.Errorf("warmBudget = %v does not outlast the engine's %v load deadline; the warm-up gives up on loads the engine would still finish",
			warmBudget, engine)
	}
}

// TestWarmBudgetFrom_FallsBackWhenTheEngineValueIsUnusable keeps the derivation
// from turning a typo in the engine constant into a zero-length budget, which
// would cancel every warm-up instantly instead of merely too early.
func TestWarmBudgetFrom_FallsBackWhenTheEngineValueIsUnusable(t *testing.T) {
	for _, bad := range []string{"", "fifteen minutes", "0s", "-1m"} {
		if got := warmBudgetFrom(bad); got != 4*time.Minute {
			t.Errorf("warmBudgetFrom(%q) = %v, want the 4m floor", bad, got)
		}
	}
	if got := warmBudgetFrom("15m"); got != 16*time.Minute {
		t.Errorf("warmBudgetFrom(\"15m\") = %v, want 16m", got)
	}
}
