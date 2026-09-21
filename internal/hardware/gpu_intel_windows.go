//go:build windows

package hardware

import "context"

// intelPCIVendorID is Intel's PCI vendor ID as it appears in the
// registry's MatchingDeviceId ("PCI\VEN_8086&DEV_E20B&...").
const intelPCIVendorID = "VEN_8086"

// intelWindowsAdapters returns one GPU entry per Intel display adapter,
// from the same registry walk the AMD and NVIDIA fallbacks use
// (gpu_windows_adapters.go). VRAMTotalMB is the driver's
// HardwareInformation figure: a discrete card's own memory, and for an
// integrated one a small dedicated figure that the carve-out arithmetic
// in integrated_windows.go recognises as a slice of RAM.
func intelWindowsAdapters(_ context.Context) []GPU {
	return windowsDisplayAdapters(intelPCIVendorID, "intel")
}

// intelVRAMFromOS has nothing to read on Windows: the registry walk above
// already carries the memory figure.
func intelVRAMFromOS(string, string) (int, bool) { return 0, false }
