//go:build linux || darwin

package hardware

import "context"

// amdWindowsFallback is the registry-probe stub for AMD GPU detection
// on non-Windows platforms. The Windows implementation lives in
// gpu_amd_windows.go.
//
// On Linux the kernel's own sysfs is the primary path (amd_sysfs.go,
// waired-agent#1485) and rocm-smi the second; neither needs a registry.
// On Darwin AMD discrete GPUs are effectively retired (Apple Silicon
// era); only Metal applies, and that is a separate VendorDetector.
func amdWindowsFallback(_ context.Context) []GPU { return nil }
