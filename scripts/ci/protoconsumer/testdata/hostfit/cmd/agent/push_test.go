package main

import "example/proto/hardware"

// A writer that lives only in a test must NOT count: a field written
// solely by test code is exactly the state #180 shipped in. This file
// exists so the guard's "non-test only" rule is actually exercised.
func fakeSummary() hardware.HardwareSummary {
	return hardware.HardwareSummary{
		UnifiedMemory: true,
		UsableVRAMMB:  24576,
		GPUs:          []hardware.GPU{{Vendor: "nvidia"}},
	}
}
