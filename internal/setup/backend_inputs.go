package setup

import (
	"strings"

	"github.com/waired-ai/waired-agent/internal/hardware"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// OllamaBackendInputs projects a profile onto the facts the backend plan
// and the ROCm-overlay decision read (waired-agent#1492).
//
// One builder for the daemon's spawn and the installer's overlay
// decision, so the two cannot read the host differently — the overlay is
// fetched for exactly the plan the daemon will then launch. It reads
// only the GPUs the engine uses (Profile.GPUs): a GPU set aside as unused
// (#1484) must not bring the ROCm overlay with it.
func OllamaBackendInputs(goos string, prof hardware.Profile) infruntime.BackendInputs {
	in := infruntime.BackendInputs{
		GOOS:         goos,
		StrixHaloAPU: hardware.StrixHaloHost(&prof),
	}
	if len(prof.GPUs) > 0 {
		in.PrimaryGPUVendor = strings.ToLower(prof.GPUs[0].Vendor)
	}
	for _, g := range prof.GPUs {
		if strings.EqualFold(g.Vendor, "amd") {
			in.AMDGPU = true
		}
	}
	return in
}
