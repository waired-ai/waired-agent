package mempressure

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// darwinPressureSysctl is Apple's own ladder: 1 normal, 2 warn, 4 critical,
// the same values a DISPATCH_SOURCE_TYPE_MEMORYPRESSURE source delivers.
const darwinPressureSysctl = "kern.memorystatus_vm_pressure_level"

type darwinSampler struct{}

func newPlatformSampler() platformSampler { return darwinSampler{} }

// facts reads the sysctl directly rather than shelling out to sysctl(8) the
// way internal/hardware's profiler does. That one runs once per profile;
// this one runs on a ticker for the length of a model load, and a
// subprocess per tick would be its own load on a host already short of
// memory.
func (darwinSampler) facts() Facts {
	f := Facts{SwapOutTotalMB: swapUsedMB(), SwapOutMBPerSec: -1}
	f.AvailMB, f.TotalMB = availTotalMB()
	v, err := unix.SysctlUint32(darwinPressureSysctl)
	if err != nil {
		f.DarwinErr = fmt.Errorf("mempressure: sysctl %s: %w", darwinPressureSysctl, err)
		return f
	}
	f.DarwinLevel = v
	return f
}

// availTotalMB reads hw.memsize for the total, and derives what is left from
// kern.memorystatus_level, which macOS publishes as the percentage still
// available. Measured on an idle 16 GiB host: 70.
//
// A percentage rather than a page count because the page-count route is
// host_statistics64, which needs a mach call this package would otherwise
// have no reason to carry. The figure only has to decide whether the host is
// under a few per cent, and a percentage answers that directly.
func availTotalMB() (avail, total uint64) {
	bytes, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0, 0
	}
	total = bytes / (1 << 20)
	pct, err := unix.SysctlUint32("kern.memorystatus_level")
	if err != nil {
		return 0, total
	}
	return total * uint64(pct) / 100, total
}

// xswUsage is struct xsw_usage from <sys/sysctl.h>: the swap file's totals
// in bytes, plus the page size and whether it is encrypted.
type xswUsage struct {
	Total     uint64
	Avail     uint64
	Used      uint64
	PageSize  uint32
	Encrypted uint32
}

// swapUsedMB is how much swap is in use, in MB.
//
// It is what macOS offers cheaply, and it is NOT a count of bytes written:
// it falls when swap is released. rateFrom treats a fall as no swapping,
// which is the right reading — releasing swap is not swapping backwards —
// and an increase is swap-out, which is what the surge term is about.
//
// The distinction that matters here is between this figure's LEVEL and its
// RATE. The reference mac mini sat with 3,282 MB in use and swapped exactly
// nothing for five minutes; a trigger on the level would have fired on an
// idle desktop.
func swapUsedMB() float64 {
	raw, err := unix.SysctlRaw("vm.swapusage")
	if err != nil || len(raw) < int(unsafe.Sizeof(xswUsage{})) {
		return -1
	}
	u := (*xswUsage)(unsafe.Pointer(&raw[0]))
	return float64(u.Used) / (1 << 20)
}

func (darwinSampler) close() error { return nil }
