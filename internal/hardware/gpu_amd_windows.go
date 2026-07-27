//go:build windows

package hardware

import "context"

// amdPCIVendorID is AMD's PCI vendor ID. The registry's
// MatchingDeviceId values for AMD display adapters consistently
// contain this substring (e.g. "PCI\VEN_1002&DEV_744C&...").
const amdPCIVendorID = "VEN_1002"

// amdWindowsFallback returns one GPU entry per AMD display adapter the
// OS knows about. VRAMTotalMB comes from the driver-supplied
// HardwareInformation values, which modern AMD drivers populate
// accurately for adapters >= 4 GiB (see readAdapterVRAMMB).
//
// It is called only after rocm-smi was found to be absent — the common
// Windows case, since ollama ships its own HIP runtime and desktop users
// rarely install the ROCm SDK. The registry walk itself is shared with
// the NVIDIA fallback; see gpu_windows_adapters.go for why the OS device
// inventory is a fallback rather than a first answer.
func amdWindowsFallback(_ context.Context) []GPU {
	return windowsDisplayAdapters(amdPCIVendorID, "amd")
}
