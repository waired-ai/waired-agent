package hardware

import "strings"

// What NVIDIA's own driver API says about a device, where it can be asked
// (waired-agent#1482).
//
// On Windows the CUDA driver library (nvcuda.dll) is loadable without cgo,
// and three of its calls answer the two questions nvidia-smi gets wrong on
// a single-pool part such as the RTX Spark N1X:
//
//   - cuDeviceGetAttribute(CU_DEVICE_ATTRIBUTE_INTEGRATED) — NVIDIA's own
//     predicate for "integrated", the one its guidance names, so the
//     classification no longer rests on the carve-out arithmetic alone;
//   - cuDeviceTotalMem — what CUDA can actually allocate. On the N1X that
//     is 46477 MiB, where nvidia-smi reports the 8128 MiB carve-out and
//     system RAM is 54.2 GiB (unsloth#11208, measured on the part): the
//     pool is smaller than RAM less the OS reserve, so it caps the budget.
//
// None of these creates a CUDA context (cuMemGetInfo would, and on a
// single-pool part that context is RAM the model then cannot have).
//
// Linux cannot do this: loading libcuda needs cgo, which the agent builds
// without. There the GB10 name table (nvidia_unified.go) stays the answer.

// cudaDevice is one device as the CUDA driver API reports it.
type cudaDevice struct {
	integrated bool
	totalMB    int
}

// cudaFactsFor answers for prof.GPUs[i] from the CUDA devices, but only
// where pairing them is certain: exactly one NVIDIA entry in the profile
// and exactly one CUDA device. CUDA's device order is not the order the
// profile's detectors produced (it defaults to fastest-first), and a
// reading attached to the wrong card is worse than none. Every single-
// pool part known today is a one-GPU machine.
func cudaFactsFor(prof *Profile, i int, devs []cudaDevice) (cudaDevice, bool) {
	if prof == nil || i < 0 || i >= len(prof.GPUs) || len(devs) != 1 {
		return cudaDevice{}, false
	}
	if !strings.EqualFold(prof.GPUs[i].Vendor, "nvidia") {
		return cudaDevice{}, false
	}
	n := 0
	for _, g := range prof.GPUs {
		if strings.EqualFold(g.Vendor, "nvidia") {
			n++
		}
	}
	if n != 1 {
		return cudaDevice{}, false
	}
	return devs[0], true
}
