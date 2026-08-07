package router

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
)

// Decision record 20260805/1620 (decision 6): a speed verdict may NOT be
// routed through the recommendation gate. It has to reach
// SelectInstallModel's ok=false, which is why hostfit.HostProbe returns a
// verdict for a CALLER to act on rather than a candidate filter.
//
// The reason is structural, not stylistic. model_picker.go's narrow helper
// keeps the previous candidate set whenever a filter rejects everything
// (`if len(pass) > 0`), and SelectInstallModel stands the whole
// recommendation pass down when nothing clears the tier floor. Between
// them, NOTHING placed in that pass can turn an ok=true host into an
// ok=false one — so a cutoff placed there would be silently inert on
// exactly the hosts it exists to catch.
//
// This asserts the property directly: standing the gate down changes no
// host's verdict, over the real catalog. The cutoff's own arithmetic is
// tested next to it, in proto/hostfit/host_cutoff_test.go.
func TestRecommendGateCanNeverWithholdEveryModel(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	for _, ramGB := range []int{2, 4, 8, 16, 32, 64, 128} {
		hw := hardware.Profile{OS: "linux", Arch: "x86_64", RAMTotalGB: ramGB}
		in := PickInput{Catalog: manifests, Hardware: hw, Engine: catalog.RuntimeOllama}

		gated, gatedOK, err := SelectInstallModel(in, InstallQualityFloorTier)
		if err != nil {
			t.Fatalf("%d GB: SelectInstallModel: %v", ramGB, err)
		}
		ungated := in
		ungated.NoRecommendGate = true
		stoodDown, stoodDownOK, err := SelectInstallModel(ungated, InstallQualityFloorTier)
		if err != nil {
			t.Fatalf("%d GB: SelectInstallModel (gate stood down): %v", ramGB, err)
		}
		if gatedOK != stoodDownOK || len(gated) != len(stoodDown) {
			t.Fatalf("%d GB: the recommendation gate changed the verdict (ok %v→%v, %d→%d candidates). "+
				"If that is now possible the host cutoff could live there; until then it must not.",
				ramGB, stoodDownOK, gatedOK, len(stoodDown), len(gated))
		}
	}
}
