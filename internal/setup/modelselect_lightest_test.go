package setup

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
)

// unifiedProfile is a synthetic Apple-Silicon-class host: RAM and the GPU
// budget are the same memory, so the capacity gate reads UsableVRAMMB
// rather than RAMTotalGB less the OS allowance.
func unifiedProfile(ramGB int) hardware.Profile {
	return hardware.Profile{
		OS:            "darwin",
		Arch:          "arm64",
		RAMTotalGB:    ramGB,
		UnifiedMemory: true,
		UsableVRAMMB:  ramGB * 1024 * 3 / 4,
		GPUs:          []hardware.GPU{{Vendor: "apple", Model: "Apple (synthetic)"}},
		Accelerators:  hardware.Accelerators{Metal: true},
	}
}

// The smallest machine Waired serves, and with what, as of 2026-08-08.
//
// A record of today's catalog and today's pricing, not a contract — but a
// load-bearing one. #522's owner decision named qwen3.5-2b as the bottom
// rung of automatic selection, and this is where a catalog or pricing
// change that moves it shows up as a diff rather than as a support
// question.
//
// The cpu class has ONE host size that lands lower, and it is the
// window-declarability trade rather than a sag: at 7 GB the OS allowance
// leaves 5120 MiB, which holds qwen3.5-0.8b's full window but not
// qwen3.5-2b's, so 0.8b is the only model that can answer a coding
// session there. At 6 GB neither can, the recommendation pass empties,
// and tier order returns qwen3.5-2b as a best-effort pick. Same shape as
// the "8 GB RAM + 2 GB card -> tier 52 truncating / + 8 GB card -> tier 42
// holding" pair documented on
// router.TestInstallPickIsMonotoneOnceRecommended, and the RAM ladder is
// separately pinned monotone by router.TestInstallPickIsMonotoneInRAM.
//
// The sweep is deliberate rather than a hand-computed threshold. The
// boundary is a function of the served-window pricing (#552), the OS
// allowance and the shipped weights — three things that move
// independently, and #568 proposes measuring the third per host.
func TestSelectBundledModel_TheBottomRung(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}

	classes := []struct {
		name    string
		profile func(int) hardware.Profile
		want    string
	}{
		{"cpu", cpuProfile, "qwen3.5-0.8b"}, // 7 GB only; see the doc above
		{"unified", unifiedProfile, "qwen3.5-2b"},
	}

	for _, class := range classes {
		lightestID, lightestTier, lightestRAM := "", 0, 0
		served := 0

		for ramGB := 2; ramGB <= 128; ramGB++ {
			in := baseInputs(class.profile(ramGB), manifests)
			in.Inference.BundledModelID = "" // nothing pinned
			in.FreeDiskBytes = fixedDisk(500)
			sel, err := SelectBundledModel(in)
			if err != nil {
				t.Fatalf("%s %d GB: %v", class.name, ramGB, err)
			}
			if !sel.EnableInference || sel.ModelID == "" {
				continue
			}
			served++
			if tier := selectedTier(t, manifests, sel.ModelID); lightestID == "" || tier < lightestTier {
				lightestID, lightestTier, lightestRAM = sel.ModelID, tier, ramGB
			}
		}

		if served == 0 {
			t.Fatalf("%s: no host size in the 2-128 GB sweep got a model", class.name)
		}
		if lightestID != class.want {
			t.Errorf("%s: lightest installed model = %q (tier %d, at %d GB), want %q. "+
				"This is the smallest machine Waired serves and with what — confirm the "+
				"move is intended before updating the expectation",
				class.name, lightestID, lightestTier, lightestRAM, class.want)
		}
		t.Logf("%s: %d of 127 host sizes install a model; lightest is %s (tier %d) at %d GB",
			class.name, served, lightestID, lightestTier, lightestRAM)
	}
}

func selectedTier(t *testing.T, manifests []catalog.Manifest, modelID string) int {
	t.Helper()
	m, ok := catalog.LookupByAlias(modelID, manifests)
	if !ok {
		t.Fatalf("selected %q is not in the offered catalog", modelID)
	}
	best := 0
	for _, v := range m.Variants {
		if v.QualityTier > best {
			best = v.QualityTier
		}
	}
	return best
}
