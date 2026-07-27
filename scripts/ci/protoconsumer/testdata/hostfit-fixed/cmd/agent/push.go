package main

import "example/proto/hardware"

func summary(host string, prof profile) hardware.HardwareSummary {
	s := hardware.HardwareSummary{
		Hostname:      host,
		UnifiedMemory: prof.unified,
	}
	s.UsableVRAMMB = prof.vram
	for _, g := range prof.gpus {
		s.GPUs = append(s.GPUs, hardware.GPU{Vendor: g.vendor, Model: g.model})
	}
	return s
}

type profile struct {
	unified bool
	vram    int
	gpus    []struct{ vendor, model string }
}
