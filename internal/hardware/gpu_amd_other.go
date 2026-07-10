//go:build !windows

package hardware

import "context"

// amdWindowsFallback is the registry-probe stub for AMD GPU detection
// on non-Windows platforms. The Windows implementation lives in
// gpu_amd_windows.go.
//
// On Linux/Darwin the rocm-smi shellout is the only AMD detection
// path: users running AMD compute workloads invariably have ROCm
// installed (which ships rocm-smi), so there's no realistic scenario
// where a fallback would fire. On Darwin AMD discrete GPUs are
// effectively retired (Apple Silicon era); only Metal applies, and
// that is a separate VendorDetector.
func amdWindowsFallback(_ context.Context) []GPU { return nil }
