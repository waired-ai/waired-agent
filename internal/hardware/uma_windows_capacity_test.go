package hardware

import (
	"testing"

	"github.com/waired-ai/waired-agent/proto/catalog"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// windowsStrixHalo builds the hostfit.Host a Windows Ryzen AI Max host
// produces, through the same helper the Windows profiler calls. It goes
// through strixHaloUMA rather than assigning UsableVRAMMB directly so a
// change to the rule reaches these assertions; a hand-written Host would
// keep passing after the producer stopped agreeing with it.
func windowsStrixHalo(ramTotalGB, ramAvailableAtInstallGB, carveOutReadingMB int) hostfit.Host {
	p := Profile{
		OS:                      "windows",
		Arch:                    "x86_64",
		CPU:                     CPUInfo{Model: "AMD RYZEN AI MAX+ 395 w/ Radeon 8060S"},
		RAMTotalGB:              ramTotalGB,
		RAMAvailableAtInstallGB: ramAvailableAtInstallGB,
		UnifiedMemory:           true,
		GPUs: []GPU{{
			Vendor: "amd", Model: "AMD Radeon(TM) 8060S Graphics",
			VRAMTotalMB: carveOutReadingMB,
		}},
	}
	p.UsableVRAMMB, p.CarveOutVRAMMB = strixHaloUMA(
		"windows", carveOutReadingMB, ramTotalGB, ramAvailableAtInstallGB)
	return p.HostFit()
}

func bundledVariant(t *testing.T, modelID, variantID string) (catalog.Manifest, catalog.Variant) {
	t.Helper()
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	for _, m := range manifests {
		if m.ModelID != modelID {
			continue
		}
		for _, v := range m.Variants {
			if v.VariantID == variantID {
				return m, v
			}
		}
		t.Fatalf("catalog has %s but not its %s variant", modelID, variantID)
	}
	t.Fatalf("catalog has no %s", modelID)
	return catalog.Manifest{}, catalog.Variant{}
}

// TestWindowsCarveOutHost_CapacityMatchesTheLoadPath is the end-to-end
// half of waired-ai/waired-agent#863: it asserts the verdicts the two
// measured configurations of one real host should now get, using the
// shipped catalog numbers rather than fixtures.
//
// A record of what was measured on that host, not a platform contract.
// The measurement (issue #863) changed only the AMD Variable Graphics
// Memory size:
//
//	96 GB carve-out  -> OS saw 31.65 GB; a 76.3 GB model failed after 27.9 min
//	512 MB carve-out -> OS saw 127.15 GB; the same model loaded in 15.0 s
//
// The kvFactor is 0.5 because the tuner selects q8_0 whenever the host
// has a GPU budget (planOllamaKV), which every row here does.
func TestWindowsCarveOutHost_CapacityMatchesTheLoadPath(t *testing.T) {
	const kvFactorQ8 = 0.5

	big, bigVariant := bundledVariant(t, "qwen3.5-122b-a10b", "q4-gguf")
	// The model the 2026-06 host review ruled the default for this
	// machine class, measured at 22.6 GB resident and 74.27 tok/s on the
	// 96 GB-carve-out configuration.
	ruled, ruledVariant := bundledVariant(t, "qwen3.6-35b-a3b", "mtp-q4-gguf")

	t.Run("96 GB carve-out refuses the model that could not load", func(t *testing.T) {
		h := windowsStrixHalo(31, 0, 96*1024)

		if got := h.CarveOutVRAMMB; got != 0 {
			t.Fatalf("CarveOutVRAMMB = %d, want 0: TotalMemoryMB would add it to RAM", got)
		}
		fit := hostfit.OllamaCapacityFit(big, bigVariant, h)
		if fit.Fits || fit.Reason != hostfit.ReasonInsufficientMemory {
			t.Errorf("OllamaCapacityFit(%s) = {Fits:%v Reason:%q need %d have %d}, "+
				"want a memory refusal: this configuration thrashed for 27.9 minutes",
				big.ModelID, fit.Fits, fit.Reason, fit.NeedMB, fit.HaveMB)
		}
	})

	t.Run("96 GB carve-out keeps the model that did run", func(t *testing.T) {
		h := windowsStrixHalo(31, 0, 96*1024)

		if fit := hostfit.OllamaCapacityFit(ruled, ruledVariant, h); !fit.Fits {
			t.Errorf("OllamaCapacityFit(%s) = {Reason:%q need %d have %d}, want Fits: "+
				"this host ran it at 74.27 tok/s",
				ruled.ModelID, fit.Reason, fit.NeedMB, fit.HaveMB)
		}
		if rec := hostfit.OllamaRecommend(ruledVariant, h); !rec.Fits {
			t.Errorf("OllamaRecommend(%s) = {Reason:%q need %d have %d}, want Fits",
				ruled.ModelID, rec.Reason, rec.NeedMB, rec.HaveMB)
		}
		plan := hostfit.OllamaPlannedRung(ruled, ruledVariant, h, kvFactorQ8, 0)
		if plan.ExpectedSpillFraction != 0 {
			t.Errorf("ExpectedSpillFraction = %v at %d tokens, want 0: a unified host has "+
				"nowhere to spill to, so any spill would drop it below the coding floor",
				plan.ExpectedSpillFraction, plan.ContextLength)
		}
	})

	t.Run("512 MB carve-out runs the large model without a spill warning", func(t *testing.T) {
		h := windowsStrixHalo(127, 0, 512)

		if fit := hostfit.OllamaCapacityFit(big, bigVariant, h); !fit.Fits {
			t.Errorf("OllamaCapacityFit(%s) = {Reason:%q need %d have %d}, want Fits: "+
				"this configuration loaded it in 15.0 s at 26.32 tok/s",
				big.ModelID, fit.Reason, fit.NeedMB, fit.HaveMB)
		}
		if rec := hostfit.OllamaRecommend(bigVariant, h); !rec.Fits {
			t.Errorf("OllamaRecommend(%s) = {Reason:%q need %d have %d}, want Fits",
				big.ModelID, rec.Reason, rec.NeedMB, rec.HaveMB)
		}
		plan := hostfit.OllamaPlannedRung(big, bigVariant, h, kvFactorQ8, 0)
		if plan.ExpectedSpillFraction != 0 {
			t.Errorf("ExpectedSpillFraction = %v at %d tokens, want 0: the runner held "+
				"76.95 GB resident with 39.7 GB of the machine still free and no page-file use",
				plan.ExpectedSpillFraction, plan.ContextLength)
		}
	})
}
