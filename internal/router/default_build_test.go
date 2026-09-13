package router

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/runtime"
)

// PRODUCT CONTRACT (decision 6 of
// docs/decisions/20260913/2355-catalog-variant-kv-and-residency-rulings.md):
// a model named without a build is served as the owner's default build
// wherever the host is recommended it, and as the lighter build the host
// can hold wherever it is not (waired-agent#1265 must not come back).
func TestFamilyDefaultBuild(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	var moe catalog.Manifest
	for _, m := range manifests {
		if m.ModelID == "qwen3.6-35b-a3b" {
			moe = m
		}
	}
	if moe.ModelID == "" {
		t.Skip("qwen3.6-35b-a3b is no longer bundled")
	}
	ver := runtime.OllamaPinnedVersion
	nv := func(ramGB, vramMB int) hardware.Profile {
		return hardware.Profile{RAMTotalGB: ramGB, GPUs: []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: vramMB}}}
	}

	// Where the default is recommended it is the answer.
	big := FamilyDefaultBuild(moe, catalog.RuntimeOllama, ver, syntheticAppleUMA(64, 0))
	if !big.Fits || big.Variant.VariantID != moe.DefaultVariant[catalog.RuntimeOllama] {
		t.Errorf("64 GB unified serves %q, want the default %q", big.Variant.VariantID, moe.DefaultVariant[catalog.RuntimeOllama])
	}

	// Where it is not, the build the host can hold is.
	small := FamilyDefaultBuild(moe, catalog.RuntimeOllama, ver, nv(64, 16303))
	best := FamilyBestFit(moe, catalog.RuntimeOllama, ver, nv(64, 16303))
	if small.Variant.VariantID == moe.DefaultVariant[catalog.RuntimeOllama] {
		t.Fatalf("a 16 GB card serves the %s default, whose weights spill there (waired#986)", small.Variant.VariantID)
	}
	if small.Variant.VariantID != best.Variant.VariantID {
		t.Errorf("16 GB card serves %q, want FamilyBestFit's %q", small.Variant.VariantID, best.Variant.VariantID)
	}

	// A hand-picked default that is not the highest tier wins where it is
	// recommended — the case FamilyBestFit alone would answer differently.
	picked := moe
	picked.DefaultVariant = map[string]string{catalog.RuntimeOllama: "mtp-q3-gguf"}
	got := FamilyDefaultBuild(picked, catalog.RuntimeOllama, ver, syntheticAppleUMA(64, 0))
	if got.Variant.VariantID != "mtp-q3-gguf" {
		t.Errorf("a recommended hand-picked default resolved to %q, want mtp-q3-gguf", got.Variant.VariantID)
	}
	if FamilyBestFit(picked, catalog.RuntimeOllama, ver, syntheticAppleUMA(64, 0)).Variant.VariantID == "mtp-q3-gguf" {
		t.Error("anti-vacuity: FamilyBestFit already picks the hand-picked default, so this case proves nothing")
	}

	// No default recorded, or one the engine cannot serve: unchanged.
	none := moe
	none.DefaultVariant = nil
	if a, b := FamilyDefaultBuild(none, catalog.RuntimeOllama, ver, syntheticAppleUMA(64, 0)), FamilyBestFit(none, catalog.RuntimeOllama, ver, syntheticAppleUMA(64, 0)); a.Variant.VariantID != b.Variant.VariantID {
		t.Errorf("no default: %q, want FamilyBestFit's %q", a.Variant.VariantID, b.Variant.VariantID)
	}
}
