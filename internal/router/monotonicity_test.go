package router

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// monotonicityHosts is a RAM × VRAM grid, walked as a ladder: for each
// RAM figure the card only ever grows, so anything that shrinks along a
// row is the picker punishing a machine for better hardware.
func monotonicityHosts() (rams []int, vrams []int) {
	return []int{8, 16, 32, 64, 128, 256},
		[]int{0, 2048, 4096, 8192, 12288, 16303, 24564, 49152}
}

func hostWithCard(ramGB, vramMB int) hardware.Profile {
	hw := hardware.Profile{OS: "linux", Arch: "x86_64", RAMTotalGB: ramGB}
	if vramMB > 0 {
		hw.GPUs = []hardware.GPU{{Vendor: "nvidia", Model: "test", VRAMTotalMB: vramMB}}
	}
	return hw
}

// TestCapacityIsMonotoneInHardware: more memory never admits fewer
// models. It is the invariant waired-ai/waired-agent#229 was opened for,
// asserted against the shipped catalog rather than a fixture, and it is
// the one property a capacity rule cannot be allowed to lose — adding
// memory to a machine must not take a model away from it.
//
// Ratifying source: waired-ai/waired#1056 decision 1 (2026-08-03),
// which makes capacity a computation over total memory. The sum is
// monotone in both terms by construction; this catches a future rule that
// reintroduces a comparison against something else.
func TestCapacityIsMonotoneInHardware(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	rams, vrams := monotonicityHosts()

	admits := func(hw hardware.Profile) map[string]bool {
		out := map[string]bool{}
		for _, m := range manifests {
			for _, v := range m.Variants {
				if !engineSupports(v, catalog.RuntimeOllama) {
					continue
				}
				if hostfit.OllamaCapacityFit(m, v, hw.HostFit()).Fits {
					out[m.ModelID+"/"+v.VariantID] = true
				}
			}
		}
		return out
	}

	for _, ramGB := range rams {
		prev := admits(hostWithCard(ramGB, 0))
		for _, vramMB := range vrams[1:] {
			cur := admits(hostWithCard(ramGB, vramMB))
			for id := range prev {
				if !cur[id] {
					t.Errorf("RAM %d GB: %s is admitted with a %d MB card but not with the "+
						"next size up. Adding memory removed a model",
						ramGB, id, vramMB)
				}
			}
			prev = cur
		}
	}
}

// TestInstallPickIsMonotoneOnceRecommended: growing the card never
// lowers a RECOMMENDED pick.
//
// The qualifier is the whole content of the test, and it is not a
// weakening for convenience. Raw quality tier is NOT monotone in VRAM,
// and cannot be while the recommendation is window-declarability:
//
//	RAM 8 GB + 2 GB card → qwen3.5-9b, tier 52, serving a 32k window
//	RAM 8 GB + 8 GB card → qwen3.5-4b, tier 42, serving the full 200k
//
// The first is a FALL-THROUGH — nothing on that host declares the coding
// window, so the recommendation pass empties and tier order picks the
// biggest thing that fits. The second is an actual recommendation. The
// bigger card lowered the tier and raised the answer, because a tier-52
// model that truncates at 32k cannot hold a coding session and a tier-42
// one that holds 200k can.
//
// So the ladder that must not sag is the recommended one: once a host is
// being given a model it can genuinely serve, more memory may only
// improve it.
func TestInstallPickIsMonotoneOnceRecommended(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	rams, vrams := monotonicityHosts()

	pick := func(hw hardware.Profile) (id string, tier int, recommended bool) {
		t.Helper()
		above, ok, err := SelectInstallModel(PickInput{
			Catalog: manifests, Hardware: hw, Engine: catalog.RuntimeOllama,
		}, InstallQualityFloorTier)
		if err != nil {
			t.Fatalf("SelectInstallModel: %v", err)
		}
		if !ok {
			return "", 0, false
		}
		return above[0].Manifest.ModelID, above[0].Variant.QualityTier, above[0].Recommendation.Fits
	}

	var exercised bool
	for _, ramGB := range rams {
		prevID, prevTier, prevRec := "", -1, false
		for _, vramMB := range vrams[1:] {
			id, tier, rec := pick(hostWithCard(ramGB, vramMB))
			switch {
			case prevRec && !rec:
				t.Errorf("RAM %d GB: a %d MB card installs %s unrecommended, where the "+
					"smaller card was recommended %s. A bigger card must not turn a model "+
					"the host can serve into one it cannot", ramGB, vramMB, id, prevID)
			case prevRec && tier < prevTier:
				t.Errorf("RAM %d GB: a %d MB card installs %s (tier %d) where the smaller "+
					"card installed %s (tier %d), both recommended. A bigger card must not "+
					"lower the pick", ramGB, vramMB, id, tier, prevID, prevTier)
			case prevRec:
				exercised = true
			}
			prevID, prevTier, prevRec = id, tier, rec
		}
	}
	if !exercised {
		t.Error("no row in the sweep had a recommended pick on two consecutive card sizes, " +
			"so this test never compared anything — re-pick the grid")
	}
}

// TestDeclaredWindowIsMonotoneInVRAM: growing the card never takes away a
// window the machine could already declare.
//
// The window sizing is where the inversion actually lived. Its budget is
// the card's memory, so a card SMALLER than the system RAM behind it
// shrinks the budget the window is sized from — which is why
// hostfit.OllamaPlannedWindow floors at what the same machine would reach
// with the accelerator removed. Without that floor this test fails on
// every row where a card is smaller than RAM, which is most of them.
func TestDeclaredWindowIsMonotoneInVRAM(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	rams, vrams := monotonicityHosts()

	for _, ramGB := range rams {
		for _, m := range manifests {
			for _, v := range m.Variants {
				if !engineSupports(v, catalog.RuntimeOllama) {
					continue
				}
				prev, prevVRAM := false, 0
				for _, vramMB := range vrams {
					hw := hostWithCard(ramGB, vramMB)
					got := hostfit.OllamaDeclaresWindow(
						m, v, hw.HostFit(), hostfit.ServingWindow200k)
					if prev && !got {
						t.Errorf("RAM %d GB, %s/%s: declares the coding window with a %d MB "+
							"card and not with a %d MB one. A bigger card took a window away",
							ramGB, m.ModelID, v.VariantID, prevVRAM, vramMB)
					}
					prev, prevVRAM = got, vramMB
				}
			}
		}
	}
}
