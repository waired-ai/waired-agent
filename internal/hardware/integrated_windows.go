//go:build windows

package hardware

import (
	"unsafe"
)

// Windows's answer to the question in integrated.go.
//
// THE FACT: Windows publishes two totals that differ by what the
// firmware took away, and a GPU carve-out is one of the things it takes.
// Microsoft says so in GetPhysicallyInstalledSystemMemory's own doc:
//
//	"The amount of memory available to the operating system can be less
//	 than the amount of memory physically installed in the computer
//	 because the BIOS and some drivers may reserve memory as I/O regions
//	 for memory-mapped devices, making the memory unavailable to the
//	 operating system and applications."
//
// NVIDIA describes the same mechanism from the other side, for GB10 on
// Windows on Arm: "Dedicated GPU memory (carveout): Reserved for the
// iGPU and reported by Windows as dedicated GPU memory. The carveout is
// physically backed by system DRAM rather than separate VRAM."
//
// So an adapter whose reported memory is no larger than the firmware's
// whole deduction from installed RAM is an adapter whose memory came out
// of RAM. A discrete card's VRAM was never deducted from anything and is
// orders of magnitude larger than any firmware reservation.
//
// WHY AN INEQUALITY AND NOT AN EQUALITY. Measured on the Strix Halo
// reference host, 2026-09-20:
//
//	installed (SMBIOS)          128.00 GiB
//	visible (GlobalMemoryStatus) 127.15 GiB
//	qwMemorySize (carve-out)      0.50 GiB
//	installed - visible           0.85 GiB
//
// The deduction is 0.85 GiB where the carve-out is 0.50 — the other
// 0.35 GiB is firmware reserving memory for other things. An equality
// test would have failed on the configuration AMD itself recommends
// (a 0.5 GB reservation). The inequality holds in both configurations
// the host has been measured in: 0.50 <= 0.85 here, and 96 <= 96.35 in
// the 96 GB carve-out configuration of waired-agent#863.
//
// WHY IT ONLY EVER SAYS YES. The arithmetic can show that memory came
// out of RAM. It cannot show the opposite, because the figure it is
// handed is not always the carve-out: for an NVIDIA adapter the profiler
// prefers NVML's reading, and on a single-pool NVIDIA part that reading
// is the whole pool — which would fail the test and produce a CONFIDENT
// "discrete" for exactly the hardware waired-agent#459 is about. A
// one-sided rule cannot make that mistake, and it costs nothing: an
// unknown answer leaves UnifiedMemory false, which is what this platform
// does today anyway.

var procGetPhysicallyInstalledSystemMemory = modKernel32.NewProc("GetPhysicallyInstalledSystemMemory")

// physicallyInstalledBytes reports what SMBIOS says is fitted, or
// ok=false when the firmware tables are malformed (the API's own
// documented failure: it returns ERROR_INVALID_DATA when the installed
// figure is smaller than the figure the OS can see).
func physicallyInstalledBytes() (uint64, bool) {
	var kilobytes uint64
	r1, _, _ := procGetPhysicallyInstalledSystemMemory.Call(uintptr(unsafe.Pointer(&kilobytes)))
	if r1 == 0 || kilobytes == 0 {
		return 0, false
	}
	return kilobytes * 1024, true
}

// osVisibleBytes is GlobalMemoryStatusEx.TotalPhys, unrounded.
//
// defaultRAM above rounds to GiB for the capacity figures, which is
// right there and wrong here: the whole quantity this file works with is
// the sub-GiB difference between two totals, and rounding both would
// leave it anywhere between -1 and +1 GiB.
func osVisibleBytes() (uint64, bool) {
	var memStatus memoryStatusEx
	memStatus.Length = uint32(unsafe.Sizeof(memStatus))
	if r1, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&memStatus))); r1 == 0 {
		return 0, false
	}
	return memStatus.TotalPhys, true
}

// integratedFromOS is Windows's answer for prof.GPUs[i].
func integratedFromOS(prof *Profile, i int) integration {
	// NVIDIA's own answer first, where the CUDA driver API gives one:
	// CU_DEVICE_ATTRIBUTE_INTEGRATED is the predicate NVIDIA names for
	// exactly this question, and it answers both ways (#1482). The
	// arithmetic below can only ever say "integrated".
	if devs, ok := cudaDevicesFromOS(); ok {
		if d, ok := cudaFactsFor(prof, i, devs); ok {
			return integratedKnown(d.integrated)
		}
	}
	if i < 0 || i >= len(prof.GPUs) || prof.GPUs[i].VRAMTotalMB <= 0 {
		return integrationUnknown()
	}
	installed, ok := physicallyInstalledBytes()
	if !ok {
		return integrationUnknown()
	}
	visible, ok := osVisibleBytes()
	if !ok {
		return integrationUnknown()
	}
	return carvedFromSystemRAM(uint64(prof.GPUs[i].VRAMTotalMB)*1024*1024, installed, visible)
}
