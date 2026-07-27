//go:build windows

package hardware

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// nvidiaPCIVendorID is NVIDIA's PCI vendor ID, as it appears in a
// display adapter's MatchingDeviceId ("PCI\VEN_10DE&DEV_2489&...").
const nvidiaPCIVendorID = "VEN_10DE"

// NVML buffer sizes, from nvml.h. Oversized is harmless; undersized
// makes the call fail with NVML_ERROR_INSUFFICIENT_SIZE.
const (
	nvmlDeviceNameBufferSize   = 96
	nvmlDeviceUUIDBufferSize   = 96
	nvmlDriverVersionBufferLen = 80
)

// nvmlSuccess is NVML_SUCCESS. Every other return code is a failure we
// degrade past rather than report — a device we cannot read is handled
// by the next layer down.
const nvmlSuccess = 0

// nvidiaFallback answers "is an NVIDIA driver alive here" when
// nvidia-smi could not, using the two sources that need neither $PATH
// nor a CLI:
//
//  1. NVML (nvml.dll) — the same library ollama loads to discover GPUs,
//     so agreeing with it is the point. It reports live devices with
//     name, VRAM, compute capability and UUID.
//  2. the display-adapter registry — the OS's device inventory, used
//     only when NVML could not be loaded at all, and only when a driver
//     library is present: the registry outlives the card it describes,
//     so an entry with no driver behind it is a removed GPU, not a
//     detected one.
func nvidiaFallback(_ context.Context) nvidiaFallbackResult {
	if gpus, ok := nvidiaNVMLDevices(); ok {
		if len(gpus) == 0 {
			// NVML initialised and reported no device. As authoritative
			// as nvidia-smi's own empty answer — stay quiet.
			return nvidiaFallbackResult{}
		}
		return nvidiaFallbackResult{GPUs: gpus, AdapterSeen: true, Source: "nvml"}
	}

	adapters := windowsDisplayAdapters(nvidiaPCIVendorID, "nvidia")
	if len(adapters) == 0 {
		return nvidiaFallbackResult{}
	}
	if !nvidiaDriverLibraryPresent() {
		return nvidiaFallbackResult{
			AdapterSeen: true,
			Source:      "display-adapter registry; no NVIDIA driver library on this host",
		}
	}
	return nvidiaFallbackResult{GPUs: adapters, AdapterSeen: true, Source: "display-adapter registry"}
}

// nvidiaDriverLibraryPresent reports whether the NVIDIA CUDA driver
// library can be loaded. This is ollama's own presence test, and it is
// what separates "a card is installed" from "a card was once installed".
func nvidiaDriverLibraryPresent() bool {
	return windows.NewLazySystemDLL("nvcuda.dll").Load() == nil
}

// nvidiaNVMLDevices enumerates GPUs through NVML. The second return is
// false when NVML could not be used at all (library missing, init
// refused) — the caller then falls through to the registry. It is true
// with an empty slice when NVML worked and reported no device.
//
// Loaded lazily from System32 (NewLazySystemDLL, not NewLazyDLL, so the
// process working directory can never supply the library) and shut down
// before returning; every proc is resolved with Find() first because
// LazyProc.Call panics on a missing export.
func nvidiaNVMLDevices() ([]GPU, bool) {
	dll := windows.NewLazySystemDLL("nvml.dll")
	if err := dll.Load(); err != nil {
		return nil, false
	}
	procs, err := nvmlProcs(dll)
	if err != nil {
		return nil, false
	}
	if r, _, _ := procs["nvmlInit_v2"].Call(); r != nvmlSuccess {
		return nil, false
	}
	defer procs["nvmlShutdown"].Call() //nolint:errcheck // shutdown failure changes nothing

	var count uint32
	if r, _, _ := procs["nvmlDeviceGetCount_v2"].Call(uintptr(unsafe.Pointer(&count))); r != nvmlSuccess {
		return nil, false
	}

	driver := nvmlDriverVersion(procs)
	var out []GPU
	for i := uint32(0); i < count; i++ {
		gpu, ok := nvmlDevice(procs, i)
		if !ok {
			continue
		}
		gpu.DriverVersion = driver
		out = append(out, gpu)
	}
	// A non-zero count whose devices all failed to read is a broken NVML,
	// not an empty host: report the failure so the registry layer runs.
	if count > 0 && len(out) == 0 {
		return nil, false
	}
	return out, true
}

// nvmlProcs resolves every NVML entry point this file calls. All or
// nothing: a partial set would panic at the first missing export.
func nvmlProcs(dll *windows.LazyDLL) (map[string]*windows.LazyProc, error) {
	names := []string{
		"nvmlInit_v2",
		"nvmlShutdown",
		"nvmlDeviceGetCount_v2",
		"nvmlDeviceGetHandleByIndex_v2",
		"nvmlDeviceGetName",
		"nvmlDeviceGetMemoryInfo",
	}
	out := make(map[string]*windows.LazyProc, len(names))
	for _, n := range names {
		p := dll.NewProc(n)
		if err := p.Find(); err != nil {
			return nil, fmt.Errorf("nvml: %s: %w", n, err)
		}
		out[n] = p
	}
	// Optional extras: their absence costs a field, not the detection.
	for _, n := range []string{
		"nvmlSystemGetDriverVersion",
		"nvmlDeviceGetUUID",
		"nvmlDeviceGetCudaComputeCapability",
	} {
		p := dll.NewProc(n)
		if err := p.Find(); err == nil {
			out[n] = p
		}
	}
	return out, nil
}

// nvmlDevice reads one device. The second return is false when the
// handle or the two mandatory fields (name, memory) could not be read.
func nvmlDevice(procs map[string]*windows.LazyProc, index uint32) (GPU, bool) {
	var handle uintptr
	if r, _, _ := procs["nvmlDeviceGetHandleByIndex_v2"].Call(
		uintptr(index), uintptr(unsafe.Pointer(&handle)),
	); r != nvmlSuccess {
		return GPU{}, false
	}

	name := make([]byte, nvmlDeviceNameBufferSize)
	if r, _, _ := procs["nvmlDeviceGetName"].Call(
		handle, uintptr(unsafe.Pointer(&name[0])), uintptr(len(name)),
	); r != nvmlSuccess {
		return GPU{}, false
	}

	// nvmlMemory_t: three unsigned long long, in bytes.
	var mem struct{ Total, Free, Used uint64 }
	if r, _, _ := procs["nvmlDeviceGetMemoryInfo"].Call(
		handle, uintptr(unsafe.Pointer(&mem)),
	); r != nvmlSuccess {
		return GPU{}, false
	}

	gpu := GPU{
		Vendor:      "nvidia",
		Model:       nvmlString(name),
		VRAMTotalMB: int(mem.Total / (1024 * 1024)),
	}
	if p, ok := procs["nvmlDeviceGetUUID"]; ok {
		uuid := make([]byte, nvmlDeviceUUIDBufferSize)
		if r, _, _ := p.Call(handle, uintptr(unsafe.Pointer(&uuid[0])), uintptr(len(uuid))); r == nvmlSuccess {
			gpu.UUID = nvmlString(uuid)
		}
	}
	if p, ok := procs["nvmlDeviceGetCudaComputeCapability"]; ok {
		var major, minor int32
		if r, _, _ := p.Call(
			handle, uintptr(unsafe.Pointer(&major)), uintptr(unsafe.Pointer(&minor)),
		); r == nvmlSuccess {
			// Same "major.minor" shape nvidia-smi's compute_cap prints.
			gpu.ComputeCap = fmt.Sprintf("%d.%d", major, minor)
		}
	}
	return gpu, true
}

// nvmlDriverVersion reads the system-wide driver version, or "" when
// that export is unavailable or fails.
func nvmlDriverVersion(procs map[string]*windows.LazyProc) string {
	p, ok := procs["nvmlSystemGetDriverVersion"]
	if !ok {
		return ""
	}
	buf := make([]byte, nvmlDriverVersionBufferLen)
	if r, _, _ := p.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf))); r != nvmlSuccess {
		return ""
	}
	return nvmlString(buf)
}

// nvmlString trims an NVML NUL-terminated C buffer to a Go string.
func nvmlString(buf []byte) string {
	if i := bytes.IndexByte(buf, 0); i >= 0 {
		buf = buf[:i]
	}
	return strings.TrimSpace(string(buf))
}
