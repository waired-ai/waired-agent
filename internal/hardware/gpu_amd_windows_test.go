//go:build windows

package hardware

import (
	"context"
	"testing"
)

// TestAMDWindowsFallback_NoCrash verifies the registry walk does not
// panic on any developer/CI host. The test runner may or may not
// have an AMD GPU installed; the contract is "returns a slice
// (possibly nil) without panicking, every returned entry has the
// AMD vendor tag set". The presence/absence of AMD hardware is not
// asserted because CI Windows runners do not ship one. VRAMTotalMB
// may now be either 0 (registry value missing on old drivers) or
// > 0 (modern drivers populate HardwareInformation.qwMemorySize),
// so that field is no longer asserted here.
//
// The walk itself now lives in gpu_windows_adapters.go and is shared
// with the NVIDIA fallback; see gpu_windows_adapters_test.go for its
// own tests.
func TestAMDWindowsFallback_NoCrash(t *testing.T) {
	gpus := amdWindowsFallback(context.Background())
	for i, g := range gpus {
		if g.Vendor != "amd" {
			t.Errorf("amdWindowsFallback()[%d].Vendor = %q, want amd", i, g.Vendor)
		}
		if g.VRAMTotalMB < 0 {
			t.Errorf("amdWindowsFallback()[%d].VRAMTotalMB = %d, want >= 0", i, g.VRAMTotalMB)
		}
	}
}
