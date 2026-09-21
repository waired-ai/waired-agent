package management

import (
	"reflect"
	"testing"

	"github.com/waired-ai/waired-agent/internal/hardware"
)

// A GPU the engine does not use by default reaches the catalog response
// by name, and never as the host's GPU (waired-agent#1484): the verdicts
// were computed for a CPU host, so the host line must describe one.
func TestHostFromProfile_UnusedGPUs(t *testing.T) {
	host := hostFromProfile(hardware.Profile{
		RAMTotalGB: 32,
		UnusedGPUs: []hardware.UnusedGPU{
			{GPU: hardware.GPU{Vendor: "amd", Model: "AMD Radeon 780M Graphics", VRAMTotalMB: 512}},
			{GPU: hardware.GPU{Vendor: "intel"}},
		},
	})
	if host.GPUModel != "" || host.VRAMTotalMB != 0 {
		t.Errorf("GPUModel=%q VRAMTotalMB=%d, want no GPU in use", host.GPUModel, host.VRAMTotalMB)
	}
	if want := []string{"AMD Radeon 780M Graphics", "intel"}; !reflect.DeepEqual(host.UnusedGPUModels, want) {
		t.Errorf("UnusedGPUModels = %v, want %v", host.UnusedGPUModels, want)
	}
}
