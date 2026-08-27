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
//
// VRAMFreeMB is left 0 here, and cannot be filled from this source: every
// value under the display-class key is a static capability the driver
// published at install time (HardwareInformation.qwMemorySize and its
// siblings), so there is no used or free counter to read. The live
// equivalents on Windows are DXGI IDXGIAdapter3::QueryVideoMemoryInfo,
// D3DKMTQueryVideoMemoryInfo, or the GPU Adapter Memory perf counters,
// and this repository binds none of them.
//
// NVIDIA is not in the same position and the difference is the binding,
// not the vendor: gpu_nvidia_windows.go loads NVML directly and reads
// nvmlMemory_t.Free without any CLI. The AMD analogue would be amd_smi /
// rocm_smi64.dll or ADLX. Until one exists, an AMD Windows host without
// ROCm reports capacity only, and TightestGPUFreeMB abstains for it
// (waired-agent#1056).
func amdWindowsFallback(_ context.Context) []GPU {
	return windowsDisplayAdapters(amdPCIVendorID, "amd")
}
