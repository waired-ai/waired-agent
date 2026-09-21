package management

import (
	"slices"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// The local catalog's serving windows follow the control plane's rule
// exactly (waired-ai/waired#1359), because the tray greys rows from one and
// the console from the other, and the two must grey the same rows. Swept
// over the shipped catalog so the rule is checked against real rope scaling,
// and asserted to exercise both arms so it cannot pass a function that
// always answers the same way.
func TestServingWindowsFor_MatchesHostfitAcrossTheCatalog(t *testing.T) {
	ms, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	reach, short := 0, 0
	for _, m := range ms {
		for _, engine := range []string{catalog.RuntimeOllama, catalog.RuntimeVLLM} {
			got := servingWindowsFor(m, engine)
			if len(got) == 0 || got[0] != hostfit.ServingWindow200k {
				t.Errorf("%s/%s = %v, want the coding window first", m.ModelID, engine, got)
				continue
			}
			want := hostfit.ReachesWindow(m, hostfit.ServingWindow1M) &&
				hostfit.EngineServesWindow(engine, hostfit.ServingWindow1M)
			if slices.Contains(got, hostfit.ServingWindow1M) != want {
				t.Errorf("%s/%s = %v, want 1M listed: %v", m.ModelID, engine, got, want)
			}
			if engine == catalog.RuntimeOllama {
				if want {
					reach++
				} else {
					short++
				}
			}
		}
	}
	if reach == 0 || short == 0 {
		t.Fatalf("catalog exercised only one arm: %d reach 1M, %d do not", reach, short)
	}
}
