package main

import "example/proto/hardware"

// Only Hostname and Model are written. UnifiedMemory, UsableVRAMMB and
// Vendor have a consumer on the control-plane side and no producer here
// — the whole point of the fixture.
func summary(host string) hardware.HardwareSummary {
	s := hardware.HardwareSummary{Hostname: host}
	s.GPUs = append(s.GPUs, hardware.GPU{Model: "unknown"})
	return s
}
