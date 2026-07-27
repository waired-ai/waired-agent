//go:build !windows && !linux

package hardware

import "context"

// nvidiaFallback is the stub for platforms with no NVIDIA driver path.
//
// darwin: no NVIDIA driver has shipped since the Kepler-era web drivers
// were dropped, and none exists for Apple Silicon at all — Metal is a
// separate VendorDetector (gpu_apple_darwin.go). An explicit
// $WAIRED_NVIDIA_SMI still works through the shared resolution chain, so
// a hypothetical eGPU host is not locked out; there is simply nothing to
// probe for automatically.
//
// Returning the zero value keeps "no NVIDIA GPU" quiet here, which is
// the correct answer rather than a degraded one.
func nvidiaFallback(_ context.Context) nvidiaFallbackResult {
	return nvidiaFallbackResult{}
}
