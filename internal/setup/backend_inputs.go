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
	return ollamaBackendInputs(goos, prof)
}

// OllamaROCmOverlayWanted is whether an ollama install on this host fetches
// the ROCm overlay: one answer for the CLI's install on every OS and for
// the daemon's start-up converge, which used to install without it and so
// took ROCm off an AMD host at every pin move (waired-agent#1511).
//
// gpuMode is WAIRED_OLLAMA_GPU_MODE (install.ps1's -OllamaGpuMode), read on
// Windows only, as it always has been: 'rocm' asks for the overlay, and
// 'vulkan', 'cuda-only' and 'cpu-only' are all served by the base archive.
// Anything else leaves the answer to the host.
func OllamaROCmOverlayWanted(goos string, prof hardware.Profile, gpuMode string) bool {
	if goos == "windows" {
		switch strings.ToLower(strings.TrimSpace(gpuMode)) {
		case "rocm":
			return true
		case "vulkan", "cuda-only", "cpu-only":
			return false
		}
	}
	return infruntime.WantsROCmOverlay(ollamaBackendInputs(goos, prof))
}

func ollamaBackendInputs(goos string, prof hardware.Profile) infruntime.BackendInputs {
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
