package mempressure

import (
	"fmt"

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
	v, err := unix.SysctlUint32(darwinPressureSysctl)
	if err != nil {
		return Facts{DarwinErr: fmt.Errorf("mempressure: sysctl %s: %w", darwinPressureSysctl, err)}
	}
	return Facts{DarwinLevel: v}
}

func (darwinSampler) close() error { return nil }
