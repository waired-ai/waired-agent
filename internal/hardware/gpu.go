// GPU detection composes vendor-specific detectors. Each vendor lives
// in its own gpu_<vendor>.go file and supplies a VendorDetector. The
// composite defaultGPU iterates over vendorDetectors and OR-merges
// their Accelerators flags, joining errors via errors.Join.
//
// Adding a new vendor (Intel Arc, ...) means creating one detector
// file and appending the function to vendorDetectors.

package hardware

import (
	"context"
	"errors"
)

// VendorDetector probes one vendor's tooling and returns 0 GPUs when
// the vendor's binary/probe is absent (NOT an error) — matching the
// "missing nvidia-smi is non-fatal" contract from Step 2. The
// Accelerators returned reflect ONLY this vendor's flags;
// composeDetectors OR-merges them across all registered vendors.
type VendorDetector func(ctx context.Context) ([]GPU, Accelerators, error)

// vendorDetectors lists every GPU vendor waired-agent probes. The
// list is explicit (rather than init()-time registration) so a reader
// can enumerate all supported vendors by opening one file. Order is
// irrelevant for merge correctness; it only affects Profile.GPUs
// ordering when multiple vendors coexist on one host.
var vendorDetectors = []VendorDetector{
	detectNvidia, // gpu_nvidia.go
	detectAMD,    // gpu_amd.go (rocm-smi + Windows registry fallback)
	detectApple,  // gpu_apple_darwin.go (system_profiler) + gpu_apple_other.go (stub)
	// Future: detectIntel (xpu-smi) — append here.
}

// composeDetectors runs every detector and OR-merges their results.
// An error from one detector is collected via errors.Join but does
// NOT suppress other detectors' GPU results — the caller
// (profiler.Profile) surfaces the joined error via Profile.Errors
// while keeping whatever GPUs were successfully detected.
func composeDetectors(ctx context.Context, detectors []VendorDetector) ([]GPU, Accelerators, error) {
	var (
		gpus  []GPU
		accel Accelerators
		errs  []error
	)
	for _, d := range detectors {
		g, a, err := d(ctx)
		if err != nil {
			errs = append(errs, err)
		}
		gpus = append(gpus, g...)
		accel.CUDA = accel.CUDA || a.CUDA
		accel.ROCm = accel.ROCm || a.ROCm
		accel.Metal = accel.Metal || a.Metal
	}
	return gpus, accel, errors.Join(errs...)
}

// defaultGPU is the Profile builder's GPU hook (wired via
// NewProfiler's gpuFn field). It dispatches to every registered
// vendor detector.
func defaultGPU(ctx context.Context) ([]GPU, Accelerators, error) {
	return composeDetectors(ctx, vendorDetectors)
}
