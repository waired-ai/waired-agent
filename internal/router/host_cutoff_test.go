package router

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/proto/hostfit"
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
// This asserts the property directly, over the real catalog: the verdict
// tracks CAPACITY and nothing else. The cutoff's own arithmetic is
// tested next to it, in proto/hostfit/host_cutoff_test.go.
//
// It used to say the same thing by standing the recommendation gate down
// and comparing the two verdicts. The stand-down hatch is gone — it had
// no production writer, and proto/modelrank declined to publish knobs
// nobody turns (waired-agent#970/#972) — so the invariant is now stated
// positively instead. That is the stronger form: "the gate changes
// nothing" only says the gate is inert, while this names the one refusal
// that IS allowed to reach ok=false and forbids every other.
func TestRecommendGateCanNeverWithholdEveryModel(t *testing.T) {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	for _, ramGB := range []int{2, 4, 8, 16, 32, 64, 128} {
		hw := hardware.Profile{OS: "linux", Arch: "x86_64", RAMTotalGB: ramGB}
		in := PickInput{Catalog: manifests, Hardware: hw, Engine: catalog.RuntimeOllama}

		// What CAPACITY admits — the one refusal waired-ai/waired#1056
		// decision 1 reserves, and the only thing allowed to make a host
		// ok=false.
		admitted := false
		for _, m := range manifests {
			for _, v := range m.Variants {
				if engineSupports(v, catalog.RuntimeOllama) &&
					hostfit.OllamaCapacityFit(m, v, hw.HostFit()).Fits {
					admitted = true
				}
			}
		}

		_, ok, err := SelectInstallModel(in)
		if err != nil {
			t.Fatalf("%d GB: SelectInstallModel: %v", ramGB, err)
		}
		if ok != admitted {
			t.Fatalf("%d GB: SelectInstallModel ok=%v but capacity admits something=%v. "+
				"Some gate above capacity changed the verdict — if that is now possible "+
				"the host cutoff could live there; until then it must not.",
				ramGB, ok, admitted)
		}
	}
}
