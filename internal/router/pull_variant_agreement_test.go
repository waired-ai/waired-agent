package router

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// TestPickedVariantIsTheVariantPulled is a PRODUCT CONTRACT: the build a
// host is offered is the build it downloads (waired-agent#1265).
//
// The two answers used to be computed by different functions asking
// different questions. PickModel reads the host; FirstPullableVariant
// reads manifest order and calls it "author preference". That was
// indistinguishable from correct while every variant of a model weighed
// about the same, and stopped being so the moment one shipped a 12.6 GB
// build beside a 22.6 GB one: a 16 GB card was offered the light build
// and would have downloaded the heavy one, which loads by spilling
// 37.7 % of its weights into system RAM — waired-ai/waired#986, on the
// host the light build exists for.
//
// Asserted over host SHAPES rather than the two that happen to expose it
// today, because what makes this break is a catalog edit, not a code
// edit: the next model to gain a lighter build reopens it everywhere.
func TestPickedVariantIsTheVariantPulled(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	engineVersion := runtime.OllamaPinnedVersion

	nv := func(ramGB, vramMB int) hardware.Profile {
		return hardware.Profile{
			RAMTotalGB: ramGB,
			GPUs:       []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: vramMB}},
		}
	}
	hosts := []struct {
		name string
		hw   hardware.Profile
	}{
		{"8 GB card", nv(64, 8188)},
		{"12 GB card", nv(64, 12288)},
		{"16 GB card", nv(64, 16303)},
		{"24 GB card", nv(64, 24467)},
		{"16 GB unified", syntheticAppleUMA(16, 0)},
		{"24 GB unified", syntheticAppleUMA(24, 0)},
		{"32 GB unified", syntheticAppleUMA(32, 0)},
		{"64 GB unified", syntheticAppleUMA(64, 0)},
		{"128 GB unified", syntheticAppleUMA(128, 0)},
		{"cpu-only 16 GB", hardware.Profile{RAMTotalGB: 16}},
		{"cpu-only 64 GB", hardware.Profile{RAMTotalGB: 64}},
	}

	// Anti-vacuity: at least one shape must be one where manifest order
	// gives a DIFFERENT answer, or this test is asserting that two
	// identical computations agree.
	manifestOrderWouldDiffer := 0

	for _, h := range hosts {
		t.Run(h.name, func(t *testing.T) {
			pick, err := PickModel(PickInput{
				Catalog: manifests, Hardware: h.hw,
				Engine: catalog.RuntimeOllama, EngineVersion: engineVersion,
			})
			if err != nil {
				t.Fatalf("PickModel: %v", err)
			}
			// What cmd/waired-agent's PullModel resolves to.
			best := FamilyBestFit(pick.Manifest, catalog.RuntimeOllama, engineVersion, h.hw)
			if !best.Fits {
				t.Fatalf("%s/%s was offered but FamilyBestFit says nothing of it fits",
					pick.Manifest.ModelID, pick.Variant.VariantID)
			}
			if best.Variant.VariantID != pick.Variant.VariantID {
				t.Errorf("offered %s/%s (%.1f GB) but would download %s (%.1f GB)",
					pick.Manifest.ModelID, pick.Variant.VariantID, pick.Variant.EstimatedWeightGB,
					best.Variant.VariantID, best.Variant.EstimatedWeightGB)
			}
			// And the build that arrives holds its weights where the
			// host can reach them — the property waired#986 is about,
			// stated directly so a future ordering change cannot satisfy
			// the equality above while both answers are wrong.
			if vd := hostfit.OllamaRecommendModel(pick.Manifest, best.Variant, h.hw.HostFit()); !vd.Fits {
				t.Errorf("the build this host downloads (%s/%s) is refused here: %s",
					pick.Manifest.ModelID, best.Variant.VariantID, vd.Reason)
			}
			if first, ok := FirstPullableVariant(pick.Manifest, catalog.RuntimeOllama, engineVersion); ok &&
				first.VariantID != pick.Variant.VariantID {
				manifestOrderWouldDiffer++
				t.Logf("manifest order would have downloaded %s (%.1f GB) instead of %s (%.1f GB)",
					first.VariantID, first.EstimatedWeightGB,
					pick.Variant.VariantID, pick.Variant.EstimatedWeightGB)
			}
		})
	}

	if manifestOrderWouldDiffer == 0 {
		t.Error("no shipped host shape is one where manifest order and the host disagree; " +
			"this test currently proves nothing and the catalog has changed under it")
	}
}
